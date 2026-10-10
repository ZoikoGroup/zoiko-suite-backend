package handler_test

import (
	"net/http"
	"testing"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
)

// These are the pre-governance tests, kept: validation, authorization, tenant
// scope and the legacy direct-issue behaviour that existing consumers rely on.
// The direct-issue path still works, but only with a verifiable approval — the
// default harness supplies an approved procurement case, as
// procurement-workflow-svc does.

func validIssueReq() domain.IssueOrderRequest {
	return domain.IssueOrderRequest{
		TenantID: tenantA, LegalEntityID: entityA, TotalAmount: 50000, CurrencyCode: "USD", CorrelationID: "corr-1",
	}
}

// ── IssueOrder ───────────────────────────────────────────────────────────────

func TestIssueOrder_Success(t *testing.T) {
	e := newEnv()
	rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	o := decode[domain.OrderDetail](t, rec)
	if o.Status != domain.OrderStatusIssued || o.ApprovalBasis != domain.ApprovalBasisProcurementCase || o.ApprovedByPrincipalID == nil {
		t.Fatalf("the legacy issue must record its verified approval: %+v", o.PurchaseOrder)
	}
	if o.Revision != 1 || o.Version != 1 {
		t.Errorf("revision/version = %d/%d, want 1/1", o.Revision, o.Version)
	}
}

func TestIssueOrder_MissingPrincipalHeader_Returns401(t *testing.T) {
	e := newEnv()
	if rec := e.post("/v1/purchase-orders/", validIssueReq(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestIssueOrder_AuthorizationDenied_Returns403(t *testing.T) {
	e := newEnv()
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authorization-svc denies, got %d", rec.Code)
	}
}

func TestIssueOrder_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	e := newEnv()
	e.authz.err = domain.ErrAuthorizationServiceUnavailable
	if rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestIssueOrder_ZeroAmount_Rejected(t *testing.T) {
	e := newEnv()
	req := validIssueReq()
	req.TotalAmount = 0
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount order, got %d", rec.Code)
	}
}

func TestIssueOrder_WithPurchaseRequestID_VerifiesUpstream_Success(t *testing.T) {
	e := newEnv()
	e.pr.summary = &purchaserequest.Summary{RequestID: "pr-1", TenantID: tenantA, LegalEntityID: entityA, Status: "APPROVED"}
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := e.post("/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if o := decode[domain.OrderDetail](t, rec); o.ApprovalBasis != domain.ApprovalBasisPurchaseRequest || o.ApprovalRef != "pr-1" {
		t.Errorf("approval basis = %q ref %q, want PURCHASE_REQUEST/pr-1", o.ApprovalBasis, o.ApprovalRef)
	}
	if e.cases.calls != 0 {
		t.Error("a requisition-backed issue must not also need a procurement case")
	}
}

func TestIssueOrder_WithPurchaseRequestID_NotApproved_Rejected(t *testing.T) {
	e := newEnv()
	e.pr.err = domain.ErrPurchaseRequestNotApproved
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for a non-APPROVED purchase request, got %d", rec.Code)
	}
}

func TestIssueOrder_PurchaseRequestServiceUnavailable_FailsClosed(t *testing.T) {
	e := newEnv()
	e.pr.err = domain.ErrPurchaseRequestServiceUnavailable
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when purchase-request-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

// A replay must resolve to the same order and publish nothing a second time.
func TestIssueOrder_IdempotentReplay_DoesNotRepublish(t *testing.T) {
	e := newEnv()
	rec1 := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first issue, got %d: %s", rec1.Code, rec1.Body.String())
	}
	published := len(e.store.outbox)
	rec2 := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if len(e.store.outbox) != published {
		t.Fatalf("the replay published %d more events", len(e.store.outbox)-published)
	}
	if decode[domain.OrderDetail](t, rec1).PurchaseOrderID != decode[domain.OrderDetail](t, rec2).PurchaseOrderID {
		t.Fatal("the replay resolved to a different order")
	}
}

// ── GetOrder / ListOrders ────────────────────────────────────────────────────

func TestGetOrder_NotFound(t *testing.T) {
	e := newEnv()
	if rec := e.do(http.MethodGet, "/v1/purchase-orders/does-not-exist", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestListOrders_NoTenantScope_Refused replaces a test that asserted a 400 when
// ?tenant_id= was absent — which documented the vulnerability as correct, since
// supplying the parameter was exactly how a caller read another tenant's
// register. The scope now comes from the header, so its absence is the failure.
func TestListOrders_NoTenantScope_Refused(t *testing.T) {
	e := newEnv()
	if rec := e.doAs(http.MethodGet, "/v1/purchase-orders/", nil, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

func TestListOrders_ForeignTenantQueryParam_Refused(t *testing.T) {
	e := newEnv()
	if rec := e.doAs(http.MethodGet, "/v1/purchase-orders/?tenant_id="+tenantB, nil, "", tenantA); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's register, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_OwnTenantQueryParam_Allowed(t *testing.T) {
	e := newEnv()
	if rec := e.doAs(http.MethodGet, "/v1/purchase-orders/?tenant_id="+tenantA, nil, "", tenantA); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when the query param agrees with the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_UnknownStatusFilter_Refused(t *testing.T) {
	e := newEnv()
	if rec := e.do(http.MethodGet, "/v1/purchase-orders/?status=ISUSED", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised status filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_AcceptsEveryNewStatus(t *testing.T) {
	e := newEnv()
	for _, s := range []string{"DRAFT", "PENDING_APPROVAL", "APPROVED", "ISSUED", "ON_HOLD", "CANCELLED", "CLOSED"} {
		if rec := e.do(http.MethodGet, "/v1/purchase-orders/?status="+s, nil, ""); rec.Code != http.StatusOK {
			t.Errorf("status filter %s refused: %d", s, rec.Code)
		}
	}
}

func TestListOrders_MalformedLegalEntityFilter_Refused(t *testing.T) {
	e := newEnv()
	if rec := e.do(http.MethodGet, "/v1/purchase-orders/?legal_entity_id=not-a-uuid", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIssueOrder_ForeignTenantBody_Refused(t *testing.T) {
	e := newEnv()
	req := validIssueReq()
	req.TenantID = tenantB
	if rec := e.doKeyed(http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA, "k1"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 issuing into another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.store.orders) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(e.store.orders))
	}
}

func TestIssueOrder_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	e := newEnv()
	req := validIssueReq()
	req.TenantID = ""
	rec := e.doKeyed(http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA, "k1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[domain.PurchaseOrder](t, rec); got.TenantID != tenantA {
		t.Fatalf("expected the order filed under the verified tenant %s, got %s", tenantA, got.TenantID)
	}
}

func TestIssueOrder_NoTenantScope_Refused(t *testing.T) {
	e := newEnv()
	if rec := e.doKeyed(http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1", "", "k1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.store.orders) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(e.store.orders))
	}
}

func TestIssueOrder_MalformedLegalEntityID_Refused(t *testing.T) {
	e := newEnv()
	req := validIssueReq()
	req.LegalEntityID = "e1"
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The upstream approval gate must be consulted in the caller's VERIFIED tenant.
// It used to be consulted in whatever tenant the body named.
func TestIssueOrder_UpstreamLookupUsesVerifiedTenant(t *testing.T) {
	e := newEnv()
	e.pr.summary = &purchaserequest.Summary{RequestID: "pr-1", TenantID: tenantA, LegalEntityID: entityA, Status: "APPROVED"}
	req := validIssueReq()
	req.TenantID = ""
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := e.doKeyed(http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA, "k1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if e.pr.gotTenantID != tenantA {
		t.Fatalf("expected the purchase-request lookup scoped to the verified tenant %s, got %q", tenantA, e.pr.gotTenantID)
	}
}

// ── legacy amend / close (now governed) ──────────────────────────────────────

// An ISSUED order's total can still be amended with the legacy body, but it is a
// material change: it creates a revision and sends the order back for approval.
func TestAmendOrder_LegacyBody_IsMaterialAndRequiresReapproval(t *testing.T) {
	e := newEnv()
	issued := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))

	rec := e.post("/v1/purchase-orders/"+issued.PurchaseOrderID+"/amend",
		domain.AmendOrderRequest{NewTotalAmount: 60000, Reason: "vendor price change"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		domain.PurchaseOrder
		Material           bool `json:"material_change"`
		RequiresReapproval bool `json:"requires_reapproval"`
		NewRevision        bool `json:"new_revision"`
	}
	got = decode[struct {
		domain.PurchaseOrder
		Material           bool `json:"material_change"`
		RequiresReapproval bool `json:"requires_reapproval"`
		NewRevision        bool `json:"new_revision"`
	}](t, rec)
	if !got.Material || !got.RequiresReapproval || !got.NewRevision {
		t.Fatalf("a price change must be material, need re-approval and create a revision: %+v", got)
	}
	if got.Status != domain.OrderStatusDraft || got.TotalAmount != 60000 || got.Revision != 2 {
		t.Fatalf("after amend: status=%s total=%v revision=%d, want DRAFT/60000/2", got.Status, got.TotalAmount, got.Revision)
	}
	if !e.authz.saw("PO_AMEND") {
		t.Error("amend was not authorized with PO_AMEND")
	}
}

func TestAmendOrder_RequiresReason(t *testing.T) {
	e := newEnv()
	issued := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))
	if rec := e.post("/v1/purchase-orders/"+issued.PurchaseOrderID+"/amend", domain.AmendOrderRequest{NewTotalAmount: 200}, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an amend with no reason, got %d", rec.Code)
	}
}

func TestAmendOrder_Closed_Rejected(t *testing.T) {
	e := newEnv()
	issued := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))
	e.command(t, issued.PurchaseOrderID, "close", "principal-1", nil)
	rec := e.post("/v1/purchase-orders/"+issued.PurchaseOrderID+"/amend", domain.AmendOrderRequest{NewTotalAmount: 200, Reason: "too late"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 amending a CLOSED order, got %d", rec.Code)
	}
}

func TestCloseOrder_FromIssued_Succeeds(t *testing.T) {
	e := newEnv()
	issued := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))
	rec := e.command(t, issued.PurchaseOrderID, "close", "principal-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[domain.OrderDetail](t, rec); got.Status != domain.OrderStatusClosed || got.ClosedByPrincipalID == nil {
		t.Fatalf("expected CLOSED with a closer, got %+v", got.PurchaseOrder)
	}
	if !e.authz.saw("PO_CLOSE") {
		t.Error("close was not authorized with PO_CLOSE")
	}
}

func TestCloseOrder_AlreadyClosed_Rejected(t *testing.T) {
	e := newEnv()
	issued := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))
	e.command(t, issued.PurchaseOrderID, "close", "principal-1", nil)
	if rec := e.command(t, issued.PurchaseOrderID, "close", "principal-1", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 closing an already-CLOSED order, got %d", rec.Code)
	}
}
