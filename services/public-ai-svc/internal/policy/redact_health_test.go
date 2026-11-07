package policy

import (
	"strings"
	"testing"
)

// Doc 06 §15, Doc 07 §14: a visitor's own health data is special-category
// data. It is masked before anything else sees the message and the turn is
// handled as sensitive input (X03), never answered with it.
func TestHealthDataIsSensitiveInput(t *testing.T) {
	for _, msg := range []string{
		"I have diabetes, can ZoikoSuite still work for me?",
		"I have type 2 diabetes and need a cheaper plan",
		"Can I store my HIV medication schedule in ZoikoSuite?",
		"I was diagnosed with cancer last month, can I pause billing?",
		"I've been diagnosed with ADHD so I need simpler screens",
		"I'm pregnant and want to know about the trial",
		"I am on antidepressants, does that matter for payroll?",
		"I take insulin every day",
		"my mental health condition makes demos hard",
		"I suffer from epilepsy, is the UI safe?",
		"Is my medical records upload secure?",
		"I had a stroke last year so typing is slow",
		"Ｉ have diabetes", // fullwidth letters fold first
	} {
		d := Classify(msg)
		if d.Exception != X03SensitiveInput {
			t.Errorf("%q: exception %q, want X03", msg, d.Exception)
		}
		if !contains(d.Redaction.Kinds, KindHealth) {
			t.Errorf("%q: kinds %v lack %s", msg, d.Redaction.Kinds, KindHealth)
		}
		lower := strings.ToLower(d.Redaction.Text)
		for _, term := range []string{"diabetes", "hiv", "cancer", "adhd", "pregnant", "antidepressants", "insulin", "epilepsy", "stroke"} {
			if strings.Contains(strings.ToLower(msg), term) && strings.Contains(lower, term) {
				t.Errorf("%q: %q survived redaction: %q", msg, term, d.Redaction.Text)
			}
		}
	}
}

// Product and HR vocabulary that mentions health is not health data.
func TestHealthDetectorFalsePositives(t *testing.T) {
	for _, msg := range []string{
		"Do you run health checks on the platform?",
		"Does the HR module manage health benefits?",
		"How is system health monitored?",
		"What does the service health dashboard show?",
		"Can employees enrol in health insurance through payroll?",
		"I have a question about pricing",
		"I have anxiety about migrating our ledgers",
		"I had a stroke of luck finding you",
		"My status page shows green",
		"What is the diagnosis feature for failed invoices?",
		"Do you support healthcare companies?",
		"my condition for buying is SSO support",
	} {
		r := Redact(msg)
		if contains(r.Kinds, KindHealth) {
			t.Errorf("%q flagged as health data: %q", msg, r.Text)
		}
	}
}
