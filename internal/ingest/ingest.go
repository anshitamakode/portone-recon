// Package ingest streams both Amazon reports into PostgreSQL, applies the mapping
// configs row by row, and accumulates the Summary while the data is being read.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/anshitamakode/portone-recon/internal/mapping"
	"github.com/anshitamakode/portone-recon/internal/money"
	"github.com/anshitamakode/portone-recon/internal/normalize"
)

// Result describes one ingest run.
type Result struct {
	RunID         int
	SettlementIDs []string
	Counts        map[string]int
	// Streaming[source][line_key] = amount, accumulated row by row during ingest
	// (in-scope only). Keys starting with "_unrouted:" are summary fields that do
	// not exist on the Summary sheet.
	Streaming map[string]map[string]money.Cents
	Warnings  []string
}

type row struct {
	id                                           int64
	fileID                                       int
	source                                       mapping.Source
	line                                         int
	kind                                         string
	raw                                          []byte
	settlementID, txType, desc, amtType, amtDesc string
	txnRef, sku, shipment, merchantOrder, adjID  string
	status                                       string
	posted, released                             time.Time
	refDate                                      string
	total                                        money.Cents
	inScope                                      bool
	scopeReason                                  string
}

type component struct {
	rowID    int64
	source   mapping.Source
	field    string
	amount   money.Cents
	configID int // 0 = none
	kind     string
	ref      string
	summary  string
	line     string
	isRecon  bool
	inScope  bool
}

type state struct {
	pay, set *mapping.Matcher
	routes   *mapping.Routes
	nextID   int64
	rows     []row
	comps    []component
	res      *Result
}

// Run ingests both files (settlement first: its settlement id defines the scope of
// the payments report) and replaces any previously ingested data.
func Run(ctx context.Context, db *sql.DB, paymentsPath, settlementsPath string) (*Result, error) {
	payRules, err := mapping.LoadFromDB(ctx, db, mapping.Payment)
	if err != nil {
		return nil, err
	}
	setRules, err := mapping.LoadFromDB(ctx, db, mapping.Settlement)
	if err != nil {
		return nil, err
	}
	if len(payRules) == 0 || len(setRules) == 0 {
		return nil, fmt.Errorf("mapping configs are empty: run `recon load-config` first")
	}
	routes, err := mapping.LoadRoutes(ctx, db)
	if err != nil {
		return nil, err
	}
	st := &state{
		pay:    mapping.NewMatcher(mapping.Payment, payRules),
		set:    mapping.NewMatcher(mapping.Settlement, setRules),
		routes: routes,
		nextID: 1,
		res: &Result{Counts: map[string]int{}, Streaming: map[string]map[string]money.Cents{
			string(mapping.Payment): {}, string(mapping.Settlement): {}}},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Idempotent re-ingestion: a run always replaces the previous one.
	if _, err := tx.ExecContext(ctx, `TRUNCATE recon_record, record_component, source_row, source_file, streaming_summary, ingest_run RESTART IDENTITY CASCADE`); err != nil {
		return nil, err
	}
	var fingerprint string
	if err := tx.QueryRowContext(ctx, `SELECT md5(
	    (SELECT COALESCE(string_agg(t::text, '|' ORDER BY id), '') FROM payment_config t) ||
	    (SELECT COALESCE(string_agg(t::text, '|' ORDER BY id), '') FROM settlement_config t))`).Scan(&fingerprint); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO ingest_run (config_fingerprint) VALUES ($1) RETURNING id`, fingerprint).Scan(&st.res.RunID); err != nil {
		return nil, err
	}

	scope, err := st.readSettlements(ctx, tx, settlementsPath)
	if err != nil {
		return nil, err
	}
	if err := st.readPayments(ctx, tx, paymentsPath, scope); err != nil {
		return nil, err
	}
	if err := st.flush(ctx, tx); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(scope))
	for id := range scope {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	st.res.SettlementIDs = ids
	if _, err := tx.ExecContext(ctx, `UPDATE ingest_run SET finished_at = now(), scope_settlement_ids = $2 WHERE id = $1`,
		st.res.RunID, pq.Array(ids)); err != nil {
		return nil, err
	}
	return st.res, tx.Commit()
}

func readFile(path string) ([]byte, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}

func insertFile(ctx context.Context, tx *sql.Tx, src mapping.Source, path, sha, preamble string, header []string) (int, error) {
	hj, _ := json.Marshal(header)
	var id int
	err := tx.QueryRowContext(ctx, `INSERT INTO source_file (source, file_name, sha256, preamble, header)
	    VALUES ($1,$2,$3,NULLIF($4,''),$5) RETURNING id`, string(src), filepath.Base(path), sha, preamble, hj).Scan(&id)
	return id, err
}

func rawJSON(header, rec []string) []byte {
	m := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(rec) {
			m[h] = rec[i]
		} else {
			m[h] = ""
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// readSettlements ingests the tab-separated settlement flat file. Returns the set
// of settlement ids it contains (the reconciliation scope).
func (st *state) readSettlements(ctx context.Context, tx *sql.Tx, path string) (map[string]bool, error) {
	b, sha, err := readFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(b))
	r.Comma = '\t'
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	col := index(header)
	for _, need := range []string{"settlement-id", "transaction-type", "amount-type", "amount-description", "amount", "posted-date"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("%s: missing column %q", path, need)
		}
	}
	fileID, err := insertFile(ctx, tx, mapping.Settlement, path, sha, "", header)
	if err != nil {
		return nil, err
	}
	scope := map[string]bool{}
	n := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		line, _ := r.FieldPos(0)
		get := func(c string) string {
			if i, ok := col[c]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		n++
		rw := row{fileID: fileID, source: mapping.Settlement, line: line, raw: rawJSON(header, rec),
			settlementID: get("settlement-id"), inScope: true}
		if get("transaction-type") == "" && get("amount-type") == "" {
			// the first data line of a settlement file is the settlement summary:
			// start/end/deposit dates and Amazon's own total-amount.
			rw.kind = "settlement_header"
			if rw.total, err = money.Parse(get("total-amount")); err != nil {
				return nil, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			if rw.posted, err = normalize.ParseSettlementTime(get("deposit-date")); err != nil {
				return nil, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			scope[rw.settlementID] = true
			st.addRow(&rw)
			st.res.Counts["settlement_header_rows"]++
			continue
		}
		rw.kind = "data"
		rw.txType, rw.amtType, rw.amtDesc = get("transaction-type"), get("amount-type"), get("amount-description")
		rw.txnRef, rw.sku, rw.shipment = get("order-id"), get("sku"), get("shipment-id")
		rw.merchantOrder, rw.adjID = get("merchant-order-id"), get("adjustment-id")
		if rw.posted, err = normalize.ParseSettlementTime(get("posted-date-time")); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		rw.released = rw.posted
		d, err := normalize.ParseSettlementDate(get("posted-date"))
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if d.IsZero() {
			d = rw.posted
		}
		rw.refDate = normalize.RefDate(d)
		if rw.total, err = money.Parse(get("amount")); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		scope[rw.settlementID] = true
		id := st.addRow(&rw)

		fields := map[string]string{"txn_ref": rw.txnRef, "sku": rw.sku, "date": rw.refDate,
			"settlement_id": rw.settlementID, "shipment_id": rw.shipment, "merchant_order_id": rw.merchantOrder,
			"adjustment_id": rw.adjID, "description": normalize.Key(rw.amtDesc)}
		rules, kind := st.set.MatchSettlement(rw.txType, rw.amtType, rw.amtDesc)
		st.addComponents(id, &rw, "amount", rw.total, rules, kind, fields, true)
		st.res.Counts["settlement_data_rows"]++
	}
	st.res.Counts["settlement_lines_read"] = n
	_, err = tx.ExecContext(ctx, `UPDATE source_file SET data_rows = $2 WHERE id = $1`, fileID, n)
	return scope, err
}

// payment report column -> config amount_field
var paymentAmountColumns = map[string]string{
	"product sales":              "product_sales",
	"shipping credits":           "shipping_credits",
	"gift wrap credits":          "gift_wrap_credits",
	"promotional rebates":        "promotional_rebates",
	"sales tax collected":        "sales_tax_collected",
	"low value goods":            "low_value_goods",
	"marketplace withheld tax":   "marketplace_withheld_tax",
	"selling fees":               "selling_fees",
	"fulfilment by amazon fees":  "fba_fees",
	"fulfillment by amazon fees": "fba_fees",
	"fba fees":                   "fba_fees",
	"other transaction fees":     "other_transaction_fees",
	"other":                      "other",
	"total":                      "total",
}

// readPayments ingests the payments (transaction) report CSV.
func (st *state) readPayments(ctx context.Context, tx *sql.Tx, path string, scope map[string]bool) error {
	b, sha, err := readFile(path)
	if err != nil {
		return err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var preamble []string
	var header []string
	for {
		rec, err := r.Read()
		if err != nil {
			return fmt.Errorf("%s: header not found: %w", path, err)
		}
		if len(rec) > 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "date/time") {
			header = rec
			break
		}
		preamble = append(preamble, strings.Join(rec, ","))
	}
	col := index(header)
	for _, need := range []string{"date/time", "settlement id", "type", "order id", "sku", "description", "total", "transaction status", "transaction release date"} {
		if _, ok := col[need]; !ok {
			return fmt.Errorf("%s: missing column %q", path, need)
		}
	}
	// amount columns in file order
	type amtCol struct {
		idx   int
		field string
	}
	var amounts []amtCol
	for i, h := range header {
		if f, ok := paymentAmountColumns[strings.ToLower(strings.TrimSpace(h))]; ok {
			amounts = append(amounts, amtCol{i, f})
		}
	}
	fileID, err := insertFile(ctx, tx, mapping.Payment, path, sha, strings.Join(preamble, "\n"), header)
	if err != nil {
		return err
	}
	n := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		line, _ := r.FieldPos(0)
		get := func(c string) string {
			if i, ok := col[c]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		n++
		rw := row{fileID: fileID, source: mapping.Payment, line: line, kind: "data", raw: rawJSON(header, rec),
			settlementID: get("settlement id"), txType: get("type"), desc: get("description"),
			txnRef: get("order id"), sku: get("sku"), status: get("transaction status")}
		if rw.posted, err = normalize.ParsePaymentTime(get("date/time")); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if rw.released, err = normalize.ParsePaymentTime(get("transaction release date")); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
		// The settlement report's posted-date is the date the money was RELEASED into
		// the settlement, in UTC. The payments report's date/time is the order posting
		// date in local time (GMT+9). So the record_ref `date` for a payment is the
		// release date converted to UTC (falls back to date/time if not released).
		refT := rw.released
		if refT.IsZero() {
			refT = rw.posted
		}
		rw.refDate = normalize.RefDate(refT)
		switch {
		case !scope[rw.settlementID]:
			rw.inScope, rw.scopeReason = false, "settlement "+rw.settlementID+" is not in the settlement report"
		case !strings.EqualFold(rw.status, "Released"):
			rw.inScope, rw.scopeReason = false, "status "+rw.status+" (not yet released into a settlement)"
		default:
			rw.inScope = true
		}
		vals := make([]money.Cents, len(amounts))
		var componentSum money.Cents
		for i, a := range amounts {
			if a.idx < len(rec) {
				v, err := money.Parse(rec[a.idx])
				if err != nil {
					return fmt.Errorf("%s line %d column %s: %w", path, line, header[a.idx], err)
				}
				vals[i] = v
				if a.field == "total" {
					rw.total = v
				} else {
					componentSum += v
				}
			}
		}
		if componentSum != rw.total {
			st.res.Counts["payment_rows_components_ne_total"]++
			st.res.Warnings = append(st.res.Warnings, fmt.Sprintf("payments line %d: components sum %s != total %s", line, componentSum, rw.total))
		}
		id := st.addRow(&rw)
		fields := map[string]string{"txn_ref": rw.txnRef, "sku": rw.sku, "date": rw.refDate,
			"settlement_id": rw.settlementID, "description": normalize.Key(rw.desc)}
		for i, a := range amounts {
			if vals[i] == 0 && a.field != "total" {
				continue // no money in this column for this row
			}
			rules, kind := st.pay.MatchPayment(rw.txType, rw.desc, a.field)
			st.addComponents(id, &rw, a.field, vals[i], rules, kind, fields, a.field == "total")
		}
		if rw.inScope {
			st.res.Counts["payment_rows_in_scope"]++
		} else {
			st.res.Counts["payment_rows_out_of_scope"]++
		}
	}
	st.res.Counts["payment_rows"] = n
	_, err = tx.ExecContext(ctx, `UPDATE source_file SET data_rows = $2 WHERE id = $1`, fileID, n)
	return err
}

func (st *state) addRow(r *row) int64 {
	r.id = st.nextID
	st.nextID++
	st.rows = append(st.rows, *r)
	return r.id
}

// addComponents maps one amount through the matched rule(s) and folds it into the
// streaming summary.
func (st *state) addComponents(rowID int64, rw *row, field string, amt money.Cents, rules []*mapping.Rule,
	kind string, fields map[string]string, reconCandidate bool) {
	src := string(rw.source)
	if len(rules) == 0 {
		st.comps = append(st.comps, component{rowID: rowID, source: rw.source, field: field, amount: amt,
			kind: mapping.KindUnmatched, ref: fmt.Sprintf("UNMAPPED:%s:L%d", src, rw.line),
			isRecon: reconCandidate, inScope: rw.inScope})
		if rw.inScope && amt != 0 {
			st.res.Counts["unmatched_components_in_scope"]++
		}
		return
	}
	for i, r := range rules {
		ref := mapping.BuildRef(rw.source, r.RecordRef, fields)
		if ref == "" {
			ref = fmt.Sprintf("NOREF:%s:L%d:cfg%d", src, rw.line, r.ID)
		}
		sf := r.SummaryField(amt)
		line, known := st.routes.Line(sf, amt > 0)
		c := component{rowID: rowID, source: rw.source, field: field, amount: amt, configID: r.ID, kind: kind,
			ref: ref, summary: sf, line: line, isRecon: reconCandidate && i == 0, inScope: rw.inScope}
		st.comps = append(st.comps, c)
		if !rw.inScope {
			continue
		}
		if kind == mapping.KindFallback && amt != 0 {
			st.res.Counts["fallback_components_in_scope"]++
		}
		// --- streaming summary: computed while ingesting, not after ---
		switch {
		case line != "":
			st.res.Streaming[src][line] += amt
		case sf != "" && !known:
			st.res.Streaming[src]["_unrouted:"+sf] += amt
		}
	}
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// flush bulk-loads rows and components with COPY, then the streaming summary.
func (st *state) flush(ctx context.Context, tx *sql.Tx) error {
	stmt, err := tx.PrepareContext(ctx, pq.CopyIn("source_row", "id", "file_id", "source", "line_no", "row_kind", "raw",
		"settlement_id", "transaction_type", "description", "amount_type", "amount_description", "txn_ref", "sku",
		"shipment_id", "merchant_order_id", "adjustment_id", "txn_status", "posted_at", "released_at", "ref_date",
		"total", "in_scope", "scope_reason"))
	if err != nil {
		return err
	}
	for _, r := range st.rows {
		if _, err := stmt.ExecContext(ctx, r.id, r.fileID, string(r.source), r.line, r.kind, string(r.raw),
			nz(r.settlementID), nz(r.txType), nz(r.desc), nz(r.amtType), nz(r.amtDesc), nz(r.txnRef), nz(r.sku),
			nz(r.shipment), nz(r.merchantOrder), nz(r.adjID), nz(r.status), nt(r.posted), nt(r.released), nz(r.refDate),
			r.total.String(), r.inScope, nz(r.scopeReason)); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('source_row','id'), (SELECT max(id) FROM source_row))`); err != nil {
		return err
	}

	stmt, err = tx.PrepareContext(ctx, pq.CopyIn("record_component", "source_row_id", "source", "amount_field", "amount",
		"config_id", "match_kind", "record_ref", "summary_field", "line_key", "is_recon_amount", "in_scope"))
	if err != nil {
		return err
	}
	for _, c := range st.comps {
		var cfg any
		if c.configID != 0 {
			cfg = c.configID
		}
		if _, err := stmt.ExecContext(ctx, c.rowID, string(c.source), c.field, c.amount.String(), cfg, c.kind, c.ref,
			nz(c.summary), nz(c.line), c.isRecon, c.inScope); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	st.res.Counts["components"] = len(st.comps)
	st.res.Counts["source_rows"] = len(st.rows)

	for src, lines := range st.res.Streaming {
		for line, amt := range lines {
			if _, err := tx.ExecContext(ctx, `INSERT INTO streaming_summary (run_id, source, line_key, amount, components)
			    VALUES ($1,$2,$3,$4,0)`, st.res.RunID, src, line, amt.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

func index(header []string) map[string]int {
	m := map[string]int{}
	for i, h := range header {
		m[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return m
}
