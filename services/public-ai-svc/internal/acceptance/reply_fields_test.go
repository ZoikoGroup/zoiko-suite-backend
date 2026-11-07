package acceptance

import (
	"regexp"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Doc 01 FR-03 / AT-01: a cited source carries its review (approval or
// effective) date; Doc 02 §15: every reply carries its safety disposition.
func TestReplyCarriesSourceDateAndDisposition(t *testing.T) {
	x := publicKBServer(t)
	s := x.session("reply-fields-1")
	e := envelope(t, x.turn(s, "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "What is ZoikoSuite?"))
	if e.Status != contract.Answered || len(e.Citations) == 0 {
		t.Fatalf("want a cited answer, got %s: %q", e.Status, e.Text)
	}
	for _, c := range e.Citations {
		if !isoDate.MatchString(c.ReviewedAt) {
			t.Errorf("citation %s has no review date: %q", c.SourceVersionID, c.ReviewedAt)
		}
	}
	if e.SafetyDisposition != contract.DispositionAllow {
		t.Errorf("answer disposition %q, want ALLOW", e.SafetyDisposition)
	}
	r := envelope(t, x.turn(s, "1a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "Pay my vendor bills for me"))
	if r.Status != contract.RefusedAction || r.AnswerClass != contract.SafeRefusal || r.SafetyDisposition != contract.DispositionDeny {
		t.Errorf("refusal: %s %s %s", r.Status, r.AnswerClass, r.SafetyDisposition)
	}
}
