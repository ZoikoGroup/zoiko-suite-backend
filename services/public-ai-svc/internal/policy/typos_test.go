package policy

import "testing"

func TestFixTypos(t *testing.T) {
	for in, want := range map[string]string{
		"whats the pricng for growth":    "whats the pricing for growth",
		"Invioces overdue":               "Invoices overdue",
		"do u intergrate with xero":      "do you integrate with xero",
		"do you have a free trail":       "do you have a free trial",
		"is there a trail period":        "is there a trial period",
		"show me the audit trail":        "show me the audit trail", // a real phrase is kept
		"pricing for ultra":              "pricing for ultra",       // "u" only as a whole word
		"multi entitiy and subsidaries":  "multi entity and subsidiaries",
		"pay my invioces":                "pay my invoices",
		"keep the books for 5 companies": "keep the books for 5 companies",
	} {
		if got := FixTypos(in); got != want {
			t.Errorf("FixTypos(%q) = %q, want %q", in, got, want)
		}
	}
}

// A misspelled action is still refused (QA-T073, Doc 01 AT-02).
func TestTyposDoNotBypassSafety(t *testing.T) {
	for _, msg := range []string{"pay my invioces", "pls pay my invocies now", "show me my payrol"} {
		if d := Classify(msg); d.Exception == "" {
			t.Errorf("%q: no exception (intent %s)", msg, d.Primary)
		}
	}
}
