package mapping

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
)

var paymentCols = []string{"transaction_type", "description", "amount_field", "record_ref",
	"to_summary_field_when_positive_amount", "to_summary_field_when_negative_amount"}
var settlementCols = []string{"transaction_type", "amount_type", "amount_description", "record_ref",
	"to_summary_field_when_positive_amount", "to_summary_field_when_negative_amount"}

// LoadCSV reads a config CSV exactly as provided. The rule ID is the physical
// line number, so documentation can refer to "payment config line 65".
func LoadCSV(path string, src Source) ([]Rule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%s: empty", path)
	}
	want := paymentCols
	if src == Settlement {
		want = settlementCols
	}
	hdr := recs[0]
	for i := range hdr {
		hdr[i] = strings.TrimPrefix(strings.TrimSpace(hdr[i]), "\uFEFF")
	}
	if strings.Join(hdr, ",") != strings.Join(want, ",") {
		return nil, fmt.Errorf("%s: unexpected header %v, want %v", path, hdr, want)
	}
	var out []Rule
	for i, rec := range recs[1:] {
		for len(rec) < len(want) {
			rec = append(rec, "")
		}
		ru := Rule{Source: src, ID: i + 2, TransactionType: rec[0], RecordRef: rec[3], PosField: rec[4], NegField: rec[5]}
		if src == Payment {
			ru.Description, ru.AmountField = rec[1], rec[2]
		} else {
			ru.AmountType, ru.Description = rec[1], rec[2]
		}
		out = append(out, ru)
	}
	return out, nil
}

// Table returns the config table name for a source.
func Table(src Source) string {
	if src == Payment {
		return "payment_config"
	}
	return "settlement_config"
}

// ReplaceInDB loads the given rules as the new baseline config (audit log reset).
func ReplaceInDB(ctx context.Context, db *sql.DB, src Source, rules []Rule) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL recon.loading = 'on'`); err != nil {
		return err
	}
	tbl := Table(src)
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config_change_log WHERE config = $1`, tbl); err != nil {
		return err
	}
	var q string
	if src == Payment {
		q = `INSERT INTO payment_config (id, transaction_type, description, amount_field, record_ref,
		      to_summary_field_when_positive_amount, to_summary_field_when_negative_amount)
		     VALUES ($1,$2,$3,$4,$5,$6,$7)`
	} else {
		q = `INSERT INTO settlement_config (id, transaction_type, amount_type, amount_description, record_ref,
		      to_summary_field_when_positive_amount, to_summary_field_when_negative_amount)
		     VALUES ($1,$2,$3,$4,$5,$6,$7)`
	}
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	for _, r := range rules {
		second, third := r.Description, r.AmountField
		if src == Settlement {
			second, third = r.AmountType, r.Description
		}
		if _, err := stmt.ExecContext(ctx, r.ID, r.TransactionType, second, third, r.RecordRef, r.PosField, r.NegField); err != nil {
			return fmt.Errorf("%s id %d: %w", tbl, r.ID, err)
		}
	}
	return tx.Commit()
}

// LoadFromDB reads the current (possibly fixed) config rules.
func LoadFromDB(ctx context.Context, db *sql.DB, src Source) ([]Rule, error) {
	var q string
	if src == Payment {
		q = `SELECT id, transaction_type, description, amount_field, record_ref,
		        to_summary_field_when_positive_amount, to_summary_field_when_negative_amount, description_match
		     FROM payment_config ORDER BY id`
	} else {
		q = `SELECT id, transaction_type, amount_type, amount_description, record_ref,
		        to_summary_field_when_positive_amount, to_summary_field_when_negative_amount, description_match
		     FROM settlement_config ORDER BY id`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r := Rule{Source: src}
		var a, b string
		if err := rows.Scan(&r.ID, &r.TransactionType, &a, &b, &r.RecordRef, &r.PosField, &r.NegField, &r.DescMatch); err != nil {
			return nil, err
		}
		if src == Payment {
			r.Description, r.AmountField = a, b
		} else {
			r.AmountType, r.Description = a, b
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Routes maps (summary_field, sign) to a summary line. Excluded fields map to "".
type Routes struct {
	line     map[[2]string]string
	excluded map[string]string
}

// LoadRoutes reads summary_field_route.
func LoadRoutes(ctx context.Context, db *sql.DB) (*Routes, error) {
	rows, err := db.QueryContext(ctx, `SELECT summary_field, sign, COALESCE(line_key,''), COALESCE(excluded_reason,'') FROM summary_field_route`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rt := &Routes{line: map[[2]string]string{}, excluded: map[string]string{}}
	for rows.Next() {
		var f, s, l, ex string
		if err := rows.Scan(&f, &s, &l, &ex); err != nil {
			return nil, err
		}
		if ex != "" {
			rt.excluded[f] = ex
			continue
		}
		rt.line[[2]string{f, s}] = l
	}
	return rt, rows.Err()
}

// Line resolves the summary line for a field and amount sign.
// ok=false means the field is neither routed nor deliberately excluded (an orphan).
func (rt *Routes) Line(field string, positive bool) (line string, ok bool) {
	if field == "" {
		return "", true
	}
	if _, ex := rt.excluded[field]; ex {
		return "", true
	}
	sign := "negative"
	if positive {
		sign = "positive"
	}
	if l, found := rt.line[[2]string{field, sign}]; found {
		return l, true
	}
	if l, found := rt.line[[2]string{field, "any"}]; found {
		return l, true
	}
	return "", false
}

// Known reports whether a field has any route or exclusion.
func (rt *Routes) Known(field string) bool {
	if _, ex := rt.excluded[field]; ex {
		return true
	}
	for k := range rt.line {
		if k[0] == field {
			return true
		}
	}
	return false
}
