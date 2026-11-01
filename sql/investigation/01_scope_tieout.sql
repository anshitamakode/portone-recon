-- Which payments rows make up the settlement? Amazon's settlement total-amount must
-- be explained by a well-defined subset of the payments report.
SELECT settlement_id, txn_status, transaction_type = 'Transfer' AS is_transfer, count(*), SUM(total)
FROM source_row WHERE source = 'payment'
GROUP BY ROLLUP (settlement_id, txn_status, transaction_type = 'Transfer')
ORDER BY 1, 2, 3;
SELECT 'settlement header total-amount' AS what, total FROM source_row WHERE row_kind = 'settlement_header'
UNION ALL
SELECT 'sum of settlement amounts', SUM(total) FROM source_row WHERE source='settlement' AND row_kind='data'
UNION ALL
SELECT 'payments: settlement 12395580393, Released, excluding Transfer', SUM(total) FROM source_row
WHERE source='payment' AND settlement_id='12395580393' AND txn_status='Released' AND transaction_type <> 'Transfer';
