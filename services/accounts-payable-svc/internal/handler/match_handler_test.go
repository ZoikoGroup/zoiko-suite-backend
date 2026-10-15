package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/handler"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/payableopenitem"
	"zoiko.io/accounts-payable-svc/internal/purchaseorder"
	"zoiko.io/accounts-payable-svc/internal/receipts"
	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// actionAuthz records the action types it is asked about and denies chosen ones.
type actionAuthz struct {
	deny map[string]bool
	seen []string
}

func (a *actionAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	a.seen = append(a.seen, action)
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// fakePO is the AP-03 reader (GetOrder / OpenQuantity / ReportProgress).
type fakePO struct {
	order    *purchaseorder.PurchaseOrder
	orderErr error
	progress []purchaseorder.LineProgress
	progErr  error
	getCalls int
}

func (p *fakePO) GetOrder(context.Context, svcclient.Caller, string) (*purchaseorder.PurchaseOrder, error) {
	p.getCalls++
	return p.order, p.orderErr
}
func (p *fakePO) OpenQuantity(context.Context, svcclient.Caller, string) ([]purchaseorder.LineProgress, error) {
	return p.progress, p.progErr
}
func (p *fakePO) ReportProgress(context.Context, svcclient.Caller, string, string, purchaseorder.ProgressReport) error {
	return nil
}

type fakeReceipts struct {
	rtd *receipts.ReceivedToDate
	err error
}

func (f *fakeReceipts) ReceivedToDate(context.Context, svcclient.Caller, string) (*receipts.ReceivedToDate, error) {
	return f.rtd, f.err
}

// matchStub is a faithful in-memory MatchStore: it applies the same rules the real
// store does (idempotent on input hash, supersession, SoD, waivability, clearing).
type matchStub struct {
	inv        *stubStore
	policies   map[string][]domain.MatchPolicy
	runs       map[string][]*domain.MatchRunRecord
	exceptions map[string][]*domain.MatchExceptionRecord // run_id -> exceptions
	lines      map[string][]domain.MatchLineResult
	idemRecs   map[string]*domain.IdemRecord
	saveCalls  int
}

func newMatchStub(inv *stubStore) *matchStub {
	return &matchStub{inv: inv, policies: map[string][]domain.MatchPolicy{}, runs: map[string][]*domain.MatchRunRecord{},
		exceptions: map[string][]*domain.MatchExceptionRecord{}, lines: map[string][]domain.MatchLineResult{}, idemRecs: map[string]*domain.IdemRecord{}}
}

func (m *matchStub) IdemClaim(_ context.Context, tenantID, key, op, hash string, _ time.Duration) (*domain.IdemRecord, bool, error) {
	k := tenantID + "|" + key
	if rec, ok := m.idemRecs[k]; ok {
		return rec, false, nil
	}
	m.idemRecs[k] = &domain.IdemRecord{Operation: op, RequestHash: hash}
	return nil, true, nil
}
func (m *matchStub) IdemComplete(_ context.Context, tenantID, key string, status int, resp []byte) error {
	rec := m.idemRecs[tenantID+"|"+key]
	rec.StatusCode, rec.Response = &status, resp
	return nil
}
func (m *matchStub) IdemRelease(_ context.Context, tenantID, key string) error {
	delete(m.idemRecs, tenantID+"|"+key)
	return nil
}

func (m *matchStub) GetMatchPolicy(_ context.Context, _, le string, version int) (*domain.MatchPolicy, error) {
	ps := m.policies[le]
	if len(ps) == 0 {
		return nil, domain.ErrMatchPolicyNotFound
	}
	if version == 0 {
		p := ps[len(ps)-1]
		return &p, nil
	}
	for _, p := range ps {
		if p.PolicyVersion == version {
			return &p, nil
		}
	}
	return nil, domain.ErrMatchPolicyNotFound
}

func (m *matchStub) CreateMatchPolicy(_ context.Context, _ string, p domain.MatchPolicy) (*domain.MatchPolicy, error) {
	if err := p.Validate(); err != nil {
		return nil, errors.Join(domain.ErrMatchPolicyInvalid, err)
	}
	p.PolicyVersion = len(m.policies[p.LegalEntityID]) + 1
	m.policies[p.LegalEntityID] = append(m.policies[p.LegalEntityID], p)
	return &p, nil
}

func (m *matchStub) live(invoiceID string) *domain.MatchRunRecord {
	for _, r := range m.runs[invoiceID] {
		if r.SupersededAt == nil {
			return r
		}
	}
	return nil
}

func (m *matchStub) view(r *domain.MatchRunRecord, inv *domain.VendorInvoice, created bool) *domain.MatchResultView {
	v := &domain.MatchResultView{Run: r, Lines: m.lines[r.RunID], Exceptions: []domain.MatchExceptionRecord{}, InvoiceMatchState: inv.MatchState, Cleared: inv.MatchCleared, Created: created}
	for _, x := range m.exceptions[r.RunID] {
		v.Exceptions = append(v.Exceptions, *x)
	}
	return v
}

func (m *matchStub) SaveMatchRun(_ context.Context, in domain.SaveMatchRunInput) (*domain.MatchResultView, error) {
	m.saveCalls++
	inv, ok := m.inv.invoices[in.InvoiceID]
	if !ok {
		return nil, domain.ErrInvoiceNotFound
	}
	if !inv.RequiresMatch() {
		return nil, domain.ErrMatchNotApplicable
	}
	if cur := m.live(in.InvoiceID); cur != nil && cur.InputHash == in.Outcome.InputHash {
		return m.view(cur, inv, false), nil
	}
	if cur := m.live(in.InvoiceID); cur != nil {
		now := time.Now()
		cur.SupersededAt = &now
	}
	r := &domain.MatchRunRecord{RunID: uuid.NewString(), InvoiceID: in.InvoiceID, LegalEntityID: inv.LegalEntityID, RunNumber: len(m.runs[in.InvoiceID]) + 1,
		Result: in.Outcome.Result, Mode: in.Policy.Mode, PolicyVersion: in.Policy.PolicyVersion, PolicySnapshot: in.Policy, PurchaseOrderID: in.PurchaseOrderID,
		PORevision: in.PORevision, InputHash: in.Outcome.InputHash, Totals: in.Outcome.Totals, RequestedBy: in.Actor, CreatedAt: time.Now()}
	m.runs[in.InvoiceID] = append([]*domain.MatchRunRecord{r}, m.runs[in.InvoiceID]...)
	m.lines[r.RunID] = in.Outcome.Lines
	for _, x := range in.Outcome.Exceptions {
		x.ExceptionID, x.RunID, x.InvoiceID, x.LegalEntityID = uuid.NewString(), r.RunID, in.InvoiceID, inv.LegalEntityID
		xc := x
		m.exceptions[r.RunID] = append(m.exceptions[r.RunID], &xc)
	}
	inv.MatchState, inv.MatchCleared, inv.MatchRequired, inv.MatchRunID = in.Outcome.Result, in.Outcome.Cleared(), true, &r.RunID
	return m.view(r, inv, true), nil
}

func (m *matchStub) SupersedeMatchRun(_ context.Context, in domain.SupersedeInput) (*domain.VendorInvoice, error) {
	inv, ok := m.inv.invoices[in.InvoiceID]
	if !ok {
		return nil, domain.ErrInvoiceNotFound
	}
	cur := m.live(in.InvoiceID)
	if cur == nil {
		return nil, domain.ErrMatchRunNotFound
	}
	now := time.Now()
	cur.SupersededAt = &now
	inv.MatchState, inv.MatchCleared, inv.MatchRunID = domain.MatchNotMatched, false, nil
	return inv, nil
}

func (m *matchStub) findException(id string) (*domain.MatchExceptionRecord, *domain.MatchRunRecord) {
	for invID, runs := range m.runs {
		for _, r := range runs {
			for _, x := range m.exceptions[r.RunID] {
				if x.ExceptionID == id {
					_ = invID
					return x, r
				}
			}
		}
	}
	return nil, nil
}

func (m *matchStub) GetMatchException(_ context.Context, _, id string) (*domain.MatchExceptionRecord, error) {
	x, _ := m.findException(id)
	if x == nil {
		return nil, domain.ErrMatchExceptionNotFound
	}
	c := *x
	return &c, nil
}

func (m *matchStub) ResolveMatchException(_ context.Context, a domain.ExceptionAction) (*domain.MatchExceptionRecord, *domain.VendorInvoice, error) {
	x, run := m.findException(a.ExceptionID)
	if x == nil {
		return nil, nil, domain.ErrMatchExceptionNotFound
	}
	inv := m.inv.invoices[x.InvoiceID]
	if run.SupersededAt != nil {
		return nil, nil, domain.ErrMatchRunSuperseded
	}
	switch a.Kind {
	case domain.ActionAcknowledge:
		x.Status, x.AcknowledgedBy = domain.ExceptionAcknowledged, a.Actor
	case domain.ActionRoute:
		x.Status, x.RoutedTo = domain.ExceptionRouted, a.RouteTo
	case domain.ActionApproveVariance:
		if !x.Waivable {
			return nil, nil, domain.ErrExceptionNotWaivable
		}
		if a.Actor == run.RequestedBy || a.Actor == inv.CreatedByPrincipalID {
			return nil, nil, domain.ErrMatchSelfWaiver
		}
		x.Status, x.ResolvedBy = domain.ExceptionVarianceApproved, a.Actor
		open := 0
		for _, y := range m.exceptions[run.RunID] {
			if y.Status != domain.ExceptionVarianceApproved {
				open++
			}
		}
		if open == 0 {
			inv.MatchState, inv.MatchCleared = domain.MatchWithinTolerance, true
		}
	}
	c := *x
	return &c, inv, nil
}

func (m *matchStub) GetMatchResult(_ context.Context, _, invoiceID string) (*domain.MatchResultView, error) {
	rs := m.runs[invoiceID]
	if len(rs) == 0 {
		return nil, domain.ErrMatchRunNotFound
	}
	return m.view(rs[0], m.inv.invoices[invoiceID], false), nil
}

func (m *matchStub) ListMatchRuns(_ context.Context, _, invoiceID string) ([]domain.MatchRunRecord, error) {
	out := []domain.MatchRunRecord{}
	for _, r := range m.runs[invoiceID] {
		out = append(out, *r)
	}
	return out, nil
}

func (m *matchStub) ListMatchExceptions(_ context.Context, f domain.ExceptionFilter) ([]domain.MatchExceptionRecord, error) {
	out := []domain.MatchExceptionRecord{}
	for _, runs := range m.runs {
		for _, r := range runs {
			if r.SupersededAt != nil {
				continue
			}
			for _, x := range m.exceptions[r.RunID] {
				if x.LegalEntityID == f.LegalEntityID && (f.Status == "" || x.Status == f.Status) {
					out = append(out, *x)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExceptionID < out[j].ExceptionID })
	return out, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

type mh struct {
	r     chi.Router
	inv   *stubStore
	ms    *matchStub
	po    *fakePO
	rec   *fakeReceipts
	authz *actionAuthz
	inv1  *domain.VendorInvoice
}

// newMatchHarness builds a router with matching enabled and one PO-backed invoice
// (10 @ 5.00, created by "creator") whose PO and receipts agree exactly.
func newMatchHarness(t *testing.T) *mh {
	t.Helper()
	st := newStubStore()
	ms := newMatchStub(st)
	po := &fakePO{
		order: &purchaseorder.PurchaseOrder{PurchaseOrderID: "po-1", LegalEntityID: entityA, CurrencyCode: "USD", POStatus: "ISSUED", Revision: 2,
			HasRevision: true, HasLines: true, Lines: []purchaseorder.Line{{LineID: "L1", Quantity: 10, UnitPrice: 5}}},
		progress: []purchaseorder.LineProgress{{LineID: "L1", OrderedQuantity: 10}},
	}
	rec := &fakeReceipts{rtd: &receipts.ReceivedToDate{PurchaseOrderID: "po-1", HasLines: true, Lines: []receipts.Line{{POLineID: "L1", ReceivedQuantity: 10}}}}
	az := &actionAuthz{deny: map[string]bool{}}

	poRef := "po-1"
	line1 := "L1"
	inv := &domain.VendorInvoice{
		InvoiceID: uuid.NewString(), TenantID: tenantA, LegalEntityID: entityA, VendorID: "v1", InvoiceNumber: "INV-1", Amount: 50, CurrencyCode: "USD",
		DocumentType: domain.DocInvoice, PurchaseOrderID: &poRef, CreatedByPrincipalID: "creator", IntakeState: domain.IntakeValidated,
		ApprovalState: domain.ApprovalNone, MatchState: domain.MatchNotMatched, Version: 1,
		Lines: []domain.VendorInvoiceLine{{InvoiceLineID: uuid.NewString(), LineNumber: 1, Quantity: 10, UnitPrice: 5, NetAmount: 50, POLineReference: &line1}},
	}
	st.invoices[inv.InvoiceID] = inv

	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(st, &stubPublisher{}, az, &stubPO{}, &stubPayables{}, zap.NewNop())
	h.WithMatching(handler.MatchDeps{Store: ms, PO: po, Receipts: rec})
	handler.RegisterRoutes(r, h)
	return &mh{r: r, inv: st, ms: ms, po: po, rec: rec, authz: az, inv1: inv}
}

func (h *mh) do(method, path string, body any, principal string) *httptest.ResponseRecorder {
	return doRequestAs(h.r, method, path, body, principal, tenantA)
}

func (h *mh) doWithKey(method, path string, body any, principal, key string) *httptest.ResponseRecorder {
	buf := new(bytes.Buffer)
	if body != nil {
		_ = json.NewEncoder(buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Tenant-Id", tenantA)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, req)
	return rec
}

func (h *mh) matchPath() string { return "/v1/invoices/" + h.inv1.InvoiceID + "/match" }

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) domain.MatchResultView {
	t.Helper()
	var v domain.MatchResultView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode view: %v (%s)", err, rec.Body.String())
	}
	return v
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestRunInvoiceMatch_Matched_Created_ThenReplayIsOK(t *testing.T) {
	h := newMatchHarness(t)
	rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec)
	if v.Run.Result != domain.MatchMatched || !v.Cleared || v.Run.RequestedBy != "matcher" || *v.Run.PORevision != 2 || v.Run.PolicyVersion != 0 || !v.Run.PolicySnapshot.IsDefault {
		t.Fatalf("expected a cleared MATCHED run on the strict default policy, got %+v", v.Run)
	}
	if h.inv1.MatchState != domain.MatchMatched || !h.inv1.MatchCleared {
		t.Fatal("the invoice's match dimension must follow the run")
	}
	again := h.do(http.MethodPost, h.matchPath()+"/reperform", nil, "matcher")
	if again.Code != http.StatusOK || decodeView(t, again).Created {
		t.Fatalf("a re-performance on unchanged evidence is a 200 no-op, got %d %s", again.Code, again.Body.String())
	}
	if h.ms.saveCalls != 2 || len(h.ms.runs[h.inv1.InvoiceID]) != 1 {
		t.Fatalf("only one run may exist, got %d", len(h.ms.runs[h.inv1.InvoiceID]))
	}
	for _, a := range []string{"INVOICE_MATCH_RUN"} {
		found := false
		for _, s := range h.authz.seen {
			found = found || s == a
		}
		if !found {
			t.Fatalf("expected %s to be authorized, saw %v", a, h.authz.seen)
		}
	}
}

// Negative path 2: invoice quantity exceeds receipt but auto-matched.
func TestRunInvoiceMatch_QuantityOverReceipt_IsAnException_NotCleared(t *testing.T) {
	h := newMatchHarness(t)
	h.rec.rtd.Lines[0].ReceivedQuantity = 6
	rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher")
	v := decodeView(t, rec)
	if rec.Code != http.StatusCreated || v.Run.Result != domain.MatchException || v.Cleared || len(v.Exceptions) != 1 || v.Exceptions[0].Category != domain.ExcQtyOverReceipt {
		t.Fatalf("expected an uncleared EXCEPTION with QTY_OVER_RECEIPT, got %d %+v", rec.Code, v)
	}
}

// Negative path 4: missing receipt treated as a zero-difference match. An outage or
// absence of receipt evidence is INCOMPLETE — recorded, never cleared, never a 5xx.
func TestRunInvoiceMatch_MissingEvidence_IsIncomplete_NeverAMatch(t *testing.T) {
	cases := map[string]func(*mh){
		"receipts unavailable":   func(h *mh) { h.rec.rtd, h.rec.err = nil, receipts.ErrUnavailable },
		"receipts have no lines": func(h *mh) { h.rec.rtd = &receipts.ReceivedToDate{HasLines: false} },
		"no receipt for the line": func(h *mh) {
			h.rec.rtd = &receipts.ReceivedToDate{HasLines: true, Lines: []receipts.Line{{POLineID: "OTHER", ReceivedQuantity: 3}}}
		},
		"PO unavailable":           func(h *mh) { h.po.order, h.po.orderErr = nil, svcclient.ErrUnavailable },
		"PO has no lines":          func(h *mh) { h.po.order.HasLines = false },
		"invoiced-to-date unknown": func(h *mh) { h.po.progErr = svcclient.ErrUnavailable },
		"PO of another entity":     func(h *mh) { h.po.order.LegalEntityID = "99999999-9999-9999-9999-999999999999" },
	}
	for name, mod := range cases {
		h := newMatchHarness(t)
		mod(h)
		rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher")
		v := decodeView(t, rec)
		if rec.Code != http.StatusCreated || v.Run.Result != domain.MatchIncomplete || v.Cleared || h.inv1.MatchCleared {
			t.Fatalf("%s: expected an uncleared INCOMPLETE run, got %d %+v", name, rec.Code, v)
		}
	}
}

func TestRunInvoiceMatch_PolicyTolerance_IsFrozenIntoTheRun(t *testing.T) {
	h := newMatchHarness(t)
	h.po.order.Lines[0].UnitPrice = 5
	h.inv1.Lines[0].UnitPrice, h.inv1.Lines[0].NetAmount = 5.05, 50.5
	// Zero tolerance by default: an exception.
	if v := decodeView(t, h.do(http.MethodPost, h.matchPath(), nil, "matcher")); v.Run.Result != domain.MatchException {
		t.Fatalf("expected EXCEPTION at zero tolerance, got %+v", v.Run)
	}
	// A policy manager (a different principal) publishes a 2% tolerance; the runner is someone else.
	pol := h.do(http.MethodPost, "/v1/match-policies", map[string]any{"legal_entity_id": entityA, "mode": "THREE_WAY", "price_tolerance_pct": 2, "reason": "approved by CFO"}, "policy-admin")
	if pol.Code != http.StatusCreated {
		t.Fatalf("policy create: %d %s", pol.Code, pol.Body.String())
	}
	v := decodeView(t, h.do(http.MethodPost, h.matchPath()+"/reperform", nil, "matcher"))
	if v.Run.Result != domain.MatchWithinTolerance || !v.Cleared || v.Run.PolicyVersion != 1 || v.Run.PolicySnapshot.PriceTolerancePct != 2 || v.Run.RunNumber != 2 {
		t.Fatalf("expected a WITHIN_TOLERANCE run #2 on policy v1 with its tolerance frozen, got %+v", v.Run)
	}
	// The policy editor cannot certify results under their own policy version.
	rec := h.do(http.MethodPost, h.matchPath()+"/reperform", nil, "policy-admin")
	if rec.Code != http.StatusForbidden || stableCodeOf(t, rec) != domain.CodeSoDConflict {
		t.Fatalf("expected 403 SOD_CONFLICT for the policy editor, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRunInvoiceMatch_Refusals(t *testing.T) {
	h := newMatchHarness(t)
	if rec := h.do(http.MethodPost, h.matchPath(), nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", rec.Code)
	}
	h.authz.deny["INVOICE_MATCH_RUN"] = true
	if rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher"); rec.Code != http.StatusForbidden {
		t.Fatalf("not authorized: expected 403, got %d", rec.Code)
	}
	h.authz.deny["INVOICE_MATCH_RUN"] = false
	if rec := h.do(http.MethodPost, "/v1/invoices/"+uuid.NewString()+"/match", nil, "matcher"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown invoice: expected 404, got %d", rec.Code)
	}
	// An invoice with no PO is not matchable.
	h.inv1.PurchaseOrderID = nil
	rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher")
	if rec.Code != http.StatusUnprocessableEntity || stableCodeOf(t, rec) != domain.CodeMatchNotApplicable {
		t.Fatalf("expected 422 MATCH_NOT_APPLICABLE, got %d %s", rec.Code, rec.Body.String())
	}
	// Matching not configured fails closed.
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(h.inv, &stubPublisher{}, h.authz, &stubPO{}, &stubPayables{}, zap.NewNop()))
	if rec := doRequestAs(r, http.MethodPost, h.matchPath(), nil, "matcher", tenantA); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not configured: expected 503, got %d", rec.Code)
	}
	var _ = payableopenitem.SourceSupplierInvoice
}

func TestRunInvoiceMatch_IdempotencyKey_ReplaysWithoutReReadingEvidence(t *testing.T) {
	h := newMatchHarness(t)
	first := h.doWithKey(http.MethodPost, h.matchPath(), nil, "matcher", "key-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	calls := h.po.getCalls
	second := h.doWithKey(http.MethodPost, h.matchPath(), nil, "matcher", "key-1")
	if second.Code != http.StatusCreated || second.Header().Get("Idempotent-Replay") != "true" || second.Body.String() != first.Body.String() {
		t.Fatalf("a repeat with the same key must replay the stored response, got %d %q", second.Code, second.Header().Get("Idempotent-Replay"))
	}
	if h.po.getCalls != calls {
		t.Fatal("a replay must not re-read the evidence")
	}
	// The same key on a different operation is refused.
	other := h.doWithKey(http.MethodPost, h.matchPath()+"/reperform", nil, "matcher", "key-1")
	if other.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key on another request must be refused, got %d", other.Code)
	}
}

// ── exceptions, SoD, queries ─────────────────────────────────────────────────

func (h *mh) twoVarianceRun(t *testing.T) domain.MatchResultView {
	t.Helper()
	h.inv1.Lines[0].UnitPrice, h.inv1.Lines[0].NetAmount = 5.5, 55
	h.rec.rtd.Lines[0].ReceivedQuantity = 6
	rec := h.do(http.MethodPost, h.matchPath(), nil, "matcher")
	v := decodeView(t, rec)
	if len(v.Exceptions) != 2 {
		t.Fatalf("expected two findings, got %d: %s", len(v.Exceptions), rec.Body.String())
	}
	return v
}

func TestRecordApprovedVariance_SoD_Reason_And_Clearing(t *testing.T) {
	h := newMatchHarness(t)
	v := h.twoVarianceRun(t)
	x0, x1 := v.Exceptions[0].ExceptionID, v.Exceptions[1].ExceptionID
	path := func(id string) string { return "/v1/match-exceptions/" + id + "/approve-variance" }

	if rec := h.do(http.MethodPost, path(x0), map[string]any{}, "controller"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a reason is required, got %d", rec.Code)
	}
	body := map[string]any{"reason": "agreed with supplier", "reference": "CR-7"}
	for _, self := range []string{"matcher", "creator"} {
		rec := h.do(http.MethodPost, path(x0), body, self)
		if rec.Code != http.StatusForbidden || stableCodeOf(t, rec) != domain.CodeSoDConflict {
			t.Fatalf("negative path 3: %s must not approve this variance, got %d %s", self, rec.Code, rec.Body.String())
		}
	}
	h.authz.deny["INVOICE_MATCH_EXCEPTION_RESOLVE"] = true
	if rec := h.do(http.MethodPost, path(x0), body, "controller"); rec.Code != http.StatusForbidden {
		t.Fatalf("without resolve authority: expected 403, got %d", rec.Code)
	}
	h.authz.deny["INVOICE_MATCH_EXCEPTION_RESOLVE"] = false

	rec := h.do(http.MethodPost, path(x0), body, "controller")
	if rec.Code != http.StatusOK || h.inv1.MatchCleared {
		t.Fatalf("one of two approved must not clear, got %d cleared=%v", rec.Code, h.inv1.MatchCleared)
	}
	if rec := h.do(http.MethodPost, path(x1), body, "controller2"); rec.Code != http.StatusOK || !h.inv1.MatchCleared || h.inv1.MatchState != domain.MatchWithinTolerance {
		t.Fatalf("all approved must clear, got %d %+v", rec.Code, h.inv1.MatchState)
	}
	if rec := h.do(http.MethodPost, path(uuid.NewString()), body, "controller"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown exception: expected 404, got %d", rec.Code)
	}
}

func TestExceptions_NotWaivable_AckRouteValidation_Listing(t *testing.T) {
	h := newMatchHarness(t)
	h.rec.rtd.Lines = nil // a PO line with no receipt record
	h.rec.rtd.HasLines = true
	v := decodeView(t, h.do(http.MethodPost, h.matchPath(), nil, "matcher"))
	if v.Run.Result != domain.MatchIncomplete || len(v.Exceptions) != 1 || v.Exceptions[0].Waivable {
		t.Fatalf("expected one non-waivable finding, got %+v", v)
	}
	id := v.Exceptions[0].ExceptionID
	rec := h.do(http.MethodPost, "/v1/match-exceptions/"+id+"/approve-variance", map[string]any{"reason": "just let it through"}, "controller")
	if rec.Code != http.StatusUnprocessableEntity || stableCodeOf(t, rec) != domain.CodeNotWaivable {
		t.Fatalf("expected 422 MATCH_EXCEPTION_NOT_WAIVABLE, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/v1/match-exceptions/"+id+"/route", map[string]any{"reason": "x"}, "clerk"); rec.Code != http.StatusBadRequest {
		t.Fatalf("route_to is required, got %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/v1/match-exceptions/"+id+"/route", map[string]any{"route_to": "ap-lead", "reason": "needs the receipt"}, "clerk"); rec.Code != http.StatusOK {
		t.Fatalf("route: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/v1/match-exceptions/"+id+"/acknowledge", nil, "clerk"); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	if h.inv1.MatchCleared {
		t.Fatal("acknowledging or routing never clears")
	}

	if rec := h.do(http.MethodGet, "/v1/match-exceptions", nil, "clerk"); rec.Code != http.StatusBadRequest {
		t.Fatalf("legal_entity_id is required, got %d", rec.Code)
	}
	rec = h.do(http.MethodGet, "/v1/match-exceptions?legal_entity_id="+entityA+"&status=ACKNOWLEDGED", nil, "clerk")
	var list struct {
		Data  []domain.MatchExceptionRecord `json:"data"`
		Count int                           `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || list.Count != 1 {
		t.Fatalf("expected one acknowledged exception, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestMatchQueries_Result_Line_Runs_Actions_Policy_Supersede(t *testing.T) {
	h := newMatchHarness(t)
	if rec := h.do(http.MethodGet, h.matchPath(), nil, "reader"); rec.Code != http.StatusNotFound {
		t.Fatalf("no run yet: expected 404, got %d", rec.Code)
	}
	acts := func() []string {
		var out struct {
			Actions []string `json:"available_actions"`
		}
		_ = json.Unmarshal(h.do(http.MethodGet, h.matchPath()+"/available-actions", nil, "reader").Body.Bytes(), &out)
		return out.Actions
	}
	if a := acts(); len(a) != 1 || a[0] != "RunInvoiceMatch" {
		t.Fatalf("before any run, only RunInvoiceMatch is available, got %v", a)
	}
	v := decodeView(t, h.do(http.MethodPost, h.matchPath(), nil, "matcher"))
	if rec := h.do(http.MethodGet, h.matchPath(), nil, "reader"); rec.Code != http.StatusOK || decodeView(t, rec).Run.RunID != v.Run.RunID {
		t.Fatalf("GetMatchResult: %d", rec.Code)
	}
	lineID := h.inv1.Lines[0].InvoiceLineID
	if rec := h.do(http.MethodGet, h.matchPath()+"/lines/"+lineID, nil, "reader"); rec.Code != http.StatusOK {
		t.Fatalf("GetLineMatchDetail: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, h.matchPath()+"/lines/"+uuid.NewString(), nil, "reader"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown line: expected 404, got %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, h.matchPath()+"/runs", nil, "reader"); rec.Code != http.StatusOK {
		t.Fatalf("runs: %d", rec.Code)
	}
	if a := acts(); len(a) != 2 || a[0] != "ReperformInvoiceMatch" || a[1] != "SupersedeMatchRun" {
		t.Fatalf("with a live run: Reperform and Supersede, got %v", a)
	}
	// Reads need the read action.
	h.authz.deny["INVOICE_MATCH_READ"] = true
	if rec := h.do(http.MethodGet, h.matchPath(), nil, "reader"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without read authority, got %d", rec.Code)
	}
	h.authz.deny["INVOICE_MATCH_READ"] = false

	// Supersede needs a reason, then invalidates the clearance.
	if rec := h.do(http.MethodPost, h.matchPath()+"/supersede", map[string]any{}, "controller"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a reason is required, got %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, h.matchPath()+"/supersede", map[string]any{"reason": "PO amended"}, "controller"); rec.Code != http.StatusOK || h.inv1.MatchCleared || h.inv1.MatchState != domain.MatchNotMatched {
		t.Fatalf("supersede must invalidate the clearance, got %d", rec.Code)
	}
	if a := acts(); len(a) != 1 || a[0] != "RunInvoiceMatch" {
		t.Fatalf("after supersede a fresh run is the only action, got %v", a)
	}

	// Policy: the default is shown when none is configured; management is a separate action.
	var p domain.MatchPolicy
	rec := h.do(http.MethodGet, "/v1/match-policies/"+entityA, nil, "reader")
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || !p.IsDefault || p.Mode != domain.MatchThreeWay || p.PriceTolerancePct != 0 {
		t.Fatalf("expected the strict default policy, got %d %+v", rec.Code, p)
	}
	h.authz.deny["MATCH_POLICY_MANAGE"] = true
	if rec := h.do(http.MethodPost, "/v1/match-policies", map[string]any{"legal_entity_id": entityA, "mode": "TWO_WAY"}, "operator"); rec.Code != http.StatusForbidden {
		t.Fatalf("running matches does not confer policy management, got %d", rec.Code)
	}
	h.authz.deny["MATCH_POLICY_MANAGE"] = false
	if rec := h.do(http.MethodPost, "/v1/match-policies", map[string]any{"legal_entity_id": entityA, "mode": "FOUR_WAY"}, "pm"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid policy is a 400, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/v1/match-policies/"+entityA+"?version=7", nil, "reader"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown version: expected 404, got %d", rec.Code)
	}
}

// stableCodeOf reads the stable machine-readable `code` (spec section 16).
func stableCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %s", rec.Body.String())
	}
	return body.Code
}
