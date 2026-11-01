// Package mapping applies the payment / settlement mapping configs to report rows:
// it finds the matching rule(s), builds the record_ref reconciliation key and picks
// the summary field by the sign of the amount.
package mapping

import (
	"sort"
	"strings"

	"github.com/anshitamakode/portone-recon/internal/money"
	"github.com/anshitamakode/portone-recon/internal/normalize"
)

// Source identifies which report / config a rule belongs to.
type Source string

const (
	Payment    Source = "payment"
	Settlement Source = "settlement"
)

// Rule is one row of a mapping config. For payments, Description is the
// `description` column and AmountField the report column it reads. For settlements,
// AmountType/Description are `amount_type`/`amount_description` and AmountField is
// always "amount".
type Rule struct {
	Source          Source
	ID              int // line number in the original CSV
	TransactionType string
	AmountType      string
	Description     string
	AmountField     string
	RecordRef       string
	PosField        string
	NegField        string
	DescMatch       string // "exact" (default) or "prefix"

	kType, kAmountType, kDesc string
}

func (r *Rule) prepare() {
	r.kType = normalize.Key(r.TransactionType)
	r.kAmountType = normalize.Key(r.AmountType)
	r.kDesc = normalize.Key(r.Description)
	if r.DescMatch == "" {
		r.DescMatch = "exact"
	}
	if r.Source == Settlement {
		r.AmountField = "amount"
	}
}

// IsFallback: an empty transaction_type is the catch-all bucket.
func (r *Rule) IsFallback() bool { return r.kType == "" }

// SummaryField routes an amount by its sign. Zero amounts are not summarised.
func (r *Rule) SummaryField(a money.Cents) string {
	switch {
	case a > 0:
		return strings.TrimSpace(r.PosField)
	case a < 0:
		return strings.TrimSpace(r.NegField)
	}
	return ""
}

// Match kinds, from most to least specific.
const (
	KindExact     = "exact"
	KindPrefix    = "prefix"
	KindWildcard  = "wildcard"
	KindFallback  = "fallback"
	KindUnmatched = "unmatched"
)

// Matcher indexes the rules of one config by transaction type.
type Matcher struct {
	source   Source
	byType   map[string][]*Rule
	fallback []*Rule
	all      []*Rule
}

// NewMatcher builds a matcher for one source's rules.
func NewMatcher(src Source, rules []Rule) *Matcher {
	m := &Matcher{source: src, byType: map[string][]*Rule{}}
	for i := range rules {
		r := &rules[i]
		r.Source = src
		r.prepare()
		m.all = append(m.all, r)
		if r.IsFallback() {
			m.fallback = append(m.fallback, r)
		} else {
			m.byType[r.kType] = append(m.byType[r.kType], r)
		}
	}
	return m
}

// Rules returns all rules (in config order).
func (m *Matcher) Rules() []*Rule { return m.all }

// MatchPayment finds the rules for one amount column of a payments row.
func (m *Matcher) MatchPayment(txType, description, amountField string) ([]*Rule, string) {
	filter := func(r *Rule) bool { return r.AmountField == amountField }
	return m.match(normalize.Key(txType), normalize.Key(description), filter)
}

// MatchSettlement finds the rules for a settlement row.
func (m *Matcher) MatchSettlement(txType, amountType, amountDescription string) ([]*Rule, string) {
	kAT := normalize.Key(amountType)
	filter := func(r *Rule) bool { return r.kAmountType == kAT }
	return m.match(normalize.Key(txType), normalize.Key(amountDescription), filter)
}

// match: transaction type first, then the description tier
// (exact > prefix > "any"); only if the transaction type has no applicable rule do
// we fall back to the empty-transaction_type catch-all rules.
// All rules of the winning tier are returned: duplicated config rows are applied
// twice on purpose, so that a double count shows up in the numbers instead of being
// silently hidden by "first match wins".
func (m *Matcher) match(kType, kDesc string, filter func(*Rule) bool) ([]*Rule, string) {
	if rs, kind := tier(m.byType[kType], kDesc, filter); len(rs) > 0 {
		return rs, kind
	}
	if rs, _ := tier(m.fallback, kDesc, filter); len(rs) > 0 {
		return rs, KindFallback
	}
	return nil, KindUnmatched
}

func tier(cands []*Rule, kDesc string, filter func(*Rule) bool) ([]*Rule, string) {
	var exact, prefix, wild []*Rule
	for _, r := range cands {
		if !filter(r) {
			continue
		}
		switch {
		case r.kDesc == "ANY":
			wild = append(wild, r)
		case r.kDesc == kDesc:
			exact = append(exact, r)
		case r.DescMatch == "prefix" && normalize.HasPrefixKey(kDesc, r.kDesc):
			prefix = append(prefix, r)
		}
	}
	switch {
	case len(exact) > 0:
		return exact, KindExact
	case len(prefix) > 0:
		// longest prefix wins
		sort.SliceStable(prefix, func(i, j int) bool { return len(prefix[i].kDesc) > len(prefix[j].kDesc) })
		best := len(prefix[0].kDesc)
		n := 0
		for n < len(prefix) && len(prefix[n].kDesc) == best {
			n++
		}
		return prefix[:n], KindPrefix
	case len(wild) > 0:
		return wild, KindWildcard
	}
	return nil, ""
}

// FieldTokens are the record_ref tokens that are replaced by row values. Any other
// token is a literal. (Lint flags literal tokens that look like field names.)
var FieldTokens = map[Source]map[string]bool{
	Payment: {"txn_ref": true, "sku": true, "date": true, "settlement_id": true, "description": true},
	Settlement: {"txn_ref": true, "sku": true, "date": true, "settlement_id": true,
		"shipment_id": true, "merchant_order_id": true, "adjustment_id": true, "description": true},
}

// BuildRef expands a "+"-joined record_ref template with the row's field values.
func BuildRef(src Source, template string, fields map[string]string) string {
	if strings.TrimSpace(template) == "" {
		return ""
	}
	parts := strings.Split(template, "+")
	known := FieldTokens[src]
	for i, tok := range parts {
		if known[tok] {
			parts[i] = strings.TrimSpace(fields[tok])
		}
	}
	return strings.Join(parts, "+")
}
