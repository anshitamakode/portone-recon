// Package report writes the reconciliation workbook:
//
//	Summary            the accounting summary in the layout of the sample report,
//	                   Payments vs Settlements, plus a tie-out to Amazon's own total
//	Consolidated Data  every in-scope record: reconciled, then unreconciled payments,
//	                   then unreconciled settlements, with an audit trail per row
//	Variance Drilldown every summary line that differs, broken down by config rule
//	Config Lint        static problems in the mapping configs
//	Run Info           scope, inputs, counts and integrity checks
package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/anshitamakode/portone-recon/internal/mapping"
	"github.com/anshitamakode/portone-recon/internal/money"
	"github.com/anshitamakode/portone-recon/internal/recon"
)

type line struct {
	key, section, label string
	row                 int
}

type styles struct {
	num, numBold, bold, header, title, sub, wrap, red, green, groupP, groupS, groupD, groupA int
}

func newStyles(f *excelize.File) (styles, error) {
	var s styles
	var err error
	numFmt := "#,##0.00;[Red]-#,##0.00;0.00"
	mk := func(st *excelize.Style) int {
		if err != nil {
			return 0
		}
		var id int
		id, err = f.NewStyle(st)
		return id
	}
	s.num = mk(&excelize.Style{CustomNumFmt: &numFmt})
	s.numBold = mk(&excelize.Style{CustomNumFmt: &numFmt, Font: &excelize.Font{Bold: true}})
	s.bold = mk(&excelize.Style{Font: &excelize.Font{Bold: true}})
	s.header = mk(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#D9E1F2"}},
		Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	s.title = mk(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 13}})
	s.sub = mk(&excelize.Style{Font: &excelize.Font{Italic: true, Color: "#595959"}})
	s.wrap = mk(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	s.red = mk(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "#C00000"}})
	s.green = mk(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "#2E7D32"}})
	s.groupP = mk(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#E2EFDA"}}})
	s.groupS = mk(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#FCE4D6"}}})
	s.groupD = mk(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#FFF2CC"}}})
	s.groupA = mk(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#EDEDED"}}})
	return s, err
}

// Write builds the workbook from the current database state.
func Write(ctx context.Context, db *sql.DB, out, label string) error {
	lines, err := loadLines(ctx, db)
	if err != nil {
		return err
	}
	f := excelize.NewFile()
	defer f.Close()
	st, err := newStyles(f)
	if err != nil {
		return err
	}
	if err := f.SetSheetName("Sheet1", "Summary"); err != nil {
		return err
	}
	tie, err := writeSummary(ctx, db, f, st, lines, label)
	if err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if _, err := f.NewSheet("Consolidated Data"); err != nil {
		return err
	}
	if err := writeConsolidated(ctx, db, f, st, lines); err != nil {
		return fmt.Errorf("consolidated: %w", err)
	}
	if _, err := f.NewSheet("Variance Drilldown"); err != nil {
		return err
	}
	if err := writeDrilldown(ctx, db, f, st); err != nil {
		return fmt.Errorf("drilldown: %w", err)
	}
	if _, err := f.NewSheet("Config Lint"); err != nil {
		return err
	}
	if err := writeLint(ctx, db, f, st); err != nil {
		return fmt.Errorf("lint: %w", err)
	}
	if _, err := f.NewSheet("Run Info"); err != nil {
		return err
	}
	if err := writeRunInfo(ctx, db, f, st, label, tie); err != nil {
		return fmt.Errorf("run info: %w", err)
	}
	f.SetActiveSheet(0)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return f.SaveAs(out)
}

func loadLines(ctx context.Context, db *sql.DB) ([]line, error) {
	rows, err := db.QueryContext(ctx, `SELECT line_key, section, label, sheet_row FROM summary_line ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.key, &l.section, &l.label, &l.row); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func cents(v sql.NullString) money.Cents {
	if !v.Valid {
		return 0
	}
	c, _ := money.Parse(v.String)
	return c
}

func cell(col, row int) string {
	n, _ := excelize.CoordinatesToCellName(col, row)
	return n
}

// tieOut is the proof that the summary agrees with what Amazon reports.
type tieOut struct {
	SettlementID, Period         string
	AmazonTotal                  money.Cents
	SumLines, InScope, NotRouted [2]money.Cents // [payment, settlement]
	NotRoutedNotes               []string
	StreamingMatches             bool
	StreamingDiffs               []string
}

func writeSummary(ctx context.Context, db *sql.DB, f *excelize.File, st styles, lines []line, label string) (tieOut, error) {
	const sh = "Summary"
	var t tieOut
	amounts := map[string][2]money.Cents{}
	rows, err := db.QueryContext(ctx, `SELECT line_key,
	        SUM(amount) FILTER (WHERE source='payment'), SUM(amount) FILTER (WHERE source='settlement')
	    FROM record_component WHERE in_scope AND line_key IS NOT NULL GROUP BY line_key`)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var k string
		var p, s sql.NullString
		if err := rows.Scan(&k, &p, &s); err != nil {
			rows.Close()
			return t, err
		}
		amounts[k] = [2]money.Cents{cents(p), cents(s)}
	}
	rows.Close()

	// tie-out figures
	var amazon, inP, inS sql.NullString
	var start, end sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT string_agg(settlement_id, ', '), SUM(total),
	        min(raw->>'settlement-start-date'), max(raw->>'settlement-end-date')
	    FROM source_row WHERE row_kind = 'settlement_header'`).Scan(&t.SettlementID, &amazon, &start, &end); err != nil {
		return t, err
	}
	t.AmazonTotal = cents(amazon)
	t.Period = strings.TrimSpace(start.String + " – " + end.String)
	if err := db.QueryRowContext(ctx, `SELECT
	        SUM(total) FILTER (WHERE source='payment'), SUM(total) FILTER (WHERE source='settlement')
	    FROM source_row WHERE in_scope AND row_kind = 'data'`).Scan(&inP, &inS); err != nil {
		return t, err
	}
	t.InScope = [2]money.Cents{cents(inP), cents(inS)}
	for _, l := range lines {
		t.SumLines[0] += amounts[l.key][0]
		t.SumLines[1] += amounts[l.key][1]
	}
	t.NotRouted = [2]money.Cents{t.InScope[0] - t.SumLines[0], t.InScope[1] - t.SumLines[1]}
	// explain the not-routed amount: rows whose total is not (fully) on a summary line
	nr, err := db.QueryContext(ctx, `
	    SELECT sr.source, COALESCE(sr.transaction_type, '') ||
	           CASE WHEN sr.transaction_type IN ('Order', 'Refund') THEN ''
	                ELSE ' · ' || COALESCE(sr.description, sr.amount_description, '') END AS what,
	           count(*), SUM(sr.total - COALESCE(x.routed, 0))::text
	    FROM source_row sr
	    LEFT JOIN (SELECT source_row_id, SUM(amount) AS routed FROM record_component
	               WHERE in_scope AND line_key IS NOT NULL GROUP BY 1) x ON x.source_row_id = sr.id
	    WHERE sr.in_scope AND sr.row_kind = 'data' AND sr.total <> COALESCE(x.routed, 0)
	    GROUP BY 1, 2 ORDER BY abs(SUM(sr.total - COALESCE(x.routed, 0))) DESC LIMIT 6`)
	if err != nil {
		return t, err
	}
	for nr.Next() {
		var src, what, amt string
		var n int
		if err := nr.Scan(&src, &what, &n, &amt); err != nil {
			nr.Close()
			return t, err
		}
		a, _ := money.Parse(amt)
		t.NotRoutedNotes = append(t.NotRoutedNotes, fmt.Sprintf("%s: %s (%d rows) %s", src, what, n, a))
	}
	nr.Close()

	// streaming (computed during ingest) must equal the SQL aggregation
	t.StreamingMatches = true
	srows, err := db.QueryContext(ctx, `SELECT source, line_key, amount FROM streaming_summary WHERE line_key NOT LIKE '\_%'`)
	if err != nil {
		return t, err
	}
	seen := map[string]bool{}
	for srows.Next() {
		var src, k string
		var a sql.NullString
		if err := srows.Scan(&src, &k, &a); err != nil {
			srows.Close()
			return t, err
		}
		i := 0
		if src == "settlement" {
			i = 1
		}
		seen[src+k] = true
		if amounts[k][i] != cents(a) {
			t.StreamingMatches = false
			t.StreamingDiffs = append(t.StreamingDiffs, fmt.Sprintf("%s %s streaming %s vs SQL %s", src, k, cents(a), amounts[k][i]))
		}
	}
	srows.Close()
	for k, v := range amounts {
		for i, src := range []string{"payment", "settlement"} {
			if v[i] != 0 && !seen[src+k] {
				t.StreamingMatches = false
				t.StreamingDiffs = append(t.StreamingDiffs, fmt.Sprintf("%s %s missing from streaming summary", src, k))
			}
		}
	}

	// ---- layout (rows/labels follow amazon_sample_output_report.xlsx) ----
	_ = f.SetColWidth(sh, "A", "A", 3)
	_ = f.SetColWidth(sh, "B", "B", 72)
	_ = f.SetColWidth(sh, "C", "E", 18)
	_ = f.SetColWidth(sh, "F", "F", 10)
	set := func(c string, v any, style int) {
		_ = f.SetCellValue(sh, c, v)
		if style != 0 {
			_ = f.SetCellStyle(sh, c, c, style)
		}
	}
	set("C1", "Payments", st.header)
	set("D1", "Settlements", st.header)
	set("E1", "Payments - Settlements", st.header)
	set("F1", "Check", st.header)
	caption := fmt.Sprintf("Amazon AU · settlement %s · %s · AUD", t.SettlementID, t.Period)
	if label != "" {
		caption += " · " + label
	}
	set("B2", caption, st.sub)

	sections := []struct {
		name string
		row  int
	}{{"Sales", 4}, {"Refunds", 15}, {"Expenses", 19}}
	writeRow := func(r int, labelText string, p, s money.Cents, bold bool) {
		ns := st.num
		ls := 0
		if bold {
			ns, ls = st.numBold, st.bold
		}
		set(cell(2, r), labelText, ls)
		set(cell(3, r), p.Float(), ns)
		set(cell(4, r), s.Float(), ns)
		set(cell(5, r), (p - s).Float(), ns)
		if p == s {
			set(cell(6, r), "OK", st.green)
		} else {
			set(cell(6, r), "DIFF", st.red)
		}
	}
	for _, sec := range sections {
		var p, s money.Cents
		for _, l := range lines {
			if l.section == sec.name {
				p += amounts[l.key][0]
				s += amounts[l.key][1]
				writeRow(l.row, l.label, amounts[l.key][0], amounts[l.key][1], false)
			}
		}
		writeRow(sec.row, sec.name, p, s, true)
	}
	for _, l := range lines {
		if l.section == "Paid To Amazon" {
			writeRow(l.row, l.label, amounts[l.key][0], amounts[l.key][1], true)
		}
	}
	for _, r := range []int{3, 14, 18, 29, 31, 33} {
		_ = f.MergeCell(sh, cell(1, r), cell(5, r))
	}

	// ---- tie-out block ----
	r := 35
	set(cell(2, r), "Tie-out to Amazon's settlement total", st.title)
	r++
	writeRow(r, "Total of all summary lines (Sales + Refunds + Expenses + Paid To Amazon)", t.SumLines[0], t.SumLines[1], true)
	r++
	set(cell(2, r), fmt.Sprintf("Amazon settlement total-amount (settlement report header, %s)", t.SettlementID), 0)
	set(cell(3, r), t.AmazonTotal.Float(), st.num)
	set(cell(4, r), t.AmazonTotal.Float(), st.num)
	r++
	set(cell(2, r), "Summary lines minus Amazon's total (must be 0)", st.bold)
	for i, col := range []int{3, 4} {
		set(cell(col, r), (t.SumLines[i] - t.AmazonTotal).Float(), st.numBold)
	}
	if t.SumLines[0] == t.AmazonTotal && t.SumLines[1] == t.AmazonTotal {
		set(cell(6, r), "OK", st.green)
	} else {
		set(cell(6, r), "DIFF", st.red)
	}
	r += 2
	set(cell(2, r), "For information: what the summary deliberately leaves out", st.bold)
	r++
	set(cell(2, r), "Total of all in-scope report amounts", 0)
	set(cell(3, r), t.InScope[0].Float(), st.num)
	set(cell(4, r), t.InScope[1].Float(), st.num)
	r++
	set(cell(2, r), "In-scope amounts not routed to any summary line", 0)
	set(cell(3, r), t.NotRouted[0].Float(), st.num)
	set(cell(4, r), t.NotRouted[1].Float(), st.num)
	r++
	for _, n := range t.NotRoutedNotes {
		set(cell(2, r), "   "+n, st.sub)
		r++
	}
	r++
	set(cell(2, r), "Scope: settlement report rows for this settlement; payments rows with the same settlement ID and status Released. "+
		"Deferred rows and rows of other settlements are ingested but excluded (see Run Info).", st.wrap)
	_ = f.MergeCell(sh, cell(2, r), cell(6, r))
	_ = f.SetRowHeight(sh, r, 45)
	r++
	set(cell(2, r), "Each column is computed independently: Payments only from payments rows, Settlements only from settlement rows.", st.sub)
	_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	return t, nil
}

// consolidated row as read from the database
type crow struct {
	ref, status, note                            string
	pTotal, sTotal, diff                         money.Cents
	refDate                                      string
	pRaw, sRaw, hdrRaw                           map[string]string
	pLines, sLines                               map[string]money.Cents
	pSrc, sSrc, sTypes, pCfg, sCfg, pKind, sKind string
	pQty                                         string
}

const consolidatedSQL = `
WITH comp AS (
    SELECT * FROM record_component WHERE in_scope
),
line_sums AS (
    SELECT record_ref, source, jsonb_object_agg(line_key, amt) AS j
    FROM (SELECT record_ref, source, line_key, SUM(amount) AS amt FROM comp
          WHERE line_key IS NOT NULL GROUP BY 1, 2, 3) x
    GROUP BY 1, 2
),
row_info AS (
    SELECT c.record_ref, c.source, min(sr.id) AS first_id,
           string_agg(DISTINCT sr.line_no::text, ',') AS src_lines,
           string_agg(DISTINCT NULLIF(concat_ws(' / ', sr.amount_type, sr.amount_description), ''), '; ') AS types,
           min(sr.ref_date)::text AS ref_date
    FROM (SELECT DISTINCT record_ref, source, source_row_id FROM comp) c
    JOIN source_row sr ON sr.id = c.source_row_id
    GROUP BY 1, 2
),
qty AS (
    SELECT c.record_ref, SUM(NULLIF(sr.raw->>'quantity', '')::numeric) AS q
    FROM (SELECT DISTINCT record_ref, source_row_id FROM comp WHERE source = 'payment') c
    JOIN source_row sr ON sr.id = c.source_row_id
    GROUP BY 1
),
cfg AS (
    SELECT record_ref, source,
           string_agg(DISTINCT COALESCE(config_id::text, '-'), ',') AS ids,
           string_agg(DISTINCT match_kind, ',') AS kinds
    FROM comp GROUP BY 1, 2
)
SELECT rr.record_ref, rr.status, COALESCE(rr.note, ''), rr.payment_total::text, rr.settlement_total::text, rr.difference::text,
       COALESCE(pi.ref_date, si.ref_date, ''),
       COALESCE(psr.raw::text, '{}'), COALESCE(ssr.raw::text, '{}'), COALESCE(hdr.raw::text, '{}'),
       COALESCE(pl.j::text, '{}'), COALESCE(sl.j::text, '{}'),
       COALESCE(pi.src_lines, ''), COALESCE(si.src_lines, ''), COALESCE(si.types, ''),
       COALESCE(pc.ids, ''), COALESCE(sc.ids, ''), COALESCE(pc.kinds, ''), COALESCE(sc.kinds, ''),
       COALESCE(q.q::text, '')
FROM recon_record rr
LEFT JOIN row_info pi ON pi.record_ref = rr.record_ref AND pi.source = 'payment'
LEFT JOIN row_info si ON si.record_ref = rr.record_ref AND si.source = 'settlement'
LEFT JOIN source_row psr ON psr.id = pi.first_id
LEFT JOIN source_row ssr ON ssr.id = si.first_id
LEFT JOIN LATERAL (SELECT raw FROM source_row h WHERE h.row_kind = 'settlement_header'
                   AND h.settlement_id = ssr.settlement_id LIMIT 1) hdr ON true
LEFT JOIN line_sums pl ON pl.record_ref = rr.record_ref AND pl.source = 'payment'
LEFT JOIN line_sums sl ON sl.record_ref = rr.record_ref AND sl.source = 'settlement'
LEFT JOIN cfg pc ON pc.record_ref = rr.record_ref AND pc.source = 'payment'
LEFT JOIN cfg sc ON sc.record_ref = rr.record_ref AND sc.source = 'settlement'
LEFT JOIN qty q ON q.record_ref = rr.record_ref
ORDER BY CASE rr.status WHEN 'reconciled' THEN 0 WHEN 'unreconciled_payment' THEN 1 ELSE 2 END,
         COALESCE(pi.ref_date, si.ref_date), rr.record_ref`

func parseRaw(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func parseLines(s string) map[string]money.Cents {
	tmp := map[string]json.Number{}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	_ = d.Decode(&tmp)
	out := map[string]money.Cents{}
	for k, v := range tmp {
		c, _ := money.Parse(v.String())
		out[k] = c
	}
	return out
}

var statusLabel = map[string]string{
	"reconciled":              "Reconciled",
	"unreconciled_payment":    "Unreconciled payment",
	"unreconciled_settlement": "Unreconciled settlement",
}

func writeConsolidated(ctx context.Context, db *sql.DB, f *excelize.File, st styles, lines []line) error {
	const sh = "Consolidated Data"
	sw, err := f.NewStreamWriter(sh)
	if err != nil {
		return err
	}
	paymentCols := []struct{ head, key string }{
		{"Date", "_date"}, {"date/time", "date/time"}, {"Transaction Status", "Transaction status"},
		{"Transaction Release Date", "Transaction Release Date"}, {"settlement id", "settlement ID"}, {"type", "type"},
		{"order id", "order ID"}, {"sku", "sku"}, {"description", "description"}, {"Quantity", "_qty"},
		{"marketplace", "marketplace"}, {"account type", "account type"}, {"fulfillment", "fulfilment"},
		{"order city", "order city"}, {"order state", "order state"}, {"order postal", "order postal"},
		{"tax collection model", "tax collection model"},
	}
	settlementCols := []string{"settlement-id", "settlement-start-date", "settlement-end-date", "deposit-date",
		"total-amount", "currency", "transaction-type", "order-id", "merchant-order-id", "adjustment-id", "shipment-id",
		"marketplace-name", "fulfillment-id", "posted-date", "posted-date-time", "order-item-code",
		"merchant-order-item-id", "merchant-adjustment-item-id", "sku", "quantity", "promotion-id"}
	headerFromSettlementHeader := map[string]bool{"settlement-start-date": true, "settlement-end-date": true,
		"deposit-date": true, "total-amount": true, "currency": true}
	lineLabels := make([]string, len(lines))
	lineSections := make([]string, len(lines))
	for i, l := range lines {
		lineLabels[i] = canonicalHead(l.label)
		lineSections[i] = l.section
	}
	audit := []string{"record_ref", "Payments source lines", "Settlement source lines", "Settlement amount types",
		"Payment config rows", "Settlement config rows", "Payment match kinds", "Settlement match kinds", "Note"}

	nP := len(paymentCols) + 1 + len(lines)    // + Total
	nS := len(settlementCols) + 1 + len(lines) // + Total
	nD := 2 + len(lines)                       // status + total diff
	nA := len(audit)
	total := nP + nS + nD + nA

	_ = sw.SetColWidth(1, total, 14)
	_ = sw.SetColWidth(nP+nS+nD+1, nP+nS+nD+1, 48)
	_ = sw.SetColWidth(total, total, 60)

	// header rows 1..10 (as in the sample report)
	row := func(r int, vals []any) error { return sw.SetRow(cell(1, r), vals) }
	if err := row(1, []any{excelize.Cell{StyleID: st.title, Value: "      Data From Amazon"}}); err != nil {
		return err
	}
	_ = row(3, []any{"DS1 =", "Payment Report"})
	_ = row(4, []any{"DS2 =", "Settlement Report"})
	r7 := make([]any, total)
	r8 := make([]any, total)
	r9 := make([]any, total)
	r10 := make([]any, total)
	c := 0
	for _, pc := range paymentCols {
		r7[c] = excelize.Cell{StyleID: st.groupP, Value: "Payment Report"}
		r10[c] = excelize.Cell{StyleID: st.header, Value: pc.head}
		c++
	}
	r7[c] = excelize.Cell{StyleID: st.groupP, Value: "Payment Report"}
	r10[c] = excelize.Cell{StyleID: st.header, Value: "Total"}
	c++
	for i := range lines {
		r7[c] = excelize.Cell{StyleID: st.groupP, Value: "Payment Report"}
		r8[c] = excelize.Cell{StyleID: st.groupP, Value: "Summary"}
		r9[c] = excelize.Cell{StyleID: st.groupP, Value: lineSections[i]}
		r10[c] = excelize.Cell{StyleID: st.header, Value: lineLabels[i]}
		c++
	}
	for _, h := range settlementCols {
		r7[c] = excelize.Cell{StyleID: st.groupS, Value: "Settlement Report"}
		r10[c] = excelize.Cell{StyleID: st.header, Value: h}
		c++
	}
	r7[c] = excelize.Cell{StyleID: st.groupS, Value: "Settlement Report"}
	r10[c] = excelize.Cell{StyleID: st.header, Value: "Total"}
	c++
	for i := range lines {
		r7[c] = excelize.Cell{StyleID: st.groupS, Value: "Settlement Report"}
		r8[c] = excelize.Cell{StyleID: st.groupS, Value: "Summary"}
		r9[c] = excelize.Cell{StyleID: st.groupS, Value: lineSections[i]}
		r10[c] = excelize.Cell{StyleID: st.header, Value: lineLabels[i]}
		c++
	}
	r10[c] = excelize.Cell{StyleID: st.header, Value: "Reconciliation Status"}
	r7[c] = excelize.Cell{StyleID: st.groupD, Value: "Reconciliation"}
	c++
	r10[c] = excelize.Cell{StyleID: st.header, Value: "total (DS1-DS2)"}
	r7[c] = excelize.Cell{StyleID: st.groupD, Value: "Reconciliation"}
	c++
	for i := range lines {
		r7[c] = excelize.Cell{StyleID: st.groupD, Value: "Reconciliation"}
		r8[c] = excelize.Cell{StyleID: st.groupD, Value: "Summary Mismatch"}
		r9[c] = excelize.Cell{StyleID: st.groupD, Value: lineSections[i]}
		r10[c] = excelize.Cell{StyleID: st.header, Value: lineLabels[i] + " (DS1-DS2)"}
		c++
	}
	for _, a := range audit {
		r7[c] = excelize.Cell{StyleID: st.groupA, Value: "Audit trail"}
		r10[c] = excelize.Cell{StyleID: st.header, Value: a}
		c++
	}
	for i, vals := range [][]any{r7, r8, r9, r10} {
		if err := row(7+i, vals); err != nil {
			return err
		}
	}

	rows, err := db.QueryContext(ctx, consolidatedSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	r := 11
	num := func(v money.Cents) excelize.Cell { return excelize.Cell{StyleID: st.num, Value: v.Float()} }
	for rows.Next() {
		var x crow
		var pT, sT, d, pRaw, sRaw, hRaw, pL, sL string
		if err := rows.Scan(&x.ref, &x.status, &x.note, &pT, &sT, &d, &x.refDate, &pRaw, &sRaw, &hRaw, &pL, &sL,
			&x.pSrc, &x.sSrc, &x.sTypes, &x.pCfg, &x.sCfg, &x.pKind, &x.sKind, &x.pQty); err != nil {
			return err
		}
		x.pTotal, _ = money.Parse(pT)
		x.sTotal, _ = money.Parse(sT)
		x.diff, _ = money.Parse(d)
		x.pRaw, x.sRaw, x.hdrRaw = parseRaw(pRaw), parseRaw(sRaw), parseRaw(hRaw)
		x.pLines, x.sLines = parseLines(pL), parseLines(sL)
		hasP, hasS := x.pSrc != "", x.sSrc != ""

		vals := make([]any, 0, total)
		for _, pc := range paymentCols {
			switch {
			case !hasP:
				vals = append(vals, nil)
			case pc.key == "_date":
				vals = append(vals, x.refDate)
			case pc.key == "_qty":
				vals = append(vals, numOrText(x.pQty))
			default:
				vals = append(vals, x.pRaw[pc.key])
			}
		}
		if hasP {
			vals = append(vals, num(x.pTotal))
			for _, l := range lines {
				vals = append(vals, num(x.pLines[l.key]))
			}
		} else {
			for i := 0; i < 1+len(lines); i++ {
				vals = append(vals, nil)
			}
		}
		for _, h := range settlementCols {
			switch {
			case !hasS:
				vals = append(vals, nil)
			case headerFromSettlementHeader[h]:
				vals = append(vals, x.hdrRaw[h])
			case h == "quantity":
				vals = append(vals, numOrText(x.sRaw["quantity-purchased"]))
			default:
				vals = append(vals, x.sRaw[h])
			}
		}
		if hasS {
			vals = append(vals, num(x.sTotal))
			for _, l := range lines {
				vals = append(vals, num(x.sLines[l.key]))
			}
		} else {
			for i := 0; i < 1+len(lines); i++ {
				vals = append(vals, nil)
			}
		}
		statusStyle := st.green
		if x.status != "reconciled" || x.diff != 0 {
			statusStyle = st.red
		}
		vals = append(vals, excelize.Cell{StyleID: statusStyle, Value: statusLabel[x.status]}, num(x.diff))
		for _, l := range lines {
			vals = append(vals, num(x.pLines[l.key]-x.sLines[l.key]))
		}
		vals = append(vals, x.ref, x.pSrc, x.sSrc, x.sTypes, x.pCfg, x.sCfg, x.pKind, x.sKind, x.note)
		if err := sw.SetRow(cell(1, r), vals); err != nil {
			return err
		}
		r++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = sw.MergeCell("A1", "J1")
	_ = sw.SetPanes(&excelize.Panes{Freeze: true, YSplit: 10, XSplit: 1, TopLeftCell: "B11", ActivePane: "bottomRight"})
	if err := sw.Flush(); err != nil {
		return err
	}
	last := cell(total, r-1)
	return f.AutoFilter(sh, "A10:"+last, nil)
}

// numOrText writes counts as numbers so Excel can sum them.
func numOrText(s string) any {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return v
	}
	return s
}

// canonicalHead matches the column titles used by the sample Consolidated sheet.
func canonicalHead(label string) string {
	switch label {
	case "Amazon fees":
		return "Amazon Fees"
	case "FBA fees":
		return "FBA Fees"
	case "Cross-account Debt Adjustment":
		return "Cross-Account Debt Adjustment"
	case "Refund expenses":
		return "Refund Expenses"
	case "Refunded sales":
		return "Refunded Sales"
	case "Promo rebates":
		return "Promo Rebates"
	}
	return label
}

func writeDrilldown(ctx context.Context, db *sql.DB, f *excelize.File, st styles) error {
	const sh = "Variance Drilldown"
	set := func(c string, v any, style int) {
		_ = f.SetCellValue(sh, c, v)
		if style != 0 {
			_ = f.SetCellStyle(sh, c, c, style)
		}
	}
	set("A1", "Every summary line where Payments ≠ Settlements, broken down by the config rule that produced each amount", st.title)
	set("A2", "Read: for a line with a difference, compare which rules feed it on each side. A rule on only one side, or the same money on different lines, is the root cause.", st.sub)
	heads := []string{"Summary line", "Line difference", "Side", "Config row", "Match kind", "Transaction type", "Description / amount type / amount description", "Amount field", "Summary field", "Components", "Amount"}
	for i, h := range heads {
		set(cell(i+1, 4), h, st.header)
	}
	widths := []float64{30, 16, 11, 11, 11, 26, 60, 22, 34, 12, 16}
	for i, w := range widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sh, col, col, w)
	}
	rows, err := db.QueryContext(ctx, `
	WITH comp AS (
	    SELECT rc.*, sr.transaction_type, sr.description AS p_desc, sr.amount_type, sr.amount_description
	    FROM record_component rc JOIN source_row sr ON sr.id = rc.source_row_id
	    WHERE rc.in_scope AND rc.summary_field IS NOT NULL
	),
	keyed AS (
	    SELECT COALESCE(line_key, '(not on summary: ' || summary_field || ')') AS line, * FROM comp
	),
	line_diff AS (
	    SELECT line, SUM(CASE WHEN source='payment' THEN amount ELSE -amount END) AS d
	    FROM keyed GROUP BY line HAVING SUM(CASE WHEN source='payment' THEN amount ELSE -amount END) <> 0
	)
	SELECT k.line, ld.d::text, k.source, COALESCE(k.config_id::text, '-'), k.match_kind,
	       COALESCE(upper(k.transaction_type), ''),
	       CASE WHEN k.source = 'payment' THEN
	            CASE WHEN count(DISTINCT k.p_desc) > 3 THEN '(' || count(DISTINCT k.p_desc) || ' distinct descriptions)'
	                 ELSE string_agg(DISTINCT left(k.p_desc, 60), '; ') END
	            ELSE string_agg(DISTINCT concat_ws(' / ', k.amount_type, k.amount_description), '; ') END,
	       k.amount_field, k.summary_field, count(*), SUM(k.amount)::text
	FROM keyed k JOIN line_diff ld ON ld.line = k.line
	GROUP BY k.line, ld.d, k.source, k.config_id, k.match_kind, upper(k.transaction_type), k.amount_field, k.summary_field
	ORDER BY k.line, k.source, SUM(k.amount)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	r := 5
	prev := ""
	for rows.Next() {
		var line, d, src, cfg, kind, tt, desc, field, sf, amt string
		var n int
		if err := rows.Scan(&line, &d, &src, &cfg, &kind, &tt, &desc, &field, &sf, &n, &amt); err != nil {
			return err
		}
		if line != prev && prev != "" {
			r++
		}
		prev = line
		dc, _ := money.Parse(d)
		ac, _ := money.Parse(amt)
		set(cell(1, r), line, st.bold)
		set(cell(2, r), dc.Float(), st.numBold)
		set(cell(3, r), src, 0)
		set(cell(4, r), cfg, 0)
		set(cell(5, r), kind, 0)
		set(cell(6, r), tt, 0)
		set(cell(7, r), desc, 0)
		set(cell(8, r), field, 0)
		set(cell(9, r), sf, 0)
		set(cell(10, r), n, 0)
		set(cell(11, r), ac.Float(), st.num)
		r++
	}
	if r == 5 {
		set("A5", "No differences: every summary line agrees.", st.green)
	}
	_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 4, TopLeftCell: "A5", ActivePane: "bottomLeft"})
	return rows.Err()
}

func writeLint(ctx context.Context, db *sql.DB, f *excelize.File, st styles) error {
	const sh = "Config Lint"
	pr, err := mapping.LoadFromDB(ctx, db, mapping.Payment)
	if err != nil {
		return err
	}
	sr, err := mapping.LoadFromDB(ctx, db, mapping.Settlement)
	if err != nil {
		return err
	}
	rt, err := mapping.LoadRoutes(ctx, db)
	if err != nil {
		return err
	}
	findings := mapping.Lint(mapping.NewMatcher(mapping.Payment, pr), mapping.NewMatcher(mapping.Settlement, sr), rt)
	set := func(c string, v any, style int) {
		_ = f.SetCellValue(sh, c, v)
		if style != 0 {
			_ = f.SetCellStyle(sh, c, c, style)
		}
	}
	set("A1", "Static checks over the mapping configs as they are in the database for this run", st.title)
	set("A2", "These catch defects that this dataset does not exercise. 'info' rows are record_ref templates used by one side only; most are legitimately one-sided.", st.sub)
	for i, h := range []string{"Severity", "Check", "Config", "Config rows", "Finding"} {
		set(cell(i+1, 4), h, st.header)
	}
	for i, w := range []float64{10, 24, 18, 14, 120} {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sh, col, col, w)
	}
	for i, fd := range findings {
		r := 5 + i
		style := 0
		if fd.Severity == "error" {
			style = st.red
		}
		ids := make([]string, len(fd.IDs))
		for j, id := range fd.IDs {
			ids[j] = fmt.Sprint(id)
		}
		set(cell(1, r), fd.Severity, style)
		set(cell(2, r), fd.Check, 0)
		set(cell(3, r), fd.Config, 0)
		set(cell(4, r), strings.Join(ids, ", "), 0)
		set(cell(5, r), fd.Message, 0)
	}
	return nil
}

func writeRunInfo(ctx context.Context, db *sql.DB, f *excelize.File, st styles, label string, t tieOut) error {
	const sh = "Run Info"
	_ = f.SetColWidth(sh, "A", "A", 52)
	_ = f.SetColWidth(sh, "B", "B", 90)
	r := 1
	put := func(k string, v any, style int) {
		_ = f.SetCellValue(sh, cell(1, r), k)
		_ = f.SetCellValue(sh, cell(2, r), v)
		if style != 0 {
			_ = f.SetCellStyle(sh, cell(1, r), cell(1, r), style)
		}
		r++
	}
	_ = f.SetCellValue(sh, "A1", "Run information")
	_ = f.SetCellStyle(sh, "A1", "A1", st.title)
	r = 3
	put("Report label", label, st.bold)
	put("Generated at (UTC)", time.Now().UTC().Format(time.RFC3339), st.bold)
	var runID int
	var fp, scope sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT id, config_fingerprint, array_to_string(scope_settlement_ids, ', ') FROM ingest_run ORDER BY id DESC LIMIT 1`).Scan(&runID, &fp, &scope); err != nil {
		return err
	}
	put("Ingest run", runID, st.bold)
	put("Config fingerprint (md5 of both config tables)", fp.String, st.bold)
	var changes int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM config_change_log`).Scan(&changes)
	put("Config changes applied since load (config_change_log)", changes, st.bold)
	put("Scope: settlement id(s)", scope.String, st.bold)
	put("Scope: settlement period", t.Period, st.bold)
	r++
	files, err := db.QueryContext(ctx, `SELECT source, file_name, sha256, data_rows FROM source_file ORDER BY id`)
	if err != nil {
		return err
	}
	for files.Next() {
		var src, name, sha string
		var n int
		if err := files.Scan(&src, &name, &sha, &n); err != nil {
			files.Close()
			return err
		}
		put("Input ("+src+")", fmt.Sprintf("%s · %d data lines · sha256 %s", name, n, sha), st.bold)
	}
	files.Close()
	r++
	c, err := recon.Build(ctx, db) // idempotent; guarantees counts match the sheet
	if err != nil {
		return err
	}
	put("Reconciled records", c.Reconciled, st.bold)
	put("Unreconciled payment records", c.UnreconciledPayment, st.bold)
	put("Unreconciled settlement records", c.UnreconciledSettlement, st.bold)
	put("Reconciled records whose totals differ", c.ReconciledWithDifference, st.bold)
	put("Reconciled records with a summary-line mismatch", c.ReconciledWithLineMismatch, st.bold)
	r++
	scopeRows, err := db.QueryContext(ctx, `SELECT source, COALESCE(scope_reason, 'in scope'), count(*), SUM(total)::text
	    FROM source_row WHERE row_kind = 'data' GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for scopeRows.Next() {
		var src, reason, amt string
		var n int
		if err := scopeRows.Scan(&src, &reason, &n, &amt); err != nil {
			scopeRows.Close()
			return err
		}
		a, _ := money.Parse(amt)
		put(fmt.Sprintf("Rows: %s · %s", src, reason), fmt.Sprintf("%d rows · total %s", n, a), st.bold)
	}
	scopeRows.Close()
	r++
	mk, err := db.QueryContext(ctx, `SELECT source, match_kind, count(*), SUM(amount)::text FROM record_component
	    WHERE in_scope GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for mk.Next() {
		var src, kind, amt string
		var n int
		if err := mk.Scan(&src, &kind, &n, &amt); err != nil {
			mk.Close()
			return err
		}
		a, _ := money.Parse(amt)
		put(fmt.Sprintf("Mapped amounts: %s · match %s", src, kind), fmt.Sprintf("%d components · sum %s", n, a), st.bold)
	}
	mk.Close()
	r++
	stream := "OK: summary computed while ingesting equals the SQL aggregation of mapped records"
	if !t.StreamingMatches {
		stream = "MISMATCH: " + strings.Join(t.StreamingDiffs, "; ")
	}
	put("Streaming summary check", stream, st.bold)
	tie := "OK"
	if t.SumLines[0] != t.AmazonTotal || t.SumLines[1] != t.AmazonTotal {
		tie = fmt.Sprintf("DIFF: payments %s, settlements %s vs Amazon %s", t.SumLines[0], t.SumLines[1], t.AmazonTotal)
	}
	put("Summary ties to Amazon settlement total-amount", tie, st.bold)
	var unmatched int
	var unmatchedAmt sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT count(*), SUM(amount)::text FROM record_component
	    WHERE in_scope AND match_kind = 'unmatched' AND amount <> 0`).Scan(&unmatched, &unmatchedAmt)
	put("In-scope amounts that matched no mapping rule", fmt.Sprintf("%d (sum %s)", unmatched, cents(unmatchedAmt)), st.bold)
	return nil
}
