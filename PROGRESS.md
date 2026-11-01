# Progress log

How I got from five files to a summary that ties out. It covers what I tried, what I
ruled out and how each variance was isolated. The queries referenced are in
`sql/investigation/`; run them against the database after `make before` or `make after`.

## 1. Read the brief, profile the files

First questions: what exactly is "the same settlement period", what links a payments
row to settlement rows, and what the summary must tie to.

Profiling (pandas, before writing any Go) gave these facts:

- **Settlement report:** 54,980 lines. The first line after the header is a summary row: settlement 12395580393, 17.07.2026 07:06 → 31.07.2026 07:06 UTC, `total-amount` **212,118.95 AUD**. All 54,979 data rows belong to that one settlement, and their amounts sum to exactly 212,118.95.
- **Payments report:** 9 lines of notes, a header, then 23,026 rows. Four settlement ids, statuses Released and Deferred. The `date/time` values run from 29 Jun to 31 Jul. On every row the amount columns sum exactly to `total`, so there are no hidden components.
- **Configs:** 149 payment rules and 137 settlement rules. Keys are upper-case with underscores; report values are mixed case with spaces, hyphens and colons.

## 2. Scope: which payments rows make up this settlement?

`01_scope_tieout.sql`: payments rows with settlement id 12395580393 and status
**Released**, excluding one `Transfer` row, sum to **212,118.95**, identical to Amazon's
total.

- The deferred rows of the same settlement id (2,398 rows, 43,540.07) are not in it. They will be released into a later settlement.
- The Transfer (-133,756.51 on 18 Jul, just after the previous settlement closed on 17 Jul) is the payout of the previous settlement. Settlement flat files never contain transfer lines.

Decision: the scope is settlement id ∈ settlement report, status Released. Everything is still ingested, and excluded rows are flagged with a reason.

## 3. The `date` in record_ref: first attempt matched nothing

The record_ref for an order is `txn_ref+sku+date`. My first prototype used the
payments `date/time` and matched **0** of the settlement's order keys. One example
showed why. Order 249-3364825-5895055 has `date/time` 9 July in the payments report but
`posted-date` 17.07.2026 in the settlement report. Its `Transaction Release Date` is
17 July 4:26:32 pm GMT+9, which is 07:26:32 UTC, exactly the settlement's
posted-date-time.

`02_date_token.sql` compares the candidate definitions:

| payments date used | matching order keys (of 13,217) |
|---|---:|
| date/time, local | 0 |
| date/time, UTC | 0 |
| release date, local (GMT+9) | 8,088 |
| **release date, UTC** | **13,217** |

Decision: payments `date` = Transaction Release Date in UTC (B1 in MAPPING_FIXES.sql).
This is a matching-logic decision, not a config change. Releases between 00:00 and
09:00 GMT+9 fall on the previous UTC day, which is why local time only got 8,088.

The release date column also mixes "17 July 2026" and "1 Aug 2026". The first Go
ingest stopped on line 13,303 with an unparseable month, so both formats are now
supported (B5).

## 4. Prototype the mapping, then a mistake in my own code

A Python prototype applied both configs and printed the summary by field per side,
plus record-level totals. Record level: 13,289 record_refs on both sides, 26 only on
payments (25 at zero value and the transfer), and none only on settlements.

It also reported 3 reconciled records whose totals differed by -6.17 or -2.87. When I
grepped the raw lines, both sides agreed to the cent. The bug was mine: I de-duplicated
rows by `(line number, record_ref)`, and a payments line number happened to equal a
settlement line number for the same order, so one settlement row was dropped. After
keying by side and line, there were 0 total differences.

The Go version avoids this by giving every source row a globally unique id. The lesson
for the investigation: check that a difference exists in the source before explaining
it.

## 5. Go pipeline

Built the schema, streaming ingest with COPY, the matcher, the reconciliation SQL and
the workbook. Decisions I made deliberately:

- **One `source_row` table for both reports**, with the untouched `raw` row and its line number, so every figure is auditable.
- **Configs in tables, keyed by original CSV line**, with a change-log trigger, so fixes are data changes that can be reviewed afterwards.
- **Duplicate rules are applied, not de-duplicated**, so a duplicated config row shows up as a double count.
- **Summary computed during ingest** (bonus), and checked against the SQL aggregation in every report.
- **Integer cents** everywhere.
- **Summary layout and field→line routing as data** (`summary_line`, `summary_field_route`), so the report code has no accounting rules in it.

The Go numbers reproduced the prototype exactly.

## 6. Before-fix report: 7 lines disagree

`out/report_before_fixes.xlsx`:

| Line | Payments | Settlements | P − S |
|---|---:|---:|---:|
| Product Charges | 348,815.93 | 348,908.77 | -92.84 |
| Shipping | 21,773.13 | 8,200.96 | 13,572.17 |
| Sales / Other | 0.00 | 11.97 | -11.97 |
| Refund expenses | 258.87 | 216.28 | 42.59 |
| Promo rebates | 0.00 | -11,273.53 | 11,273.53 |
| Expenses / Amazon fees | -266,349.92 | -132,593.41 | -133,756.51 |
| Expenses / Other | -11,273.53 | 0.00 | -11,273.53 |
| **Total of lines** | **91,872.39** | **212,118.95** | vs Amazon 212,118.95 |

The settlement column already tied to Amazon. The payments column was 120,246.56 off,
and 6,942 records reconciled on totals but had their amounts on different lines.

## 7. Tracing each variance to its rule

The **Variance Drilldown** sheet (`05_variance_by_rule.sql`) lists, for each line that
differs, the rules feeding it on each side. I read it line by line.

**Amazon fees -133,756.51 (F1).** On the payments side, one component comes from rule
**110**, match kind **fallback**, type TRANSFER, description "To account ending with:
334". The TRANSFER rules 62 and 63 expect `TO_ACCOUNT_ENDING` exactly, so the row fell
to the catch-all `,any,total → expenses_amazon_fees`.

- Ruled out "the transfer belongs in the settlement": the settlement file has no transfer lines, and the in-scope total without it equals Amazon's.
- Ruled out hard-coding `..._WITH:_334` in the config, because it would break when the bank account changes.
- Fix: a `description_match = 'prefix'` column honoured by the matcher, set on 62 and 63. I also added two `other` rules for TRANSFER. The TRANSFER rules only cover `total`, so the same row's `other` column still falls to catch-all 111. I verified this by running the fixes without the inserts: there are 27 unreconciled payments instead of 26, and the extra one is a stray `+12395580393+2026-07-18` record.

**Promo rebates +11,273.53 / Expenses Other -11,273.53 (F2).** Payments rule 65 sends
`promotional_rebates` to `expenses_other`. Settlement rules 72 and 73 send Promotion
Principal and Shipping to `expenses_promotional_rebates`. The amounts are identical
(3,983 records). Fix: update rule 65.

**Product Charges -92.84, Shipping +13,572.17, Sales Other -11.97 (F3, F4, F5).**
These three lines move together, so I decomposed them:

- Payments rules 71 and 72 are the same key (ORDER / any / sales_tax_collected), mapped to Product Charges **and** Shipping. Lint flags it as `duplicate-key`. The same holds for low_value_goods (rules 5 and 6).
- `03_tax_components.sql`: the payments `sales tax collected` total (13,675.59) equals settlement Tax 13,765.71 + ShippingTax 581.68 + GiftWrapTax 0.36 + TaxDiscount -672.16. So the settlement splits tax over three lines that the payments report cannot split.
- `04_lvg_vs_tax.sql`: on all 50 low-value-goods lines, the withheld amount exactly cancels the tax collected on the line.
- Decomposition, which checks out to the cent:
  - Product Charges -92.84 = tax -90.12 + LVG -2.72.
  - Shipping +13,572.17 = duplicated tax and LVG (+13,478.97) − settlement shipping tax, tax discount and LVG (-93.20).
  - Sales Other -11.97 = gift wrap 11.61 lost on payments (rule 70 targets `sales_gift_wrap_credits`, which is not a Summary line, so lint flags `orphan-summary-field`) + gift wrap tax 0.36 that exists only on the settlement side.
- Fix: all order tax and LVG go to the **Tax** line on both sides (it was unused by any order rule), the duplicates are deleted, and gift wrap goes to Sales Other.
- Alternative considered: keep tax inside Product Charges. It also balances, but leaves the Tax line empty and mixes GST into revenue. It is noted in MAPPING_FIXES.sql as a presentation choice for finance.

**Refund expenses +42.59 (F6).** Settlement rules 4, 11 and 20 put refund Tax,
ShippingTax and TaxDiscount (-41.09, -3.19, +1.69) on Refund expenses. Payments rule 14
(REFUND / sales_tax_collected) has empty summary fields, so the payments side never
counts that money. Fix: route it to refunded_expenses. I considered moving refund tax
to the Tax line as well, but left it: the four settlement rules agree with each other,
so changing them is a finance decision, not a defect fix.

**Looked at and deliberately not changed.** FBA per-unit fulfilment fees are on Amazon
fees, not FBA fees, on both sides. That is consistent and causes no variance. The
payments `fba_fees` column also bundles shipping and gift wrap chargebacks, so a move
would have to be all or nothing. Changing it would be presentation, not a defect.

## 8. Latent defects: rules this data never touches

`recon lint` (also the **Config Lint** sheet) runs static checks:

- duplicate keys,
- summary fields that are not on the sheet,
- unknown or repeated record_ref tokens,
- one-sided sign routing,
- record_ref templates that exist on only one side.

On top of that, I compared the configs rule by rule. That gave C1–C10 in
MAPPING_FIXES.sql. Examples:

- Settlement 138 builds its key with a non-field token `record_type` and repeats `settlement_id`.
- AWD storage is keyed `AWD_STORAGE_FEES` on payments but `AWD_STORAGE_FEE` on settlements.
- SAFE-T reimbursements go to FBA fees on payments but Inventory Reimbursements on settlements.
- Negative FBA fee corrections are silently dropped.
- The catch-all rule books any unknown payments line as an Amazon fee. That rule is what hid F1.

I proposed these but did not apply them: without a row exercising them I cannot prove
the fix is right.

## 9. After fixes

`make after` re-ingests from the raw files with the fixed configs:

- All 21 Summary lines agree.
- Total of lines = **212,118.95** on both sides = Amazon's `total-amount`.
- Records: 13,289 reconciled, 26 unreconciled payments (25 zero-value lines and the transfer, each explained in the Note column), 0 unreconciled settlements.
- Reconciled records with a total difference: 0. With a per-line difference: **6,942 → 0** (`06_record_level_mismatch.sql`).
- `07_config_change_log.sql` lists the 18 config changes the fixes made, from the audit trigger.

## 10. Verification

- **Independent re-implementation.** I fed the fixed configs, exported from the database, back into the separate Python prototype, which has different code and different normalisation. It reproduces every summary field and the 13,289 / 26 / 0 counts.
- **Streaming vs SQL.** The summary accumulated during ingest equals the SQL aggregation of the mapped records (checked in every report, Run Info).
- **Tie-out.** Both columns equal Amazon's `total-amount`. What is left out (the transfer) is listed on the Summary sheet.
- **Tests.** Unit tests for money parsing, normalisation, date parsing, the matcher tiers, record_ref and lint. An end-to-end test on a fixture checks the UTC release-date match, scope filtering, duplicate double counting, the catch-all trap, the prefix fix, the streaming summary and idempotent re-ingest.

## Open questions for the team

1. Tax presentation (assumption 3): Tax line, or inside Product Charges?
2. Should refund tax also move to the Tax line, for consistency with F4?
3. Should the catch-all rule keep routing unknown totals to Amazon fees (C10)?
4. Do the payments "order ID" values for inbound defect / placement fees carry the shipment id (C8)?
