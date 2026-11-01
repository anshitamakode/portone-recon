// Package normalize turns the inconsistent spellings found in Amazon reports and in
// the mapping configs into one canonical form, and parses the two date formats.
package normalize

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// separators: whitespace, underscore, hyphen, en/em dash and colon all count as the
// same separator. The configs write "FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN",
// "FBA_LONG-TERM_STORAGE_FEE" and "FBA_LONG_TERM_STORAGE_FEE"; the reports write
// "FBA Inventory Reimbursement - Customer Return", "other-transaction", "Base fee".
var sepRun = regexp.MustCompile(`[\s_\-\x{2013}\x{2014}:]+`)

// Key returns the canonical matching key: upper case, every run of separators
// collapsed to a single "_", no leading/trailing "_".
//
//	Key("FBA Inventory Reimbursement - Customer Return") == "FBA_INVENTORY_REIMBURSEMENT_CUSTOMER_RETURN"
//	Key("FBA_INVENTORY_REIMBURSEMENT_-_CUSTOMER_RETURN") == "FBA_INVENTORY_REIMBURSEMENT_CUSTOMER_RETURN"
//	Key("other-transaction") == "OTHER_TRANSACTION"
func Key(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ToUpper(strings.TrimSpace(s))
	s = sepRun.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// IsWildcard reports whether a config value is the "any" wildcard.
func IsWildcard(s string) bool { return Key(s) == "ANY" }

// HasPrefixKey reports whether canonical key k starts with canonical prefix p on a
// token boundary ("TO_ACCOUNT_ENDING" matches "TO_ACCOUNT_ENDING_WITH_334" but
// "FBA_FEE" does not match "FBA_FEES").
func HasPrefixKey(k, p string) bool {
	if p == "" {
		return false
	}
	return k == p || strings.HasPrefix(k, p+"_")
}

// gmtOffset matches the zone suffix used by the payments report, e.g. "GMT+9",
// "GMT+10", "GMT+5:30", "GMT-3".
var gmtOffset = regexp.MustCompile(`\s*GMT([+-])(\d{1,2})(?::?(\d{2}))?\s*$`)

// ParsePaymentTime parses "17 July 2026 4:26:32 pm GMT+9" into an absolute time.
func ParsePaymentTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	m := gmtOffset.FindStringSubmatchIndex(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("payment date %q: missing GMT offset", s)
	}
	sign, hh, mm := s[m[2]:m[3]], s[m[4]:m[5]], "0"
	if m[6] >= 0 {
		mm = s[m[6]:m[7]]
	}
	h, _ := strconv.Atoi(hh)
	mi, _ := strconv.Atoi(mm)
	off := h*3600 + mi*60
	if sign == "-" {
		off = -off
	}
	loc := time.FixedZone(fmt.Sprintf("GMT%s%s", sign, hh), off)
	body := strings.TrimSpace(s[:m[0]])
	// The report mixes full and abbreviated month names ("17 July 2026", "1 Aug
	// 2026") within the same file, and some locales write "Sept".
	body = strings.Replace(body, " Sept ", " Sep ", 1)
	var lastErr error
	for _, layout := range []string{"2 January 2006 3:04:05 pm", "2 Jan 2006 3:04:05 pm"} {
		t, err := time.ParseInLocation(layout, body, loc)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, fmt.Errorf("payment date %q: %w", s, lastErr)
}

// ParseSettlementTime parses "17.07.2026 07:26:32 UTC".
func ParseSettlementTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	s = strings.TrimSuffix(s, " UTC")
	t, err := time.ParseInLocation("02.01.2006 15:04:05", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("settlement date-time %q: %w", s, err)
	}
	return t, nil
}

// ParseSettlementDate parses "17.07.2026".
func ParseSettlementDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("02.01.2006", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("settlement date %q: %w", s, err)
	}
	return t, nil
}

// RefDate formats the `date` token of a record_ref: the UTC calendar date.
func RefDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}
