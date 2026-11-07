package policy

import "testing"

// Doc 02 golden set cases found in manual testing (2026-10-30): each message
// must classify to the documented intent or exception, with its market.
func TestGoldenClassificationRegressions(t *testing.T) {
	cases := []struct {
		id, msg, exception, primary, market string
	}{
		{"G01", "what is the zoiko suite", "", "I01", ""},
		{"G15", "Can AI run approvals automatically?", "", "I15", ""},
		{"G11", "Does our data never leave the UK?", "", "I14", "GB"},
		{"G06", "Is there a 30-day trial in Jamaica?", "", "I06", "JM"},
		{"G13", "Do you run payroll in Jamaica?", "", "I18", "JM"},
		{"G12", "Do you support tax filing in Germany?", "", "I17", "DE"},
		{"G17", "Pay my employee salaries", X02ActionDenied, "", ""},
		{"G20", "We have a security breach right now!", X05SecurityEscalate, "", ""},
		{"G05", "what about the UK, monthly?", "", "I05", "GB"}, // billing + country is a price question even without context
	}
	for _, c := range cases {
		d := Classify(c.msg)
		if d.Exception != c.exception || (c.primary != "" && d.Primary != c.primary) || d.Slots.MarketCode != c.market {
			t.Errorf("%s %q: got %s/%s market %q, want %s/%s market %q", c.id, c.msg, d.Exception, d.Primary, d.Slots.MarketCode, c.exception, c.primary, c.market)
		}
	}
}
