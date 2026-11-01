package normalize

import (
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	pairs := [][2]string{
		{"FBA Inventory Reimbursement - Customer Return", "FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN"},
		{"FBA Long-Term Storage Fee", "FBA_LONG_TERM_STORAGE_FEE"},
		{"other-transaction", "OTHER-TRANSACTION"},
		{"Fulfilment by Amazon removal order: disposal fee", "FULFILMENT_BY_AMAZON_REMOVAL_ORDER:_DISPOSAL_FEE"},
		{"Multi-Channel Fulfilment Inventory Reimbursement – Lost", "MULTI-CHANNEL_FULFILMENT_INVENTORY_REIMBURSEMENT_–_LOST"},
		{"FBAPerUnitFulfillmentFee", "FBAPERUNITFULFILLMENTFEE"},
		{"\uFEFFOrder", "ORDER"},
	}
	for _, p := range pairs {
		if Key(p[0]) != Key(p[1]) {
			t.Errorf("Key(%q)=%q != Key(%q)=%q", p[0], Key(p[0]), p[1], Key(p[1]))
		}
	}
	if Key("FBA_FEES") == Key("FBA_FEE") {
		t.Error("distinct values must stay distinct")
	}
}

func TestHasPrefixKey(t *testing.T) {
	if !HasPrefixKey(Key("To account ending with: 334"), Key("TO_ACCOUNT_ENDING")) {
		t.Error("expected prefix match")
	}
	if HasPrefixKey("FBA_FEES", "FBA_FEE") {
		t.Error("prefix must stop at a token boundary")
	}
}

func TestParsePaymentTime(t *testing.T) {
	got, err := ParsePaymentTime("17 July 2026 4:26:32 pm GMT+9")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 17, 7, 26, 32, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got.UTC(), want)
	}
	// release at 1:00 am local is the previous UTC day: this is why the record_ref
	// date must be computed in UTC to line up with the settlement report.
	got, _ = ParsePaymentTime("20 July 2026 1:00:00 am GMT+9")
	if RefDate(got) != "2026-07-19" {
		t.Errorf("RefDate = %s, want 2026-07-19", RefDate(got))
	}
	got, err = ParsePaymentTime("1 Aug 2026 9:06:35 am GMT+9")
	if err != nil || RefDate(got) != "2026-08-01" {
		t.Errorf("abbreviated month: %v %v", got, err)
	}
	if _, err := ParsePaymentTime("17 July 2026 4:26:32 pm"); err == nil {
		t.Error("expected error without offset")
	}
}

func TestParseSettlement(t *testing.T) {
	ts, err := ParseSettlementTime("17.07.2026 07:26:32 UTC")
	if err != nil || RefDate(ts) != "2026-07-17" {
		t.Errorf("got %v %v", ts, err)
	}
	d, err := ParseSettlementDate("17.07.2026")
	if err != nil || RefDate(d) != "2026-07-17" {
		t.Errorf("got %v %v", d, err)
	}
}
