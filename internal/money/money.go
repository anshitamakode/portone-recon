// Package money handles currency amounts as integer cents so that sums are exact.
// Floating point is never used for amounts anywhere in the pipeline.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// Cents is an amount in minor units (1/100 of the currency).
type Cents int64

// Parse converts a report amount ("1,234.56", "-0.37", "", "0") into cents.
// Empty strings are zero. More than two decimals is an error rather than a silent
// rounding, because a rounding error in a reconciliation is a real problem.
func Parse(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, nil
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasDot && len(frac) > 2 {
		// allow trailing zeros beyond 2 decimals ("1.500"), reject real precision
		if strings.TrimRight(frac[2:], "0") != "" {
			return 0, fmt.Errorf("amount %q has more than 2 decimal places", s)
		}
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	c := Cents(w*100 + f)
	if neg {
		c = -c
	}
	return c, nil
}

// String renders cents as a plain decimal ("-1234.56"), suitable for PostgreSQL numeric.
func (c Cents) String() string {
	sign := ""
	v := int64(c)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Float is only for writing to Excel cells (display). Never sum floats.
func (c Cents) Float() float64 { return float64(c) / 100 }
