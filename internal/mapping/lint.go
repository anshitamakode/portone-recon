package mapping

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Finding is a static problem in the configs, independent of any data.
type Finding struct {
	Severity string // "error" (will produce wrong numbers when hit) | "warn" | "info"
	Check    string
	Config   string
	IDs      []int
	Message  string
}

func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s %s %v: %s", f.Severity, f.Check, f.Config, f.IDs, f.Message)
}

var fieldLike = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$|^[a-z]+$`)

// Lint runs static checks over both configs. These checks find defects that do not
// show up in this particular dataset because no row exercises the rule.
func Lint(pay, set *Matcher, rt *Routes) []Finding {
	var out []Finding
	for _, m := range []*Matcher{pay, set} {
		tbl := Table(m.source)
		// 1. duplicated match keys -> amounts are counted once per duplicate
		groups := map[string][]int{}
		for _, r := range m.all {
			k := strings.Join([]string{r.kType, r.kAmountType, r.kDesc, r.AmountField}, "|")
			groups[k] = append(groups[k], r.ID)
		}
		for k, ids := range groups {
			if len(ids) > 1 {
				out = append(out, Finding{"error", "duplicate-key", tbl, ids,
					fmt.Sprintf("%d rules share the match key %s; every matching amount is mapped %d times", len(ids), strings.ReplaceAll(k, "|", " / "), len(ids))})
			}
		}
		for _, r := range m.all {
			// 2. summary fields that land nowhere on the Summary sheet
			reported := map[string]bool{}
			for _, f := range []string{strings.TrimSpace(r.PosField), strings.TrimSpace(r.NegField)} {
				if f != "" && !rt.Known(f) && !reported[f] {
					reported[f] = true
					out = append(out, Finding{"error", "orphan-summary-field", tbl, []int{r.ID},
						fmt.Sprintf("summary field %q is not a line on the Summary sheet; amounts routed to it disappear from the summary", f)})
				}
			}
			// 3. one sign routed, the other dropped
			p, n := strings.TrimSpace(r.PosField), strings.TrimSpace(r.NegField)
			if (p == "") != (n == "") {
				out = append(out, Finding{"warn", "one-sided-sign", tbl, []int{r.ID},
					fmt.Sprintf("positive -> %q, negative -> %q: amounts of the other sign are silently not summarised", p, n)})
			}
			// 4. record_ref tokens that look like a field but are not one
			if strings.TrimSpace(r.RecordRef) == "" && (p != "" || n != "") {
				out = append(out, Finding{"error", "empty-record-ref", tbl, []int{r.ID}, "summarised amounts with no record_ref can never be reconciled"})
			}
			seen := map[string]int{}
			for _, tok := range strings.Split(r.RecordRef, "+") {
				seen[tok]++
				if !FieldTokens[m.source][tok] && fieldLike.MatchString(tok) {
					out = append(out, Finding{"error", "unknown-ref-token", tbl, []int{r.ID},
						fmt.Sprintf("record_ref token %q looks like a field name but is not a known field; it is used as the literal text %q", tok, tok)})
				}
			}
			for tok, c := range seen {
				if c > 1 && FieldTokens[m.source][tok] {
					out = append(out, Finding{"error", "repeated-ref-token", tbl, []int{r.ID},
						fmt.Sprintf("record_ref repeats %q; the key cannot equal the counterpart key", tok)})
				}
			}
		}
	}
	// 5. record_ref "shapes" that exist on one side only: those records can never
	//    reconcile. Informational: some are legitimately one-sided (bank transfers).
	shape := func(m *Matcher) map[string][]int {
		s := map[string][]int{}
		for _, r := range m.all {
			if strings.TrimSpace(r.RecordRef) != "" {
				s[r.RecordRef] = append(s[r.RecordRef], r.ID)
			}
		}
		return s
	}
	ps, ss := shape(pay), shape(set)
	for ref, ids := range ps {
		if _, ok := ss[ref]; !ok {
			out = append(out, Finding{"info", "one-sided-record-ref", "payment_config", uniq(ids),
				fmt.Sprintf("record_ref %q has no identical template in settlement_config", ref)})
		}
	}
	for ref, ids := range ss {
		if _, ok := ps[ref]; !ok {
			out = append(out, Finding{"info", "one-sided-record-ref", "settlement_config", uniq(ids),
				fmt.Sprintf("record_ref %q has no identical template in payment_config", ref)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		rank := map[string]int{"error": 0, "warn": 1, "info": 2}
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		if out[i].Config != out[j].Config {
			return out[i].Config < out[j].Config
		}
		return out[i].IDs[0] < out[j].IDs[0]
	})
	return out
}

func uniq(ids []int) []int {
	sort.Ints(ids)
	out := ids[:0]
	for i, v := range ids {
		if i == 0 || v != ids[i-1] {
			out = append(out, v)
		}
	}
	return out
}
