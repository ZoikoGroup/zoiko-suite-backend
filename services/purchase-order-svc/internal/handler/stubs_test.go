package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/handler"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/procurementcase"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
	"zoiko.io/purchase-order-svc/internal/store"
	"zoiko.io/purchase-order-svc/internal/supplier"
)

// tenant_id and legal_entity_id are uuid columns, so the fixtures are UUIDs —
// "t1"/"e1" would be refused by the handler's own identifier checks now that a
// malformed id is a 400 rather than a 503 from the driver.
const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entityA = "33333333-3333-3333-3333-333333333333"
)

// ── stub store ───────────────────────────────────────────────────────────────

// stubStore is an in-memory handler.Store. It keeps the state machine, the
// maker-checker rule, version checks and the outbox, so handler tests can assert
// on behaviour; the SQL itself is covered by the real-Postgres store suites.
type stubStore struct {
	orders     map[string]*domain.OrderDetail
	amendments map[string][]domain.PurchaseOrderAmendment
	revisions  map[string][]domain.Revision
	events     map[string][]domain.OrderEvent
	outbox     []string // event types, in order
	progress   map[string]domain.ProgressRequest
	idem       map[string]*domain.IdempotencyRecord

	createErr, getErr, listErr, transitionErr, amendErr, progressErr error
	amendCalls, transitionCalls                                      int
	lastTransition                                                   store.TransitionInput
}

func newStubStore() *stubStore {
	return &stubStore{
		orders: map[string]*domain.OrderDetail{}, amendments: map[string][]domain.PurchaseOrderAmendment{},
		revisions: map[string][]domain.Revision{}, events: map[string][]domain.OrderEvent{},
		progress: map[string]domain.ProgressRequest{}, idem: map[string]*domain.IdempotencyRecord{},
	}
}

func (s *stubStore) tenantOf(ctx context.Context) string { return svcmiddleware.TenantFromContext(ctx) }

func (s *stubStore) emit(types ...string) { s.outbox = append(s.outbox, types...) }

func (s *stubStore) log(id, evt string, from domain.OrderStatus, o *domain.OrderDetail, actor string) {
	s.events[id] = append(s.events[id], domain.OrderEvent{EventType: evt, FromStatus: from, ToStatus: o.Status, Revision: o.Revision, Version: o.Version, ActorID: actor})
	s.emit(evt)
}

func (s *stubStore) lookup(ctx context.Context, id string) *domain.OrderDetail {
	o, ok := s.orders[id]
	if !ok || o.TenantID != s.tenantOf(ctx) {
		return nil
	}
	return o
}

func makeLines(in []domain.LineInput) []domain.Line {
	out := make([]domain.Line, 0, len(in))
	for i, l := range in {
		n := l.LineNumber
		if n == 0 {
			n = i + 1
		}
		out = append(out, domain.Line{LineID: uuid.NewString(), LineNumber: n, ItemRef: l.ItemRef, Description: l.Description,
			Quantity: l.Quantity, UnitPrice: l.UnitPrice, UOM: l.UOM, LineAmount: l.Amount(), DeliveryDate: l.DeliveryDate, DeliveryLocation: l.DeliveryLocation})
	}
	return out
}

func (s *stubStore) newOrder(in store.CreateDraftInput) *domain.OrderDetail {
	total := in.TotalAmount
	lines := makeLines(in.Lines)
	if len(lines) > 0 {
		total = 0
		for _, l := range lines {
			total += l.LineAmount
		}
	}
	var supplierRef *string
	if in.SupplierRef != "" {
		v := in.SupplierRef
		supplierRef = &v
	}
	return &domain.OrderDetail{PurchaseOrder: domain.PurchaseOrder{
		PurchaseOrderID: uuid.NewString(), TenantID: in.TenantID, LegalEntityID: in.LegalEntityID,
		PurchaseRequestID: in.PurchaseRequestID, VendorProfileID: in.VendorProfileID, SupplierRef: supplierRef,
		PONumber: fmt.Sprintf("PO-%06d", len(s.orders)+1), Status: domain.OrderStatusDraft, TotalAmount: total,
		CurrencyCode: in.CurrencyCode, Version: 1, Revision: 1, PreparedByPrincipalID: in.PreparedBy,
		SupplierExceptionRef: in.SupplierExceptionRef, SupplierExceptionBy: in.SupplierExceptionBy,
		CorrelationID: in.CorrelationID, CreatedAt: time.Now().UTC(),
	}, Lines: lines}
}

func (s *stubStore) existing(tenant, corr string) *domain.OrderDetail {
	for _, o := range s.orders {
		if o.TenantID == tenant && o.CorrelationID == corr {
			return o
		}
	}
	return nil
}

func (s *stubStore) CreateDraft(_ context.Context, in store.CreateDraftInput) (*domain.OrderDetail, bool, error) {
	if s.createErr != nil {
		return nil, false, s.createErr
	}
	if e := s.existing(in.TenantID, in.CorrelationID); e != nil {
		return e, false, nil
	}
	o := s.newOrder(in)
	s.orders[o.PurchaseOrderID] = o
	s.log(o.PurchaseOrderID, store.EventCreated, "", o, in.PreparedBy)
	return o, true, nil
}

func (s *stubStore) CreateIssued(_ context.Context, in store.IssuedInput) (*domain.OrderDetail, bool, error) {
	if s.createErr != nil {
		return nil, false, s.createErr
	}
	if e := s.existing(in.TenantID, in.CorrelationID); e != nil {
		return e, false, nil
	}
	o := s.newOrder(in.CreateDraftInput)
	now := time.Now().UTC()
	o.Status = domain.OrderStatusIssued
	o.ApprovalBasis, o.ApprovalRef = in.ApprovalBasis, in.ApprovalRef
	o.ApprovedByPrincipalID, o.ApprovedAt = &in.ApprovedBy, &now
	o.IssuedByPrincipalID, o.IssuedAt = in.PreparedBy, &now
	s.orders[o.PurchaseOrderID] = o
	s.emit(store.EventCreated, store.EventSubmitted, store.EventApproved, store.EventIssued, "purchase.order.issued")
	return o, true, nil
}

func (s *stubStore) GetOrder(ctx context.Context, id string) (*domain.PurchaseOrder, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	o := s.lookup(ctx, id)
	if o == nil {
		return nil, nil
	}
	h := o.PurchaseOrder
	return &h, nil
}

func (s *stubStore) GetOrderDetail(ctx context.Context, id string) (*domain.OrderDetail, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	o := s.lookup(ctx, id)
	if o == nil {
		return nil, nil
	}
	cp := *o
	return &cp, nil
}

func (s *stubStore) ListOrders(_ context.Context, f domain.ListOrdersFilter) ([]domain.PurchaseOrder, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.PurchaseOrder
	for _, o := range s.orders {
		if o.TenantID == f.TenantID && (f.Status == "" || string(o.Status) == f.Status) {
			out = append(out, o.PurchaseOrder)
		}
	}
	return out, nil
}

func (s *stubStore) ListAmendments(_ context.Context, id string) ([]domain.PurchaseOrderAmendment, error) {
	return s.amendments[id], nil
}
func (s *stubStore) ListRevisions(_ context.Context, id string) ([]domain.Revision, error) {
	return s.revisions[id], nil
}
func (s *stubStore) ListEvents(_ context.Context, id string) ([]domain.OrderEvent, error) {
	return s.events[id], nil
}

func (s *stubStore) AmendOrder(ctx context.Context, tenantID, id, actor string, req domain.AmendOrderRequest) (*store.AmendResult, error) {
	s.amendCalls++
	if s.amendErr != nil {
		return nil, s.amendErr
	}
	o := s.lookup(ctx, id)
	if o == nil {
		return nil, domain.ErrOrderNotFound
	}
	switch o.Status {
	case domain.OrderStatusDraft, domain.OrderStatusPendingApproval, domain.OrderStatusApproved, domain.OrderStatusIssued:
	default:
		return nil, domain.ErrInvalidTransition
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != o.Version {
		return nil, domain.ErrStaleVersion
	}
	before := *o
	next := *o
	if req.Lines != nil {
		next.Lines = makeLines(req.Lines)
		next.TotalAmount = 0
		for _, l := range next.Lines {
			next.TotalAmount += l.LineAmount
		}
	} else if req.NewTotalAmount > 0 {
		next.TotalAmount = req.NewTotalAmount
	}
	if req.SupplierRef != nil {
		next.SupplierRef = req.SupplierRef
	}
	material := domain.IsMaterialChange(before, next)
	if !material && req.Lines == nil && req.NewTotalAmount == 0 && req.SupplierRef == nil {
		return nil, store.ErrNoChange
	}
	res := &store.AmendResult{Material: material}
	newRev := o.Status == domain.OrderStatusApproved || o.Status == domain.OrderStatusIssued
	if newRev {
		s.revisions[id] = append(s.revisions[id], domain.Revision{PurchaseOrderID: id, Revision: o.Revision, StatusAtSnapshot: o.Status, Snapshot: before, SupersededByRevision: o.Revision + 1})
		next.Revision++
		res.NewRevision = true
	}
	if material && (o.Status != domain.OrderStatusDraft) {
		next.Status = domain.OrderStatusDraft
		next.ApprovedByPrincipalID, next.ApprovedAt, next.ApprovalBasis = nil, nil, ""
		res.RequiresReapproval = true
	}
	next.Version++
	s.amendments[id] = append(s.amendments[id], domain.PurchaseOrderAmendment{PurchaseOrderID: id, FromVersion: before.Version, ToVersion: next.Version,
		PreviousTotalAmount: before.TotalAmount, NewTotalAmount: next.TotalAmount, Reason: req.Reason, AmendedByPrincipalID: actor})
	*o = next
	s.log(id, store.EventAmended, before.Status, o, actor)
	res.Order = *o
	return res, nil
}

func (s *stubStore) Transition(ctx context.Context, in store.TransitionInput) (*domain.OrderDetail, error) {
	s.transitionCalls++
	s.lastTransition = in
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	o := s.lookup(ctx, in.OrderID)
	if o == nil {
		return nil, domain.ErrOrderNotFound
	}
	if in.ExpectedVersion != nil && *in.ExpectedVersion != o.Version {
		return nil, domain.ErrStaleVersion
	}
	from := o.Status
	var to domain.OrderStatus
	var evt string
	switch in.Command {
	case store.CmdSubmit:
		to, evt = domain.OrderStatusPendingApproval, store.EventSubmitted
		if len(o.Lines) == 0 && o.TotalAmount <= 0 {
			return nil, domain.ErrNoLines
		}
		if from != domain.OrderStatusDraft {
			return nil, domain.ErrInvalidTransition
		}
		o.SubmittedByPrincipalID = &in.Actor
	case store.CmdApprove:
		to, evt = domain.OrderStatusApproved, store.EventApproved
		if from != domain.OrderStatusPendingApproval {
			return nil, domain.ErrInvalidTransition
		}
		if in.Actor == o.PreparedByPrincipalID || (o.SubmittedByPrincipalID != nil && in.Actor == *o.SubmittedByPrincipalID) {
			return nil, domain.ErrSoDConflict
		}
		o.ApprovedByPrincipalID, o.ApprovalBasis = &in.Actor, domain.ApprovalBasisWorkflow
	case store.CmdIssue:
		to, evt = domain.OrderStatusIssued, store.EventIssued
		if from != domain.OrderStatusApproved {
			return nil, domain.ErrInvalidTransition
		}
		o.IssuedByPrincipalID = in.Actor
		if in.SupplierExceptionRef != "" {
			o.SupplierExceptionRef, o.SupplierExceptionBy = in.SupplierExceptionRef, in.SupplierExceptionBy
		}
	case store.CmdHold:
		to, evt = domain.OrderStatusOnHold, store.EventHeld
		if from != domain.OrderStatusApproved && from != domain.OrderStatusIssued {
			return nil, domain.ErrInvalidTransition
		}
		o.HeldFromStatus, o.HoldReason = string(from), in.Reason
	case store.CmdRelease:
		if from != domain.OrderStatusOnHold {
			return nil, domain.ErrInvalidTransition
		}
		to, evt = domain.OrderStatus(o.HeldFromStatus), store.EventHoldReleased
		o.HeldFromStatus, o.HoldReason = "", ""
	case store.CmdCancel:
		to, evt = domain.OrderStatusCancelled, store.EventCancelled
		if from.IsTerminal() {
			return nil, domain.ErrInvalidTransition
		}
		o.CancellationReason = in.Reason
	case store.CmdClose:
		to, evt = domain.OrderStatusClosed, store.EventClosed
		if from != domain.OrderStatusIssued {
			return nil, domain.ErrInvalidTransition
		}
		o.ClosedByPrincipalID = &in.Actor
	}
	o.Status = to
	o.Version++
	s.log(in.OrderID, evt, from, o, in.Actor)
	if evt == store.EventClosed || evt == store.EventIssued {
		s.emit("legacy-alias")
	}
	cp := *o
	return &cp, nil
}

func (s *stubStore) RecordProgress(ctx context.Context, tenantID, id, lineID, actor string, req domain.ProgressRequest, _ string) (*domain.ProgressResult, error) {
	if s.progressErr != nil {
		return nil, s.progressErr
	}
	o := s.lookup(ctx, id)
	if o == nil {
		return nil, domain.ErrOrderNotFound
	}
	if o.Status != domain.OrderStatusIssued {
		return nil, domain.ErrOrderNotIssued
	}
	var line *domain.Line
	for i := range o.Lines {
		if o.Lines[i].LineID == lineID {
			line = &o.Lines[i]
		}
	}
	if line == nil {
		return nil, domain.ErrLineNotFound
	}
	key := tenantID + "|" + req.SourceRef + "|" + req.Kind
	if prior, ok := s.progress[key]; ok {
		if prior.Quantity != req.Quantity || prior.DeltaSign != req.DeltaSign {
			return nil, domain.ErrProgressRefReused
		}
		return &domain.ProgressResult{LineID: lineID, Kind: req.Kind, Replayed: true, OrderedQuantity: line.Quantity}, nil
	}
	var done float64
	for k, p := range s.progress {
		_ = k
		if p.Kind == req.Kind {
			done += p.Quantity * float64(p.DeltaSign)
		}
	}
	if done+req.Quantity*float64(req.DeltaSign) > line.Quantity+1e-9 {
		return nil, domain.ErrProgressExceedsOrder
	}
	s.progress[key] = req
	return &domain.ProgressResult{LineID: lineID, Kind: req.Kind, OrderedQuantity: line.Quantity, ReceivedQuantity: done + req.Quantity}, nil
}

func (s *stubStore) OpenQuantity(ctx context.Context, id string) ([]domain.LineProgress, bool, error) {
	o := s.lookup(ctx, id)
	if o == nil {
		return nil, false, nil
	}
	out := []domain.LineProgress{}
	for _, l := range o.Lines {
		out = append(out, domain.LineProgress{LineID: l.LineID, LineNumber: l.LineNumber, OrderedQuantity: l.Quantity,
			OpenReceiptQuantity: l.Quantity, OpenInvoiceQuantity: l.Quantity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LineNumber < out[j].LineNumber })
	return out, true, nil
}

func idemKey(scope, key string) string { return scope + "|" + key }

func (s *stubStore) BeginIdempotent(_ context.Context, scope, key, hash string) (*domain.IdempotencyRecord, bool, error) {
	if rec, ok := s.idem[idemKey(scope, key)]; ok {
		cp := *rec
		return &cp, false, nil
	}
	s.idem[idemKey(scope, key)] = &domain.IdempotencyRecord{RequestHash: hash}
	return nil, true, nil
}

func (s *stubStore) CompleteIdempotent(_ context.Context, scope, key string, code int, body []byte) error {
	if rec, ok := s.idem[idemKey(scope, key)]; ok && !rec.Completed {
		rec.Completed, rec.StatusCode, rec.Body = true, code, append([]byte(nil), body...)
	}
	return nil
}

func (s *stubStore) ReleaseIdempotent(_ context.Context, scope, key string) error {
	if rec, ok := s.idem[idemKey(scope, key)]; ok && !rec.Completed {
		delete(s.idem, idemKey(scope, key))
	}
	return nil
}

// ── stub clients ─────────────────────────────────────────────────────────────

// stubAuthZ records every check. sodRules makes the own-object layer deny an
// actor who owns the object, as authorization-svc does.
type stubAuthZ struct {
	err      error
	denyOnly string // when set, only this action is denied
	sodRules bool
	actions  []string
	owners   map[string]string // action -> owner passed to CheckAllowedOwnObject
}

func (a *stubAuthZ) decide(action string) error {
	a.actions = append(a.actions, action)
	if a.err != nil && (a.denyOnly == "" || a.denyOnly == action) {
		return a.err
	}
	return nil
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	return a.decide(action)
}

func (a *stubAuthZ) CheckAllowedOwnObject(_ context.Context, principal, _, action, owner string) error {
	if a.owners == nil {
		a.owners = map[string]string{}
	}
	a.owners[action] = owner
	if err := a.decide(action); err != nil {
		return err
	}
	if a.sodRules && principal == owner {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthZ) saw(action string) bool {
	for _, x := range a.actions {
		if x == action {
			return true
		}
	}
	return false
}

type stubPRClient struct {
	summary *purchaserequest.Summary
	err     error

	// gotTenantID records the scope the upstream lookup was made in. The tenant
	// used to come from the issue body, so a body naming another tenant had its
	// referenced purchase request checked in THAT tenant too.
	gotTenantID string
}

func (c *stubPRClient) GetApprovedRequest(_ context.Context, tenantID, _, _ string) (*purchaserequest.Summary, error) {
	c.gotTenantID = tenantID
	return c.summary, c.err
}

// stubSupplier answers AP-01 eligibility per supplier_ref.
type stubSupplier struct {
	answers map[string]supplier.Eligibility
	err     error
	calls   int
}

func newStubSupplier() *stubSupplier {
	return &stubSupplier{answers: map[string]supplier.Eligibility{}}
}

func (s *stubSupplier) set(ref string, e supplier.Eligibility) {
	e.SupplierRef = ref
	s.answers[ref] = e
}

func (s *stubSupplier) active(ref string) {
	s.set(ref, supplier.Eligibility{Status: "ACTIVE", EligibleForNewCommitments: true})
}

func (s *stubSupplier) Eligibility(_ context.Context, _, _, _, ref string) (*supplier.Eligibility, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	e, ok := s.answers[ref]
	if !ok {
		return nil, domain.ErrSupplierUnknown
	}
	return &e, nil
}

// stubCases verifies procurement cases; by default every case is approved.
type stubCases struct {
	err   error
	calls int
}

func (c *stubCases) VerifyApproved(_ context.Context, tenantID, _, entity, caseID string, _ float64, _ string) (*procurementcase.Case, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	by := "case-approver"
	return &procurementcase.Case{CaseID: caseID, TenantID: tenantID, LegalEntityID: entity, Status: "APPROVED", ApprovedByPrincipalID: &by}, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

type env struct {
	store    *stubStore
	authz    *stubAuthZ
	pr       *stubPRClient
	supplier *stubSupplier
	cases    *stubCases
	router   chi.Router
}

var keySeq atomic.Int64

// newEnv builds a handler with every dependency present and permissive: the
// supplier "SUP-1" is eligible, procurement cases are approved, authorization
// grants.
func newEnv(opts ...handler.Options) *env {
	e := &env{store: newStubStore(), authz: &stubAuthZ{}, pr: &stubPRClient{}, supplier: newStubSupplier(), cases: &stubCases{}}
	e.supplier.active("SUP-1")
	h := handler.New(e.store, e.authz, e.pr, zap.NewNop()).WithSupplierClient(e.supplier).WithProcurementCases(e.cases)
	if len(opts) > 0 {
		h.WithOptions(opts[0])
	}
	r := chi.NewRouter()
	// newRouter mounts TenantContext, which the real server mounts in
	// cmd/server/main.go. A handler harness must mount the middleware the handler
	// depends on, or every handler sees an empty tenant scope.
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

// do sends a request in tenantA's scope, which is the ordinary case.
func (e *env) do(method, path string, body any, principal string) *httptest.ResponseRecorder {
	return e.doAs(method, path, body, principal, tenantA)
}

// doAs sends a request in an explicit tenant scope; tenantID "" omits the
// X-Tenant-Id header entirely, which is how a request with no verified scope is
// simulated.
func (e *env) doAs(method, path string, body any, principal, tenantID string) *httptest.ResponseRecorder {
	return e.doKeyed(method, path, body, principal, tenantID, "")
}

// doKeyed sends a request with an explicit Idempotency-Key ("" = none).
func (e *env) doKeyed(method, path string, body any, principal, tenantID, key string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// post sends a POST with a fresh Idempotency-Key (as every real client must).
func (e *env) post(path string, body any, principal string) *httptest.ResponseRecorder {
	return e.doKeyed(http.MethodPost, path, body, principal, tenantA, fmt.Sprintf("key-%d", keySeq.Add(1)))
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func twoLines() []domain.LineInput {
	return []domain.LineInput{
		{ItemRef: "SKU-1", Description: "bolts", Quantity: 10, UnitPrice: 5, UOM: "EA"},
		{ItemRef: "SKU-2", Description: "nuts", Quantity: 4, UnitPrice: 2.5, UOM: "EA"},
	}
}

func validDraft() domain.CreateDraftRequest {
	return domain.CreateDraftRequest{
		LegalEntityID: entityA, SupplierRef: "SUP-1", CurrencyCode: "USD",
		CorrelationID: "conv-" + uuid.NewString(), Lines: twoLines(),
	}
}

// createDraft posts a valid draft as "maker" and returns it.
func (e *env) createDraft(t *testing.T) domain.OrderDetail {
	t.Helper()
	rec := e.post("/v1/purchase-orders/draft", validDraft(), "maker")
	if rec.Code != http.StatusCreated {
		t.Fatalf("createDraft: %d %s", rec.Code, rec.Body.String())
	}
	return decode[domain.OrderDetail](t, rec)
}

func version(o domain.OrderDetail) *int { v := o.Version; return &v }

func (e *env) command(t *testing.T, id, cmd, principal string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return e.post("/v1/purchase-orders/"+id+"/"+cmd, body, principal)
}

// issued takes a new order through the whole governed flow.
func (e *env) issued(t *testing.T) domain.OrderDetail {
	t.Helper()
	o := e.createDraft(t)
	for _, step := range []struct{ cmd, who string }{{"submit", "maker"}, {"approve", "checker"}, {"issue", "buyer"}} {
		body := any(nil)
		if step.cmd == "approve" {
			cur := decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, ""))
			body = domain.CommandRequest{ExpectedVersion: version(cur)}
		}
		rec := e.command(t, o.PurchaseOrderID, step.cmd, step.who, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.cmd, rec.Code, rec.Body.String())
		}
		o = decode[domain.OrderDetail](t, rec)
	}
	return o
}

var _ = math.Abs

// httpRec abbreviates the recorder type in table tests.
type httpRec = httptest.ResponseRecorder

// storeErrNoChange is the store's "nothing to amend" sentinel.
var storeErrNoChange error = store.ErrNoChange

// newEnvWithoutCases is a handler with no procurement-case client wired, so a
// direct issue without a requisition has no way to verify its approval.
func newEnvWithoutCases() *env {
	e := &env{store: newStubStore(), authz: &stubAuthZ{}, pr: &stubPRClient{}, supplier: newStubSupplier(), cases: &stubCases{}}
	e.supplier.active("SUP-1")
	h := handler.New(e.store, e.authz, e.pr, zap.NewNop()).WithSupplierClient(e.supplier)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}
