package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ── Stubs for soft-close override tests ────────────────────────────────────────

// stubCloseForPolicy returns a stub that returns the given posting policy
type stubCloseForPolicy struct {
	postingPolicy string
	periodState   string
}

func (c *stubCloseForPolicy) CheckPeriodOpen(_ context.Context, _, _, _ string) error {
	switch c.postingPolicy {
	case "OPEN", "REOPENED":
		return nil
	case "RESTRICTED":
		return domain.ErrSoftCloseOverrideRequired
	case "CLOSE_JOURNALS_ONLY", "CLOSED":
		return domain.ErrPeriodHardClosed
	default:
		return domain.ErrPeriodHardClosed
	}
}

// CheckPeriodOpenAt returns exactly what CheckPeriodOpen does, like the real client.
func (c *stubCloseForPolicy) CheckPeriodOpenAt(ctx context.Context, tenantID string, ref close.PeriodRef) error {
	return c.CheckPeriodOpen(ctx, tenantID, ref.LegalEntityID, ref.PeriodName)
}

var _ close.Client = (*stubCloseForPolicy)(nil)

// selectiveAuthZ allows all actions except one specific action to test authz denial
type selectiveAuthZ struct {
	seen       []string
	denyAction string
}

func (a *selectiveAuthZ) CheckAllowed(_ context.Context, _, _, actionType string) error {
	a.seen = append(a.seen, actionType)
	if actionType == a.denyAction {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// newSoftCloseRouter builds a router with the given close client and authz
func newSoftCloseRouter(s *stubStore, c close.Client, a handler.AuthZClient) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(s, &stubPublisher{}, a, c, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// ── Test Cases ────────────────────────────────────────────────────────────────

// TestSoftCloseOverride_A_OPEN_NoOverrideRequired verifies OPEN period allows posting without override
func TestSoftCloseOverride_A_OPEN_NoOverrideRequired(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "OPEN", periodState: "OPEN"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for OPEN period, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify no GL_SOFT_CLOSE_POSTING_OVERRIDE action was checked
	for _, action := range authz.seen {
		if action == "GL_SOFT_CLOSE_POSTING_OVERRIDE" {
			t.Errorf("GL_SOFT_CLOSE_POSTING_OVERRIDE should not be checked for OPEN period, but was")
		}
	}
}

// TestSoftCloseOverride_B_SOFT_CLOSE_ValidOverride verifies SOFT_CLOSE allows posting with valid override
func TestSoftCloseOverride_B_SOFT_CLOSE_ValidOverride(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Emergency correction approved by CFO"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for SOFT_CLOSE with valid override, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify GL_SOFT_CLOSE_POSTING_OVERRIDE was checked
	found := false
	for _, action := range authz.seen {
		if action == "GL_SOFT_CLOSE_POSTING_OVERRIDE" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GL_SOFT_CLOSE_POSTING_OVERRIDE action was not checked")
	}
}

// TestSoftCloseOverride_C_SOFT_CLOSE_AuthzDenied verifies SOFT_CLOSE rejects when authz denies
func TestSoftCloseOverride_C_SOFT_CLOSE_AuthzDenied(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	// Only deny GL_SOFT_CLOSE_POSTING_OVERRIDE, allow actionCreateJournal
	authz := &selectiveAuthZ{denyAction: "GL_SOFT_CLOSE_POSTING_OVERRIDE"}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Emergency correction approved by CFO"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authz denies, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSoftCloseOverride_D_SOFT_CLOSE_EmptyReason verifies SOFT_CLOSE rejects empty reason
func TestSoftCloseOverride_D_SOFT_CLOSE_EmptyReason(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "" // empty reason
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty reason, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify authz was NOT checked (fail fast on reason validation)
	for _, action := range authz.seen {
		if action == "GL_SOFT_CLOSE_POSTING_OVERRIDE" {
			t.Errorf("GL_SOFT_CLOSE_POSTING_OVERRIDE should not be checked when reason is empty")
		}
	}
}

// TestSoftCloseOverride_E_HARD_CLOSED_NoOverrideWorks verifies HARD_CLOSED rejects even with override
func TestSoftCloseOverride_E_HARD_CLOSED_NoOverrideWorks(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "CLOSED", periodState: "HARD_CLOSED"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Emergency correction approved by CFO"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 for HARD_CLOSED even with override, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSoftCloseOverride_F_CLOSE_REVIEW_TreatedAsHardClosed verifies CLOSE_REVIEW is treated as HARD_CLOSED
func TestSoftCloseOverride_F_CLOSE_REVIEW_TreatedAsHardClosed(t *testing.T) {
	c := &stubCloseForPolicy{postingPolicy: "CLOSE_JOURNALS_ONLY", periodState: "CLOSE_REVIEW"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(newStubStore(), c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Emergency correction approved by CFO"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 for CLOSE_REVIEW treated as hard closed, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSoftCloseOverride_OverridePersistedOnHeader verifies the reason is stored on JournalHeader
func TestSoftCloseOverride_OverridePersistedOnHeader(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(s, c, authz)

	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Persisted reason test"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Decode response to get journal ID
	var created domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Find the created journal and verify the reason is stored
	journal := s.journals[created.JournalID]
	if journal == nil {
		t.Fatal("journal not found in stub store")
	}

	if journal.SoftCloseOverrideReason == nil {
		t.Fatal("SoftCloseOverrideReason is nil, expected pointer to string")
	}
	if *journal.SoftCloseOverrideReason != "Persisted reason test" {
		t.Errorf("expected SoftCloseOverrideReason='Persisted reason test', got '%s'", *journal.SoftCloseOverrideReason)
	}
}

// TestSoftCloseOverride_PostJournalHonorsOverride verifies PostJournal also honors override from creation
func TestSoftCloseOverride_PostJournalHonorsOverride(t *testing.T) {
	s := newStubStore()
	c := &stubCloseForPolicy{postingPolicy: "RESTRICTED", periodState: "SOFT_CLOSE"}
	authz := &recordingAuthZ{}
	r := newSoftCloseRouter(s, c, authz)

	// Create a pending journal with override
	req := validCreateReq()
	req.OverrideSoftClose = true
	req.SoftCloseOverrideReason = "Reason for posting"
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}

	// Extract journal ID from response
	var created domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Validate first (PENDING -> VALIDATED)
	rec = doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rec.Code, rec.Body.String())
	}

	// Now post it - should honor the override from creation (no new override needed)
	rec = doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for PostJournal with override, got %d: %s", rec.Code, rec.Body.String())
	}
}