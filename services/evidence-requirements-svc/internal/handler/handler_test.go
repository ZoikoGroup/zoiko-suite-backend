// Package handler_test covers evidence-requirements-svc's HTTP surface
// against stub collaborators.
//
// Three of these tests exist specifically to catch defects that are live
// elsewhere on this platform, and should not be deleted as redundant:
//
//   - TestCreateRequirement_CallsAuthorization asserts the authz client is
//     actually INVOKED, not merely constructed. All ten Phase 5 services wire
//     an authz client into their handler and never call it, so their
//     governance gate is dead code that no test noticed.
//   - TestCreateRequirement_AuthzUnavailable_FailsClosed asserts a network
//     failure denies rather than permits. Phase 5's and two Phase 4 services'
//     clients fail open.
//   - TestEvaluate_NoRequirementsDefined asserts an empty catalog is NOT
//     reported as SATISFIED — the distinction tax-determination-svc loses
//     when it fabricates a synthetic ZERO-TAX rule.
//
// Run: go test ./internal/handler/
package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

const (
	testTenant    = "11111111-1111-1111-1111-111111111111"
	testEntity    = "22222222-2222-2222-2222-222222222222"
	testPrincipal = "principal-abc"
	otherTenant   = "99999999-9999-9999-9999-999999999999"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	effective []domain.EvidenceRequirement
	effectErr error

	requirement    *domain.EvidenceRequirement
	created        bool
	createErr      error
	endDateErr     error
	endDated       *domain.EvidenceRequirement
	evaluation     *domain.EvidenceEvaluation
	evalCreated    bool
	recordEvalErr  error
	recordedEvals  []domain.EvidenceEvaluation
	getRequirement *domain.EvidenceRequirement

	// listFilter records what ListRequirements was actually called with, so a
	// test can assert the catalog was read in the caller's verified tenant
	// rather than in whatever ?tenant_id= named.
	listFilter domain.ListRequirementsFilter
}

func (s *stubStore) CreateRequirement(_ context.Context, r *domain.EvidenceRequirement) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	if !s.created && s.requirement != nil {
		*r = *s.requirement
	}
	return s.created, nil
}

func (s *stubStore) GetRequirement(_ context.Context, _ string) (*domain.EvidenceRequirement, error) {
	return s.getRequirement, nil
}

func (s *stubStore) ListRequirements(_ context.Context, filter domain.ListRequirementsFilter) ([]domain.EvidenceRequirement, error) {
	s.listFilter = filter
	return s.effective, nil
}

func (s *stubStore) EffectiveRequirements(_ context.Context, _, _, _, _ string, _ time.Time) ([]domain.EvidenceRequirement, error) {
	return s.effective, s.effectErr
}

func (s *stubStore) EndDateRequirement(_ context.Context, _, _ string, _ time.Time, _, _ string) (*domain.EvidenceRequirement, error) {
	if s.endDateErr != nil {
		return nil, s.endDateErr
	}
	return s.endDated, nil
}

func (s *stubStore) RecordEvaluation(_ context.Context, e *domain.EvidenceEvaluation) (bool, error) {
	if s.recordEvalErr != nil {
		return false, s.recordEvalErr
	}
	if !s.evalCreated && s.evaluation != nil {
		*e = *s.evaluation
	}
	s.recordedEvals = append(s.recordedEvals, *e)
	return s.evalCreated, nil
}

func (s *stubStore) GetEvaluation(_ context.Context, _ string) (*domain.EvidenceEvaluation, error) {
	return s.evaluation, nil
}

type stubPublisher struct{ published []domain.EvidenceEvaluation }

func (p *stubPublisher) PublishEvaluation(_ context.Context, e domain.EvidenceEvaluation) {
	p.published = append(p.published, e)
}

type stubAuthz struct {
	err    error
	calls  int
	action string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, actionType string) error {
	a.calls++
	a.action = actionType
	return a.err
}

type stubDocs struct {
	err   error
	calls int
}

func (d *stubDocs) VerifyDocument(_ context.Context, _, _, _ string) error {
	d.calls++
	return d.err
}

// ── harness ──────────────────────────────────────────────────────────────────

func newRouter(store handler.Store, pub handler.Publisher, az handler.AuthZClient, docs handler.DocumentVaultClient) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(store, pub, az, docs, zap.NewNop()))
	return r
}

// do issues a request with the standard verified-identity headers. Pass
// tenant=="" or principal=="" to simulate a request that never passed
// gateway-auth-svc.
func do(t *testing.T, h http.Handler, method, path string, body any, tenant, principal string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func requirement(evidenceType string, spec domain.RequirementSpec) domain.EvidenceRequirement {
	payload, _ := json.Marshal(spec)
	return domain.EvidenceRequirement{
		EvidenceRequirementID: "req-" + evidenceType,
		TenantID:              testTenant,
		DomainCode:            "FINANCE",
		ActionType:            "JOURNAL_POST",
		EvidenceType:          evidenceType,
		RequirementPayload:    payload,
		EffectiveFrom:         time.Now().UTC().Add(-time.Hour),
	}
}

func evaluateBody(artifacts ...domain.PresentArtifact) domain.EvaluateRequest {
	return domain.EvaluateRequest{
		LegalEntityID:    testEntity,
		DomainCode:       "FINANCE",
		ActionType:       "JOURNAL_POST",
		PresentArtifacts: artifacts,
		CorrelationID:    "corr-1",
	}
}

func decodeEvaluate(t *testing.T, rec *httptest.ResponseRecorder) domain.EvaluateResponse {
	t.Helper()
	var out domain.EvaluateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// ── evaluate ─────────────────────────────────────────────────────────────────

// An empty catalog must NOT report SATISFIED. See package doc.
func TestEvaluate_NoRequirementsDefined(t *testing.T) {
	st := &stubStore{effective: nil, evalCreated: true}
	pub := &stubPublisher{}
	h := newRouter(st, pub, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeNoRequirementsDefined, got.Outcome)
	assert.NotEqual(t, domain.OutcomeSatisfied, got.Outcome,
		"an unconfigured gate must never be indistinguishable from a verified one")
	assert.Empty(t, got.Unmet)

	// The publisher IS consulted; it is the publisher that decides neither
	// §8.6 event is true of this outcome and emits nothing to Kafka. That
	// guarantee is asserted in internal/events/publisher_test.go, which can
	// see the broker; this stub only sees the call.
	require.Len(t, pub.published, 1)
	assert.Equal(t, domain.OutcomeNoRequirementsDefined, pub.published[0].Outcome)
}

func TestEvaluate_Missing_NamesEachUnmetRequirement(t *testing.T) {
	st := &stubStore{
		effective: []domain.EvidenceRequirement{
			requirement("SIGNATURE", domain.RequirementSpec{Description: "board signature required"}),
			requirement("APPROVAL_RECORD", domain.RequirementSpec{}),
		},
		evalCreated: true,
	}
	pub := &stubPublisher{}
	h := newRouter(st, pub, &stubAuthz{}, &stubDocs{})

	// Only the approval record is supplied.
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{EvidenceType: "APPROVAL_RECORD", ReferenceID: "wf-1"}),
		testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, got.Outcome)
	require.Len(t, got.Unmet, 1)
	assert.Equal(t, "SIGNATURE", got.Unmet[0].EvidenceType)
	assert.Equal(t, "req-SIGNATURE", got.Unmet[0].EvidenceRequirementID)
	assert.Contains(t, got.Unmet[0].Reason, "board signature required",
		"a blocked caller must learn what to produce, not just that it is blocked")

	require.Len(t, pub.published, 1)
	assert.Equal(t, domain.OutcomeMissing, pub.published[0].Outcome)
}

func TestEvaluate_Satisfied(t *testing.T) {
	st := &stubStore{
		effective:   []domain.EvidenceRequirement{requirement("APPROVAL_RECORD", domain.RequirementSpec{})},
		evalCreated: true,
	}
	pub := &stubPublisher{}
	h := newRouter(st, pub, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{EvidenceType: "APPROVAL_RECORD", ReferenceID: "wf-1"}),
		testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeSatisfied, got.Outcome)
	assert.Empty(t, got.Unmet)
	require.Len(t, pub.published, 1)
}

// minimum_count is data, not code — two artifacts required, one supplied.
func TestEvaluate_MinimumCountEnforced(t *testing.T) {
	st := &stubStore{
		effective:   []domain.EvidenceRequirement{requirement("APPROVAL_RECORD", domain.RequirementSpec{MinimumCount: 2})},
		evalCreated: true,
	}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{EvidenceType: "APPROVAL_RECORD", ReferenceID: "wf-1"}),
		testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, got.Outcome)
	require.Len(t, got.Unmet, 1)
	assert.Contains(t, got.Unmet[0].Reason, "requires 2 matching artifact(s), 1 present")
}

// artifact_subtype is also data — a present artifact of the wrong subtype
// does not satisfy the requirement.
func TestEvaluate_ArtifactSubtypeMismatch(t *testing.T) {
	st := &stubStore{
		effective: []domain.EvidenceRequirement{
			requirement("SUPPORTING_DOCUMENT", domain.RequirementSpec{ArtifactSubtype: "INVOICE"}),
		},
		evalCreated: true,
	}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{
			EvidenceType: "SUPPORTING_DOCUMENT", ReferenceID: "doc-1", ArtifactSubtype: "CONTRACT",
		}),
		testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, domain.OutcomeMissing, decodeEvaluate(t, rec).Outcome)
}

// A malformed requirement_payload must block, not silently vanish from the
// gate.
func TestEvaluate_UnreadablePayload_FailsClosed(t *testing.T) {
	bad := requirement("SIGNATURE", domain.RequirementSpec{})
	bad.RequirementPayload = json.RawMessage(`{not json`)
	st := &stubStore{effective: []domain.EvidenceRequirement{bad}, evalCreated: true}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, got.Outcome)
	require.Len(t, got.Unmet, 1)
	assert.Contains(t, got.Unmet[0].Reason, "unreadable")
}

// A document the caller claims but that does not exist must not count.
func TestEvaluate_UnverifiableDocument_DoesNotCount(t *testing.T) {
	st := &stubStore{
		effective:   []domain.EvidenceRequirement{requirement("SUPPORTING_DOCUMENT", domain.RequirementSpec{})},
		evalCreated: true,
	}
	docs := &stubDocs{err: domain.ErrDocumentNotFound}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, docs)

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{EvidenceType: "SUPPORTING_DOCUMENT", ReferenceID: "doc-ghost"}),
		testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, got.Outcome,
		"claiming a nonexistent document must not walk through the gate")
	require.Len(t, got.Unmet, 1)
	assert.Contains(t, got.Unmet[0].Reason, "does not exist")
	assert.Equal(t, 1, docs.calls, "the asserted document must actually be verified, not trusted")
}

// document-vault unreachable means the determination cannot be made —
// recording MISSING off an infrastructure outage would write a false fact
// into an append-only evidence ledger.
func TestEvaluate_DocumentServiceUnavailable_Returns503(t *testing.T) {
	st := &stubStore{
		effective:   []domain.EvidenceRequirement{requirement("SUPPORTING_DOCUMENT", domain.RequirementSpec{})},
		evalCreated: true,
	}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{err: domain.ErrDocumentServiceUnavailable})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate",
		evaluateBody(domain.PresentArtifact{EvidenceType: "SUPPORTING_DOCUMENT", ReferenceID: "doc-1"}),
		testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, st.recordedEvals, "no determination may be recorded when it could not be made")
}

// A replayed evaluation returns the original determination and publishes
// nothing further.
func TestEvaluate_Replay_DoesNotRepublish(t *testing.T) {
	original := &domain.EvidenceEvaluation{
		EvaluationID:  "eval-original",
		TenantID:      testTenant,
		LegalEntityID: testEntity,
		DomainCode:    "FINANCE",
		ActionType:    "JOURNAL_POST",
		Outcome:       domain.OutcomeMissing,
		UnmetPayload:  json.RawMessage(`[{"evidence_requirement_id":"req-SIGNATURE","evidence_type":"SIGNATURE","reason":"stored reason"}]`),
		CorrelationID: "corr-1",
	}
	st := &stubStore{
		effective:   []domain.EvidenceRequirement{requirement("SIGNATURE", domain.RequirementSpec{})},
		evaluation:  original,
		evalCreated: false,
	}
	pub := &stubPublisher{}
	h := newRouter(st, pub, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeEvaluate(t, rec)
	assert.Equal(t, "eval-original", got.EvaluationID)
	require.Len(t, got.Unmet, 1)
	assert.Equal(t, "stored reason", got.Unmet[0].Reason,
		"the recorded decision is authoritative on replay, not a fresh re-evaluation")
	assert.Empty(t, pub.published, "a replay must not republish the event")
}

func TestEvaluate_MissingTenant_Returns400(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), "", testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "missing_tenant",
		"absent tenant scope must be rejected, never defaulted to a placeholder tenant")
}

func TestEvaluate_MissingPrincipal_Returns401(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestEvaluate_MissingCorrelationID_Returns400(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	body := evaluateBody()
	body.CorrelationID = ""
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", body, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEvaluate_StoreUnavailable_Returns503(t *testing.T) {
	st := &stubStore{effectErr: domain.ErrStoreUnavailable}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── catalog mutation ─────────────────────────────────────────────────────────

func createBody() domain.CreateRequirementRequest {
	return domain.CreateRequirementRequest{
		TenantID:      testTenant,
		DomainCode:    "FINANCE",
		ActionType:    "JOURNAL_POST",
		EvidenceType:  "SUPPORTING_DOCUMENT",
		CorrelationID: "corr-create-1",
	}
}

// The authz client must actually be invoked. See package doc.
func TestCreateRequirement_CallsAuthorization(t *testing.T) {
	az := &stubAuthz{}
	h := newRouter(&stubStore{created: true}, &stubPublisher{}, az, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", createBody(), testTenant, testPrincipal)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, 1, az.calls, "authorization must be checked, not merely wired")
	assert.Equal(t, "EVIDENCE_REQUIREMENT_CREATE", az.action)
}

func TestCreateRequirement_AuthzDenied_Returns403(t *testing.T) {
	az := &stubAuthz{err: domain.ErrAuthorizationDenied}
	st := &stubStore{created: true}
	h := newRouter(st, &stubPublisher{}, az, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", createBody(), testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// Fail-closed on an unreachable authorization-svc. See package doc.
func TestCreateRequirement_AuthzUnavailable_FailsClosed(t *testing.T) {
	az := &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}
	h := newRouter(&stubStore{created: true}, &stubPublisher{}, az, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", createBody(), testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"an unreachable authorization-svc must deny, never permit")
}

// A body tenant_id that disagrees with the verified header must not be
// honoured — otherwise a caller writes into another tenant's catalog.
func TestCreateRequirement_TenantScopeMismatch_Returns403(t *testing.T) {
	az := &stubAuthz{}
	body := createBody()
	body.TenantID = "99999999-9999-9999-9999-999999999999"
	h := newRouter(&stubStore{created: true}, &stubPublisher{}, az, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", body, testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant_scope_mismatch")
	assert.Equal(t, 0, az.calls, "scope must be rejected before authorization is consulted")
}

func TestCreateRequirement_Idempotent_ReplayReturns200(t *testing.T) {
	existing := &domain.EvidenceRequirement{
		EvidenceRequirementID: "req-existing",
		TenantID:              testTenant,
		DomainCode:            "FINANCE",
		ActionType:            "JOURNAL_POST",
		EvidenceType:          "SUPPORTING_DOCUMENT",
		CorrelationID:         "corr-create-1",
	}
	st := &stubStore{created: false, requirement: existing}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", createBody(), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)

	var got domain.EvidenceRequirement
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "req-existing", got.EvidenceRequirementID)
}

func TestCreateRequirement_MissingFields_Returns400(t *testing.T) {
	h := newRouter(&stubStore{created: true}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	for _, field := range []string{"domain_code", "action_type", "evidence_type", "correlation_id"} {
		body := createBody()
		switch field {
		case "domain_code":
			body.DomainCode = ""
		case "action_type":
			body.ActionType = ""
		case "evidence_type":
			body.EvidenceType = ""
		case "correlation_id":
			body.CorrelationID = ""
		}
		rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", body, testTenant, testPrincipal)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "missing %s must be rejected", field)
	}
}

// ── retirement ───────────────────────────────────────────────────────────────

func TestEndDate_AlreadyRetired_Returns422(t *testing.T) {
	st := &stubStore{
		getRequirement: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", TenantID: testTenant},
		endDateErr:     domain.ErrAlreadyRetired,
	}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements/req-1/end-date",
		domain.EndDateRequirementRequest{Reason: "superseded"}, testTenant, testPrincipal)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"a second retirement must be rejected, never a silent no-op")
	assert.Contains(t, rec.Body.String(), "already_retired")
}

func TestEndDate_RequiresReason(t *testing.T) {
	st := &stubStore{getRequirement: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1"}}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements/req-1/end-date",
		domain.EndDateRequirementRequest{}, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEndDate_NotFound_Returns404(t *testing.T) {
	h := newRouter(&stubStore{getRequirement: nil}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements/nope/end-date",
		domain.EndDateRequirementRequest{Reason: "x"}, testTenant, testPrincipal)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEndDate_CallsAuthorization(t *testing.T) {
	az := &stubAuthz{}
	st := &stubStore{
		getRequirement: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", TenantID: testTenant},
		endDated:       &domain.EvidenceRequirement{EvidenceRequirementID: "req-1"},
	}
	h := newRouter(st, &stubPublisher{}, az, &stubDocs{})

	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements/req-1/end-date",
		domain.EndDateRequirementRequest{Reason: "superseded"}, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, az.calls)
	assert.Equal(t, "EVIDENCE_REQUIREMENT_RETIRE", az.action)
}

// ── reads ────────────────────────────────────────────────────────────────────

// TestListRequirements_NoTenantScope_Refused replaces a test that asserted a 400
// when ?tenant_id= was absent — which documented the vulnerability as correct,
// since supplying the parameter was exactly how a caller read another tenant's
// catalog. The scope now comes from the header, so its absence is the failure and
// its presence is no longer required.
func TestListRequirements_NoTenantScope_Refused(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet, "/v1/evidence-requirements", nil, "", testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListRequirements_NoQueryParam_UsesVerifiedScope(t *testing.T) {
	st := &stubStore{}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet, "/v1/evidence-requirements", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, testTenant, st.listFilter.TenantID,
		"the catalog must be read in the caller's verified tenant, not in whatever the query named")
}

// TestListRequirements_ForeignTenantQueryParam_Refused is the regression test for
// the read half of this service's tenant scoping: ?tenant_id= was handed straight
// to the store, which both filtered on it and set app.tenant_id from it.
func TestListRequirements_ForeignTenantQueryParam_Refused(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet, "/v1/evidence-requirements?tenant_id="+otherTenant, nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// A malformed legal_entity_id is compared as text against a cast column, so it
// matched nothing — and for an evidence gate, "no requirements" reads as
// permission to proceed.
func TestListRequirements_MalformedLegalEntityFilter_Refused(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet, "/v1/evidence-requirements?legal_entity_id=not-a-uuid", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListRequirements_RejectsBadAsOf(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet,
		"/v1/evidence-requirements?tenant_id="+testTenant+"&as_of=yesterday", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetEvaluation_NotFound_Returns404(t *testing.T) {
	h := newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
	rec := do(t, h, http.MethodGet, "/v1/evidence/evaluations/eval-nope", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
