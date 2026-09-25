package money_test

import (
	"testing"

	"zoiko.io/commercial-account-svc/internal/money"
)

func TestParse_AcceptsExactDecimals(t *testing.T) {
	for _, s := range []string{"0", "49", "49.00", "0.0015", "999999999999999.9999"} {
		d, err := money.Parse(s)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", s, err)
			continue
		}
		if d.String() != s {
			t.Errorf("Parse(%q).String() = %q, want the input unchanged", s, d.String())
		}
	}
}

// Every one of these would parse as a float somewhere. The price book must
// refuse them rather than reinterpret them.
func TestParse_RefusesAnythingThatIsNotAPlainDecimal(t *testing.T) {
	for _, s := range []string{
		"", "-1", "+5", "1e3", "1E3", "049", ".5", "5.", "0.00015", "1,000", "NaN", "Inf",
		"1000000000000000", " 5", "5 ",
	} {
		if _, err := money.Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted a value the price book cannot store exactly", s)
		}
	}
}

func TestDecimal_ScaleAndCompare(t *testing.T) {
	a, _ := money.Parse("1.5")
	b, _ := money.Parse("1.50")
	c, _ := money.Parse("1.49")
	if a.Scale() != 1 || b.Scale() != 2 {
		t.Fatalf("scale: got %d and %d, want 1 and 2", a.Scale(), b.Scale())
	}
	if a.Cmp(b) != 0 {
		t.Error("1.5 and 1.50 must compare equal")
	}
	if c.Cmp(a) >= 0 {
		t.Error("1.49 must compare below 1.5")
	}
	z, _ := money.Parse("0.0000")
	if !z.IsZero() {
		t.Error("0.0000 must be zero")
	}
}
