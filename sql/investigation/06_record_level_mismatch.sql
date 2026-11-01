-- Records whose totals agree but whose amounts land on different summary lines.
-- A Summary sheet that agrees in aggregate can still hide offsetting errors here.
SELECT rr.record_ref, l.line_key, l.p, l.s
FROM recon_record rr
JOIN LATERAL (
    SELECT line_key,
           COALESCE(SUM(amount) FILTER (WHERE source = 'payment'), 0)    AS p,
           COALESCE(SUM(amount) FILTER (WHERE source = 'settlement'), 0) AS s
    FROM record_component WHERE record_ref = rr.record_ref AND in_scope AND line_key IS NOT NULL
    GROUP BY line_key
) l ON l.p <> l.s
WHERE rr.status = 'reconciled'
ORDER BY rr.record_ref, l.line_key
LIMIT 50;
-- counts
SELECT status, count(*), count(*) FILTER (WHERE summary_mismatch) AS with_line_mismatch,
       count(*) FILTER (WHERE difference <> 0) AS with_total_difference
FROM recon_record GROUP BY status;
