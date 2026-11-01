-- Schema for the Amazon Payments vs Settlement reconciliation.
-- Applied by `recon init`. Safe to re-run: everything is CREATE ... IF NOT EXISTS
-- and the seed tables are upserted.

-- ---------------------------------------------------------------------------
-- 1. Files that were ingested (one row per physical file per ingest run)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS source_file (
    id            serial PRIMARY KEY,
    source        text        NOT NULL CHECK (source IN ('payment', 'settlement')),
    file_name     text        NOT NULL,
    sha256        text        NOT NULL,
    preamble      text,                 -- free-text lines before the header (payments report notes)
    header        jsonb       NOT NULL, -- column names exactly as in the file
    data_rows     integer     NOT NULL DEFAULT 0,
    ingested_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 2. ONE table for the rows of BOTH reports.
--    `raw` keeps the untouched row (header -> value) so every number can be traced
--    back to file + line. The typed columns are a normalised projection used for
--    matching; they never replace the raw payload.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS source_row (
    id                  bigserial PRIMARY KEY,
    file_id             integer   NOT NULL REFERENCES source_file (id) ON DELETE CASCADE,
    source              text      NOT NULL CHECK (source IN ('payment', 'settlement')),
    line_no             integer   NOT NULL,           -- 1-based physical line in the file
    row_kind            text      NOT NULL CHECK (row_kind IN ('data', 'settlement_header')),
    raw                 jsonb     NOT NULL,
    -- normalised projection
    settlement_id       text,
    transaction_type    text,     -- payments: type          | settlement: transaction-type
    description         text,     -- payments: description   | settlement: NULL
    amount_type         text,     -- payments: NULL          | settlement: amount-type
    amount_description  text,     -- payments: NULL          | settlement: amount-description
    txn_ref             text,     -- payments: order ID      | settlement: order-id
    sku                 text,
    shipment_id         text,
    merchant_order_id   text,
    adjustment_id       text,
    txn_status          text,     -- payments: Transaction status (Released/Deferred)
    posted_at           timestamptz, -- payments: date/time  | settlement: posted-date-time
    released_at         timestamptz, -- payments: Transaction Release Date | settlement: posted-date-time
    ref_date            date,     -- the value of the `date` token in record_ref
    total               numeric(14, 2), -- payments: total  | settlement: amount (header row: total-amount)
    in_scope            boolean   NOT NULL DEFAULT true,
    scope_reason        text,     -- why a row is out of scope (other settlement / deferred)
    UNIQUE (file_id, line_no)
);
CREATE INDEX IF NOT EXISTS source_row_source_idx ON source_row (source, in_scope);

-- ---------------------------------------------------------------------------
-- 3. Mapping configs, loaded verbatim from the CSVs. `id` = the CSV line number,
--    so "config row 65" means line 65 of the original file.
--    `description_match` is a schema extension (see MAPPING_FIXES.sql): 'exact'
--    (default, original behaviour) or 'prefix'.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payment_config (
    id                                    integer PRIMARY KEY,
    transaction_type                      text NOT NULL DEFAULT '',
    description                           text NOT NULL DEFAULT '',
    amount_field                          text NOT NULL,
    record_ref                            text NOT NULL DEFAULT '',
    to_summary_field_when_positive_amount text NOT NULL DEFAULT '',
    to_summary_field_when_negative_amount text NOT NULL DEFAULT '',
    description_match                     text NOT NULL DEFAULT 'exact'
        CHECK (description_match IN ('exact', 'prefix'))
);

CREATE TABLE IF NOT EXISTS settlement_config (
    id                                    integer PRIMARY KEY,
    transaction_type                      text NOT NULL DEFAULT '',
    amount_type                           text NOT NULL DEFAULT '',
    amount_description                    text NOT NULL DEFAULT '',
    record_ref                            text NOT NULL DEFAULT '',
    to_summary_field_when_positive_amount text NOT NULL DEFAULT '',
    to_summary_field_when_negative_amount text NOT NULL DEFAULT '',
    description_match                     text NOT NULL DEFAULT 'exact'
        CHECK (description_match IN ('exact', 'prefix'))
);

-- Every change to a config table is logged, so the fixes are auditable even
-- after they have been applied.
CREATE TABLE IF NOT EXISTS config_change_log (
    id          bigserial PRIMARY KEY,
    config      text        NOT NULL,
    op          text        NOT NULL,
    config_id   integer,
    old_row     jsonb,
    new_row     jsonb,
    changed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION log_config_change() RETURNS trigger AS $$
BEGIN
    INSERT INTO config_change_log (config, op, config_id, old_row, new_row)
    VALUES (TG_TABLE_NAME, TG_OP,
            COALESCE(NEW.id, OLD.id),
            CASE WHEN TG_OP IN ('UPDATE', 'DELETE') THEN to_jsonb(OLD) END,
            CASE WHEN TG_OP IN ('UPDATE', 'INSERT') THEN to_jsonb(NEW) END);
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS payment_config_audit ON payment_config;
CREATE TRIGGER payment_config_audit AFTER INSERT OR UPDATE OR DELETE ON payment_config
    FOR EACH ROW WHEN (current_setting('recon.loading', true) IS DISTINCT FROM 'on')
    EXECUTE FUNCTION log_config_change();
DROP TRIGGER IF EXISTS settlement_config_audit ON settlement_config;
CREATE TRIGGER settlement_config_audit AFTER INSERT OR UPDATE OR DELETE ON settlement_config
    FOR EACH ROW WHEN (current_setting('recon.loading', true) IS DISTINCT FROM 'on')
    EXECUTE FUNCTION log_config_change();

-- ---------------------------------------------------------------------------
-- 4. Summary sheet layout and how config summary fields land on it.
--    Kept as data so the report code has no hard-coded accounting rules.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS summary_line (
    line_key   text PRIMARY KEY,
    section    text    NOT NULL,   -- Sales / Refunds / Expenses / Paid To Amazon
    label      text    NOT NULL,
    sheet_row  integer NOT NULL UNIQUE, -- row on the Summary sheet (matches the sample)
    sort_order integer NOT NULL UNIQUE
);

-- sign: 'any' | 'positive' | 'negative'. line_key NULL with a reason = a field that
-- is deliberately not part of the P&L summary (balance-sheet items).
CREATE TABLE IF NOT EXISTS summary_field_route (
    summary_field text NOT NULL,
    sign          text NOT NULL CHECK (sign IN ('any', 'positive', 'negative')),
    line_key      text REFERENCES summary_line (line_key),
    excluded_reason text,
    PRIMARY KEY (summary_field, sign),
    CHECK ((line_key IS NULL) <> (excluded_reason IS NULL))
);

-- ---------------------------------------------------------------------------
-- 5. Ingest runs + the summary computed WHILE streaming (brownie points).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ingest_run (
    id            serial PRIMARY KEY,
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    scope_settlement_ids text[],
    config_fingerprint   text,     -- md5 of both config tables at ingest time
    notes         text
);

CREATE TABLE IF NOT EXISTS streaming_summary (
    run_id     integer NOT NULL REFERENCES ingest_run (id) ON DELETE CASCADE,
    source     text    NOT NULL,
    line_key   text    NOT NULL,   -- summary line, or '_unrouted:<field>' / '_unsummarised'
    amount     numeric(14, 2) NOT NULL,
    components integer NOT NULL,
    PRIMARY KEY (run_id, source, line_key)
);

-- ---------------------------------------------------------------------------
-- 6. Mapped amounts. One row per (source row x amount field x matching config).
--    Payments rows explode into one component per non-zero amount column (plus
--    `total`, always); settlement rows have one amount each.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS record_component (
    id              bigserial PRIMARY KEY,
    source_row_id   bigint  NOT NULL REFERENCES source_row (id) ON DELETE CASCADE,
    source          text    NOT NULL,
    amount_field    text    NOT NULL,
    amount          numeric(14, 2) NOT NULL,
    config_id       integer,           -- NULL when no rule matched
    match_kind      text    NOT NULL CHECK (match_kind IN ('exact', 'prefix', 'wildcard', 'fallback', 'unmatched')),
    record_ref      text    NOT NULL,  -- 'UNMAPPED:...' when no rule matched
    summary_field   text,              -- after sign-based routing ('' / NULL = not summarised)
    line_key        text,              -- summary sheet line the amount lands on
    is_recon_amount boolean NOT NULL,  -- the amount used for record-level matching
    in_scope        boolean NOT NULL
);
CREATE INDEX IF NOT EXISTS record_component_ref_idx ON record_component (record_ref);
CREATE INDEX IF NOT EXISTS record_component_row_idx ON record_component (source_row_id);

-- ---------------------------------------------------------------------------
-- 7. Reconciliation result, one row per record_ref (in scope only).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS recon_record (
    record_ref        text PRIMARY KEY,
    status            text NOT NULL CHECK (status IN ('reconciled', 'unreconciled_payment', 'unreconciled_settlement')),
    payment_total     numeric(14, 2) NOT NULL,
    settlement_total  numeric(14, 2) NOT NULL,
    difference        numeric(14, 2) NOT NULL,
    payment_rows      integer NOT NULL,
    settlement_rows   integer NOT NULL,
    summary_mismatch  boolean NOT NULL,   -- any summary line differs for this record
    note              text
);

-- ---------------------------------------------------------------------------
-- Seed: summary layout (rows match amazon_sample_output_report.xlsx)
-- ---------------------------------------------------------------------------
INSERT INTO summary_line (line_key, section, label, sheet_row, sort_order) VALUES
    ('sales.product_charges',              'Sales',          'Product Charges',               5,  1),
    ('sales.tax',                          'Sales',          'Tax',                           6,  2),
    ('sales.shipping',                     'Sales',          'Shipping',                      7,  3),
    ('sales.amazon_fees',                  'Sales',          'Amazon fees',                   8,  4),
    ('sales.inventory_reimbursements',     'Sales',          'Inventory Reimbursements',      9,  5),
    ('sales.cross_account_debt',           'Sales',          'Cross-account Debt Adjustment', 10, 6),
    ('sales.other',                        'Sales',          'Other',                         11, 7),
    ('sales.fba_fees',                     'Sales',          'FBA Fees',                      12, 8),
    ('sales.micro_deposit_failed',         'Sales',          'Micro Deposit (Failed)',        13, 9),
    ('refunds.refund_expenses',            'Refunds',        'Refund expenses',               16, 10),
    ('refunds.refunded_sales',             'Refunds',        'Refunded sales',                17, 11),
    ('expenses.promo_rebates',             'Expenses',       'Promo rebates',                 20, 12),
    ('expenses.fba_fees',                  'Expenses',       'FBA fees',                      21, 13),
    ('expenses.cost_of_advertising',       'Expenses',       'Cost of Advertising',           22, 14),
    ('expenses.shipping_charges',          'Expenses',       'Shipping Charges',              23, 15),
    ('expenses.amazon_fees',               'Expenses',       'Amazon fees',                   24, 16),
    ('expenses.reversed_reimbursements',   'Expenses',       'Reversed Reimbursements',       25, 17),
    ('expenses.cross_account_debt',        'Expenses',       'Cross-account Debt Adjustment', 26, 18),
    ('expenses.other',                     'Expenses',       'Other',                         27, 19),
    ('expenses.micro_deposit',             'Expenses',       'Micro Deposit',                 28, 20),
    ('paid_to_amazon',                     'Paid To Amazon', 'Paid To Amazon',                32, 21)
ON CONFLICT (line_key) DO UPDATE SET section = EXCLUDED.section, label = EXCLUDED.label,
    sheet_row = EXCLUDED.sheet_row, sort_order = EXCLUDED.sort_order;

-- Seed: how every summary field used by the configs lands on the sheet.
-- Fields named by the configs but absent here are reported as "orphan" by `recon lint`.
INSERT INTO summary_field_route (summary_field, sign, line_key, excluded_reason) VALUES
    ('sales_product_charges',              'any',      'sales.product_charges', NULL),
    ('sales_tax',                          'any',      'sales.tax', NULL),
    ('sales_shipping',                     'any',      'sales.shipping', NULL),
    ('sales_amazon_fees',                  'any',      'sales.amazon_fees', NULL),
    ('sales_inventory_reimbursements',     'any',      'sales.inventory_reimbursements', NULL),
    ('sales_cross_account_debt_adjustment','any',      'sales.cross_account_debt', NULL),
    ('sales_other',                        'any',      'sales.other', NULL),
    ('sales_fba_fees',                     'any',      'sales.fba_fees', NULL),
    ('refunded_expenses',                  'any',      'refunds.refund_expenses', NULL),
    ('refunded_sales',                     'any',      'refunds.refunded_sales', NULL),
    ('expenses_promotional_rebates',       'any',      'expenses.promo_rebates', NULL),
    ('expenses_fba_fees',                  'any',      'expenses.fba_fees', NULL),
    ('expenses_cost_of_advertising',       'any',      'expenses.cost_of_advertising', NULL),
    ('expenses_shipping_charges',          'any',      'expenses.shipping_charges', NULL),
    ('expenses_amazon_fees',               'any',      'expenses.amazon_fees', NULL),
    ('expenses_reversed_reimbursements',   'any',      'expenses.reversed_reimbursements', NULL),
    ('expenses_cross_account_debt_adjustment','any',   'expenses.cross_account_debt', NULL),
    ('expenses_other',                     'any',      'expenses.other', NULL),
    ('paid_to_amazon',                     'any',      'paid_to_amazon', NULL),
    -- composite fields: the line is chosen by the sign of the amount
    ('bank_account_transfer_round_off',    'positive', 'sales.micro_deposit_failed', NULL),
    ('bank_account_transfer_round_off',    'negative', 'expenses.micro_deposit', NULL),
    ('total_refund_expense_or_sales_amt',  'positive', 'refunds.refund_expenses', NULL),
    ('total_refund_expense_or_sales_amt',  'negative', 'refunds.refunded_sales', NULL),
    ('total_adjustment_other_buyer_recharge_amt', 'positive', 'sales.other', NULL),
    ('total_adjustment_other_buyer_recharge_amt', 'negative', 'expenses.other', NULL),
    -- balance-sheet movements: not part of the P&L summary by design
    ('current_reserve_amount',             'any',      NULL, 'reserve balance (balance sheet, not P&L)'),
    ('beginning_balance',                  'any',      NULL, 'opening balance carried in (balance sheet)'),
    ('amazon_carried_forward',             'any',      NULL, 'balance carried forward (balance sheet)')
ON CONFLICT (summary_field, sign) DO UPDATE SET line_key = EXCLUDED.line_key,
    excluded_reason = EXCLUDED.excluded_reason;
