package ncd

import (
	"strings"
	"testing"
	"time"
)

// TC-03: arbitrary variable values must never panic the typed validator or
// escape an HTML surface, and a rendered subject must never carry a line
// break.
func FuzzRenderEscapesAndRefusesHeaderInjection(f *testing.F) {
	for _, seed := range []string{"INV-1", "<script>alert(1)</script>", "a\r\nBcc: x@example.com", "{{invoice_no}}", "&<>\"'"} {
		f.Add(seed)
	}
	intent := Intent{
		AllowedChannels: []string{ChannelEmail},
		VariableContract: []VariableSpec{
			{Name: "v", Type: "string", Required: true, Sensitivity: Level("S1")},
			{Name: "n", Type: "number", Sensitivity: Level("S1")},
			{Name: "d", Type: "date", Sensitivity: Level("S1")},
			{Name: "m", Type: "money", Sensitivity: Level("S1")},
			{Name: "e", Type: "email", Sensitivity: Level("S1")},
			{Name: "u", Type: "url", Sensitivity: Level("S1")},
		},
	}
	tv := TemplateVersion{
		Channel: ChannelEmail, Locale: "en-GB", Subject: "Notice {{v}}", Body: "<p>{{v}}</p>",
	}
	f.Fuzz(func(t *testing.T, value string) {
		vars := map[string]string{
			"v": value, "n": "1", "d": "2026-01-31", "m": "1.00 EUR",
			"e": "a@example.com", "u": "https://example.com/a",
		}
		out, refusal := Render(intent, tv, vars)
		if refusal != nil {
			return
		}
		if strings.ContainsAny(out.Subject, "\r\n") {
			t.Fatalf("rendered subject contains a line break: %q", out.Subject)
		}
		if strings.Contains(value, "<script>") && strings.Contains(out.Body, "<script>") {
			t.Fatalf("HTML variable was not escaped: %q", out.Body)
		}
	})
}

// TC-03: every declared type must reject arbitrary input without panicking.
func FuzzValidateVariablesTypes(f *testing.F) {
	for _, seed := range []string{"", "1", "2026-13-99", "1.00 XYZ", "not-an-email", "javascript:alert(1)", "a\r\nb"} {
		f.Add(seed)
	}
	contract := []VariableSpec{
		{Name: "s", Type: "string"}, {Name: "n", Type: "number"}, {Name: "d", Type: "date"},
		{Name: "m", Type: "money"}, {Name: "e", Type: "email"}, {Name: "u", Type: "url"},
		{Name: "x", Type: "enum", AllowedValues: []string{"A", "B"}},
	}
	f.Fuzz(func(t *testing.T, value string) {
		for _, spec := range contract {
			intent := Intent{VariableContract: []VariableSpec{spec}, ApprovedURLDomains: []string{"example.com"}}
			_ = ValidateVariables(intent, map[string]string{spec.Name: value})
		}
	})
}

// TC-18: S2/S3 facts are blocked from subjects and from non-in-app bodies,
// including through placeholders and named sensitive terms.
func TestTC18_SensitiveValuesBlockedOnLowConfidentialitySurfaces(t *testing.T) {
	intent := Intent{
		PurposeClass: PurposeTransactional, Sensitivity: Level("S2"),
		AllowedChannels: []string{ChannelEmail, ChannelInApp},
		VariableContract: []VariableSpec{
			{Name: "amount", Type: "money", Required: true, Sensitivity: Level("S2")},
		},
	}
	cases := []struct {
		name    string
		tv      TemplateVersion
		check   string
		blocked bool
	}{
		{
			name:  "S2 value in email subject",
			tv:    TemplateVersion{Channel: ChannelEmail, Locale: "en-GB", Subject: "Invoice {{amount}}", Body: "<p>Ready</p>"},
			check: "subject_sensitivity", blocked: true,
		},
		{
			name:  "S2 value in email body",
			tv:    TemplateVersion{Channel: ChannelEmail, Locale: "en-GB", Subject: "Invoice ready", Body: "<p>{{amount}}</p>"},
			check: "channel_sensitivity", blocked: true,
		},
		{
			name:  "S2 value in an email link",
			tv:    TemplateVersion{Channel: ChannelEmail, Locale: "en-GB", Subject: "Invoice ready", Body: `<p><a href="https://example.com/pay?amount={{amount}}">Pay</a></p>`},
			check: "channel_sensitivity", blocked: true,
		},
		{
			name:  "named sensitive fact in a protected subject",
			tv:    TemplateVersion{Channel: ChannelEmail, Locale: "en-GB", Subject: "Your salary review", Body: "<p>Sign in</p>"},
			check: "subject_sensitivity", blocked: true,
		},
		{
			name:  "S2 value on the authenticated in-app surface",
			tv:    TemplateVersion{Channel: ChannelInApp, Locale: "en-GB", Subject: "Invoice", Body: "<p>{{amount}}</p>"},
			check: "channel_sensitivity", blocked: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := ValidateTemplate(intent, tc.tv, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
			var found bool
			for _, f := range report.Findings {
				if f.Check == tc.check && f.Severity == "BLOCK" {
					found = true
				}
			}
			if found != tc.blocked {
				t.Fatalf("%s: blocked=%v, want %v; findings=%+v", tc.check, found, tc.blocked, report.Findings)
			}
		})
	}
}
