package handler_test

// ACC-14's CLOSE_REVIEW policy is "Only designated close journals/workflows
// permitted". These tests pin both halves of the boundary: an ordinary journal
// is still refused exactly as before, and a designated close journal is
// admitted only with a valid designation, a matching journal type, and the
// dedicated GL_CLOSE_REVIEW_POSTING action. A hard-closed period never admits
// either kind.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
)

func closeReviewRouter(s *stubStore, authz handler.AuthZClient) (chi.Router, *stubCloseForPolicy) {
	c := &stubCloseForPolicy{postingPolicy: "CLOSE_JOURNALS_ONLY", periodState: "CLOSE_REVIEW"}
	return newSoftCloseRouter(s, c, authz), c
}

func TestCloseReview_OrdinaryJournalStillRefused(t *testing.T) {
	s := newStubStore()
	r, _ := closeReviewRouter(s, &recordingAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("ordinary posting in CLOSE_REVIEW must be refused with 412, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"period_locked"`) {
		t.Fatalf("ordinary posting must keep the existing period_locked refusal, got %s", rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("a refused ordinary journal must not be stored, got %d", len(s.journals))
	}
}

func TestCloseReview_DesignatedCloseJournalAllowed(t *testing.T) {
	s := newStubStore()
	authz := &recordingAuthZ{}
	r, _ := closeReviewRouter(s, authz)

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("designated close journal must be admitted in CLOSE_REVIEW, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 1 {
		t.Fatalf("expected one stored journal, got %d", len(s.journals))
	}
	for _, h := range s.journals {
		if h.CloseJournalDesignation == nil || *h.CloseJournalDesignation != domain.CloseJournalDesignationAccrualRecognition {
			t.Fatalf("stored designation = %v, want %s", h.CloseJournalDesignation, domain.CloseJournalDesignationAccrualRecognition)
		}
	}
	found := false
	for _, action := range authz.seen {
		if action == "GL_CLOSE_REVIEW_POSTING" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GL_CLOSE_REVIEW_POSTING was not checked; seen %v", authz.seen)
	}
}

func TestCloseReview_DesignatedCloseJournalRequiresDedicatedAction(t *testing.T) {
	s := newStubStore()
	authz := &selectiveAuthZ{denyAction: "GL_CLOSE_REVIEW_POSTING"}
	r, _ := closeReviewRouter(s, authz)

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("designation without GL_CLOSE_REVIEW_POSTING must be 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("unauthorized designated journal must not be stored, got %d", len(s.journals))
	}
}

func TestCloseReview_InvalidDesignationRefusedEvenInOpen(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	req := validCreateReq()
	req.CloseJournalDesignation = "NOT_A_CLOSE_WORKFLOW"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unrecognised designation must be refused with 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_close_journal_designation") {
		t.Fatalf("expected invalid_close_journal_designation, got %s", rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("invalid designation must not be stored, got %d", len(s.journals))
	}
}

func TestCloseReview_DesignationMustMatchJournalType(t *testing.T) {
	s := newStubStore()
	r, _ := closeReviewRouter(s, &recordingAuthZ{})

	req := validCreateReq() // STANDARD
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a designation that disagrees with journal_type must be 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("mismatched designation must not be stored, got %d", len(s.journals))
	}
}

func TestSoftClose_DesignationIsNotAnOverride(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a designation alone must not satisfy SOFT_CLOSE, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "soft_close_override_required") {
		t.Fatalf("expected soft_close_override_required, got %s", rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("journal without the SOFT_CLOSE override must not be stored, got %d", len(s.journals))
	}
}

func TestHardClosed_DesignatedCloseJournalStillRefused(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "CLOSED", periodState: "HARD_CLOSED"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a designation must never admit a journal to CLOSED, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"period_locked"`) {
		t.Fatalf("expected period_locked, got %s", rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("hard-closed designated journal must not be stored, got %d", len(s.journals))
	}
}

func TestPostJournal_CloseReviewDesignatedHeaderAllowed(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	create := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created domain.JournalWithLines
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// The period moves to CLOSE_REVIEW after the journal was created. The
	// designation is on the stored header, so posting still works.
	c.postingPolicy, c.periodState = "CLOSE_JOURNALS_ONLY", "CLOSE_REVIEW"
	validate := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if validate.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", validate.Code, validate.Body.String())
	}
	post := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")
	if post.Code != http.StatusOK {
		t.Fatalf("designated header must post in CLOSE_REVIEW, got %d: %s", post.Code, post.Body.String())
	}
}

func TestPostJournal_CloseReviewDesignatedHeaderRequiresActionAtPost(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	// The action is not needed while the period is OPEN; the designation is
	// still validated, but CLOSE_REVIEW is the state that requires the grant.
	authz := &selectiveAuthZ{denyAction: "GL_CLOSE_REVIEW_POSTING"}
	r := newSoftCloseRouter(s, c, authz)

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	create := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created domain.JournalWithLines
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	validate := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if validate.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", validate.Code, validate.Body.String())
	}

	// The designation was accepted while OPEN, but the period is now
	// CLOSE_REVIEW. The post must re-check the dedicated action rather than
	// trusting the create-time decision.
	c.postingPolicy, c.periodState = "CLOSE_JOURNALS_ONLY", "CLOSE_REVIEW"
	post := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")
	if post.Code != http.StatusForbidden {
		t.Fatalf("post without GL_CLOSE_REVIEW_POSTING must be 403, got %d: %s", post.Code, post.Body.String())
	}
}

func TestPostJournal_CloseReviewOrdinaryHeaderStillRefused(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	create := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created domain.JournalWithLines
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	c.postingPolicy, c.periodState = "CLOSE_JOURNALS_ONLY", "CLOSE_REVIEW"
	validate := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if validate.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", validate.Code, validate.Body.String())
	}
	post := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")
	if post.Code != http.StatusPreconditionFailed {
		t.Fatalf("ordinary header must still be refused in CLOSE_REVIEW, got %d: %s", post.Code, post.Body.String())
	}
	if !strings.Contains(post.Body.String(), `"error":"period_locked"`) {
		t.Fatalf("expected period_locked, got %s", post.Body.String())
	}
}

func TestPostJournal_HardClosedDesignatedHeaderStillRefused(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	r := newSoftCloseRouter(s, c, &recordingAuthZ{})

	req := validCreateReq()
	req.JournalType = domain.JournalTypeAccrual
	req.JournalPurpose = "ACCRUAL"
	req.CloseJournalDesignation = domain.CloseJournalDesignationAccrualRecognition
	create := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created domain.JournalWithLines
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	c.postingPolicy, c.periodState = "CLOSED", "HARD_CLOSED"
	validate := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if validate.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", validate.Code, validate.Body.String())
	}
	post := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")
	if post.Code != http.StatusPreconditionFailed {
		t.Fatalf("designated header must be refused once hard closed, got %d: %s", post.Code, post.Body.String())
	}
}
