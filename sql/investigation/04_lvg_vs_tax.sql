-- Low-value-goods tax: is it the GST Amazon collected and withheld on the same line?
SELECT count(*) AS lvg_rows,
       SUM((raw->>'sales tax collected')::numeric) AS tax_collected_on_those_rows,
       SUM((raw->>'low value goods')::numeric)     AS lvg_withheld,
       count(*) FILTER (WHERE (raw->>'sales tax collected')::numeric + (raw->>'low value goods')::numeric = 0) AS rows_netting_to_zero
FROM source_row WHERE source='payment' AND in_scope AND (raw->>'low value goods')::numeric <> 0;
