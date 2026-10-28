package domain

import (
	"errors"
	"testing"
)

const testJournalID = "5b0f6a3e-2c1d-4e8f-9a7b-6c5d4e3f2a1b"

// A5: the reference names a general-ledger-svc journal by UUID, and only on
// EXECUTED. Its spelling is canonical, so one journal is stored one way.
func TestValidateAuthoritativeRef(t *testing.T) {
	if err := ValidateAuthoritativeRef(StatusApproved, nil); err != nil {
		t.Fatalf("no reference on approval: %v", err)
	}
	ref := &AuthoritativeRef{Kind: AuthoritativeRefGLJournal, JournalID: " 5B0F6A3E-2C1D-4E8F-9A7B-6C5D4E3F2A1B "}
	if err := ValidateAuthoritativeRef(StatusExecuted, ref); err != nil || ref.JournalID != testJournalID {
		t.Fatalf("journal reference on EXECUTED: %v, stored as %q", err, ref.JournalID)
	}
	for name, c := range map[string]struct {
		to  SuggestionStatus
		ref AuthoritativeRef
	}{
		"on APPROVED":       {StatusApproved, AuthoritativeRef{Kind: AuthoritativeRefGLJournal, JournalID: testJournalID}},
		"on REJECTED":       {StatusRejected, AuthoritativeRef{Kind: AuthoritativeRefGLJournal, JournalID: testJournalID}},
		"no kind":           {StatusExecuted, AuthoritativeRef{JournalID: testJournalID}},
		"unknown kind":      {StatusExecuted, AuthoritativeRef{Kind: "ADJUSTMENT", JournalID: testJournalID}},
		"no journal id":     {StatusExecuted, AuthoritativeRef{Kind: AuthoritativeRefGLJournal}},
		"free-text journal": {StatusExecuted, AuthoritativeRef{Kind: AuthoritativeRefGLJournal, JournalID: "JE-2026-10 write-off"}},
	} {
		ref := c.ref
		if err := ValidateAuthoritativeRef(c.to, &ref); !errors.Is(err, ErrInvalidAuthoritativeRef) {
			t.Errorf("%s: %v, want ErrInvalidAuthoritativeRef", name, err)
		}
	}
}

// A5: an accounting action is not EXECUTED without its journal; a
// recommendation carried out in bank-reconciliation-svc does not take one.
func TestValidateExecutionRef(t *testing.T) {
	ref := &AuthoritativeRef{Kind: AuthoritativeRefGLJournal, JournalID: testJournalID}
	for _, rec := range []ResolutionRecommendation{RecommendationWriteOff, RecommendationTimingAdjustment} {
		if err := ValidateExecutionRef(rec, StatusExecuted, nil); !errors.Is(err, ErrAuthoritativeRefRequired) {
			t.Errorf("%s EXECUTED without a journal: %v, want ErrAuthoritativeRefRequired", rec, err)
		}
		if err := ValidateExecutionRef(rec, StatusExecuted, ref); err != nil {
			t.Errorf("%s EXECUTED with a journal: %v", rec, err)
		}
		if err := ValidateExecutionRef(rec, StatusApproved, nil); err != nil {
			t.Errorf("%s APPROVED needs no journal: %v", rec, err)
		}
	}
	for _, r := range []*AuthoritativeRef{nil, ref} {
		if err := ValidateExecutionRef(RecommendationManualReview, StatusExecuted, r); err != nil {
			t.Errorf("MANUAL_REVIEW EXECUTED with ref %v: %v", r, err)
		}
	}
	for _, rec := range []ResolutionRecommendation{RecommendationConfirmDuplicate, RecommendationAutoMatch} {
		if err := ValidateExecutionRef(rec, StatusExecuted, nil); err != nil {
			t.Errorf("%s EXECUTED without a journal: %v", rec, err)
		}
		if err := ValidateExecutionRef(rec, StatusExecuted, ref); !errors.Is(err, ErrInvalidAuthoritativeRef) {
			t.Errorf("%s EXECUTED with a journal: %v, want ErrInvalidAuthoritativeRef", rec, err)
		}
	}
}

// The reference arrives on the apply body next to the status.
func TestApplyResolutionRequest_DecodesAuthoritativeRef(t *testing.T) {
	var r ApplyResolutionRequest
	body := `{"suggestion_status":"EXECUTED","authoritative_ref":{"kind":"GL_JOURNAL","journal_id":"` + testJournalID + `"}}`
	if err := r.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if r.AuthoritativeRef == nil || r.AuthoritativeRef.Kind != AuthoritativeRefGLJournal || r.AuthoritativeRef.JournalID != testJournalID {
		t.Fatalf("decoded %+v", r.AuthoritativeRef)
	}
}
