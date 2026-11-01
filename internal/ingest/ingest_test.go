package ingest_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/anshitamakode/portone-recon/internal/db"
	"github.com/anshitamakode/portone-recon/internal/ingest"
	"github.com/anshitamakode/portone-recon/internal/mapping"
	"github.com/anshitamakode/portone-recon/internal/money"
	"github.com/anshitamakode/portone-recon/internal/recon"
)

// End-to-end test on a tiny fixture. Needs a disposable database:
//
//	RECON_TEST_DATABASE_URL=postgres://recon:recon@localhost/recon_test?sslmode=disable go test ./...
//
// (never point it at the real database: ingest truncates the data tables).
func setup(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("RECON_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RECON_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for src, path := range map[mapping.Source]string{mapping.Payment: "testdata/payment_config.csv", mapping.Settlement: "testdata/settlement_config.csv"} {
		rules, err := mapping.LoadCSV(path, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := mapping.ReplaceInDB(ctx, conn, src, rules); err != nil {
			t.Fatal(err)
		}
	}
	return conn, ctx
}

func lineSum(t *testing.T, conn *sql.DB, src, line string) money.Cents {
	t.Helper()
	var v sql.NullString
	if err := conn.QueryRow(`SELECT SUM(amount)::text FROM record_component WHERE in_scope AND source=$1 AND line_key=$2`, src, line).Scan(&v); err != nil {
		t.Fatal(err)
	}
	c, _ := money.Parse(v.String)
	return c
}

func TestEndToEnd(t *testing.T) {
	conn, ctx := setup(t)
	defer conn.Close()

	res, err := ingest.Run(ctx, conn, "testdata/payments.csv", "testdata/settlements.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Counts["payment_rows_in_scope"]; got != 3 {
		t.Errorf("in-scope payment rows = %d, want 3 (deferred and other-settlement rows excluded)", got)
	}
	if got := res.Counts["payment_rows_out_of_scope"]; got != 2 {
		t.Errorf("out-of-scope payment rows = %d, want 2", got)
	}
	c, err := recon.Build(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	// 249-1 was released at 01:00 GMT+9 on 20 July = 19 July UTC = settlement posted-date.
	if c.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1 (record_ref date must be the UTC release date)", c.Reconciled)
	}
	if c.UnreconciledPayment != 2 || c.UnreconciledSettlement != 0 {
		t.Errorf("unreconciled = %d/%d, want 2/0 (transfer + zero-value line)", c.UnreconciledPayment, c.UnreconciledSettlement)
	}
	// the duplicated tax rule double counts: shipping gets the tax a second time
	if got := lineSum(t, conn, "payment", "sales.shipping"); got != 200 {
		t.Errorf("payments shipping = %s, want 2.00 (duplicate rule applied)", got)
	}
	// the transfer falls to the catch-all and is booked as an Amazon fee
	if got := lineSum(t, conn, "payment", "expenses.amazon_fees"); got != -10617 {
		t.Errorf("payments amazon fees = %s, want -106.17", got)
	}
	if res.Streaming["payment"]["sales.shipping"] != 200 {
		t.Errorf("streaming summary should match: %v", res.Streaming["payment"])
	}

	// apply the equivalent of F1 + F4 and re-ingest
	if _, err := conn.Exec(`UPDATE payment_config SET description_match='prefix' WHERE id=8;
	                        DELETE FROM payment_config WHERE id=5`); err != nil {
		t.Fatal(err)
	}
	res, err = ingest.Run(ctx, conn, "testdata/payments.csv", "testdata/settlements.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recon.Build(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"sales.product_charges", "sales.shipping", "expenses.amazon_fees", "expenses.promo_rebates"} {
		p, s := lineSum(t, conn, "payment", line), lineSum(t, conn, "settlement", line)
		if p != s {
			t.Errorf("after fixes %s: payments %s != settlements %s", line, p, s)
		}
		if res.Streaming["payment"][line] != p {
			t.Errorf("streaming %s = %s, SQL = %s", line, res.Streaming["payment"][line], p)
		}
	}
	var kind string
	if err := conn.QueryRow(`SELECT rc.match_kind FROM record_component rc JOIN source_row sr ON sr.id = rc.source_row_id
	    WHERE sr.transaction_type = 'Transfer' AND rc.amount_field = 'total'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != mapping.KindPrefix {
		t.Errorf("transfer matched by %s, want prefix", kind)
	}
	// re-ingest is idempotent
	var n1, n2 int
	_ = conn.QueryRow(`SELECT count(*) FROM source_row`).Scan(&n1)
	if _, err := ingest.Run(ctx, conn, "testdata/payments.csv", "testdata/settlements.txt"); err != nil {
		t.Fatal(err)
	}
	_ = conn.QueryRow(`SELECT count(*) FROM source_row`).Scan(&n2)
	if n1 != n2 {
		t.Errorf("re-ingest changed row count %d -> %d", n1, n2)
	}
}
