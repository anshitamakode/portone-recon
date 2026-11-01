// Package recon matches payments and settlement records on record_ref.
package recon

import (
	"context"
	"database/sql"
)

// Granularity: a payments row is one order line with all its amounts in columns;
// the settlement report has one row per amount component, and the same order line
// can also appear in several payments rows (e.g. a split shipment). We therefore
// aggregate BOTH sides to one total per record_ref before matching:
//
//	payment side   = sum of the `total` column of the payments rows with that ref
//	settlement side = sum of `amount` of the settlement rows with that ref
//
// and compare per summary line as well, so offsetting mis-mappings inside a record
// are caught even when the record totals agree.
const buildSQL = `
TRUNCATE recon_record;
WITH comp AS (
    SELECT rc.*, sr.transaction_type
    FROM record_component rc
    JOIN source_row sr ON sr.id = rc.source_row_id
    WHERE rc.in_scope
),
agg AS (
    SELECT record_ref,
           COALESCE(SUM(amount) FILTER (WHERE source = 'payment'    AND is_recon_amount), 0) AS p_total,
           COALESCE(SUM(amount) FILTER (WHERE source = 'settlement' AND is_recon_amount), 0) AS s_total,
           COUNT(DISTINCT source_row_id) FILTER (WHERE source = 'payment')    AS p_rows,
           COUNT(DISTINCT source_row_id) FILTER (WHERE source = 'settlement') AS s_rows,
           bool_and(amount = 0) AS all_zero,
           bool_or(match_kind = 'fallback' AND amount <> 0) AS used_fallback,
           bool_or(match_kind = 'unmatched' AND amount <> 0) AS has_unmatched,
           string_agg(DISTINCT upper(transaction_type), ', ') AS types
    FROM comp
    GROUP BY record_ref
),
lines AS (
    SELECT record_ref, line_key,
           COALESCE(SUM(amount) FILTER (WHERE source = 'payment'), 0)    AS p,
           COALESCE(SUM(amount) FILTER (WHERE source = 'settlement'), 0) AS s
    FROM comp
    WHERE line_key IS NOT NULL
    GROUP BY record_ref, line_key
),
mm AS (
    SELECT record_ref, bool_or(p <> s) AS mismatch FROM lines GROUP BY record_ref
)
INSERT INTO recon_record (record_ref, status, payment_total, settlement_total, difference,
                          payment_rows, settlement_rows, summary_mismatch, note)
SELECT a.record_ref,
       CASE WHEN a.p_rows > 0 AND a.s_rows > 0 THEN 'reconciled'
            WHEN a.p_rows > 0 THEN 'unreconciled_payment'
            ELSE 'unreconciled_settlement' END,
       a.p_total, a.s_total, a.p_total - a.s_total, a.p_rows, a.s_rows,
       (a.p_rows > 0 AND a.s_rows > 0 AND COALESCE(mm.mismatch, false)),
       NULLIF(concat_ws('; ',
         CASE WHEN a.has_unmatched THEN 'no mapping rule matched this amount' END,
         CASE WHEN a.used_fallback THEN 'matched only the catch-all rule (empty transaction_type)' END,
         CASE WHEN a.p_rows > 0 AND a.s_rows = 0 AND a.all_zero
              THEN 'zero-value payments line: the settlement report does not list $0 lines' END,
         CASE WHEN a.p_rows > 0 AND a.s_rows = 0 AND a.types LIKE '%TRANSFER%'
              THEN 'disbursement to the seller''s bank account: settlement flat files carry no transfer lines (it pays out the previous settlement)' END,
         CASE WHEN a.p_rows > 0 AND a.s_rows > 0 AND a.p_total <> a.s_total THEN 'record totals differ' END,
         CASE WHEN a.p_rows > 0 AND a.s_rows > 0 AND a.p_total = a.s_total AND COALESCE(mm.mismatch, false)
              THEN 'totals agree but the amounts land on different summary lines' END
       ), '')
FROM agg a
LEFT JOIN mm ON mm.record_ref = a.record_ref;
`

// Counts by status.
type Counts struct {
	Reconciled, UnreconciledPayment, UnreconciledSettlement int
	ReconciledWithDifference, ReconciledWithLineMismatch    int
}

// Build runs the reconciliation.
func Build(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	if _, err := db.ExecContext(ctx, buildSQL); err != nil {
		return c, err
	}
	err := db.QueryRowContext(ctx, `SELECT
	    COUNT(*) FILTER (WHERE status = 'reconciled'),
	    COUNT(*) FILTER (WHERE status = 'unreconciled_payment'),
	    COUNT(*) FILTER (WHERE status = 'unreconciled_settlement'),
	    COUNT(*) FILTER (WHERE status = 'reconciled' AND difference <> 0),
	    COUNT(*) FILTER (WHERE status = 'reconciled' AND summary_mismatch)
	  FROM recon_record`).Scan(&c.Reconciled, &c.UnreconciledPayment, &c.UnreconciledSettlement,
		&c.ReconciledWithDifference, &c.ReconciledWithLineMismatch)
	return c, err
}
