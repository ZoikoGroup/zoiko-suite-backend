package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	"zoiko.io/purchase-request-svc/internal/handler"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/purchaseorder"
	"zoiko.io/purchase-request-svc/internal/spendcontrols"
	"zoiko.io/purchase-request-svc/internal/store"
)

// tenant_id and legal_entity_id are uuid columns, so the fixtures are UUIDs —
// "t1"/"e1" would be refused by the handler's own identifier checks now that a
// malformed id is a 400 rather than a 503 from the driver.
const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entityA = "33333333-3333-3333-3333-333333333333"
)

func ver(n int) *int { return &n }

// ── stub store ───────────────────────────────────────────────────────────────

// stubStore replicates PgStore's observable semantics for Apply and
// AmendRequest: tenant scoping, expected_version, the legal-from/transition
// tables, the guard, version bumps, and "amendment always returns the
// requisition to DRAFT and invalidates the approval".
type stubStore struct {
	requests      map[string]*domain.PurchaseRequest
	byCorrelation map[string]string
	history       map[string][]domain.HistoryEntry
	events        []string

	createErr error
	getErr    error
	listErr   error
}

func newStubStore() *stubStore {
	return &stubStore{
		requests: map[string]*domain.PurchaseRequest{}, byCorrelation: map[string]string{},
		history: map[string][]domain.HistoryEntry{},
	}
}

func (s *stubStore) seed(r *domain.PurchaseRequest) *domain.PurchaseRequest {
	if r.Version == 0 {
		r.Version = 1
	}
	if r.TenantID == "" {
		r.TenantID = tenantA
	}
	if r.LegalEntityID == "" {
		r.LegalEntityID = entityA
	}
	if r.CurrencyCode == "" {
		r.CurrencyCode = "USD"
	}
	if r.BudgetDecision == "" {
		r.BudgetDecision = domain.BudgetNotChecked
	}
	s.requests[r.RequestID] = r
	return r
}

func (s *stubStore) CreateRequest(ctx context.Context, r *domain.PurchaseRequest) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	key := r.TenantID + "|" + r.CorrelationID
	if r.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[key]; ok {
			*r = *s.requests[existingID]
			return false, nil
		}
		s.byCorrelation[key] = r.RequestID
	}
	r.Status, r.Version, r.BudgetDecision = domain.RequestStatusDraft, 1, domain.BudgetNotChecked
	r.RecomputeAmount()
	s.requests[r.RequestID] = r
	s.events = append(s.events, "PurchaseRequisitionCreated")
	return true, nil
}

// GetRequest is tenant-scoped like the real store: another tenant's request is
// indistinguishable from an absent one.
func (s *stubStore) GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	r, ok := s.requests[requestID]
	if !ok || r.TenantID != svcmiddleware.TenantFromContext(ctx) {
		return nil, nil
	}
	c := *r
	return &c, nil
}

func (s *stubStore) ListRequests(_ context.Context, f domain.ListRequestsFilter) ([]domain.PurchaseRequest, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.PurchaseRequest
	for _, r := range s.requests {
		if r.TenantID == f.TenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) GetHistory(_ context.Context, requestID string) ([]domain.HistoryEntry, error) {
	return s.history[requestID], nil
}

func (s *stubStore) record(r *domain.PurchaseRequest, action string, from domain.RequestStatus, actor, reason string) {
	f := string(from)
	s.history[r.RequestID] = append(s.history[r.RequestID], domain.HistoryEntry{
		RequestID: r.RequestID, Version: r.Version, Action: action, FromStatus: &f, ToStatus: string(r.Status),
		Actor: actor, Reason: reason, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) Apply(_ context.Context, t store.Transition) (*domain.PurchaseRequest, error) {
	cur, ok := s.requests[t.RequestID]
	if !ok || cur.TenantID != t.TenantID {
		return nil, domain.ErrRequestNotFound
	}
	if t.ExpectedVersion != nil && cur.Version != *t.ExpectedVersion {
		return nil, domain.ErrStaleVersion
	}
	legal := false
	for _, f := range t.From {
		legal = legal || cur.Status == f
	}
	if !legal || !domain.CanTransition(cur.Status, t.To) {
		return nil, domain.ErrInvalidTransition
	}
	if t.Guard != nil {
		if err := t.Guard(cur); err != nil {
			return nil, err
		}
	}
	from, actor, now := cur.Status, t.Meta.Actor, time.Now().UTC()
	cur.Status = t.To
	cur.Version++
	if t.Budget != nil {
		cur.BudgetDecision, cur.BudgetBasis = t.Budget.Decision, t.Budget.Basis
	}
	switch t.To {
	case domain.RequestStatusPending:
		cur.SubmittedByPrincipalID = &actor
	case domain.RequestStatusApproved:
		cur.ApprovedByPrincipalID, cur.ApprovedAt = &actor, &now
	case domain.RequestStatusRejected:
		reason := t.Meta.Reason
		cur.RejectedByPrincipalID, cur.RejectedAt, cur.RejectionReason = &actor, &now, &reason
	case domain.RequestStatusCancelled:
		reason := t.Meta.Reason
		cur.CancelledByPrincipalID, cur.CancelledAt, cur.CancellationReason = &actor, &now, &reason
	case domain.RequestStatusConverted:
		po := t.PurchaseOrderID
		cur.ConvertedPurchaseOrderID, cur.ConvertedByPrincipalID, cur.ConvertedAt = &po, &actor, &now
	}
	for _, ev := range t.Events {
		s.events = append(s.events, ev.Type)
	}
	s.record(cur, t.Action, from, actor, t.Meta.Reason)
	c := *cur
	return &c, nil
}

func (s *stubStore) AmendRequest(_ context.Context, a store.Amendment) (*domain.PurchaseRequest, bool, error) {
	cur, ok := s.requests[a.RequestID]
	if !ok || cur.TenantID != a.TenantID {
		return nil, false, domain.ErrRequestNotFound
	}
	if a.ExpectedVersion != nil && cur.Version != *a.ExpectedVersion {
		return nil, false, domain.ErrStaleVersion
	}
	switch cur.Status {
	case domain.RequestStatusDraft, domain.RequestStatusPending, domain.RequestStatusApproved:
	default:
		return nil, false, domain.ErrInvalidTransition
	}
	from := cur.Status
	work := *cur
	work.Lines = append([]domain.RequestLine(nil), cur.Lines...)
	if err := a.Apply(&work); err != nil {
		return nil, false, err
	}
	work.RecomputeAmount()
	invalidated := from == domain.RequestStatusApproved
	actor := a.Meta.Actor
	work.Status, work.Version = domain.RequestStatusDraft, cur.Version+1
	work.BudgetDecision, work.BudgetBasis = domain.BudgetNotChecked, ""
	work.ApprovedByPrincipalID, work.ApprovedAt, work.SubmittedByPrincipalID = nil, nil, nil
	work.LastAmendedByPrincipalID = &actor
	if invalidated {
		work.ApprovalInvalidatedCount++
	}
	*cur = work
	s.events = append(s.events, "PurchaseRequisitionAmended")
	s.record(cur, "AMENDED", from, actor, a.Meta.Reason)
	c := *cur
	return &c, invalidated, nil
}

// ── other stubs ──────────────────────────────────────────────────────────────

// stubAuthZ models authorization-svc: a blanket error for every check, and —
// unless skipOwnObject — its own-object SoD layer, which denies a principal
// acting on an object they own.
type stubAuthZ struct {
	err           error
	skipOwnObject bool
	actions       []string
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	a.actions = append(a.actions, action)
	return a.err
}

func (a *stubAuthZ) CheckAllowedOwnObject(_ context.Context, principal, _, action, owner string) error {
	a.actions = append(a.actions, action)
	if a.err != nil {
		return a.err
	}
	if !a.skipOwnObject && principal == owner {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type stubSpend struct {
	outcome string
	err     error
	calls   []spendcontrols.CheckInput
}

func (s *stubSpend) Check(_ context.Context, in spendcontrols.CheckInput) (*spendcontrols.Result, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return nil, s.err
	}
	return &spendcontrols.Result{Outcome: s.outcome, Basis: "stub basis"}, nil
}

type stubPO struct {
	calls   []purchaseorder.DraftInput
	err     error
	created purchaseorder.Created
}

func (p *stubPO) CreateDraft(_ context.Context, in purchaseorder.DraftInput) (*purchaseorder.Created, error) {
	p.calls = append(p.calls, in)
	if p.err != nil {
		return nil, p.err
	}
	c := p.created
	if c.PurchaseOrderID == "" {
		c = purchaseorder.Created{PurchaseOrderID: "44444444-4444-4444-4444-444444444444", PONumber: "PO-1", Status: "DRAFT"}
	}
	return &c, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

type env struct {
	store *stubStore
	authz *stubAuthZ
	spend *stubSpend
	po    *stubPO
	cfg   handler.Config
}

func newEnv() *env {
	return &env{store: newStubStore(), authz: &stubAuthZ{}, spend: &stubSpend{outcome: "ALLOWED"}, po: &stubPO{}}
}

// router mounts TenantContext, which the real server mounts in
// cmd/server/main.go. A handler harness must mount the middleware the handler
// depends on, or every handler under test sees an empty tenant scope.
func (e *env) router() chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	var spend spendcontrols.Client = e.spend
	h := handler.New(e.store, e.authz, spend, e.po, e.cfg, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// doRequest sends a request in tenantA's scope, which is the ordinary case.
func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, tenantA)
}

// doRequestAs sends a request in an explicit tenant scope; tenantID "" omits the
// X-Tenant-Id header entirely, which is how a request with no verified scope is
// simulated.
func doRequestAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e
}

func decodePR(t *testing.T, rec *httptest.ResponseRecorder) domain.PurchaseRequest {
	t.Helper()
	var got domain.PurchaseRequest
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v (%s)", err, rec.Body.String())
	}
	return got
}

const base = "/v1/purchase-requests/"

func pending(id, requester string) *domain.PurchaseRequest {
	return &domain.PurchaseRequest{
		RequestID: id, RequestedByPrincipalID: requester, Status: domain.RequestStatusPending,
		Description: "laptops", BudgetDecision: domain.BudgetNotRequired,
		Lines:  []domain.RequestLine{{LineNumber: 1, Description: "laptops", Category: "GENERAL", Quantity: 1, Amount: 100}},
		Amount: 100,
	}
}

func withStatus(r *domain.PurchaseRequest, s domain.RequestStatus) *domain.PurchaseRequest {
	r.Status = s
	return r
}

// ── CreateRequest ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateRequestRequest {
	return domain.CreateRequestRequest{
		TenantID:      tenantA,
		LegalEntityID: entityA,
		Description:   "50 laptops",
		Amount:        50000,
		CurrencyCode:  "USD",
		CorrelationID: "corr-1",
	}
}

func TestCreateRequest_Success(t *testing.T) {
	e := newEnv()
	rec := doRequest(e.router(), http.MethodPost, base, validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePR(t, rec)
	if got.Status != domain.RequestStatusDraft || got.Amount != 50000 || len(got.Lines) != 1 {
		t.Fatalf("a header-only create must become a DRAFT with one GENERAL line summing to the amount, got %+v", got)
	}
	if got.RequestedByPrincipalID != "principal-1" {
		t.Fatalf("requester must be the verified principal, got %q", got.RequestedByPrincipalID)
	}
	if len(e.authz.actions) != 1 || e.authz.actions[0] != "PR_REQUEST_CREATE" {
		t.Fatalf("expected a PR_REQUEST_CREATE check, got %v", e.authz.actions)
	}
}

func TestCreateRequest_MissingCorrelationID_Rejected(t *testing.T) {
	r := newEnv().router()
	req := validCreateReq()
	req.CorrelationID = ""
	if rec := doRequest(r, http.MethodPost, base, req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d", rec.Code)
	}
}

func TestCreateRequest_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	e := newEnv()
	r := e.router()
	req := validCreateReq()

	first := doRequest(r, http.MethodPost, base, req, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	firstPR := decodePR(t, first)

	retry := doRequest(r, http.MethodPost, base, req, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call with the same correlation_id, got %d: %s", retry.Code, retry.Body.String())
	}
	if retryPR := decodePR(t, retry); retryPR.RequestID != firstPR.RequestID {
		t.Fatalf("retried call resolved to a different request_id (%s) than the original (%s)", retryPR.RequestID, firstPR.RequestID)
	}
	if len(e.store.requests) != 1 {
		t.Fatalf("a replay must not create a second requisition, got %d", len(e.store.requests))
	}
}

func TestCreateRequest_MissingPrincipalHeader_Returns401(t *testing.T) {
	if rec := doRequest(newEnv().router(), http.MethodPost, base, validCreateReq(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestCreateRequest_AuthorizationDenied_Returns403(t *testing.T) {
	e := newEnv()
	e.authz.err = domain.ErrAuthorizationDenied
	rec := doRequest(e.router(), http.MethodPost, base, validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden || errCode(t, rec).Code != "FORBIDDEN" {
		t.Fatalf("expected 403 FORBIDDEN when authorization-svc denies, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.requests) != 0 {
		t.Fatal("a denied create must write nothing")
	}
}

func TestCreateRequest_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	e := newEnv()
	e.authz.err = domain.ErrAuthorizationServiceUnavailable
	if rec := doRequest(e.router(), http.MethodPost, base, validCreateReq(), "principal-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestCreateRequest_ZeroAmount_Rejected(t *testing.T) {
	req := validCreateReq()
	req.Amount = 0
	if rec := doRequest(newEnv().router(), http.MethodPost, base, req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount request, got %d", rec.Code)
	}
}

func TestCreateRequest_WithLines_AmountIsSumOfLines(t *testing.T) {
	req := validCreateReq()
	req.Amount = 0
	req.Lines = []domain.RequestLineInput{
		{Description: "a", Category: "GENERAL", Quantity: 2, Amount: 30.10},
		{Description: "b", Category: "GENERAL", Quantity: 1, Amount: 20.20},
	}
	rec := doRequest(newEnv().router(), http.MethodPost, base, req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodePR(t, rec); got.Amount != 50.30 || len(got.Lines) != 2 {
		t.Fatalf("expected amount 50.30 over 2 lines, got %v / %d", got.Amount, len(got.Lines))
	}
}

// ── Submit (budget / policy) ─────────────────────────────────────────────────

func draftWith(id, requester, category string, amount float64) *domain.PurchaseRequest {
	return &domain.PurchaseRequest{
		RequestID: id, RequestedByPrincipalID: requester, Status: domain.RequestStatusDraft, Description: "x",
		Lines:  []domain.RequestLine{{LineNumber: 1, Description: "x", Category: category, Quantity: 1, Amount: amount}},
		Amount: amount,
	}
}

func TestSubmit_UncontrolledCategory_NoBudgetCallNeeded(t *testing.T) {
	e := newEnv()
	e.store.seed(draftWith("r1", "maker", "GENERAL", 100))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/submit", domain.VersionedRequest{ExpectedVersion: ver(1)}, "maker")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePR(t, rec)
	if got.Status != domain.RequestStatusPending || got.BudgetDecision != domain.BudgetNotRequired || got.Version != 2 {
		t.Fatalf("expected PENDING_APPROVAL / NOT_REQUIRED / v2, got %+v", got)
	}
	if len(e.spend.calls) != 0 {
		t.Fatalf("an uncontrolled category must not consume budget, got %d checks", len(e.spend.calls))
	}
}

// Spec negative path 3: a budget check must not be bypassable for a controlled
// category — not by an unreachable service, not by a missing one, not by a
// BLOCKED answer.
func TestSubmit_ControlledCategory_BlockedBudget_Refused(t *testing.T) {
	e := newEnv()
	e.cfg.ControlledCategories = []string{"CAPEX"}
	e.spend.outcome = "BLOCKED"
	e.store.seed(draftWith("r1", "maker", "CAPEX", 5000))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/submit", nil, "maker")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Code != "BUDGET_BLOCKED" {
		t.Fatalf("expected 422 BUDGET_BLOCKED, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusDraft {
		t.Fatal("a refused submission must leave the requisition a DRAFT")
	}
}

func TestSubmit_ControlledCategory_BudgetServiceDown_FailsClosed(t *testing.T) {
	e := newEnv()
	e.cfg.ControlledCategories = []string{"CAPEX"}
	e.spend.err = spendcontrols.ErrUnavailable
	e.store.seed(draftWith("r1", "maker", "CAPEX", 5000))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/submit", nil, "maker")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec).Code != "DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("expected 503 DEPENDENCY_UNAVAILABLE, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusDraft {
		t.Fatal("an unverifiable budget must not let a controlled requisition through")
	}
}

func TestSubmit_ControlledCategory_NoBudgetServiceConfigured_FailsClosed(t *testing.T) {
	e := newEnv()
	e.cfg.ControlledCategories = []string{"*"}
	e.store.seed(draftWith("r1", "maker", "CAPEX", 5000))
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(e.store, e.authz, nil, e.po, e.cfg, zap.NewNop()))
	if rec := doRequest(r, http.MethodPost, base+"r1/submit", nil, "maker"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no budget service, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSubmit_ControlledCategory_Allowed_RecordsDecisionAndIdempotentCorrelation(t *testing.T) {
	e := newEnv()
	e.cfg.ControlledCategories = []string{"capex"} // case-insensitive
	e.store.seed(draftWith("r1", "maker", "CAPEX", 5000))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/submit", nil, "maker")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodePR(t, rec); got.BudgetDecision != domain.BudgetAllowed {
		t.Fatalf("expected the ALLOWED decision recorded, got %s", got.BudgetDecision)
	}
	if len(e.spend.calls) != 1 || e.spend.calls[0].CorrelationID != "pr:r1:v1:CAPEX" || e.spend.calls[0].Amount != 5000 {
		t.Fatalf("budget check must be keyed by (requisition, version, category), got %+v", e.spend.calls)
	}
}

func TestSubmit_NotDraft_Refused_And_StaleVersion_Conflicts(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "maker"))
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/submit", nil, "maker"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 submitting a non-DRAFT requisition, got %d", rec.Code)
	}
	e.store.seed(draftWith("r2", "maker", "GENERAL", 10))
	rec := doRequest(e.router(), http.MethodPost, base+"r2/submit", domain.VersionedRequest{ExpectedVersion: ver(7)}, "maker")
	if rec.Code != http.StatusConflict || errCode(t, rec).Code != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── ApproveRequest / RejectRequest ───────────────────────────────────────────

func TestApproveRequest_FromPending_Succeeds(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePR(t, rec)
	// The response must echo who decided and when — a 200 that says APPROVED
	// without the approver would be a record-shaped lie about what was written.
	if got.Status != domain.RequestStatusApproved || got.ApprovedByPrincipalID == nil || *got.ApprovedByPrincipalID != "approver" || got.ApprovedAt == nil {
		t.Fatalf("expected APPROVED with approver and timestamp, got %+v", got)
	}
	if got.Version != 2 {
		t.Fatalf("approval must bump the version, got %d", got.Version)
	}
	seen := map[string]bool{}
	for _, ev := range e.store.events {
		seen[ev] = true
	}
	if !seen["PurchaseRequisitionApproved"] || !seen["purchase.request.approved"] {
		t.Fatalf("expected the spec event and its legacy alias, got %v", e.store.events)
	}
	if len(e.store.history["r1"]) != 1 || e.store.history["r1"][0].Action != "APPROVED" {
		t.Fatalf("expected an APPROVED history row, got %+v", e.store.history["r1"])
	}
}

func TestApproveRequest_RequiresExpectedVersion(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", nil, "approver")
	if rec.Code != http.StatusBadRequest || errCode(t, rec).Error != "expected_version_required" {
		t.Fatalf("expected 400 expected_version_required, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatal("nothing may change without an expected_version")
	}
}

func TestApproveRequest_ExpectedVersionFromEnvelopeHeader(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	req := httptest.NewRequest(http.MethodPost, base+"r1/approve", nil)
	req.Header.Set("X-Principal-Id", "approver")
	req.Header.Set("X-Tenant-Id", tenantA)
	req.Header.Set("X-Expected-Version", "1")
	rec := httptest.NewRecorder()
	e.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Expected-Version must satisfy the requirement, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestApproveRequest_StaleVersion_Conflicts(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(9)}, "approver")
	if rec.Code != http.StatusConflict || errCode(t, rec).Code != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestApproveRequest_AlreadyApproved_Rejected(t *testing.T) {
	// Both fork branches are terminal for the decision — approving an
	// already-APPROVED request must be rejected, not silently re-approved.
	e := newEnv()
	e.store.seed(withStatus(pending("r1", "requester"), domain.RequestStatusApproved))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Code != "INVALID_TRANSITION" {
		t.Fatalf("expected 422 INVALID_TRANSITION approving an already-APPROVED request, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestApproveRequest_AuthorizationDenied_Returns403(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatal("a denied approval must not change the requisition")
	}
}

func TestApproveRequest_Expired_Refused_And_MovesToExpired(t *testing.T) {
	e := newEnv()
	past := time.Now().Add(-time.Hour)
	r := e.store.seed(pending("r1", "requester"))
	r.ExpiresAt = &past
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusConflict || errCode(t, rec).Code != "REQUISITION_EXPIRED" {
		t.Fatalf("expected 409 REQUISITION_EXPIRED, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusExpired {
		t.Fatalf("an elapsed requisition must be EXPIRED, got %s", e.store.requests["r1"].Status)
	}
}

// Spec negative path 1: a requester self-approves above the threshold. Checked
// with authorization-svc's own-object layer out of the picture, so the handler's
// own rule is what is being proven.
func TestApproveRequest_RequesterSelfApprovesAboveThreshold_Refused(t *testing.T) {
	e := newEnv()
	e.cfg = handler.Config{ApprovalThreshold: 1000, MakerChecker: false}
	e.authz.skipOwnObject = true
	r := pending("r1", "requester")
	r.Amount = 50000
	e.store.seed(r)
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "requester")
	if rec.Code != http.StatusForbidden || errCode(t, rec).Code != "SOD_CONFLICT" {
		t.Fatalf("expected 403 SOD_CONFLICT, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatal("a refused self-approval must leave the requisition PENDING_APPROVAL")
	}
}

func TestApproveRequest_SelfApprovalBelowThreshold_AllowedOnlyWithoutMakerChecker(t *testing.T) {
	e := newEnv()
	e.cfg = handler.Config{ApprovalThreshold: 1000, MakerChecker: false}
	e.authz.skipOwnObject = true
	e.store.seed(pending("r1", "requester")) // amount 100, below the threshold
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "requester"); rec.Code != http.StatusOK {
		t.Fatalf("maker-checker off and below threshold: expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	e2 := newEnv()
	e2.cfg = handler.Config{ApprovalThreshold: 1000, MakerChecker: true}
	e2.authz.skipOwnObject = true
	e2.store.seed(pending("r1", "requester"))
	if rec := doRequest(e2.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "requester"); rec.Code != http.StatusForbidden {
		t.Fatalf("maker-checker on: expected 403 at any amount, got %d", rec.Code)
	}
}

func TestApproveRequest_AboveThreshold_SubmitterAndLastAmenderAreNotIndependent(t *testing.T) {
	for _, who := range []string{"submitter", "amender"} {
		e := newEnv()
		e.cfg = handler.Config{ApprovalThreshold: 1000, MakerChecker: true}
		e.authz.skipOwnObject = true
		r := pending("r1", "requester")
		r.Amount = 50000
		s, a := "submitter", "amender"
		r.SubmittedByPrincipalID, r.LastAmendedByPrincipalID = &s, &a
		e.store.seed(r)
		rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, who)
		if rec.Code != http.StatusForbidden || errCode(t, rec).Error != "independent_approver_required" {
			t.Fatalf("%s: expected 403 independent_approver_required, got %d %s", who, rec.Code, rec.Body.String())
		}
		if rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "independent"); rec.Code != http.StatusOK {
			t.Fatalf("an independent approver must succeed, got %d %s", rec.Code, rec.Body.String())
		}
	}
}

// Spec negative path 3, approval side: even if a controlled requisition reached
// PENDING_APPROVAL without a recorded ALLOWED decision, approval refuses.
func TestApproveRequest_ControlledCategory_WithoutAllowedDecision_Refused(t *testing.T) {
	e := newEnv()
	e.cfg.ControlledCategories = []string{"CAPEX"}
	r := pending("r1", "requester")
	r.Lines[0].Category = "CAPEX"
	r.BudgetDecision = domain.BudgetNotRequired
	e.store.seed(r)
	rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Code != "BUDGET_CHECK_REQUIRED" {
		t.Fatalf("expected 422 BUDGET_CHECK_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatal("an approval around a bypassed budget check must not happen")
	}

	e.store.requests["r1"].BudgetDecision = domain.BudgetAllowed
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver"); rec.Code != http.StatusOK {
		t.Fatalf("with an ALLOWED decision approval must succeed, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRejectRequest_RequiresReason(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/reject", domain.RejectRequestRequest{ExpectedVersion: ver(1)}, "approver"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a reject with no reason, got %d", rec.Code)
	}
}

func TestRejectRequest_FromPending_Succeeds(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/reject",
		domain.RejectRequestRequest{Reason: "over budget", ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePR(t, rec)
	if got.Status != domain.RequestStatusRejected || got.RejectedByPrincipalID == nil || *got.RejectedByPrincipalID != "approver" ||
		got.RejectedAt == nil || got.RejectionReason == nil || *got.RejectionReason != "over budget" {
		t.Fatalf("expected REJECTED with rejector, time and reason, got %+v", got)
	}
}

// The requester may not reject their own requisition either — SoD applies to
// both decision outcomes.
func TestRejectRequest_SelfRejection_Refused(t *testing.T) {
	e := newEnv()
	e.authz.skipOwnObject = true // prove the handler's own rule, not authorization-svc's
	e.store.seed(pending("r1", "principal-1"))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/reject",
		domain.RejectRequestRequest{Reason: "trying to self-reject", ExpectedVersion: ver(1)}, "principal-1")
	if rec.Code != http.StatusForbidden || errCode(t, rec).Code != "SOD_CONFLICT" {
		t.Fatalf("expected 403 SOD_CONFLICT for self-rejection, got %d: %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatalf("expected status to remain PENDING_APPROVAL, got %s", e.store.requests["r1"].Status)
	}
}

func TestRejectRequest_AlreadyRejected_Rejected(t *testing.T) {
	e := newEnv()
	e.store.seed(withStatus(pending("r1", "requester"), domain.RequestStatusRejected))
	rec := doRequest(e.router(), http.MethodPost, base+"r1/reject",
		domain.RejectRequestRequest{Reason: "trying again", ExpectedVersion: ver(1)}, "approver")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 rejecting an already-REJECTED request, got %d", rec.Code)
	}
}

// ── Amend (material changes invalidate approval) ─────────────────────────────

// Spec negative path 2: the amount of an approved requisition changed before
// conversion. The change is allowed only by dropping the requisition back to
// DRAFT and discarding the approval, after which it cannot convert.
func TestAmendApprovedRequest_InvalidatesApproval_AndBlocksConversion(t *testing.T) {
	e := newEnv()
	r := withStatus(pending("r1", "requester"), domain.RequestStatusApproved)
	a, at := "approver", time.Now()
	r.ApprovedByPrincipalID, r.ApprovedAt = &a, &at
	e.store.seed(r)

	newLines := []domain.RequestLineInput{{Description: "laptops", Category: "GENERAL", Quantity: 1, Amount: 9999}}
	rec := doRequest(e.router(), http.MethodPost, base+"r1/amend",
		domain.AmendRequestRequest{ExpectedVersion: ver(1), Lines: &newLines, Reason: "price went up"}, "requester")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		domain.PurchaseRequest
		ApprovalInvalidated bool `json:"approval_invalidated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.ApprovalInvalidated || got.Status != domain.RequestStatusDraft || got.ApprovedByPrincipalID != nil ||
		got.ApprovalInvalidatedCount != 1 || got.Amount != 9999 || got.BudgetDecision != domain.BudgetNotChecked {
		t.Fatalf("expected a DRAFT with the approval and budget decision discarded and the new amount, got %+v", got)
	}

	rec = doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order",
		domain.ConvertRequestRequest{SupplierRef: "supplier-1"}, "buyer")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Error != "not_convertible" {
		t.Fatalf("an amended (re-DRAFTed) requisition must not convert, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.po.calls) != 0 {
		t.Fatal("no purchase order may be created for an amended requisition")
	}
}

func TestAmendRequest_PendingDropsToDraft_TerminalRefused_StaleConflicts(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("p", "requester"))
	d := "renamed"
	rec := doRequest(e.router(), http.MethodPost, base+"p/amend", domain.AmendRequestRequest{ExpectedVersion: ver(1), Description: &d}, "requester")
	if rec.Code != http.StatusOK || decodePR(t, rec).Status != domain.RequestStatusDraft {
		t.Fatalf("amending a PENDING_APPROVAL requisition must return it to DRAFT, got %d %s", rec.Code, rec.Body.String())
	}

	e.store.seed(withStatus(pending("c", "requester"), domain.RequestStatusCancelled))
	if rec := doRequest(e.router(), http.MethodPost, base+"c/amend", domain.AmendRequestRequest{Description: &d}, "requester"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a terminal requisition must refuse amendment, got %d", rec.Code)
	}

	e.store.seed(draftWith("s", "requester", "GENERAL", 1))
	if rec := doRequest(e.router(), http.MethodPost, base+"s/amend", domain.AmendRequestRequest{ExpectedVersion: ver(5), Description: &d}, "requester"); rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on a stale amend, got %d", rec.Code)
	}

	empty := ""
	e.store.seed(draftWith("e", "requester", "GENERAL", 1))
	if rec := doRequest(e.router(), http.MethodPost, base+"e/amend", domain.AmendRequestRequest{Description: &empty}, "requester"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an amendment must not blank the description, got %d", rec.Code)
	}
}

// ── Cancel / Convert ─────────────────────────────────────────────────────────

func TestCancelRequest_RequiresReason_And_Cancels(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/cancel", domain.CancelRequestRequest{}, "requester"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a reason, got %d", rec.Code)
	}
	rec := doRequest(e.router(), http.MethodPost, base+"r1/cancel", domain.CancelRequestRequest{Reason: "not needed"}, "requester")
	if rec.Code != http.StatusOK || decodePR(t, rec).Status != domain.RequestStatusCancelled {
		t.Fatalf("expected CANCELLED, got %d %s", rec.Code, rec.Body.String())
	}
}

// Spec negative path 4: a cancelled requisition converted to a PO. Every
// non-APPROVED state is refused, and no purchase order is ever requested.
func TestConvert_OnlyApprovedConverts_CancelledAndOthersRefused(t *testing.T) {
	for _, st := range []domain.RequestStatus{
		domain.RequestStatusCancelled, domain.RequestStatusRejected, domain.RequestStatusExpired,
		domain.RequestStatusPending, domain.RequestStatusDraft,
	} {
		e := newEnv()
		e.store.seed(withStatus(pending("r1", "requester"), st))
		rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order",
			domain.ConvertRequestRequest{SupplierRef: "supplier-1"}, "buyer")
		if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Error != "not_convertible" {
			t.Fatalf("%s: expected 422 not_convertible, got %d %s", st, rec.Code, rec.Body.String())
		}
		if len(e.po.calls) != 0 {
			t.Fatalf("%s: a purchase order must never be requested", st)
		}
		if e.store.requests["r1"].Status != st {
			t.Fatalf("%s: a refused conversion must not change state, now %s", st, e.store.requests["r1"].Status)
		}
	}
}

func TestConvert_Approved_CreatesPO_RecordsLink_AndReplaysIdempotently(t *testing.T) {
	e := newEnv()
	r := withStatus(pending("r1", "requester"), domain.RequestStatusApproved)
	r.Lines = []domain.RequestLine{{LineNumber: 1, ItemRef: "SKU", Description: "laptops", Category: "GENERAL", Quantity: 4, Amount: 400, UnitOfMeasure: "EA"}}
	r.Amount = 400
	e.store.seed(r)

	rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order",
		domain.ConvertRequestRequest{SupplierRef: "supplier-1"}, "buyer")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePR(t, rec)
	if got.Status != domain.RequestStatusConverted || got.ConvertedPurchaseOrderID == nil ||
		*got.ConvertedPurchaseOrderID != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("expected CONVERTED with the PO link recorded, got %+v", got)
	}
	if len(e.po.calls) != 1 {
		t.Fatalf("expected one PO request, got %d", len(e.po.calls))
	}
	call := e.po.calls[0]
	if call.CorrelationID != "pr-convert-r1" || call.SupplierRef != "supplier-1" || call.PurchaseRequestID != "r1" ||
		len(call.Lines) != 1 || call.Lines[0].UnitPrice != 100 || call.Lines[0].Quantity != 4 || call.Lines[0].LineAmount != 400 {
		t.Fatalf("the PO draft must be keyed by the requisition and carry unit price = amount/qty, got %+v", call)
	}

	again := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order",
		domain.ConvertRequestRequest{SupplierRef: "supplier-1"}, "buyer")
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("converting twice must replay the recorded link, got %d replay=%q", again.Code, again.Header().Get("Idempotent-Replay"))
	}
	if len(e.po.calls) != 1 {
		t.Fatalf("a replay must not ask purchase-order-svc again, got %d calls", len(e.po.calls))
	}
}

func TestConvert_SupplierResolution_And_Failures(t *testing.T) {
	approved := func() *env {
		e := newEnv()
		e.store.seed(withStatus(pending("r1", "requester"), domain.RequestStatusApproved))
		return e
	}
	e := approved()
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order", nil, "buyer"); rec.Code != http.StatusBadRequest ||
		errCode(t, rec).Error != "supplier_required" {
		t.Fatalf("no supplier anywhere: expected 400 supplier_required, got %d %s", rec.Code, rec.Body.String())
	}

	e = approved()
	e.store.requests["r1"].PreferredSupplierRef = "preferred-1"
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order", nil, "buyer"); rec.Code != http.StatusOK {
		t.Fatalf("the preferred supplier must be used, got %d %s", rec.Code, rec.Body.String())
	}
	if e.po.calls[0].SupplierRef != "preferred-1" {
		t.Fatalf("expected preferred-1, got %q", e.po.calls[0].SupplierRef)
	}

	e = approved()
	e.po.err = &purchaseorder.RefusedError{Status: 422, Code: "SUPPLIER_HELD", Detail: "held"}
	rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order", domain.ConvertRequestRequest{SupplierRef: "s"}, "buyer")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec).Code != "PURCHASE_ORDER_REFUSED" {
		t.Fatalf("a refusal from purchase-order-svc must surface as 422, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusApproved {
		t.Fatal("a refused PO must leave the requisition APPROVED (retryable)")
	}

	e = approved()
	e.po.err = purchaseorder.ErrUnavailable
	rec = doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order", domain.ConvertRequestRequest{SupplierRef: "s"}, "buyer")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec).Code != "DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("an unreachable purchase-order-svc must be 503, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.requests["r1"].Status != domain.RequestStatusApproved {
		t.Fatal("an unreachable PO service must leave the requisition APPROVED")
	}

	e = approved()
	if rec := doRequest(e.router(), http.MethodPost, base+"r1/convert-to-purchase-order",
		domain.ConvertRequestRequest{SupplierRef: "s", ExpectedVersion: ver(3)}, "buyer"); rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on a stale convert, got %d", rec.Code)
	}
}

// ── reads ────────────────────────────────────────────────────────────────────

func TestGetRequest_NotFound(t *testing.T) {
	rec := doRequest(newEnv().router(), http.MethodGet, base+"does-not-exist", nil, "reader")
	if rec.Code != http.StatusNotFound || errCode(t, rec).Code != "REQUEST_NOT_FOUND" {
		t.Fatalf("expected 404 REQUEST_NOT_FOUND, got %d %s", rec.Code, rec.Body.String())
	}
}

// A read with no principal is refused — unless it declares itself an internal
// service call (purchase-order-svc verifying a requisition is APPROVED), which
// is still tenant-scoped.
func TestGetRequest_NoPrincipal_RefusedUnlessSystemChannel_AndAlwaysTenantScoped(t *testing.T) {
	e := newEnv()
	e.store.seed(withStatus(pending("r1", "requester"), domain.RequestStatusApproved))
	r := e.router()
	if rec := doRequest(r, http.MethodGet, base+"r1", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d", rec.Code)
	}
	sys := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, base+"r1", nil)
		req.Header.Set("X-Source-Channel", "system")
		req.Header.Set("X-Tenant-Id", tenant)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := sys(tenantA); rec.Code != http.StatusOK {
		t.Fatalf("a system read in the owning tenant must succeed, got %d", rec.Code)
	}
	if rec := sys(tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("a system read must still be tenant-scoped, got %d", rec.Code)
	}
}

func TestGetRequest_OtherTenant_IsNotFound_NotForbidden(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	if rec := doRequestAs(e.router(), http.MethodGet, base+"r1", nil, "reader", tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's requisition must look absent (404), got %d", rec.Code)
	}
	if rec := doRequestAs(e.router(), http.MethodPost, base+"r1/cancel", domain.CancelRequestRequest{Reason: "x"}, "attacker", tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant must not be able to command it, got %d", rec.Code)
	}
	if e.store.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatal("cross-tenant command must not change anything")
	}
}

func TestGetApprovalStatus_ReflectsThresholdAndIndependence(t *testing.T) {
	e := newEnv()
	e.cfg = handler.Config{ApprovalThreshold: 1000, MakerChecker: true}
	r := pending("r1", "requester")
	r.Amount = 50000
	e.store.seed(r)
	rec := doRequest(e.router(), http.MethodGet, base+"r1/approval-status", nil, "reader")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var st domain.ApprovalStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.AboveThreshold || !st.IndependentApproverNeeded || st.ApprovalThreshold != 1000 || st.Status != domain.RequestStatusPending {
		t.Fatalf("unexpected approval status %+v", st)
	}
}

func TestGetAvailableActions_FollowStateAndHideApproveFromRequester(t *testing.T) {
	actions := func(e *env, id, caller string) map[string]bool {
		rec := doRequest(e.router(), http.MethodGet, base+id+"/available-actions", nil, caller)
		var resp struct {
			AvailableActions []string `json:"available_actions"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		m := map[string]bool{}
		for _, a := range resp.AvailableActions {
			m[a] = true
		}
		return m
	}
	e := newEnv()
	e.cfg.MakerChecker = true
	e.store.seed(draftWith("d", "requester", "GENERAL", 1))
	e.store.seed(pending("p", "requester"))
	e.store.seed(withStatus(pending("a", "requester"), domain.RequestStatusApproved))
	e.store.seed(withStatus(pending("x", "requester"), domain.RequestStatusCancelled))

	if a := actions(e, "d", "requester"); !a["SubmitRequisition"] || !a["AmendRequisition"] || a["ApproveRequisition"] {
		t.Fatalf("DRAFT actions wrong: %v", a)
	}
	if a := actions(e, "p", "requester"); a["ApproveRequisition"] || a["RejectRequisition"] || !a["CancelRequisition"] {
		t.Fatalf("the requester must not be offered Approve/Reject on their own requisition: %v", a)
	}
	if a := actions(e, "p", "approver"); !a["ApproveRequisition"] || !a["RejectRequisition"] {
		t.Fatalf("an independent approver must be offered Approve/Reject: %v", a)
	}
	if a := actions(e, "a", "buyer"); !a["ConvertToPurchaseOrder"] {
		t.Fatalf("APPROVED must offer conversion: %v", a)
	}
	if a := actions(e, "x", "buyer"); len(a) != 0 {
		t.Fatalf("a terminal requisition offers nothing: %v", a)
	}
}

func TestGetHistory_ReturnsAppendOnlyTrail(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("r1", "requester"))
	doRequest(e.router(), http.MethodPost, base+"r1/approve", domain.VersionedRequest{ExpectedVersion: ver(1)}, "approver")
	rec := doRequest(e.router(), http.MethodGet, base+"r1/history", nil, "reader")
	var resp struct {
		History []domain.HistoryEntry `json:"history"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || len(resp.History) != 1 || resp.History[0].Action != "APPROVED" || resp.History[0].Actor != "approver" {
		t.Fatalf("expected one APPROVED history row by the approver, got %d %+v", rec.Code, resp.History)
	}
}

// ── ListRequests ─────────────────────────────────────────────────────────────

// TestListRequests_NoTenantScope_Refused replaces a test that asserted a 400
// when ?tenant_id= was absent — which documented the vulnerability as correct,
// since supplying the parameter was exactly how a caller read another tenant's
// register. The scope now comes from the header, so its absence is the failure.
func TestListRequests_NoTenantScope_Refused(t *testing.T) {
	rec := doRequestAs(newEnv().router(), http.MethodGet, base, nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestListRequests_ForeignTenantQueryParam_Refused is the regression test for
// the headline defect: ?tenant_id= was handed straight to the store, which both
// filtered on it and set app.tenant_id from it, so the tenant the caller named
// satisfied the RLS policy on the way past.
func TestListRequests_ForeignTenantQueryParam_Refused(t *testing.T) {
	rec := doRequestAs(newEnv().router(), http.MethodGet, base+"?tenant_id="+tenantB, nil, "reader", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's register, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListRequests_OwnTenantQueryParam_Allowed_AndOnlyOwnRowsReturned(t *testing.T) {
	e := newEnv()
	e.store.seed(pending("mine", "requester"))
	other := pending("theirs", "requester")
	other.TenantID = tenantB
	e.store.seed(other)
	rec := doRequestAs(e.router(), http.MethodGet, base+"?tenant_id="+tenantA, nil, "reader", tenantA)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when the query param agrees with the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []domain.PurchaseRequest
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].RequestID != "mine" {
		t.Fatalf("expected only the caller's own register, got %+v", list)
	}
}

func TestListRequests_UnknownStatusFilter_Refused(t *testing.T) {
	if rec := doRequest(newEnv().router(), http.MethodGet, base+"?status=PENDNIG", nil, "reader"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised status filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A malformed legal_entity_id is compared as text against a cast column, so it
// matched nothing and read as "this entity has raised no requests".
func TestListRequests_MalformedLegalEntityFilter_Refused(t *testing.T) {
	if rec := doRequest(newEnv().router(), http.MethodGet, base+"?legal_entity_id=not-a-uuid", nil, "reader"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListRequests_StoreError_FailsWith503(t *testing.T) {
	e := newEnv()
	e.store.listErr = errors.New("db down")
	if rec := doRequest(e.router(), http.MethodGet, base, nil, "reader"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// ── tenant scope on create ───────────────────────────────────────────────────

// TestCreateRequest_ForeignTenantBody_Refused is the write half of the same
// defect: tenant_id in the body was the only source of the stored tenant.
func TestCreateRequest_ForeignTenantBody_Refused(t *testing.T) {
	e := newEnv()
	req := validCreateReq()
	req.TenantID = tenantB
	rec := doRequestAs(e.router(), http.MethodPost, base, req, "principal-1", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 creating into another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.store.requests) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(e.store.requests))
	}
}

// A body that omits tenant_id is fine — the tenant is the verified scope, and
// that is the tenant the row must be filed under.
func TestCreateRequest_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	e := newEnv()
	req := validCreateReq()
	req.TenantID = ""
	rec := doRequestAs(e.router(), http.MethodPost, base, req, "principal-1", tenantA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodePR(t, rec); got.TenantID != tenantA {
		t.Fatalf("expected the request filed under the verified tenant %s, got %s", tenantA, got.TenantID)
	}
}

func TestCreateRequest_NoTenantScope_Refused(t *testing.T) {
	e := newEnv()
	rec := doRequestAs(e.router(), http.MethodPost, base, validCreateReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.store.requests) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(e.store.requests))
	}
}

func TestCreateRequest_MalformedLegalEntityID_Refused(t *testing.T) {
	req := validCreateReq()
	req.LegalEntityID = "e1"
	if rec := doRequest(newEnv().router(), http.MethodPost, base, req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}
