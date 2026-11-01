-- Which payments timestamp lines up with the settlement posted-date? Count order keys
-- (order id + sku + date) found in both reports under each candidate definition.
WITH s AS (SELECT DISTINCT txn_ref, sku, ref_date FROM source_row
           WHERE source='settlement' AND transaction_type='Order'),
p AS (SELECT txn_ref, sku,
             (posted_at   AT TIME ZONE 'Asia/Tokyo')::date AS posted_local,
             (posted_at   AT TIME ZONE 'UTC')::date        AS posted_utc,
             (released_at AT TIME ZONE 'Asia/Tokyo')::date AS released_local,
             (released_at AT TIME ZONE 'UTC')::date        AS released_utc
      FROM source_row WHERE source='payment' AND in_scope AND transaction_type='Order')
SELECT (SELECT count(*) FROM s) AS settlement_order_keys,
       count(DISTINCT (p.txn_ref, p.sku, p.posted_local))   FILTER (WHERE (p.txn_ref, p.sku, p.posted_local)   IN (SELECT * FROM s)) AS date_time_local,
       count(DISTINCT (p.txn_ref, p.sku, p.posted_utc))     FILTER (WHERE (p.txn_ref, p.sku, p.posted_utc)     IN (SELECT * FROM s)) AS date_time_utc,
       count(DISTINCT (p.txn_ref, p.sku, p.released_local)) FILTER (WHERE (p.txn_ref, p.sku, p.released_local) IN (SELECT * FROM s)) AS release_local,
       count(DISTINCT (p.txn_ref, p.sku, p.released_utc))   FILTER (WHERE (p.txn_ref, p.sku, p.released_utc)   IN (SELECT * FROM s)) AS release_utc
FROM p;
