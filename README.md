# Amazon Payments vs Settlement reconciliation

Go + PostgreSQL pipeline that ingests Amazon's Payments report and Settlement report for
the same settlement, maps both through config-driven rules, reconciles them record by
record, and produces an accounting Summary that ties out to the settlement total
Amazon itself reports. It also contains the investigation and fixes for the defects
in the mapping configs.

**Result for settlement 12395580393 (17–31 Jul 2026, AUD), after `MAPPING_FIXES.sql`:**

| | Payments | Settlements | Difference |
|---|---:|---:|---:|
| Sales | 357,801.39 | 357,801.39 | 0.00 |
| Refunds | -1,815.09 | -1,815.09 | 0.00 |
| Expenses | -143,867.35 | -143,867.35 | 0.00 |
| Paid To Amazon | 0.00 | 0.00 | 0.00 |
| **Total of summary lines** | **212,118.95** | **212,118.95** | **0.00** |
| Amazon settlement `total-amount` | 212,118.95 | 212,118.95 | |

All 21 Summary lines agree, and both columns equal Amazon's own total. Records:
**13,289 reconciled · 26 unreconciled payments · 0 unreconciled settlements.** Every
unreconciled record is explained:

- 25 are zero-value payments lines. The settlement report has no $0 lines.
- 1 is the bank transfer of -133,756.51. It is the payout of the previous settlement, and settlement files carry no transfer lines.

No reconciled record has a total difference or a per-line difference.

Before the fixes, 7 Summary lines disagreed, the Payments column was 120,246.56 away
from Amazon's total, and 6,942 reconciled records had their amounts on different
lines on each side. `MAPPING_FIXES.sql` and [PROGRESS.md](PROGRESS.md) document every
defect, its root cause and the fix. The before and after reports and the database dump
are shared separately (link in the submission email), because they contain buyer order data.

---

## Run it

Requirements: Go ≥ 1.22, PostgreSQL ≥ 13 (or Docker), `psql`. Dependencies are vendored.

```bash
# 1. database (or use your own and set DATABASE_URL)
make db-up                      # docker compose: postgres on :5432, user/pass/db = recon
export DATABASE_URL=postgres://recon:recon@localhost:5432/recon?sslmode=disable

# 2. put the five assignment files in ./data (see data/README.md)

# 3. full flow, twice: configs as provided, then with MAPPING_FIXES.sql
make before   # load-config -> lint -> ingest -> reconcile -> out/report_before_fixes.xlsx
make after    # load-config -> MAPPING_FIXES.sql -> ingest -> reconcile -> out/report_after_fixes.xlsx

make test     # unit tests + end-to-end test on a fixture (uses database recon_test)
make dump     # pg_dump of the after-fix state -> out/recon_after_fixes.sql.gz
```

`make after` runs the same steps as these commands:

```bash
bin/recon load-config                        # configs as provided, change log reset
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f MAPPING_FIXES.sql
bin/recon ingest                             # both reports -> source_row, record_component
bin/recon reconcile                          # record_component -> recon_record
bin/recon report -out out/report.xlsx        # workbook
bin/recon lint                               # static checks on the configs in the DB
```

A full run takes about 10 s on a laptop: 78k source rows and 172k mapped amounts.
Re-running is idempotent, because each ingest replaces the previous one.

## What the workbook contains

| Sheet | |
|---|---|
| **Summary** | Layout of the sample report: Payments, Settlements, the difference, and an OK/DIFF check per line. Below it is the tie-out to Amazon's `total-amount` and the amounts the summary deliberately leaves out (the transfer). |
| **Consolidated Data** | One row per `record_ref`: reconciled first, then unreconciled payments, then unreconciled settlements. Columns follow the sample: payment side, settlement side, then the per-line differences (DS1-DS2). An **audit trail** follows on every row: record_ref, source file line numbers on both sides, config rows used, match kinds, and a note. |
| **Variance Drilldown** | For every line that differs, the config rules that put each amount there, per side. Before the fixes it shows each root cause directly. After them it reads "No differences". |
| **Config Lint** | Static problems in the configs (duplicate keys, summary fields that are not on the sheet, broken record_ref templates, one-sided keys). |
| **Run Info** | Input files with sha256, scope, row counts by scope reason, match-kind counts, the config fingerprint, the number of config changes applied, and the integrity checks (streaming summary = SQL summary, tie-out). |

Any number can be traced back to its source: Summary line → Consolidated row → file line numbers → `source_row.raw`.

## Design

### Flow

```
amazon_settlements_data.txt ─┐                                  ┌─> Summary
amazon_payments_data.csv  ───┼─> ingest ─> source_row (1 table) ┤
payment/settlement configs ──┘     │        record_component    ├─> reconcile ─> recon_record ─> Consolidated
   (tables, fixable by SQL)        └─> streaming summary ───────┘                                Drilldown / Lint / Run Info
```

### Schema (internal/db/schema.sql)

| Table | Why |
|---|---|
| `source_file` | One row per ingested file, with its sha256, header and the payments report's preamble notes. |
| `source_row` | **Both reports in one table**, as required. `raw jsonb` keeps the untouched row (header → value) and `line_no` keeps its physical line, so any figure traces back to the file. Typed columns hold a normalised projection used for matching (transaction type, description / amount type / amount description, txn_ref, sku, shipment id, timestamps, `ref_date`, `total`). `in_scope` and `scope_reason` record why a row is excluded, without deleting it. |
| `payment_config`, `settlement_config` | The configs, verbatim. `id` = line number in the original CSV, so a fix can point to "payment_config 65". Fixes are data changes. Every change is logged by trigger in `config_change_log`. `description_match` ('exact' \| 'prefix') is the one schema extension, explained under fix F1. |
| `summary_line`, `summary_field_route` | The Summary sheet layout, and which config summary field lands on which line. These are data, not code. Composite fields pick their line by sign (`bank_account_transfer_round_off`, `total_refund_expense_or_sales_amt`, `total_adjustment_other_buyer_recharge_amt`). Balance-sheet fields (`current_reserve_amount`, `beginning_balance`, `amazon_carried_forward`) are explicitly excluded, with a reason. |
| `record_component` | One mapped amount per (source row × amount column × matching rule). A payments row explodes into one component per non-zero amount column, plus `total`, which is the amount used for reconciliation. A settlement row is one component. Each component carries the rule id, match kind, record_ref, summary field and summary line. |
| `streaming_summary` | The summary accumulated **while the rows are being ingested** (bonus requirement). The report checks that it equals the SQL aggregation of `record_component`. |
| `recon_record` | One row per record_ref: status, totals per side, difference, row counts, a per-line mismatch flag, and a note. |
| `ingest_run` | The run, its scope and an md5 fingerprint of both config tables. |

### Applying the configs (internal/mapping)

- **Normalisation.** Report values and config keys are compared on a canonical key: upper case, with any run of space, `_`, `-`, `–` or `:` collapsed to `_`. Without it, "FBA Inventory Reimbursement - Customer Return" ≠ `FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN` and `FBA_LONG_TERM` ≠ `FBA_LONG-TERM`.
- **Rule selection.** Match on transaction type plus the amount column (payments) or amount type (settlement). Then the description tier: exact, then `prefix` (only for rules that declare it), then `any`. Only if the transaction type has no applicable rule do the empty-transaction_type catch-all rules apply.
- **Duplicates are applied, not hidden.** All rules in the winning tier are used. A duplicated config row shows up as a double count instead of being masked by "first match wins". That is how F4 and F5 surfaced.
- **record_ref.** The template is split on `+`. Known field tokens are replaced by row values (`txn_ref`, `sku`, `date`, `settlement_id`, `description`, plus `shipment_id`, `merchant_order_id` and `adjustment_id` on the settlement side). Everything else is a literal (`ADJUSTMENT`, `GENERAL ADJUSTMENT`, …).
- **The `date` token** is the UTC calendar date of the moment the money entered the settlement. For the settlement report that is `posted-date`. For the payments report it is `Transaction Release Date` converted from GMT+9 to UTC, not `date/time`, which is the order posting date. This took the order-key match from 0 to 13,217 / 13,217.
- **Sign routing.** amount > 0 → `to_summary_field_when_positive_amount`, < 0 → negative, 0 → not summarised. An empty field means the amount is ingested and reconciled but not summarised.
- **Money** is parsed into integer cents. No floats touch an amount, and a value with more than two decimals is an error, not a rounding.

### Reconciliation and granularity (internal/recon)

A payments row is one order line with its amounts in columns. The settlement report has
one row per amount component, so a typical order line is 3–6 settlement rows, and one
order line can also be split over several payments rows. **Both sides are aggregated to
one total per record_ref before matching.** The payments side is the sum of the `total`
column, and the settlement side is the sum of `amount`. Both totals and every summary
line are compared, so offsetting mis-mappings inside a record are caught even when the
record totals agree. The status rule is: present on both sides → reconciled; payments
only → unreconciled payment; settlement only → unreconciled settlement.

### Scope

The settlement report holds one settlement (12395580393). The payments report covers
29 Jun – 31 Jul and four settlement ids, with Released and Deferred lines.
**Payments rows of that settlement id with status Released, excluding the bank transfer,
add up to Amazon's `total-amount` to the cent (212,118.95).** So the reconciliation
scope is that settlement id with status Released. Out-of-scope rows are still ingested
and are listed in Run Info:

- 2,398 deferred rows (43,540.07). They will be released into a later settlement.
- 7,156 rows of three other settlements.

## Mapping defects found

Details, evidence and the exact SQL are in [MAPPING_FIXES.sql](MAPPING_FIXES.sql). The investigation is in [PROGRESS.md](PROGRESS.md).

| # | Rule(s) | Defect | Variance it caused |
|---|---|---|---:|
| F1 | payment 62/63 (+110) | TRANSFER description is "To account ending with: 334", so the exact match fails, the row falls to the catch-all, and the bank payout is booked as an Amazon fee. Fixed with `description_match = 'prefix'`, a schema extension, because the original schema cannot express it. | Amazon fees -133,756.51 |
| F2 | payment 65 | ORDER promotional_rebates → expenses_other, while settlement sends the same money to Promo rebates. | Promo rebates / Other ±11,273.53 |
| F3 | payment 70 | Gift wrap goes to `sales_gift_wrap_credits`, a field that is not on the Summary sheet, so the money vanishes. | Other 11.61 |
| F4 | payment 71/72, settlement 61/63/65/95 | Order tax is mapped twice on payments (Product Charges and Shipping). Settlement splits it four ways, which the single payments column cannot reproduce. All order tax now goes to the **Tax** line on both sides. | Shipping +13,572.17, Product Charges -92.84 (with F5) |
| F5 | payment 5/6, settlement 118/119/127 | Low-value-goods tax is mapped twice on payments. It now goes to Tax, next to the tax it cancels. | (in F4 figures) |
| F6 | payment 14 | Refund tax is never summarised on payments; settlement routes it to Refund expenses. | Refund expenses 42.59 |

Pipeline interpretation, not config: the `date` token (B1), normalisation (B2),
duplicate handling (B3), scope (B4), and mixed month formats (B5).

There are also ten latent defects in rules this settlement does not exercise (C1–C10).
Examples: a record_ref with an unknown `record_type` token, AWD storage keys that
differ between the configs, and SAFE-T reimbursements routed to different lines. They
are documented with proposed SQL, commented out, because no data here can prove the
fix.

## Assumptions

1. The reconciliation scope is the settlement in the settlement report: its settlement id, status Released (see Scope).
2. A record's date is the UTC date of release into the settlement (see `date` token).
3. Tax belongs on the Summary's **Tax** line, not inside Product Charges or Shipping. This is required to make the two reports agree, because the payments report has a single tax column. Keeping tax inside Product Charges also closes the gap, but leaves the Tax line empty, so it is a presentation choice for finance. Low-value-goods withholding sits on the same line as the tax it cancels. Refund tax stays on Refund expenses, as the settlement config already does.
4. Line routing for composite summary fields: `total_refund_expense_or_sales_amt` goes to Refund expenses when positive and Refunded sales when negative. `total_adjustment_other_buyer_recharge_amt` goes to Sales Other / Expenses Other. `bank_account_transfer_round_off` goes to Micro Deposit (Failed) / Micro Deposit. None of these is exercised by this settlement.
5. Zero-value payments lines (no amount in any column) are kept as unreconciled payments with value 0 rather than dropped, so nothing in the input disappears silently.
6. `Account type` and `tax collection model` (sample Consolidated columns) do not exist in this payments file, so they are left blank.

## Layout

```
cmd/recon/            CLI
internal/db/          connection + schema.sql (embedded)
internal/money/       integer-cent amounts
internal/normalize/   canonical keys, date parsing
internal/mapping/     config loading, rule matching, record_ref, lint
internal/ingest/      streaming ingest of both reports (+ end-to-end test & fixture)
internal/recon/       reconciliation SQL
internal/report/      Excel workbook (excelize)
sql/investigation/    the queries used to find and prove each defect
MAPPING_FIXES.sql     the fixes
PROGRESS.md           how I got there
```

Note on dependencies: `golang.org/x/*` modules are replaced by their official GitHub
mirrors (`github.com/golang/*`) in `go.mod`, because the sandbox the code was built in
could not reach golang.org. The modules are vendored, so `make build` needs no network.

Built with Claude (agentic development). Every finding was verified by a query in
`sql/investigation/` and by an independent re-implementation (see PROGRESS.md).