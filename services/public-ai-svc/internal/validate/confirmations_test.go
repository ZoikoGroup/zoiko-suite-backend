package validate

import (
	"errors"
	"testing"

	"zoiko.io/public-ai-svc/internal/model"
)

// Doc 09 §11, §13 (ADM-T008/T010): a public answer never confirms an action
// or promises a response that no receiving system has confirmed.
func TestFalseConfirmationsRejected(t *testing.T) {
	ev := []model.Evidence{{Ref: "e1", Title: "Demo", Text: "Book a demo through the official demo page."}}
	for _, text := range []string{
		"I've booked your demo for tomorrow.",
		"Your request has been forwarded to the sales team.",
		"The team will contact you shortly.",
		"Support will get back to you within 24 hours.",
		"Your case number is ZS-12345.",
		"Your personal data has been deleted.",
		"We have escalated your enquiry.",
	} {
		if _, err := Check(model.Draft{Text: text, EvidenceRefs: []string{"e1"}}, nil, ev); !errors.Is(err, ErrFalseConfirmation) && !errors.Is(err, ErrProhibitedPhrase) && !errors.Is(err, ErrUnsupportedFact) {
			t.Errorf("%q passed validation: %v", text, err)
		}
	}
	// Route-only wording stays allowed.
	ok := "You can request a demo through the official demo page. I haven't booked anything for you."
	if _, err := Check(model.Draft{Text: ok, EvidenceRefs: []string{"e1"}}, nil, ev); err != nil {
		t.Fatalf("route-only wording rejected: %v", err)
	}
}

func TestUnfilledPlaceholdersRejected(t *testing.T) {
	ev := []model.Evidence{{Ref: "e1", Title: "Pricing", Text: "The Growth plan is listed at [price] per month. Trial: {{days}} days."}}
	for _, text := range []string{
		"The Growth plan is listed at [price] per month.",
		"A trial of [N] days is available.",
		"A trial of {{days}} days is available.",
	} {
		if _, err := Check(model.Draft{Text: text, EvidenceRefs: []string{"e1"}}, nil, ev); !errors.Is(err, ErrPlaceholder) {
			t.Errorf("%q: got %v, want ErrPlaceholder", text, err)
		}
	}
}
