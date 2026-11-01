-- Does the payments "sales tax collected" column equal the sum of the settlement tax
-- components for the same records? (It decides where tax can be routed.)
WITH s AS (
  SELECT amount_type, amount_description, SUM(total) amt
  FROM source_row WHERE source='settlement' AND in_scope AND row_kind='data' AND transaction_type='Order'
    AND (amount_description ILIKE '%tax%' )
  GROUP BY 1,2
)
SELECT 'settlement '||amount_type||' / '||amount_description AS item, amt FROM s
UNION ALL
SELECT 'settlement: sum of all order tax components', SUM(amt) FROM s WHERE amount_type <> 'ItemWithheldTax'
UNION ALL
SELECT 'payments: ORDER sales tax collected', SUM(amount) FROM record_component rc JOIN source_row sr ON sr.id=rc.source_row_id
  WHERE rc.in_scope AND rc.source='payment' AND sr.transaction_type='Order' AND rc.amount_field='sales_tax_collected' AND rc.config_id=71
UNION ALL
SELECT 'payments: ORDER low value goods', SUM(amount) FROM record_component rc JOIN source_row sr ON sr.id=rc.source_row_id
  WHERE rc.in_scope AND rc.source='payment' AND sr.transaction_type='Order' AND rc.amount_field='low_value_goods' AND rc.config_id=6;
