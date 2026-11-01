package mapping

import (
	"testing"

	"github.com/anshitamakode/portone-recon/internal/money"
)

func payRules() []Rule {
	return []Rule{
		{ID: 2, TransactionType: "ORDER", Description: "any", AmountField: "product_sales", RecordRef: "txn_ref+sku+date", PosField: "sales_product_charges", NegField: "sales_product_charges"},
		{ID: 3, TransactionType: "ORDER", Description: "any", AmountField: "low_value_goods", RecordRef: "txn_ref+sku+date", PosField: "sales_shipping", NegField: "sales_shipping"},
		{ID: 4, TransactionType: "ORDER", Description: "any", AmountField: "low_value_goods", RecordRef: "txn_ref+sku+date", PosField: "sales_product_charges", NegField: "sales_product_charges"},
		{ID: 5, TransactionType: "ADJUSTMENT", Description: "FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN", AmountField: "other", RecordRef: "txn_ref+sku+ADJUSTMENT+settlement_id+date", PosField: "sales_inventory_reimbursements", NegField: "expenses_reversed_reimbursements"},
		{ID: 6, TransactionType: "TRANSFER", Description: "TO_ACCOUNT_ENDING", AmountField: "total", RecordRef: "TRANSFER+description+settlement_id+date"},
		{ID: 7, TransactionType: "", Description: "any", AmountField: "total", RecordRef: "txn_ref+settlement_id+date", PosField: "expenses_amazon_fees", NegField: "expenses_amazon_fees"},
	}
}

func TestMatchTiers(t *testing.T) {
	m := NewMatcher(Payment, payRules())
	rs, kind := m.MatchPayment("Adjustment", "FBA Inventory Reimbursement - Customer Return", "other")
	if kind != KindExact || len(rs) != 1 || rs[0].ID != 5 {
		t.Fatalf("separator-insensitive exact match failed: %v %v", rs, kind)
	}
	rs, kind = m.MatchPayment("Order", "BIODANCE Mask", "product_sales")
	if kind != KindWildcard || rs[0].ID != 2 {
		t.Fatalf("wildcard: %v %v", rs, kind)
	}
	// duplicated rules are all returned (double count is surfaced, not hidden)
	rs, _ = m.MatchPayment("Order", "x", "low_value_goods")
	if len(rs) != 2 {
		t.Fatalf("want both duplicate rules, got %d", len(rs))
	}
	// exact-only description does not match a suffixed description -> catch-all
	rs, kind = m.MatchPayment("Transfer", "To account ending with: 334", "total")
	if kind != KindFallback || rs[0].ID != 7 {
		t.Fatalf("want fallback, got %v %v", rs, kind)
	}
	// with prefix matching enabled on the rule, the TRANSFER rule wins
	rules := payRules()
	rules[4].DescMatch = "prefix"
	m = NewMatcher(Payment, rules)
	rs, kind = m.MatchPayment("Transfer", "To account ending with: 334", "total")
	if kind != KindPrefix || rs[0].ID != 6 {
		t.Fatalf("want prefix rule 6, got %v %v", rs, kind)
	}
	if _, kind = m.MatchPayment("Order", "x", "selling_fees"); kind != KindUnmatched {
		t.Fatalf("want unmatched, got %v", kind)
	}
}

func TestSummaryFieldBySign(t *testing.T) {
	r := payRules()[3]
	if r.SummaryField(money.Cents(100)) != "sales_inventory_reimbursements" ||
		r.SummaryField(money.Cents(-100)) != "expenses_reversed_reimbursements" ||
		r.SummaryField(0) != "" {
		t.Fatal("sign routing wrong")
	}
}

func TestBuildRef(t *testing.T) {
	f := map[string]string{"txn_ref": "249-1", "sku": "SKU1", "date": "2026-07-17", "settlement_id": "123", "description": "TO_ACCOUNT_ENDING_WITH_334"}
	if got := BuildRef(Payment, "txn_ref+sku+ADJUSTMENT+settlement_id+date", f); got != "249-1+SKU1+ADJUSTMENT+123+2026-07-17" {
		t.Errorf("got %s", got)
	}
	if got := BuildRef(Payment, "GENERAL ADJUSTMENT+sku+settlement_id+date", f); got != "GENERAL ADJUSTMENT+SKU1+123+2026-07-17" {
		t.Errorf("literal with space: %s", got)
	}
	if got := BuildRef(Settlement, "shipment_id+X", map[string]string{"shipment_id": "S9"}); got != "S9+X" {
		t.Errorf("got %s", got)
	}
}

func TestLintFindsPlantedProblems(t *testing.T) {
	pay := NewMatcher(Payment, payRules())
	set := NewMatcher(Settlement, []Rule{
		{ID: 2, TransactionType: "OTHER-TRANSACTION", AmountType: "OTHER-TRANSACTION", Description: "PROMOTION_ADJUSTMENT",
			RecordRef: "ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id", PosField: "x_unknown", NegField: "x_unknown"},
	})
	rt := &Routes{line: map[[2]string]string{{"sales_product_charges", "any"}: "sales.product_charges",
		{"sales_shipping", "any"}: "sales.shipping", {"sales_inventory_reimbursements", "any"}: "sales.inventory_reimbursements",
		{"expenses_reversed_reimbursements", "any"}: "expenses.reversed_reimbursements", {"expenses_amazon_fees", "any"}: "expenses.amazon_fees"},
		excluded: map[string]string{}}
	got := map[string]bool{}
	for _, f := range Lint(pay, set, rt) {
		got[f.Check] = true
	}
	for _, want := range []string{"duplicate-key", "orphan-summary-field", "unknown-ref-token", "repeated-ref-token"} {
		if !got[want] {
			t.Errorf("lint did not report %s", want)
		}
	}
}
