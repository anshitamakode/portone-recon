package money

import "testing"

func TestParse(t *testing.T) {
	cases := map[string]Cents{
		"":           0,
		"0":          0,
		"14.90":      1490,
		"-3.52":      -352,
		"-97,919.76": -9791976,
		"212118.95":  21211895,
		"5":          500,
		"-0.4":       -40,
		".5":         50,
		"1.500":      150,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := Parse("1.234"); err == nil {
		t.Error("expected error for 3 significant decimals")
	}
	if _, err := Parse("abc"); err == nil {
		t.Error("expected error for non-numeric")
	}
}

func TestString(t *testing.T) {
	for c, want := range map[Cents]string{0: "0.00", -352: "-3.52", 21211895: "212118.95", -5: "-0.05"} {
		if got := c.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(c), got, want)
		}
	}
}
