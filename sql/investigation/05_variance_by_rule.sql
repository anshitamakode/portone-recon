-- For every summary line where Payments <> Settlements: which config rule put which
-- amount there, on each side. (Same query that feeds the "Variance Drilldown" sheet.)
WITH comp AS (
    SELECT rc.*, sr.transaction_type,
           COALESCE(sr.description, concat_ws(' / ', sr.amount_type, sr.amount_description)) AS what
    FROM record_component rc JOIN source_row sr ON sr.id = rc.source_row_id
    WHERE rc.in_scope AND rc.summary_field IS NOT NULL
),
keyed AS (SELECT COALESCE(line_key, '(orphan) ' || summary_field) AS line, * FROM comp),
diff AS (
    SELECT line FROM keyed GROUP BY line
    HAVING SUM(CASE WHEN source = 'payment' THEN amount ELSE -amount END) <> 0
)
SELECT k.line, k.source, k.config_id, k.match_kind, upper(k.transaction_type) AS txn_type,
       k.amount_field, k.summary_field, count(*) AS n, SUM(k.amount) AS amount
FROM keyed k JOIN diff USING (line)
GROUP BY 1, 2, 3, 4, 5, 6, 7
ORDER BY 1, 2, amount;
