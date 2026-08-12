package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	"zoiko.io/vendor-due-diligence-svc/internal/handler"
	"zoiko.io/vendor-due-diligence-svc/internal/middleware"
	"zoiko.io/vendor-due-diligence-svc/internal/store"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	checks   map[string]*domain.VendorDDCheck // keyed by check_id
	byCorr   map[string]string                // correlation_id -> check_id
	evidence map[string][]domain.VendorDDEvidence

	// Injected failures. concludeErr is the important one: the split
	// AddEvidence/CompleteCheck pair this replaced swallowed the evidence
	// failure, and nothing exercised that branch.
	concludeErr    error
	markFailedErr  error
	listChecksErr  error
	lastListFilter store.ListFilter
	markFailedCall int
	// noTenant makes every method behave as it does when X-Tenant-Id never
	// arrived, which used to be reported to the caller as 503 store_unavailable.
	noTenant bool
}

func newStubStore() *stubStore {
	return &stubStore{
		checks:   make(map[string]*domain.VendorDDCheck),
		byCorr:   make(map[string]string),
		evidence: make(map[string][]domain.VendorDDEvidence),
	}
}

func (s *stubStore) CreateCheck(_ context.Context, c *domain.VendorDDCheck) (bool, error) {
	if s.noTenant {
		return false, domain.ErrTenantMissing
	}
	if id, ok := s.byCorr[c.CorrelationID]; ok {
		*c = *s.checks[id]
		return false, nil
	}
	stored := *c
	s.checks[c.CheckID] = &stored
	s.byCorr[c.CorrelationID] = c.CheckID
	return true, nil
}

func (s *stubStore) GetCheck(_ context.Context, id string) (*domain.VendorDDCheck, error) {
	if s.noTenant {
		return nil, domain.ErrTenantMissing
	}
	c, ok := s.checks[id]
	if !ok {
		return nil, domain.ErrCheckNotFound
	}
	copied := *c
	return &copied, nil
}

func (s *stubStore) ListChecks(_ context.Context, f store.ListFilter) ([]domain.VendorDDCheck, error) {
	s.lastListFilter = f
	if s.noTenant {
		return nil, domain.ErrTenantMissing
	}
	if s.listChecksErr != nil {
		return nil, s.listChecksErr
	}
	var out []domain.VendorDDCheck
	for _, c := range s.checks {
		if f.LegalEntityID != "" && c.LegalEntityID != f.LegalEntityID {
			continue
		}
		if f.CounterpartyID != "" && c.CounterpartyID != f.CounterpartyID {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) ConcludeCheck(
	_ context.Context,
	checkID, riskOutcome, screeningBasis, screeningSource string,
	evidence *domain.VendorDDEvidence,
) error {
	if s.concludeErr != nil {
		return s.concludeErr
	}
	c, ok := s.checks[checkID]
	if !ok {
		return domain.ErrCheckNotFound
	}
	if c.Status != domain.StatusStarted {
		return domain.ErrCheckAlreadyConcluded
	}
	c.Status = domain.StatusCompleted
	c.RiskOutcome = riskOutcome
	c.ScreeningBasis = screeningBasis
	c.ScreeningSource = screeningSource
	// Atomic in the real store, so the stub must not be able to record one
	// without the other — a stub that could would hide the very defect the
	// single transaction exists to prevent.
	s.evidence[checkID] = append(s.evidence[checkID], *evidence)
	return nil
}

func (s *stubStore) MarkFailed(_ context.Context, checkID, reason string) error {
	s.markFailedCall++
	if s.markFailedErr != nil {
		return s.markFailedErr
	}
	c, ok := s.checks[checkID]
	if !ok {
		return domain.ErrCheckNotFound
	}
	c.Status = domain.StatusFailed
	c.ScreeningBasis = reason
	return nil
}

func (s *stubStore) ListEvidence(_ context.Context, checkID string) ([]domain.VendorDDEvidence, error) {
	if s.noTenant {
		return nil, domain.ErrTenantMissing
	}
	return s.evidence[checkID], nil
}

type stubPublisher struct {
	started, completed, failed int
	lastFailReason             string
}

func (p *stubPublisher) PublishStarted(_ context.Context, _ string, _ domain.VendorDDCheck) {
	p.started++
}
func (p *stubPublisher) PublishCompleted(_ context.Context, _ string, _ domain.VendorDDCheck) {
	p.completed++
}
func (p *stubPublisher) PublishFailed(_ context.Context, _ string, _ domain.VendorDDCheck, reason string) {
	p.failed++
	p.lastFailReason = reason
}

type stubAuthZ struct {
	err error
	// Scopes records what each call was authorized against, which is how the
	// fail-open list is asserted: the old code made NO call at all when
	// legal_entity_id was omitted.
	scopes  []string
	actions []string
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, legalEntityID, actionType string) error {
	a.scopes = append(a.scopes, legalEntityID)
	a.actions = append(a.actions, actionType)
	return a.err
}

type stubCounterparty struct {
	complianceCalls, riskCalls int
	failCompliance             bool
}

func (c *stubCounterparty) UpdateComplianceStatus(_ context.Context, _, _, _ string) error {
	c.complianceCalls++
	if c.failCompliance {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *stubCounterparty) UpdateRiskCategory(_ context.Context, _, _, _ string) error {
	c.riskCalls++
	return nil
}

// ── router factory ─────────────────────────────────────────────────────────────

// UUIDs, not readable labels. The authorization scope must be a UUID — see
// handler.validScope — so fixtures like "tenant-abc" would now be refused with 400
// before any authorization call, which is exactly the behaviour under test
// elsewhere and would make every other test here assert the wrong thing.
const (
	testTenant = "11111111-1111-1111-1111-111111111111"
	testEntity = "22222222-2222-2222-2222-222222222222"
	otherEntiy = "22222222-2222-2222-2222-2222222222ee"
	testCP     = "33333333-3333-3333-3333-333333333333"
)

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cp *stubCounterparty) chi.Router {
	return newRouterWithTenant(s, pub, authz, cp, testTenant)
}

func newRouterWithTenant(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cp *stubCounterparty, tenant string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cp, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func doRaw(r chi.Router, method, path, body, principalID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func validBody(vendorName, correlationID string) map[string]any {
	return map[string]any{
		"counterparty_id": testCP,
		"legal_entity_id": testEntity,
		"vendor_name":     vendorName,
		"correlation_id":  correlationID,
	}
}

// errorBody reads the platform error shape. The console parses `error`/`detail`
// (plus `field`/`message`); this service used to answer `error_code`/
// `error_message`, so every failure reached the UI as a bare status code.
func errorBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not a JSON object: %v (%s)", err, rr.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("error body has no `error` key, which the admin console parses: %s", rr.Body.String())
	}
	if _, ok := body["error_code"]; ok {
		t.Errorf("error body still uses the old error_code shape: %s", rr.Body.String())
	}
	return body
}

func decodeDetail(t *testing.T, rr *httptest.ResponseRecorder) domain.CheckDetailResponse {
	t.Helper()
	var resp domain.CheckDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	return resp
}

// ── CreateCheck: identity and authorization ───────────────────────────────────

func TestCreateCheck_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Acme Corp", "corr-1"), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
	errorBody(t, rr)
}

func TestCreateCheck_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Acme Corp", "corr-1"), "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
	if got := errorBody(t, rr)["error"]; got != "forbidden" {
		t.Errorf("expected error=forbidden got %q", got)
	}
}

func TestCreateCheck_AuthzUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthzServiceUnavailabe}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Acme Corp", "corr-1"), "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable authorization-svc must refuse the action, got %d", rr.Code)
	}
	if got := errorBody(t, rr)["error"]; got != "authz_unavailable" {
		t.Errorf("expected error=authz_unavailable got %q", got)
	}
}

// Missing X-Tenant-Id previously answered 503 store_unavailable, telling a caller
// who had simply omitted a header that the database was down.
func TestCreateCheck_MissingTenant_IsUnauthorizedNotOutage(t *testing.T) {
	s := newStubStore()
	s.noTenant = true
	r := newRouterWithTenant(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{}, "")
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Acme Corp", "corr-1"), "principal-1")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a missing tenant scope, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "identity_missing" {
		t.Errorf("expected error=identity_missing got %q", got)
	}
}

// ── CreateCheck: request validation ───────────────────────────────────────────

func TestCreateCheck_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": testCP,
		"legal_entity_id": testEntity,
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
	if got := errorBody(t, rr)["error"]; got != "missing_fields" {
		t.Errorf("expected error=missing_fields got %q", got)
	}
}

// A blank-but-present vendor name passed the old `!= ""` test, then screening
// trimmed it to "" and matched nothing — producing a COMPLETED/CLEAR check for a
// vendor with no name. A clean due-diligence result is the worst possible answer
// to "who is this vendor?".
func TestCreateCheck_WhitespaceVendorName_IsRejectedNotCleared(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("   ", "corr-blank"), "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a blank vendor name must be refused, not screened; got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.checks) != 0 {
		t.Errorf("no check should have been recorded, got %d", len(s.checks))
	}
	if pub.started != 0 || pub.completed != 0 {
		t.Errorf("no events should have been published, got started=%d completed=%d", pub.started, pub.completed)
	}
}

// An unknown field is refused rather than discarded. document_reference is
// optional, so a misspelling would otherwise conclude a check whose evidence
// referenced no document, indistinguishable from a caller who sent none.
func TestCreateCheck_UnknownFieldRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doRaw(r, http.MethodPost, "/v1/vendor-checks/", `{
		"counterparty_id":"`+testCP+`","legal_entity_id":"`+testEntity+`","vendor_name":"Acme",
		"correlation_id":"corr-x","documnet_reference":"doc-1"
	}`, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "unknown_field" {
		t.Errorf("expected error=unknown_field got %q", got)
	}
}

func TestCreateCheck_OversizedBodyRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doRaw(r, http.MethodPost, "/v1/vendor-checks/",
		`{"vendor_name":"`+strings.Repeat("A", 70<<10)+`"}`, "principal-1")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized body, got %d", rr.Code)
	}
	if got := errorBody(t, rr)["error"]; got != "request_too_large" {
		t.Errorf("expected error=request_too_large got %q", got)
	}
}

func TestCreateCheck_MalformedJSON(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doRaw(r, http.MethodPost, "/v1/vendor-checks/", `{"vendor_name":`, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
	if got := errorBody(t, rr)["error"]; got != "invalid_json" {
		t.Errorf("expected error=invalid_json got %q", got)
	}
}

// ── CreateCheck: screening outcomes ───────────────────────────────────────────

func TestCreateCheck_CleanVendor_Allowed(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	authz := &stubAuthZ{}
	r := newRouter(s, pub, authz, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Totally Legitimate Vendor Inc", "corr-clean"), "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeDetail(t, rr)

	if resp.Check.Status != domain.StatusCompleted {
		t.Errorf("expected COMPLETED got %q", resp.Check.Status)
	}
	if resp.Check.RiskOutcome != domain.RiskClear {
		t.Errorf("expected CLEAR got %q", resp.Check.RiskOutcome)
	}
	// The point of putting this on the wire: CLEAR here is a stub-denylist miss,
	// not a sanctions clearance, and a consumer can only know that if the record
	// says which screening ran.
	if resp.Check.ScreeningSource != domain.ScreeningSourceStubDenylist {
		t.Errorf("expected screening_source=%s got %q — without it CLEAR is indistinguishable from a real clearance",
			domain.ScreeningSourceStubDenylist, resp.Check.ScreeningSource)
	}
	if resp.Check.CompletedAt == nil {
		t.Error("a COMPLETED check must carry completed_at")
	}
	if resp.Replayed {
		t.Error("a first submission must not be marked replayed")
	}
	if len(resp.Evidence) != 1 {
		t.Fatalf("expected 1 evidence entry got %d", len(resp.Evidence))
	}
	if resp.Evidence[0].EvidenceType != domain.EvidenceTypeSanctionsScreening {
		t.Errorf("unexpected evidence type %q", resp.Evidence[0].EvidenceType)
	}
	// The response's evidence must actually be in the store. The old code
	// swallowed the evidence write's failure and returned the record anyway.
	if got := len(s.evidence[resp.Check.CheckID]); got != 1 {
		t.Errorf("response reported evidence the store does not hold: store has %d rows", got)
	}
	if pub.started != 1 || pub.completed != 1 || pub.failed != 0 {
		t.Errorf("expected 1 started + 1 completed, got started=%d completed=%d failed=%d",
			pub.started, pub.completed, pub.failed)
	}
	if cp.complianceCalls != 1 {
		t.Errorf("expected 1 compliance-status push got %d", cp.complianceCalls)
	}
	if cp.riskCalls != 0 {
		t.Errorf("expected no risk-category push for a clean vendor, got %d", cp.riskCalls)
	}
	if len(authz.actions) != 1 || authz.actions[0] != "VENDOR_DD_INITIATE" {
		t.Errorf("expected one VENDOR_DD_INITIATE check, got %v", authz.actions)
	}
}

func TestCreateCheck_DenylistedVendor_Flagged(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Acme Sanctioned Holdings", "corr-flagged"), "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeDetail(t, rr)

	if resp.Check.RiskOutcome != domain.RiskFlagged {
		t.Errorf("expected FLAGGED got %q", resp.Check.RiskOutcome)
	}
	if cp.riskCalls != 1 {
		t.Errorf("expected a risk-category push for a flagged vendor, got %d", cp.riskCalls)
	}
}

// The list is an exact match, so whitespace variation must not defeat it — the
// only screening there is should not be dodgeable with a stray keystroke.
func TestCreateCheck_DenylistMatchIgnoresWhitespaceVariation(t *testing.T) {
	for _, name := range []string{
		"  Restricted Trading Corp  ",
		"Restricted  Trading   Corp",
		"restricted trading corp",
		"RESTRICTED TRADING CORP",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
			rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody(name, "corr-"+name), "principal-1")
			if rr.Code != http.StatusCreated {
				t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
			}
			if got := decodeDetail(t, rr).Check.RiskOutcome; got != domain.RiskFlagged {
				t.Errorf("%q must match the denylist, got risk_outcome=%q", name, got)
			}
		})
	}
}

func TestCreateCheck_DocumentReferenceRecordedOnEvidence(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	body := validBody("Documented Vendor Ltd", "corr-doc")
	body["document_reference"] = "vault-doc-9f2c"

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", body, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeDetail(t, rr)
	if len(resp.Evidence) != 1 || resp.Evidence[0].DocumentReference != "vault-doc-9f2c" {
		t.Errorf("document_reference did not reach the evidence row: %+v", resp.Evidence)
	}
}

func TestCreateCheck_CounterpartyPushFails_StillReturnsResult(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{failCompliance: true}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Another Clean Vendor", "corr-cp-down"), "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("counterparty-management-svc being unreachable must not fail the due diligence result, got %d: %s", rr.Code, rr.Body.String())
	}
	if decodeDetail(t, rr).Check.Status != domain.StatusCompleted {
		t.Error("expected COMPLETED")
	}
}

// ── CreateCheck: the conclusion failure path ──────────────────────────────────

// The central fix. Recording the outcome and its evidence is one transaction, so
// a failure leaves NO conclusion — and the response must not report one. Before,
// the evidence write's error was logged and swallowed, the completion ran anyway,
// and the response carried an evidence record the store did not hold.
func TestCreateCheck_ConclusionFails_MarksFailedAndReportsNoResult(t *testing.T) {
	s := newStubStore()
	s.concludeErr = errors.New("deadlock detected")
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(s, pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Unlucky Vendor Ltd", "corr-conclude-fail"), "principal-1")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the conclusion cannot be recorded, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "store_unavailable" {
		t.Errorf("expected error=store_unavailable got %q", got)
	}

	// FAILED is now reachable, where before the row was abandoned in STARTED.
	if s.markFailedCall != 1 {
		t.Errorf("expected the check to be marked FAILED once, got %d calls", s.markFailedCall)
	}
	for _, c := range s.checks {
		if c.Status != domain.StatusFailed {
			t.Errorf("expected the check to be FAILED, got %q", c.Status)
		}
		if c.RiskOutcome != "" {
			t.Errorf("a FAILED check must carry no risk outcome, got %q", c.RiskOutcome)
		}
	}

	// vendor.dd.failed is declared in the spec and had no code path that could
	// emit it. Nothing downstream could learn a screening had been lost.
	if pub.failed != 1 {
		t.Errorf("expected 1 vendor.dd.failed event, got %d", pub.failed)
	}
	if pub.completed != 0 {
		t.Errorf("a failed conclusion must not publish vendor.dd.completed, got %d", pub.completed)
	}
	if pub.lastFailReason == "" {
		t.Error("the failed event must carry a reason")
	}

	// The counterparty record must not be enriched from a conclusion that does
	// not exist — pushing VERIFIED here would mark a vendor verified on the
	// strength of a screening whose result was lost.
	if cp.complianceCalls != 0 {
		t.Errorf("expected no counterparty push for a failed check, got %d", cp.complianceCalls)
	}
	if len(s.evidence) != 0 {
		t.Errorf("expected no evidence recorded, got %v", s.evidence)
	}
}

// When marking FAILED also fails, the row stays STARTED — but the failed event is
// still published, because it is then the only trace the attempt happened.
func TestCreateCheck_ConclusionAndMarkFailedBothFail_StillPublishesFailure(t *testing.T) {
	s := newStubStore()
	s.concludeErr = errors.New("connection refused")
	s.markFailedErr = errors.New("connection refused")
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Doubly Unlucky Ltd", "corr-both-fail"), "principal-1")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d", rr.Code)
	}
	if pub.failed != 1 {
		t.Errorf("expected the failure to still be published, got %d", pub.failed)
	}
}

// A concurrent conclusion is not a failure of the check: the outcome stands, and
// marking it FAILED would destroy a valid result.
func TestCreateCheck_AlreadyConcluded_IsConflictNotFailure(t *testing.T) {
	s := newStubStore()
	s.concludeErr = domain.ErrCheckAlreadyConcluded
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Raced Vendor Ltd", "corr-race"), "principal-1")

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "check_already_concluded" {
		t.Errorf("expected error=check_already_concluded got %q", got)
	}
	if s.markFailedCall != 0 {
		t.Error("a check concluded by another request must not be marked FAILED")
	}
	if pub.failed != 0 {
		t.Errorf("expected no failed event for a race, got %d", pub.failed)
	}
}

// ── CreateCheck: idempotency ──────────────────────────────────────────────────

func TestCreateCheck_IdempotentReplay(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	body := validBody("Repeat Vendor", "corr-retry")

	rr1 := doReq(r, http.MethodPost, "/v1/vendor-checks/", body, "principal-1")
	resp1 := decodeDetail(t, rr1)

	rr2 := doReq(r, http.MethodPost, "/v1/vendor-checks/", body, "principal-1")
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200 (replay) got %d: %s", rr2.Code, rr2.Body.String())
	}
	resp2 := decodeDetail(t, rr2)

	if resp2.Check.CheckID != resp1.Check.CheckID {
		t.Fatalf("retried check resolved to a different check_id (%s) than the original (%s)",
			resp2.Check.CheckID, resp1.Check.CheckID)
	}
	if !resp2.Replayed {
		t.Error("a replay must say so on the body, not only in the status code")
	}
	// The stored outcome, not a freshly-built STARTED struct.
	if resp2.Check.Status != domain.StatusCompleted || resp2.Check.RiskOutcome != domain.RiskClear {
		t.Errorf("replay must return the stored conclusion, got status=%q outcome=%q",
			resp2.Check.Status, resp2.Check.RiskOutcome)
	}
	if len(resp2.Evidence) != 1 {
		t.Errorf("replay must return the stored evidence, got %d rows", len(resp2.Evidence))
	}
	// Retry must not re-run screening or re-notify counterparty-management-svc.
	if pub.started != 1 || pub.completed != 1 {
		t.Errorf("expected exactly 1 started + 1 completed event across both requests, got started=%d completed=%d",
			pub.started, pub.completed)
	}
	if cp.complianceCalls != 1 {
		t.Errorf("expected exactly 1 compliance push across both requests, got %d", cp.complianceCalls)
	}
}

// A replay can resolve to a check an earlier attempt abandoned in STARTED. It
// answers 200 and carries no risk outcome, so it must be marked replayed and read
// as unconcluded rather than as a fresh clean answer.
func TestCreateCheck_ReplayOfStalledCheck_ReportsUnconcluded(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	// Seed a check left in STARTED, as a lost conclusion would.
	s.checks["stalled-id"] = &domain.VendorDDCheck{
		CheckID:       "stalled-id",
		TenantID:      testTenant,
		LegalEntityID: testEntity,
		VendorName:    "Stalled Vendor Ltd",
		Status:        domain.StatusStarted,
		CorrelationID: "corr-stalled",
	}
	s.byCorr["corr-stalled"] = "stalled-id"

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/",
		validBody("Stalled Vendor Ltd", "corr-stalled"), "principal-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeDetail(t, rr)
	if !resp.Replayed {
		t.Error("expected replayed=true")
	}
	if resp.Check.Status != domain.StatusStarted {
		t.Errorf("expected the stalled STARTED status to be reported, got %q", resp.Check.Status)
	}
	if resp.Check.Concluded() {
		t.Error("a STARTED check must not report as concluded")
	}
	if resp.Check.RiskOutcome != "" {
		t.Errorf("a STARTED check must carry no risk outcome, got %q", resp.Check.RiskOutcome)
	}
}

// ── GetCheck ───────────────────────────────────────────────────────────────────

func TestGetCheck_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/does-not-exist", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
	errorBody(t, rr)
}

func TestGetCheck_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/some-id", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestGetCheck_AuthorizedAgainstTheRecordsEntity(t *testing.T) {
	s := newStubStore()
	authz := &stubAuthZ{}
	r := newRouter(s, &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Readable Vendor", "corr-read"), "principal-1")
	id := decodeDetail(t, rr).Check.CheckID

	authz.scopes = nil
	authz.actions = nil
	rr = doReq(r, http.MethodGet, "/v1/vendor-checks/"+id, nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(authz.actions) != 1 || authz.actions[0] != "VENDOR_DD_VIEW" {
		t.Errorf("expected one VENDOR_DD_VIEW check, got %v", authz.actions)
	}
	if len(authz.scopes) != 1 || authz.scopes[0] != testEntity {
		t.Errorf("expected the check's own legal entity as the scope, got %v", authz.scopes)
	}
	if resp := decodeDetail(t, rr); len(resp.Evidence) != 1 {
		t.Errorf("expected the stored evidence, got %d rows", len(resp.Evidence))
	}
}

func TestGetCheck_AuthzDenied(t *testing.T) {
	s := newStubStore()
	authz := &stubAuthZ{}
	r := newRouter(s, &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", validBody("Hidden Vendor", "corr-hidden"), "principal-1")
	id := decodeDetail(t, rr).Check.CheckID

	authz.err = domain.ErrAuthorizationDenied
	rr = doReq(r, http.MethodGet, "/v1/vendor-checks/"+id, nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

// ── ListChecks ─────────────────────────────────────────────────────────────────

func TestListChecks_EmptyIsEmptyArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if rr.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", rr.Body.String())
	}
}

// The fail-open read. Omitting legal_entity_id used to skip the authorization
// call entirely, so any caller with a tenant header could list the tenant's whole
// screening history — including which vendors were flagged.
func TestListChecks_UnscopedListIsStillAuthorized(t *testing.T) {
	authz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	r := newRouter(newStubStore(), &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("an unfiltered list must still be authorized, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(authz.scopes) != 1 {
		t.Fatalf("expected exactly one authorization call, got %d", len(authz.scopes))
	}
	if authz.scopes[0] != testTenant {
		t.Errorf("expected the tenant as the fallback scope, got %q", authz.scopes[0])
	}
	if authz.actions[0] != "VENDOR_DD_VIEW" {
		t.Errorf("expected VENDOR_DD_VIEW, got %q", authz.actions[0])
	}
}

func TestListChecks_FilteredListAuthorizesAgainstTheEntity(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouter(newStubStore(), &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/?legal_entity_id="+otherEntiy, nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if len(authz.scopes) != 1 || authz.scopes[0] != otherEntiy {
		t.Errorf("expected the named entity as the scope, got %v", authz.scopes)
	}
}

// A malformed entity filter is 400, not 503.
//
// It is the authorization SCOPE that must be a UUID — not this table's columns,
// which are VARCHAR. authorization-svc stores legal_entity_id in a uuid column and
// answers 503 store_unavailable for a malformed one, and from here that is
// indistinguishable from authorization-svc being down, so a mistyped filter was
// reported as the authorization plane having failed. Checked on this side instead.
func TestListChecks_NonUUIDEntityFilterIsBadRequestNotOutage(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouter(newStubStore(), &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/?legal_entity_id=not-a-uuid", nil, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "invalid_scope" {
		t.Errorf("expected error=invalid_scope got %q", got)
	}
	if len(authz.scopes) != 0 {
		t.Errorf("must not ask authorization-svc about a scope that cannot be one, got %v", authz.scopes)
	}
}

func TestCreateCheck_NonUUIDEntityIsBadRequestNotOutage(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouter(newStubStore(), &stubPublisher{}, authz, &stubCounterparty{})

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": testCP,
		"legal_entity_id": "not-a-uuid",
		"vendor_name":     "Acme Corp",
		"correlation_id":  "corr-badscope",
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
	if got := errorBody(t, rr)["error"]; got != "invalid_scope" {
		t.Errorf("expected error=invalid_scope got %q", got)
	}
	if len(authz.scopes) != 0 {
		t.Errorf("must not call authorization-svc with an unusable scope, got %v", authz.scopes)
	}
}

// No entity and no tenant: nothing to authorize against, so fail closed rather
// than authorize against an empty scope.
func TestListChecks_NoEntityAndNoTenant_FailsClosed(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouterWithTenant(newStubStore(), &stubPublisher{}, authz, &stubCounterparty{}, "")

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(authz.scopes) != 0 {
		t.Errorf("must not authorize against an empty scope, got %v", authz.scopes)
	}
}

func TestListChecks_DefaultLimitApplied(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	if rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1"); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if s.lastListFilter.Limit != 50 {
		t.Errorf("expected a default limit of 50, got %d — an unbounded list returns the whole history",
			s.lastListFilter.Limit)
	}
	if s.lastListFilter.Offset != 0 {
		t.Errorf("expected offset 0, got %d", s.lastListFilter.Offset)
	}
}

func TestListChecks_PaginationPassedThrough(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	if rr := doReq(r, http.MethodGet, "/v1/vendor-checks/?limit=10&offset=20", nil, "principal-1"); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if s.lastListFilter.Limit != 10 || s.lastListFilter.Offset != 20 {
		t.Errorf("expected limit=10 offset=20, got %+v", s.lastListFilter)
	}
}

// strconv.Atoi's error was discarded platform-wide: limit=abc silently became the
// default and offset=-1 reached Postgres and answered 503.
func TestListChecks_InvalidPaginationRejected(t *testing.T) {
	cases := []struct{ query, wantCode string }{
		{"limit=abc", "invalid_limit"},
		{"limit=0", "invalid_limit"},
		{"limit=500", "invalid_limit"},
		{"offset=-1", "invalid_offset"},
		{"offset=xyz", "invalid_offset"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
			rr := doReq(r, http.MethodGet, "/v1/vendor-checks/?"+tc.query, nil, "principal-1")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d: %s", tc.query, rr.Code, rr.Body.String())
			}
			if got := errorBody(t, rr)["error"]; got != tc.wantCode {
				t.Errorf("expected error=%s got %q", tc.wantCode, got)
			}
		})
	}
}

// A malformed COUNTERPARTY filter, unlike a malformed entity, is not an error: it
// is never used as an authorization scope, and counterparty_id is VARCHAR(255) in
// this schema rather than a uuid column, so the comparison is valid and matches
// nothing. Rejecting it would claim a validation neither the schema nor
// authorization-svc performs.
func TestListChecks_NonUUIDCounterpartyFilterMatchesNothing(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/?counterparty_id=not-a-uuid", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "[]\n" {
		t.Errorf("expected an empty register, got %q", rr.Body.String())
	}
}

func TestListChecks_StoreDownDoesNotEchoDriverText(t *testing.T) {
	s := newStubStore()
	s.listChecksErr = errors.New(`ERROR: relation "vendor_dd_checks" does not exist (SQLSTATE 42P01)`)
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "SQLSTATE") {
		t.Errorf("raw driver text reached the client: %s", rr.Body.String())
	}
}

func TestListChecks_FiltersComposeWithAnd(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})

	if rr := doReq(r, http.MethodGet,
		"/v1/vendor-checks/?legal_entity_id="+testEntity+"&counterparty_id="+testCP, nil,
		"principal-1"); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if s.lastListFilter.LegalEntityID != testEntity || s.lastListFilter.CounterpartyID != testCP {
		t.Errorf("both filters must reach the store, got %+v", s.lastListFilter)
	}
}
