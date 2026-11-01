-- =============================================================================
-- MAPPING_FIXES.sql
-- Fixes for the defects found in amazon_payment_configs_au_old.csv and
-- amazon_settlement_configs_au.csv while reconciling settlement 12395580393.
--
-- Config ids are the line numbers of the original CSV files (header = line 1), so
-- "payment_config 65" is line 65 of amazon_payment_configs_au_old.csv.
-- New rows get id 1000 + the line number of the rule they extend.
-- Every statement also checks the old value in its WHERE clause, so running it
-- against an unexpected config changes nothing instead of changing the wrong row.
-- All changes are recorded by trigger in config_change_log.
--
-- Run:  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f MAPPING_FIXES.sql
--       then re-ingest (make after).
--
-- Contents
--   A. Defects that caused variances in this settlement   (applied)
--        F1  bank transfer booked as Amazon fees           -133,756.51
--        F2  promo rebates on the wrong line               +/-11,273.53
--        F3  gift wrap routed to a field not on the sheet        11.61
--        F4  order tax counted twice / split unmatchably      13,675.59
--        F5  low-value-goods tax counted twice                   196.62
--        F6  refund tax never summarised on payments side         42.59
--   B. Fixes in the pipeline's interpretation, not in the config (no SQL)
--   C. Latent defects: rules this settlement does not exercise (NOT applied;
--      proposed statements are commented out)
-- =============================================================================

BEGIN;

-- -----------------------------------------------------------------------------
-- F1. Bank transfer falls through to the catch-all rule and is booked as an
--     Amazon fee.
--     Rule:    payment_config 62/63  TRANSFER / TO_YOUR_ACCOUNT_ENDING|TO_ACCOUNT_ENDING
--              / total, record_ref TRANSFER+description+settlement_id+date, no summary.
--     Data:    the report's description is "To account ending with: 334" (the
--              last digits of the bank account are part of the text), so the exact
--              match fails. The row then matches catch-all payment_config 110
--              (empty transaction_type / any / total -> expenses_amazon_fees) and
--              111 (any / other). The payout of the previous settlement,
--              -133,756.51, appears as Amazon fees on the Payments side.
--     Why a schema change: the config schema can only express exact descriptions
--              or "any". Hard-coding "TO_ACCOUNT_ENDING_WITH:_334" would work today
--              and break when the seller's bank account changes. I added a column,
--              description_match ('exact' default = original behaviour | 'prefix'),
--              that the matcher honours (exact > prefix > any > catch-all). Only
--              these rules use it.
--     Also:    the TRANSFER rules only cover the `total` column; the same row's
--              `other` column (-133,756.51) still fell to catch-all 111 and created
--              a stray record_ref. Two `other` rules with the same record_ref and no
--              summary close that gap, mirroring MICRO_DEPOSIT (lines 60/61).
--     After:   the transfer is matched by rule 63, is not summarised (correct: it is
--              a payout, not P&L), and is an explained unreconciled payment
--              (settlement flat files carry no transfer lines).
-- -----------------------------------------------------------------------------
UPDATE payment_config SET description_match = 'prefix'
 WHERE id IN (62, 63) AND transaction_type = 'TRANSFER' AND amount_field = 'total'
   AND description IN ('TO_YOUR_ACCOUNT_ENDING', 'TO_ACCOUNT_ENDING');

INSERT INTO payment_config (id, transaction_type, description, amount_field, record_ref,
       to_summary_field_when_positive_amount, to_summary_field_when_negative_amount, description_match)
VALUES (1062, 'TRANSFER', 'TO_YOUR_ACCOUNT_ENDING', 'other', 'TRANSFER+description+settlement_id+date', '', '', 'prefix'),
       (1063, 'TRANSFER', 'TO_ACCOUNT_ENDING',      'other', 'TRANSFER+description+settlement_id+date', '', '', 'prefix');

-- -----------------------------------------------------------------------------
-- F2. Promotional rebates land on Expenses / Other on the Payments side.
--     Rule:    payment_config 65  ORDER / any / promotional_rebates
--              -> expenses_other (both signs)
--     Counterpart: settlement_config 72/73 ORDER / Promotion / Principal|Shipping
--              -> expenses_promotional_rebates. Same money: payments
--              promotional_rebates = Promotion/Principal + Promotion/Shipping =
--              -11,273.53 across 3,983 records, cent for cent.
--     Effect:  Promo rebates  P 0 vs S -11,273.53; Expenses/Other  P -11,273.53 vs S 0.
--     Fix:     route to expenses_promotional_rebates (the "Promo rebates" line).
-- -----------------------------------------------------------------------------
UPDATE payment_config
   SET to_summary_field_when_positive_amount = 'expenses_promotional_rebates',
       to_summary_field_when_negative_amount = 'expenses_promotional_rebates'
 WHERE id = 65 AND transaction_type = 'ORDER' AND amount_field = 'promotional_rebates'
   AND to_summary_field_when_positive_amount = 'expenses_other';

-- -----------------------------------------------------------------------------
-- F3. Gift wrap credits routed to a summary field that does not exist on the sheet.
--     Rule:    payment_config 70  ORDER / any / gift_wrap_credits -> sales_gift_wrap_credits
--     Effect:  "sales_gift_wrap_credits" is not a Summary line (it appears in no
--              other rule), so 11.61 vanishes from the Payments summary. The
--              settlement side maps the same money (ORDER / ItemPrice / GiftWrap,
--              settlement_config 66) to sales_other.
--     Fix:     route to sales_other.
-- -----------------------------------------------------------------------------
UPDATE payment_config
   SET to_summary_field_when_positive_amount = 'sales_other',
       to_summary_field_when_negative_amount = 'sales_other'
 WHERE id = 70 AND transaction_type = 'ORDER' AND amount_field = 'gift_wrap_credits'
   AND to_summary_field_when_positive_amount = 'sales_gift_wrap_credits';

-- -----------------------------------------------------------------------------
-- F4. Order tax: counted twice on the Payments side, and split across three lines
--     on the Settlement side in a way the Payments report cannot reproduce.
--     Rules:   payment_config 71  ORDER / any / sales_tax_collected -> sales_product_charges
--              payment_config 72  ORDER / any / sales_tax_collected -> sales_shipping   (duplicate key)
--              settlement_config 65 ORDER / ItemPrice / Tax          -> sales_product_charges
--              settlement_config 63 ORDER / ItemPrice / ShippingTax  -> sales_shipping
--              settlement_config 95 ORDER / Promotion / TaxDiscount  -> sales_shipping
--              settlement_config 61 ORDER / ItemPrice / GiftWrapTax  -> sales_other
--     Data:    the payments column "sales tax collected" (13,675.59) is exactly
--              Tax 13,765.71 + ShippingTax 581.68 + GiftWrapTax 0.36 + TaxDiscount
--              -672.16 (sql/investigation/03_tax_components.sql). One column on one
--              side, four components on the other.
--     Effect:  the duplicate adds 13,675.59 to Shipping; and even without it, Product
--              Charges / Shipping / Other could not agree, because the payments
--              report has no way to tell shipping tax from product tax.
--     Fix:     every order-tax component goes to sales_tax (the "Tax" line of the
--              Summary sheet, which no order rule used) on both sides, and the
--              duplicate payments rule is deleted. GST collected is a liability, not
--              revenue, so it belongs on its own line rather than inside product
--              charges or shipping.
--     Alternative considered: keep tax inside Product Charges (delete 72 and point
--              63/95/61 at sales_product_charges). It also closes the variance, but
--              leaves the Tax line empty and mixes GST into revenue. Presentation
--              choice to confirm with finance.
-- -----------------------------------------------------------------------------
DELETE FROM payment_config
 WHERE id = 72 AND transaction_type = 'ORDER' AND amount_field = 'sales_tax_collected'
   AND to_summary_field_when_positive_amount = 'sales_shipping';

UPDATE payment_config
   SET to_summary_field_when_positive_amount = 'sales_tax',
       to_summary_field_when_negative_amount = 'sales_tax'
 WHERE id = 71 AND transaction_type = 'ORDER' AND amount_field = 'sales_tax_collected'
   AND to_summary_field_when_positive_amount = 'sales_product_charges';

UPDATE settlement_config
   SET to_summary_field_when_positive_amount = 'sales_tax',
       to_summary_field_when_negative_amount = 'sales_tax'
 WHERE transaction_type = 'ORDER'
   AND ((id = 65 AND amount_type = 'ITEMPRICE' AND amount_description = 'TAX'         AND to_summary_field_when_positive_amount = 'sales_product_charges')
     OR (id = 63 AND amount_type = 'ITEMPRICE' AND amount_description = 'SHIPPINGTAX' AND to_summary_field_when_positive_amount = 'sales_shipping')
     OR (id = 95 AND amount_type = 'PROMOTION' AND amount_description = 'TAXDISCOUNT' AND to_summary_field_when_positive_amount = 'sales_shipping')
     OR (id = 61 AND amount_type = 'ITEMPRICE' AND amount_description = 'GIFTWRAPTAX' AND to_summary_field_when_positive_amount = 'sales_other'));

-- -----------------------------------------------------------------------------
-- F5. Low-value-goods (LVG) tax: counted twice on the Payments side.
--     Rules:   payment_config 5   ORDER / any / low_value_goods -> sales_shipping
--              payment_config 6   ORDER / any / low_value_goods -> sales_product_charges (duplicate key)
--              settlement_config 119 ORDER / ItemWithheldTax / LowValueGoodsTax-Principal -> sales_product_charges
--              settlement_config 118 ORDER / ItemWithheldTax / LowValueGoodsTax-Shipping  -> sales_shipping
--              settlement_config 127 ORDER / ItemWithheldTax / LowValueGoodsTax-Other     -> sales_other
--     Data:    on all 50 LVG lines the withheld amount exactly cancels the tax
--              collected on the same line (sql/investigation/04_lvg_vs_tax.sql):
--              Amazon collects the GST on low-value imports and remits it itself.
--     Effect:  -196.62 counted in both Product Charges and Shipping; the same
--              one-column-vs-split problem as F4.
--     Fix:     LVG withholding goes to sales_tax on both sides, next to the tax it
--              cancels (the Tax line then nets to zero for those orders, which is the
--              economic truth); delete the duplicate.
-- -----------------------------------------------------------------------------
DELETE FROM payment_config
 WHERE id = 5 AND transaction_type = 'ORDER' AND amount_field = 'low_value_goods'
   AND to_summary_field_when_positive_amount = 'sales_shipping';

UPDATE payment_config
   SET to_summary_field_when_positive_amount = 'sales_tax',
       to_summary_field_when_negative_amount = 'sales_tax'
 WHERE id = 6 AND transaction_type = 'ORDER' AND amount_field = 'low_value_goods'
   AND to_summary_field_when_positive_amount = 'sales_product_charges';

UPDATE settlement_config
   SET to_summary_field_when_positive_amount = 'sales_tax',
       to_summary_field_when_negative_amount = 'sales_tax'
 WHERE transaction_type = 'ORDER' AND amount_type = 'ITEMWITHHELDTAX'
   AND ((id = 119 AND amount_description = 'LOWVALUEGOODSTAX-PRINCIPAL' AND to_summary_field_when_positive_amount = 'sales_product_charges')
     OR (id = 118 AND amount_description = 'LOWVALUEGOODSTAX-SHIPPING'  AND to_summary_field_when_positive_amount = 'sales_shipping')
     OR (id = 127 AND amount_description = 'LOWVALUEGOODSTAX-OTHER'     AND to_summary_field_when_positive_amount = 'sales_other'));

-- -----------------------------------------------------------------------------
-- F6. Refund tax is never summarised on the Payments side.
--     Rule:    payment_config 14  REFUND / any / sales_tax_collected -> '' / ''
--     Counterpart: settlement_config 4 (Tax, negative), 11 (ShippingTax), 12
--              (GiftWrapTax) and 20 (TaxDiscount) all route refund tax to
--              refunded_expenses.
--     Effect:  Refund expenses  P 258.87 vs S 216.28: the -42.59 of tax refunded to
--              buyers exists only on the Settlement side, and the Payments summary
--              stops tying to Amazon's total.
--     Fix:     route to refunded_expenses, matching the settlement rules. (Minimal
--              fix. Moving refund tax to the Tax line would be the logical sequel to
--              F4, but changes four settlement rules that are consistent with each
--              other, so I left that for finance to decide.)
-- -----------------------------------------------------------------------------
UPDATE payment_config
   SET to_summary_field_when_positive_amount = 'refunded_expenses',
       to_summary_field_when_negative_amount = 'refunded_expenses'
 WHERE id = 14 AND transaction_type = 'REFUND' AND amount_field = 'sales_tax_collected'
   AND to_summary_field_when_positive_amount = '' AND to_summary_field_when_negative_amount = '';

-- Guard: fail the whole script if any fix did not apply exactly as intended.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM config_change_log;
  -- F1: 2 updated + 2 inserted | F2: 1 | F3: 1 | F4: 1 deleted + 1 + 4 updated
  -- F5: 1 deleted + 1 + 3 updated | F6: 1                          = 18
  IF n <> 18 THEN
    RAISE EXCEPTION 'MAPPING_FIXES: expected 18 config changes, got % (configs not in the expected original state?)', n;
  END IF;
END $$;

COMMIT;

-- =============================================================================
-- B. Fixed in the pipeline's interpretation of the configs (matching logic),
--    not in the config data. No SQL; documented here as requested.
-- =============================================================================
-- B1. The `date` token of record_ref.
--     For the settlement report, `date` = posted-date, the UTC date the money was
--     released into the settlement. The payments report's `date/time` is the order
--     posting time in GMT+9, which is a different event. Using it matches 0 of the
--     13,217 settlement order keys; release date in GMT+9 matches 8,088; release date
--     converted to UTC matches 13,217 / 13,217 (sql/investigation/02_date_token.sql).
--     => payments `date` = Transaction Release Date, converted to UTC.
-- B2. Normalisation. Config keys and report values are compared on a canonical key:
--     upper case, runs of space / "_" / "-" / en dash / ":" collapsed to "_".
--     ("FBA Inventory Reimbursement - Customer Return" =
--     "FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN", "other-transaction" =
--     "OTHER-TRANSACTION", "FBA_LONG_TERM" = "FBA_LONG-TERM".)
-- B3. Duplicate rules are applied, not de-duplicated. The matcher returns every rule
--     in the best tier, so a duplicated config row shows up as a double count in the
--     numbers instead of being hidden by "first match wins". (That is how F4 and F5
--     surfaced.)
-- B4. Scope. Only payments rows of the settlement in the settlement report AND with
--     Transaction status = Released are reconciled. Deferred rows (2,398, 43,540.07)
--     are released into a later settlement; rows of other settlement ids belong to
--     other periods. They are ingested and kept (in_scope = false, scope_reason).
-- B5. Mixed month formats in the payments report ("17 July 2026", "1 Aug 2026") are
--     both parsed.

-- =============================================================================
-- C. Latent defects: rules not exercised by settlement 12395580393, so no variance
--    can prove the fix. Found by `recon lint` and by comparing the two configs
--    rule by rule. NOT applied; proposed statements are commented out.
-- =============================================================================

-- C1. settlement_config 138  OTHER-TRANSACTION / OTHER-TRANSACTION / PROMOTION_ADJUSTMENT
--     record_ref "ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id":
--     `record_type` is not a field (it becomes the literal text "record_type") and
--     settlement_id is repeated, so it can never equal the payments key
--     ADJUSTMENT_OTHER+settlement_id+date (payment_config 38/39).
-- UPDATE settlement_config SET record_ref = 'ADJUSTMENT_OTHER+settlement_id+date'
--  WHERE id = 138 AND record_ref = 'ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id';

-- C2. payment_config 31  ADJUSTMENT / FBA_INVENTORY_REIMBURSEMENT_-_FEE_CORRECTION / other
--     positive -> sales_other, negative -> '' : negative fee corrections are silently
--     dropped from the summary. Every comparable rule routes both signs.
-- UPDATE payment_config SET to_summary_field_when_negative_amount = 'expenses_other'
--  WHERE id = 31 AND to_summary_field_when_negative_amount = '';

-- C3. AWD storage keys differ between the configs:
--     payment_config 7/93     txn_ref+AWD_STORAGE_FEES+settlement_id+date
--     settlement_config 48    txn_ref+AWD_STORAGE_FEE+settlement_id+date
--     (the sibling AWD processing / transportation rules use identical keys on both
--     sides), so AWD storage fees would never reconcile.
-- UPDATE settlement_config SET record_ref = 'txn_ref+AWD_STORAGE_FEES+settlement_id+date'
--  WHERE id = 48 AND record_ref = 'txn_ref+AWD_STORAGE_FEE+settlement_id+date';

-- C4. SAFE-T reimbursements land on different lines:
--     payment_config 59 (other) -> expenses_fba_fees, settlement_config 79 ->
--     sales_inventory_reimbursements. A SAFE-T claim is a reimbursement to the seller.
-- UPDATE payment_config SET to_summary_field_when_positive_amount = 'sales_inventory_reimbursements',
--                           to_summary_field_when_negative_amount = 'sales_inventory_reimbursements'
--  WHERE id = 59 AND to_summary_field_when_positive_amount = 'expenses_fba_fees';

-- C5. settlement_config 4  REFUND / ItemPrice / Tax: positive -> total_refund_expense_or_sales_amt,
--     negative -> refunded_expenses, while ShippingTax (11) and GiftWrapTax (12)
--     route both signs to refunded_expenses. All 13 refund-tax lines here are negative.
-- UPDATE settlement_config SET to_summary_field_when_positive_amount = 'refunded_expenses'
--  WHERE id = 4 AND to_summary_field_when_positive_amount = 'total_refund_expense_or_sales_amt';

-- C6. Tax on A-to-z / chargeback refunds has settlement rules (81, 80, 103-109) but no
--     payments rule for sales_tax_collected / low_value_goods under
--     A-TO-Z_GUARANTEE_CLAIM or CHARGEBACK_REFUND (and no catch-all for those
--     columns), so those amounts would be unmatched on the Payments side. Needs
--     INSERTs once the expected lines are agreed (refunded_expenses / expenses_other
--     respectively, mirroring settlement 81 and 107).

-- C7. DEALS: payment_config 8 keys deals fees as txn_ref+settlement_id+date, the
--     settlement side (92/93/115/116) as DEALS_PROMOTION_FEE[_SPECIAL]+settlement_id+date.
--     They can never reconcile. The payments report does not show whether a deal fee
--     is "special", so the fix needs a sample payments deals line first.

-- C8. INBOUND_DEFECT_FEE (payment 83/84 vs settlement 49) and
--     FBA_INBOUND_PLACEMENT_SERVICE_FEE (payment 96/97 vs settlement 24): payments
--     keys start with txn_ref, settlement keys with shipment_id. Correct only if the
--     payments "order ID" column carries the shipment id for these fee lines; this
--     settlement has none to verify against.

-- C9. settlement_config 124 (TRANSFER_OF_FUNDS_UNSUCCESSFUL...) has an empty record_ref
--     and no summary field: a failed disbursement could never reconcile with
--     payment_config 135 (FAILED_DISBURSEMENT+description+settlement_id+date).

-- C10. payment_config 110 (catch-all: empty transaction_type / any / total ->
--     expenses_amazon_fees) books ANY unrecognised payments transaction as an Amazon
--     fee. That is how F1 stayed hidden in plain sight. The settlement config has no
--     catch-all, so the two sides treat unknown lines differently. Recommendation:
--     route the catch-all to no summary field, so unknown lines show up in the
--     tie-out ("not routed") instead of inside a real expense line. The pipeline
--     already reports every catch-all match (Run Info, Variance Drilldown).
