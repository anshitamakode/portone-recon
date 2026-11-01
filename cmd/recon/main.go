// Command recon reconciles Amazon's Payments report against its Settlement report.
//
//	recon init                                  create / migrate the schema
//	recon load-config -payments P -settlements S load the mapping configs as provided
//	recon lint                                  static checks over the configs in the DB
//	recon ingest -payments P -settlements S     ingest both reports (replaces previous run)
//	recon reconcile                             match records on record_ref
//	recon report -out F.xlsx                    write the Excel report
//	recon run -payments P -settlements S -out F ingest + reconcile + report
//
// The database is taken from $DATABASE_URL.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/anshitamakode/portone-recon/internal/db"
	"github.com/anshitamakode/portone-recon/internal/ingest"
	"github.com/anshitamakode/portone-recon/internal/mapping"
	"github.com/anshitamakode/portone-recon/internal/recon"
	"github.com/anshitamakode/portone-recon/internal/report"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	payments := fs.String("payments", "data/amazon_payments_data.csv", "payments report CSV")
	settlements := fs.String("settlements", "data/amazon_settlements_data.txt", "settlement report TXT")
	payCfg := fs.String("payment-config", "data/amazon_payment_configs_au_old.csv", "payment mapping config CSV")
	setCfg := fs.String("settlement-config", "data/amazon_settlement_configs_au.csv", "settlement mapping config CSV")
	out := fs.String("out", "out/report.xlsx", "Excel report path")
	label := fs.String("label", "", "label printed on the report (e.g. 'before fixes')")
	_ = fs.Parse(args)

	conn, err := db.Open(ctx)
	if err != nil {
		log.Fatalf("connect: %v (set DATABASE_URL)", err)
	}
	defer conn.Close()
	must := func(err error) {
		if err != nil {
			log.Fatalf("%s: %v", cmd, err)
		}
	}

	switch cmd {
	case "init":
		must(db.Migrate(ctx, conn))
		fmt.Println("schema ready")
	case "load-config":
		must(db.Migrate(ctx, conn))
		pr, err := mapping.LoadCSV(*payCfg, mapping.Payment)
		must(err)
		sr, err := mapping.LoadCSV(*setCfg, mapping.Settlement)
		must(err)
		must(mapping.ReplaceInDB(ctx, conn, mapping.Payment, pr))
		must(mapping.ReplaceInDB(ctx, conn, mapping.Settlement, sr))
		fmt.Printf("loaded %d payment rules, %d settlement rules (original configs, change log reset)\n", len(pr), len(sr))
	case "lint":
		findings, err := lint(ctx, conn)
		must(err)
		for _, f := range findings {
			fmt.Println(f)
		}
	case "ingest":
		must(doIngest(ctx, conn, *payments, *settlements))
	case "reconcile":
		must(doReconcile(ctx, conn))
	case "report":
		must(report.Write(ctx, conn, *out, *label))
		fmt.Println("wrote", *out)
	case "run":
		must(doIngest(ctx, conn, *payments, *settlements))
		must(doReconcile(ctx, conn))
		must(report.Write(ctx, conn, *out, *label))
		fmt.Println("wrote", *out)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: recon {init|load-config|lint|ingest|reconcile|report|run} [flags]")
	os.Exit(2)
}

func lint(ctx context.Context, conn *sql.DB) ([]mapping.Finding, error) {
	pr, err := mapping.LoadFromDB(ctx, conn, mapping.Payment)
	if err != nil {
		return nil, err
	}
	sr, err := mapping.LoadFromDB(ctx, conn, mapping.Settlement)
	if err != nil {
		return nil, err
	}
	rt, err := mapping.LoadRoutes(ctx, conn)
	if err != nil {
		return nil, err
	}
	return mapping.Lint(mapping.NewMatcher(mapping.Payment, pr), mapping.NewMatcher(mapping.Settlement, sr), rt), nil
}

func doIngest(ctx context.Context, conn *sql.DB, payments, settlements string) error {
	t0 := time.Now()
	res, err := ingest.Run(ctx, conn, payments, settlements)
	if err != nil {
		return err
	}
	fmt.Printf("ingest run %d in %s, scope settlement(s) %v\n", res.RunID, time.Since(t0).Round(time.Millisecond), res.SettlementIDs)
	keys := make([]string, 0, len(res.Counts))
	for k := range res.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-36s %d\n", k, res.Counts[k])
	}
	if len(res.Warnings) > 0 {
		fmt.Printf("  %d warnings, first: %s\n", len(res.Warnings), res.Warnings[0])
	}
	fmt.Println("  streaming summary (computed during ingest, in scope):")
	for _, src := range []string{"payment", "settlement"} {
		lines := make([]string, 0)
		for l := range res.Streaming[src] {
			lines = append(lines, l)
		}
		sort.Strings(lines)
		for _, l := range lines {
			fmt.Printf("    %-10s %-36s %14s\n", src, l, res.Streaming[src][l])
		}
	}
	return nil
}

func doReconcile(ctx context.Context, conn *sql.DB) error {
	c, err := recon.Build(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Printf("reconciled %d | unreconciled payments %d | unreconciled settlements %d | reconciled with total difference %d | reconciled with summary-line mismatch %d\n",
		c.Reconciled, c.UnreconciledPayment, c.UnreconciledSettlement, c.ReconciledWithDifference, c.ReconciledWithLineMismatch)
	return nil
}
