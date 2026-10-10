package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/handler"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
	"zoiko.io/purchase-order-svc/internal/supplier"
)

// ── CreatePurchaseOrder (the /draft endpoint purchase-request-svc calls) ──────

func TestCreateDraft_Success_ContractShape(t *testing.T) {
	e := newEnv()
	rec := e.post("/v1/purchase-orders/draft", validDraft(), "maker")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	// purchase-request-svc reads exactly these three keys from the response.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"purchase_order_id", "po_number", "po_status", "lines", "revision", "version", "tenant_id", "legal_entity_id"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response is missing %q: %s", k, rec.Body.String())
		}
	}
	o := decode[domain.OrderDetail](t, rec)
	if o.Status != domain.OrderStatusDraft || o.TotalAmount != 60 || len(o.Lines) != 2 || o.PreparedByPrincipalID != "maker" || o.Revision != 1 {
		t.Errorf("unexpected draft: %+v", o.PurchaseOrder)
	}
	if o.IssuedAt != nil {
		t.Error("a draft must not carry issue evidence")
	}
	if !e.authz.saw("PO_CREATE") {
		t.Error("creation was not authorized with PO_CREATE")
	}
	if len(e.store.outbox) != 1 || e.store.outbox[0] != "PurchaseOrderCreated" {
		t.Errorf("outbox = %v, want [PurchaseOrderCreated]", e.store.outbox)
	}
}

// A retried conversion (same correlation_id) resolves to the SAME order.
func TestCreateDraft_RetryResolvesToSameOrder(t *testing.T) {
	e := newEnv()
	req := validDraft()
	first := e.post("/v1/purchase-orders/draft", req, "maker")
	again := e.post("/v1/purchase-orders/draft", req, "maker")
	if first.Code != http.StatusCreated || again.Code != http.StatusOK {
		t.Fatalf("want 201 then 200, got %d then %d", first.Code, again.Code)
	}
	if decode[domain.OrderDetail](t, first).PurchaseOrderID != decode[domain.OrderDetail](t, again).PurchaseOrderID {
		t.Fatal("the retry created a second order")
	}
	if len(e.store.orders) != 1 {
		t.Fatalf("%d orders exist, want 1", len(e.store.orders))
	}
}

func TestCreateDraft_Validation(t *testing.T) {
	cases := map[string]func(*domain.CreateDraftRequest){
		"no lines":           func(r *domain.CreateDraftRequest) { r.Lines = nil },
		"no supplier":        func(r *domain.CreateDraftRequest) { r.SupplierRef = " " },
		"bad currency":       func(r *domain.CreateDraftRequest) { r.CurrencyCode = "DOLLARS" },
		"no correlation":     func(r *domain.CreateDraftRequest) { r.CorrelationID = "" },
		"bad entity":         func(r *domain.CreateDraftRequest) { r.LegalEntityID = "nope" },
		"zero quantity":      func(r *domain.CreateDraftRequest) { r.Lines[0].Quantity = 0 },
		"negative price":     func(r *domain.CreateDraftRequest) { r.Lines[0].UnitPrice = -1 },
		"no uom":             func(r *domain.CreateDraftRequest) { r.Lines[0].UOM = "" },
		"no item or text":    func(r *domain.CreateDraftRequest) { r.Lines[0].ItemRef, r.Lines[0].Description = "", "" },
		"line amount lies":   func(r *domain.CreateDraftRequest) { r.Lines[0].LineAmount = 1 },
		"duplicate line no.": func(r *domain.CreateDraftRequest) { r.Lines[0].LineNumber, r.Lines[1].LineNumber = 3, 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			req := validDraft()
			mutate(&req)
			rec := e.post("/v1/purchase-orders/draft", req, "maker")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(e.store.orders) != 0 {
				t.Fatal("an invalid draft was stored")
			}
		})
	}
}

func TestCreateDraft_Denied_And_UnverifiedRequisition(t *testing.T) {
	e := newEnv()
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := e.post("/v1/purchase-orders/draft", validDraft(), "maker"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}

	e = newEnv()
	e.pr.err = domain.ErrPurchaseRequestNotApproved
	req := validDraft()
	id := "pr-1"
	req.PurchaseRequestID = &id
	if rec := e.post("/v1/purchase-orders/draft", req, "maker"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a draft citing an unapproved requisition: expected 422, got %d", rec.Code)
	}
}

// ── supplier eligibility (AP-01) ─────────────────────────────────────────────

// Negative paths #3/#4: inactive/held supplier used for a new PO.
func TestSupplierEligibility_BlocksCreateSubmitAndIssue(t *testing.T) {
	// create
	e := newEnv()
	e.supplier.set("SUP-1", supplier.Eligibility{Status: "RETIRED"})
	rec := e.post("/v1/purchase-orders/draft", validDraft(), "maker")
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "SUPPLIER_NOT_ELIGIBLE" {
		t.Fatalf("create to an inactive supplier: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.orders) != 0 {
		t.Fatal("an order was created for an ineligible supplier")
	}

	// A supplier that goes on hold AFTER the draft is blocked at submit and at issue.
	e = newEnv()
	o := e.createDraft(t)
	e.supplier.set("SUP-1", supplier.Eligibility{Status: "ON_HOLD", IsOnHold: true})
	if rec := e.command(t, o.PurchaseOrderID, "submit", "maker", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("submit to a held supplier: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	e.supplier.active("SUP-1")
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	e.command(t, o.PurchaseOrderID, "approve", "checker", domain.CommandRequest{ExpectedVersion: version(decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, "")))})
	e.supplier.set("SUP-1", supplier.Eligibility{Status: "ON_HOLD", IsOnHold: true})
	rec = e.command(t, o.PurchaseOrderID, "issue", "buyer", nil)
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "SUPPLIER_NOT_ELIGIBLE" {
		t.Fatalf("issue to a held supplier without an exception: %d %s", rec.Code, rec.Body.String())
	}
	if got := decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, "")); got.Status != domain.OrderStatusApproved {
		t.Errorf("a refused issue changed the status to %s", got.Status)
	}
}

func TestSupplierEligibility_HeldSupplierExceptionNeedsPermission(t *testing.T) {
	e := newEnv()
	e.supplier.set("SUP-1", supplier.Eligibility{Status: "ON_HOLD", IsOnHold: true})

	// An exception reference alone is not enough: the caller needs PO_SUPPLIER_EXCEPTION.
	e.authz.err, e.authz.denyOnly = domain.ErrAuthorizationDenied, "PO_SUPPLIER_EXCEPTION"
	req := validDraft()
	req.SupplierExceptionRef = "EXC-1"
	if rec := e.post("/v1/purchase-orders/draft", req, "maker"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("exception without the permission: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	e.authz.err = nil
	req = validDraft()
	req.SupplierExceptionRef = "EXC-1"
	rec := e.post("/v1/purchase-orders/draft", req, "maker")
	if rec.Code != http.StatusCreated {
		t.Fatalf("exception with the permission: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if o := decode[domain.OrderDetail](t, rec); o.SupplierExceptionRef != "EXC-1" || o.SupplierExceptionBy != "maker" {
		t.Errorf("the exception was not recorded with who granted it: %+v", o.PurchaseOrder)
	}

	// The exception only covers a supplier ON HOLD — never an inactive one.
	e.supplier.set("SUP-1", supplier.Eligibility{Status: "RETIRED"})
	req = validDraft()
	req.SupplierExceptionRef = "EXC-1"
	if rec := e.post("/v1/purchase-orders/draft", req, "maker"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an exception must not unlock a retired supplier: got %d", rec.Code)
	}
}

func TestSupplierEligibility_FailsClosed(t *testing.T) {
	e := newEnv()
	e.supplier.err = domain.ErrSupplierServiceUnavailable
	if rec := e.post("/v1/purchase-orders/draft", validDraft(), "maker"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("AP-01 down: expected 503, got %d", rec.Code)
	}
	e = newEnv()
	if rec := e.post("/v1/purchase-orders/draft", func() domain.CreateDraftRequest { r := validDraft(); r.SupplierRef = "GHOST"; return r }(), "maker"); rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "SUPPLIER_UNKNOWN" {
		t.Fatalf("a supplier with no profile: got %d", rec.Code)
	}
	// A deployment that forgot to wire the supplier client must not silently skip the check.
	bare := handler.New(newStubStore(), &stubAuthZ{}, &stubPRClient{}, nil)
	_ = bare
	e2 := newEnv()
	e2.supplier.answers = map[string]supplier.Eligibility{}
	e2.supplier.err = domain.ErrSupplierServiceUnavailable
	if rec := e2.post("/v1/purchase-orders/draft", validDraft(), "maker"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

// ── submit / approve (maker-checker) ─────────────────────────────────────────

func TestApprove_RequiresExpectedVersion(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	rec := e.command(t, o.PurchaseOrderID, "approve", "checker", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve without expected_version: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if e.store.transitionCalls != 1 { // only the submit reached the store
		t.Errorf("the refused approval reached the store")
	}
}

func TestApprove_StaleVersion_Refused(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	stale := o.Version // the version BEFORE submit
	rec := e.command(t, o.PurchaseOrderID, "approve", "checker", domain.CommandRequest{ExpectedVersion: &stale})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("approving a stale version: %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path #5/#29: maker approves own PO, and the preparer is the "owner"
// authorization-svc's own-object SoD layer is asked about.
func TestApprove_PreparerRefused_ByAuthorizationSvcAndByStore(t *testing.T) {
	e := newEnv()
	e.authz.sodRules = true
	o := e.createDraft(t)
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	cur := decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, ""))

	rec := e.command(t, o.PurchaseOrderID, "approve", "maker", domain.CommandRequest{ExpectedVersion: version(cur)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the preparer approving: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if e.authz.owners["PO_APPROVE"] != "maker" {
		t.Errorf("the own-object check must name the preparer as owner, got %q", e.authz.owners["PO_APPROVE"])
	}

	// Even if authorization-svc's layer were off, the store refuses the submitter.
	e.authz.sodRules = false
	e.command(t, o.PurchaseOrderID, "submit", "x", nil) // no-op: already pending
	rec = e.command(t, o.PurchaseOrderID, "approve", "maker", domain.CommandRequest{ExpectedVersion: version(cur)})
	if rec.Code != http.StatusForbidden || codeOf(t, rec) != "SOD_CONFLICT" {
		t.Fatalf("the store's own maker-checker rule: %d %s", rec.Code, rec.Body.String())
	}
	if got := decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, "")); got.Status != domain.OrderStatusPendingApproval {
		t.Errorf("a refused approval changed the status to %s", got.Status)
	}

	rec = e.command(t, o.PurchaseOrderID, "approve", "checker", domain.CommandRequest{ExpectedVersion: version(cur)})
	if rec.Code != http.StatusOK {
		t.Fatalf("an independent approver: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The approval authority tier: above the threshold, the high-value permission.
func TestApprove_HighValueThreshold_RequiresHighValueAction(t *testing.T) {
	e := newEnv(handler.Options{HighValueThreshold: 50})
	o := e.createDraft(t) // total 60 > 50
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	cur := decode[domain.OrderDetail](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, ""))

	e.authz.err, e.authz.denyOnly = domain.ErrAuthorizationDenied, "PO_APPROVE_HIGHVALUE"
	if rec := e.command(t, o.PurchaseOrderID, "approve", "checker", domain.CommandRequest{ExpectedVersion: version(cur)}); rec.Code != http.StatusForbidden {
		t.Fatalf("a signer without the high-value permission: expected 403, got %d", rec.Code)
	}
	if e.authz.saw("PO_APPROVE") {
		t.Error("above the threshold the ordinary PO_APPROVE must not be what is checked")
	}
	e.authz.err = nil
	if rec := e.command(t, o.PurchaseOrderID, "approve", "checker", domain.CommandRequest{ExpectedVersion: version(cur)}); rec.Code != http.StatusOK {
		t.Fatalf("with the high-value permission: %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path #1 (AP-03): no path to ISSUED without approval.
func TestIssue_WithoutApproval_Refused(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	for _, step := range []string{"issue", "approve"} {
		var body any
		if step == "approve" {
			body = domain.CommandRequest{ExpectedVersion: version(o)}
		}
		if rec := e.command(t, o.PurchaseOrderID, step, "checker", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s from DRAFT: expected 422, got %d", step, rec.Code)
		}
	}
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	if rec := e.command(t, o.PurchaseOrderID, "issue", "buyer", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("issue from PENDING_APPROVAL: expected 422, got %d", rec.Code)
	}
}

func TestFullGovernedFlow_RecordsEvidenceAndEvents(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	if o.Status != domain.OrderStatusIssued || o.ApprovalBasis != domain.ApprovalBasisWorkflow ||
		o.ApprovedByPrincipalID == nil || *o.ApprovedByPrincipalID != "checker" || o.IssuedByPrincipalID != "buyer" {
		t.Fatalf("unexpected issued order: %+v", o.PurchaseOrder)
	}
	want := []string{"PurchaseOrderCreated", "PurchaseOrderSubmitted", "PurchaseOrderApproved", "PurchaseOrderIssued", "legacy-alias"}
	if strings.Join(e.store.outbox, ",") != strings.Join(want, ",") {
		t.Errorf("outbox = %v, want %v", e.store.outbox, want)
	}
	for _, a := range []string{"PO_CREATE", "PO_SUBMIT", "PO_APPROVE", "PO_ISSUE"} {
		if !e.authz.saw(a) {
			t.Errorf("%s was never checked", a)
		}
	}
	hist := decode[[]domain.OrderEvent](t, e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/history", nil, ""))
	if len(hist) != 4 {
		t.Errorf("history has %d rows, want 4", len(hist))
	}
}

// ── hold / release / cancel / close ──────────────────────────────────────────

func TestHoldReleaseCancel(t *testing.T) {
	e := newEnv()
	o := e.issued(t)

	if rec := e.command(t, o.PurchaseOrderID, "hold", "ops", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("hold without a reason: expected 400, got %d", rec.Code)
	}
	rec := e.command(t, o.PurchaseOrderID, "hold", "ops", domain.CommandRequest{Reason: "supplier dispute"})
	held := decode[domain.OrderDetail](t, rec)
	if rec.Code != http.StatusOK || held.Status != domain.OrderStatusOnHold || held.HoldReason != "supplier dispute" || held.HeldFromStatus != "ISSUED" {
		t.Fatalf("hold: %d %+v", rec.Code, held.PurchaseOrder)
	}
	// Receipts/invoices are refused while held (contract: only ISSUED accepts them).
	line := held.Lines[0].LineID
	prog := e.post("/v1/purchase-orders/"+o.PurchaseOrderID+"/lines/"+line+"/progress",
		domain.ProgressRequest{Kind: "RECEIVED", Quantity: 1, SourceRef: "r1", DeltaSign: 1}, "ap04")
	if prog.Code != http.StatusConflict || codeOf(t, prog) != "ORDER_NOT_ISSUED" {
		t.Errorf("progress on a held order: %d %s", prog.Code, prog.Body.String())
	}
	if rec := e.command(t, o.PurchaseOrderID, "close", "ops", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("close while held: expected 422, got %d", rec.Code)
	}

	rel := e.command(t, o.PurchaseOrderID, "release-hold", "ops", nil)
	if got := decode[domain.OrderDetail](t, rel); rel.Code != http.StatusOK || got.Status != domain.OrderStatusIssued || got.HoldReason != "" {
		t.Fatalf("release: %d %+v", rel.Code, got.PurchaseOrder)
	}

	if rec := e.command(t, o.PurchaseOrderID, "cancel", "ops", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("cancel without a reason: expected 400, got %d", rec.Code)
	}
	c := e.command(t, o.PurchaseOrderID, "cancel", "ops", domain.CommandRequest{Reason: "no longer needed"})
	if got := decode[domain.OrderDetail](t, c); c.Code != http.StatusOK || got.Status != domain.OrderStatusCancelled || got.CancellationReason != "no longer needed" {
		t.Fatalf("cancel: %d %+v", c.Code, got.PurchaseOrder)
	}
	if rec := e.command(t, o.PurchaseOrderID, "release-hold", "ops", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("anything after cancel: expected 422, got %d", rec.Code)
	}
	for _, a := range []string{"PO_HOLD", "PO_CANCEL"} {
		if !e.authz.saw(a) {
			t.Errorf("%s was never checked", a)
		}
	}
}

func TestCancel_WithReceipts_Conflict(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	e.store.transitionErr = domain.ErrHasProgress
	rec := e.command(t, o.PurchaseOrderID, "cancel", "ops", domain.CommandRequest{Reason: "x"})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "HAS_PROGRESS" {
		t.Fatalf("cancel with receipts: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCommands_UnknownOrder_NotFound_AndDenied(t *testing.T) {
	e := newEnv()
	if rec := e.command(t, "00000000-0000-0000-0000-000000000000", "submit", "maker", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown order: expected 404, got %d", rec.Code)
	}
	o := e.createDraft(t)
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := e.command(t, o.PurchaseOrderID, "submit", "maker", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("denied: expected 403, got %d", rec.Code)
	}
	e.authz.err = domain.ErrAuthorizationServiceUnavailable
	if rec := e.command(t, o.PurchaseOrderID, "submit", "maker", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz down: expected 503 (fail closed), got %d", rec.Code)
	}
	e.authz.err = nil
	if rec := e.command(t, o.PurchaseOrderID, "submit", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", rec.Code)
	}
}

func TestSubmit_NoLines_Unprocessable(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	e.store.orders[o.PurchaseOrderID].Lines = nil
	e.store.orders[o.PurchaseOrderID].TotalAmount = 0
	rec := e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "NO_LINES" {
		t.Fatalf("submit with nothing to commit to: %d %s", rec.Code, rec.Body.String())
	}
}

// ── amendments & revisions ───────────────────────────────────────────────────

func TestAmend_MaterialChange_ReapprovalAndRevisionEndpoints(t *testing.T) {
	e := newEnv()
	o := e.issued(t)

	bigger := []domain.LineInput{
		{LineNumber: 1, ItemRef: "SKU-1", Quantity: 20, UnitPrice: 5, UOM: "EA"}, // 10 -> 20
		{LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"},
	}
	rec := e.post("/v1/purchase-orders/"+o.PurchaseOrderID+"/amend", domain.AmendOrderRequest{Lines: bigger, Reason: "more bolts"}, "amender")
	if rec.Code != http.StatusOK {
		t.Fatalf("amend: %d %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["requires_reapproval"] != true || res["material_change"] != true || res["po_status"] != "DRAFT" {
		t.Fatalf("a quantity increase after approval must require re-approval: %v", res)
	}

	// Cannot issue without re-approving (negative path #8).
	if rec := e.command(t, o.PurchaseOrderID, "issue", "buyer", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("issue after a material amendment: expected 422, got %d", rec.Code)
	}

	// The superseded revision is preserved and readable (GetPORevision).
	list := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/revisions", nil, "")
	var lr struct {
		Current   int               `json:"current_revision"`
		Revisions []json.RawMessage `json:"revisions"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &lr)
	if lr.Current != 2 || len(lr.Revisions) != 1 {
		t.Fatalf("revisions: current=%d n=%d, want 2 and 1", lr.Current, len(lr.Revisions))
	}
	old := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/revisions/1", nil, "")
	var ov struct {
		Current  bool `json:"current"`
		Snapshot struct {
			Total float64 `json:"total_amount"`
			Lines []struct {
				Quantity float64 `json:"quantity"`
			} `json:"lines"`
		} `json:"snapshot"`
	}
	_ = json.Unmarshal(old.Body.Bytes(), &ov)
	if old.Code != http.StatusOK || ov.Current || ov.Snapshot.Total != 60 || ov.Snapshot.Lines[0].Quantity != 10 {
		t.Fatalf("revision 1 must hold the ORIGINAL issued content: %d %s", old.Code, old.Body.String())
	}
	cur := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/revisions/2", nil, "")
	if !strings.Contains(cur.Body.String(), `"current":true`) {
		t.Errorf("revision 2 should be served as the current one: %s", cur.Body.String())
	}
	if rec := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/revisions/9", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown revision: expected 404, got %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/revisions/abc", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed revision: expected 400, got %d", rec.Code)
	}
}

func TestAmend_Validation_NoChange_AndSupplierRecheck(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	path := "/v1/purchase-orders/" + o.PurchaseOrderID + "/amend"

	cases := map[string]domain.AmendOrderRequest{
		"empty":            {Reason: "x"},
		"bad lines":        {Reason: "x", Lines: []domain.LineInput{{ItemRef: "a", Quantity: 0, UnitPrice: 1, UOM: "EA"}}},
		"bad currency":     {Reason: "x", CurrencyCode: strPtr("DOLLARS")},
		"empty supplier":   {Reason: "x", SupplierRef: strPtr(" ")},
		"negative total":   {Reason: "x", NewTotalAmount: -5},
		"no reason (body)": {NewTotalAmount: 5},
	}
	for name, req := range cases {
		if rec := e.post(path, req, "amender"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}

	// Switching to an ineligible supplier is refused before the order changes.
	e.supplier.set("SUP-2", supplier.Eligibility{Status: "SUSPENDED"})
	if rec := e.post(path, domain.AmendOrderRequest{Reason: "switch", SupplierRef: strPtr("SUP-2")}, "amender"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("amend to an ineligible supplier: expected 422, got %d", rec.Code)
	}
	if e.store.amendCalls != 0 {
		t.Error("an invalid amendment reached the store")
	}
	e.store.amendErr = domainNoChange()
	if rec := e.post(path, domain.AmendOrderRequest{Reason: "x", NewTotalAmount: 60}, "amender"); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "NO_CHANGE" {
		t.Errorf("a no-op amendment: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAmend_BelowProgress_Conflict(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	e.store.amendErr = domain.ErrAmendmentBelowProgress
	rec := e.post("/v1/purchase-orders/"+o.PurchaseOrderID+"/amend", domain.AmendOrderRequest{Reason: "cut", NewTotalAmount: 1}, "amender")
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "AMENDMENT_BELOW_PROGRESS" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ── reads: the cross-service contract ────────────────────────────────────────

// Contract #2: GET adds `revision` and `lines`; po_status is ISSUED while open.
func TestGetOrder_ContractShape_LinesAndRevision(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	rec := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil, "")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"purchase_order_id", "tenant_id", "legal_entity_id", "po_status", "total_amount", "currency_code", "revision", "version", "lines"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("GET is missing %q", k)
		}
	}
	var lines []map[string]any
	_ = json.Unmarshal(raw["lines"], &lines)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	for _, k := range []string{"line_id", "line_number", "item_ref", "description", "quantity", "unit_price", "uom", "line_amount"} {
		if _, ok := lines[0][k]; !ok {
			t.Errorf("a line is missing %q: %v", k, lines[0])
		}
	}
	var status string
	_ = json.Unmarshal(raw["po_status"], &status)
	if status != "ISSUED" {
		t.Errorf("po_status = %q, want ISSUED", status)
	}
}

// A legacy header-only PO still answers with `lines`, as an empty array.
func TestGetOrder_LegacyHeaderOnly_HasEmptyLinesArray(t *testing.T) {
	e := newEnv()
	legacy := decode[domain.OrderDetail](t, e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"))
	rec := e.do(http.MethodGet, "/v1/purchase-orders/"+legacy.PurchaseOrderID, nil, "")
	if !strings.Contains(rec.Body.String(), `"lines":[]`) {
		t.Errorf("expected an empty lines array, got %s", rec.Body.String())
	}
}

func TestOpenQuantity_And_Statuses(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	rec := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/open-quantity", nil, "")
	var oq struct {
		Lines []map[string]any `json:"lines"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &oq)
	if rec.Code != http.StatusOK || len(oq.Lines) != 2 {
		t.Fatalf("open-quantity: %d %s", rec.Code, rec.Body.String())
	}
	for _, k := range []string{"line_id", "ordered_quantity", "received_quantity", "invoiced_quantity", "open_receipt_quantity", "open_invoice_quantity"} {
		if _, ok := oq.Lines[0][k]; !ok {
			t.Errorf("open-quantity line is missing %q", k)
		}
	}
	for _, p := range []string{"receipt-status", "invoice-status"} {
		r := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/"+p, nil, "")
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"status":"NOT_STARTED"`) {
			t.Errorf("%s: %d %s", p, r.Code, r.Body.String())
		}
	}
	for _, p := range []string{"open-quantity", "receipt-status", "invoice-status"} {
		if r := e.do(http.MethodGet, "/v1/purchase-orders/00000000-0000-0000-0000-000000000000/"+p, nil, ""); r.Code != http.StatusNotFound {
			t.Errorf("%s for an unknown order: expected 404, got %d", p, r.Code)
		}
	}
}

func TestAvailableActions_FollowStateAndMakerChecker(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	actions := func(principal string) []string {
		rec := e.do(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID+"/available-actions", nil, principal)
		var v struct {
			A []string `json:"available_actions"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v.A
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	if a := actions("maker"); !has(a, "SubmitPOForApproval") || has(a, "IssuePurchaseOrder") || has(a, "ApprovePurchaseOrder") {
		t.Errorf("DRAFT actions: %v", a)
	}
	e.command(t, o.PurchaseOrderID, "submit", "maker", nil)
	if a := actions("maker"); has(a, "ApprovePurchaseOrder") {
		t.Errorf("the preparer must not be offered Approve: %v", a)
	}
	if a := actions("checker"); !has(a, "ApprovePurchaseOrder") {
		t.Errorf("an independent principal must be offered Approve: %v", a)
	}
}

// ── progress (the AP-04 / AP-05 push contract) ───────────────────────────────

func TestProgress_Contract_ReplayExceedsAndValidation(t *testing.T) {
	e := newEnv()
	o := e.issued(t)
	path := "/v1/purchase-orders/" + o.PurchaseOrderID + "/lines/" + o.Lines[0].LineID + "/progress"
	push := domain.ProgressRequest{Kind: "RECEIVED", Quantity: 6, Amount: 30, SourceRef: "rcpt-1", DeltaSign: 1}

	first := e.post(path, push, "ap04")
	if first.Code != http.StatusCreated {
		t.Fatalf("first push: %d %s", first.Code, first.Body.String())
	}
	if again := e.post(path, push, "ap04"); again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"replayed":true`) {
		t.Fatalf("replay: %d %s", again.Code, again.Body.String())
	}
	// AP-04 keys on the 409 + code to stop retrying: it is permanent.
	over := e.post(path, domain.ProgressRequest{Kind: "RECEIVED", Quantity: 6, SourceRef: "rcpt-2", DeltaSign: 1}, "ap04")
	if over.Code != http.StatusConflict || codeOf(t, over) != "PROGRESS_EXCEEDS_ORDER" {
		t.Fatalf("over-receipt: %d %s", over.Code, over.Body.String())
	}
	if reused := e.post(path, domain.ProgressRequest{Kind: "RECEIVED", Quantity: 5, SourceRef: "rcpt-1", DeltaSign: 1}, "ap04"); reused.Code != http.StatusConflict || codeOf(t, reused) != "PROGRESS_REF_REUSED" {
		t.Fatalf("reused ref: %d %s", reused.Code, reused.Body.String())
	}

	bad := map[string]domain.ProgressRequest{
		"bad kind":      {Kind: "SHIPPED", Quantity: 1, SourceRef: "x", DeltaSign: 1},
		"zero quantity": {Kind: "RECEIVED", Quantity: 0, SourceRef: "x", DeltaSign: 1},
		"bad sign":      {Kind: "RECEIVED", Quantity: 1, SourceRef: "x", DeltaSign: 2},
		"no source_ref": {Kind: "RECEIVED", Quantity: 1, DeltaSign: 1},
		"negative amt":  {Kind: "RECEIVED", Quantity: 1, Amount: -1, SourceRef: "x", DeltaSign: 1},
	}
	for name, req := range bad {
		if rec := e.post(path, req, "ap04"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, rec.Code)
		}
	}
	if rec := e.post("/v1/purchase-orders/"+o.PurchaseOrderID+"/lines/not-a-uuid/progress", push, "ap04"); rec.Code != http.StatusNotFound {
		t.Errorf("malformed line id: expected 404, got %d", rec.Code)
	}
	if rec := e.post("/v1/purchase-orders/"+o.PurchaseOrderID+"/lines/00000000-0000-0000-0000-000000000000/progress", domain.ProgressRequest{Kind: "RECEIVED", Quantity: 1, SourceRef: "z", DeltaSign: 1}, "ap04"); rec.Code != http.StatusNotFound || codeOf(t, rec) != "LINE_NOT_FOUND" {
		t.Errorf("unknown line: %d %s", rec.Code, rec.Body.String())
	}
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := e.post(path, domain.ProgressRequest{Kind: "RECEIVED", Quantity: 1, SourceRef: "q", DeltaSign: 1}, "ap04"); rec.Code != http.StatusForbidden {
		t.Errorf("progress without permission: expected 403, got %d", rec.Code)
	}
}

// ── idempotency & error codes ────────────────────────────────────────────────

// An Idempotency-Key replay returns the stored answer and does not run the
// command again.
func TestIdempotencyKey_ReplayDoesNotRunAgain(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	path := "/v1/purchase-orders/" + o.PurchaseOrderID + "/submit"

	first := e.doKeyed(http.MethodPost, path, nil, "maker", tenantA, "same-key")
	calls := e.store.transitionCalls
	replay := e.doKeyed(http.MethodPost, path, nil, "maker", tenantA, "same-key")
	if first.Code != http.StatusOK || replay.Code != http.StatusOK {
		t.Fatalf("codes %d / %d", first.Code, replay.Code)
	}
	if replay.Header().Get("Idempotent-Replay") != "true" || replay.Body.String() != first.Body.String() {
		t.Errorf("replay header=%q body-equal=%v", replay.Header().Get("Idempotent-Replay"), replay.Body.String() == first.Body.String())
	}
	if e.store.transitionCalls != calls {
		t.Fatal("the replay ran the command again")
	}

	// Same key, different request.
	other := e.createDraft(t)
	rec := e.doKeyed(http.MethodPost, "/v1/purchase-orders/"+other.PurchaseOrderID+"/submit", nil, "maker", tenantA, "same-key")
	if rec.Code == http.StatusOK && rec.Header().Get("Idempotent-Replay") == "true" {
		t.Fatal("a key for one order replayed another order's answer")
	}
	body := e.doKeyed(http.MethodPost, path, domain.CommandRequest{Reason: "different body"}, "maker", tenantA, "same-key")
	if body.Code != http.StatusUnprocessableEntity || codeOf(t, body) != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("same key, different body: %d %s", body.Code, body.Body.String())
	}
}

// Server failures and permission refusals are not cached: the retry runs again.
func TestIdempotencyKey_FailuresAreNotCached(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	path := "/v1/purchase-orders/" + o.PurchaseOrderID + "/submit"

	e.store.transitionErr = domain.ErrStoreUnavailable
	if rec := e.doKeyed(http.MethodPost, path, nil, "maker", tenantA, "retry-key"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	e.store.transitionErr = nil
	if rec := e.doKeyed(http.MethodPost, path, nil, "maker", tenantA, "retry-key"); rec.Code != http.StatusOK {
		t.Fatalf("the retry after a 503 must run the command: got %d %s", rec.Code, rec.Body.String())
	}

	o2 := e.createDraft(t)
	path2 := "/v1/purchase-orders/" + o2.PurchaseOrderID + "/submit"
	e.authz.err = domain.ErrAuthorizationDenied
	if rec := e.doKeyed(http.MethodPost, path2, nil, "maker", tenantA, "perm-key"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	e.authz.err = nil
	if rec := e.doKeyed(http.MethodPost, path2, nil, "maker", tenantA, "perm-key"); rec.Code != http.StatusOK {
		t.Fatalf("a stored 403 must not outlive the permission being granted: got %d", rec.Code)
	}
}

func TestErrors_CarryStableCodes(t *testing.T) {
	e := newEnv()
	cases := []struct {
		name, want string
		rec        func() *httpRec
	}{
		{"validation", "INVALID_LINE", func() *httpRec {
			r := validDraft()
			r.Lines = nil
			return e.post("/v1/purchase-orders/draft", r, "maker")
		}},
		{"not found", "ORDER_NOT_FOUND", func() *httpRec { return e.do(http.MethodGet, "/v1/purchase-orders/nope", nil, "") }},
		{"tenant", "TENANT_SCOPE_MISSING", func() *httpRec { return e.doAs(http.MethodGet, "/v1/purchase-orders/", nil, "", "") }},
		{"identity", "IDENTITY_MISSING", func() *httpRec { return e.post("/v1/purchase-orders/draft", validDraft(), "") }},
	}
	for _, c := range cases {
		rec := c.rec()
		if got := codeOf(t, rec); got != c.want {
			t.Errorf("%s: code = %q, want %q (%d %s)", c.name, got, c.want, rec.Code, rec.Body.String())
		}
	}
	// The pre-existing `error` field is unchanged for consumers that read it.
	rec := e.do(http.MethodGet, "/v1/purchase-orders/nope", nil, "")
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "order_not_found" {
		t.Errorf("the existing error field changed: %v", body)
	}
}

// ── legacy direct issue: approval is no longer optional ──────────────────────

// A PO cannot reach ISSUED without a verifiable approval — including via the
// legacy endpoint.
func TestLegacyIssue_NoVerifiableApproval_Refused(t *testing.T) {
	e := newEnv()
	e.cases.err = domain.ErrProcurementCaseNotApproved
	rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "PO_APPROVAL_REQUIRED" {
		t.Fatalf("an unapproved procurement case: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.orders) != 0 {
		t.Fatal("an unapproved order was stored")
	}

	e.cases.err = domain.ErrProcurementCaseMismatch
	if rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a case that does not match the order: expected 422, got %d", rec.Code)
	}
	e.cases.err = domain.ErrProcurementCaseServiceUnavailable
	if rec := e.post("/v1/purchase-orders/", validIssueReq(), "principal-1"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("procurement-workflow-svc down: expected 503 (fail closed), got %d", rec.Code)
	}

	// No client wired and no requisition: nothing can verify the approval.
	bare := newEnv()
	h := handler.New(bare.store, bare.authz, bare.pr, nil).WithSupplierClient(bare.supplier)
	_ = h
	noCases := newEnvWithoutCases()
	if rec := noCases.post("/v1/purchase-orders/", validIssueReq(), "principal-1"); rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "PO_APPROVAL_REQUIRED" {
		t.Errorf("no way to verify an approval: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyIssue_WithLines_AndSupplier(t *testing.T) {
	e := newEnv()
	req := validIssueReq()
	req.SupplierRef = "SUP-1"
	req.Lines = twoLines()
	req.TotalAmount = 60
	rec := e.post("/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if o := decode[domain.OrderDetail](t, rec); o.TotalAmount != 60 || len(o.Lines) != 2 || o.Status != domain.OrderStatusIssued {
		t.Errorf("%+v", o.PurchaseOrder)
	}

	req.CorrelationID = "other"
	req.TotalAmount = 999
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusBadRequest {
		t.Errorf("a total that disagrees with the lines: expected 400, got %d", rec.Code)
	}

	e.supplier.set("SUP-1", supplier.Eligibility{Status: "ON_HOLD", IsOnHold: true})
	req.TotalAmount = 60
	req.CorrelationID = "held"
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("legacy issue to a held supplier: expected 422, got %d", rec.Code)
	}
}

func TestLegacyIssue_RequisitionVerifiedInVerifiedTenant_NoProcurementCaseCall(t *testing.T) {
	e := newEnv()
	e.pr.summary = &purchaserequest.Summary{RequestID: "pr-9", Status: "APPROVED"}
	req := validIssueReq()
	id := "pr-9"
	req.PurchaseRequestID = &id
	if rec := e.post("/v1/purchase-orders/", req, "principal-1"); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if e.cases.calls != 0 {
		t.Error("procurement-workflow-svc was called although a requisition backs the order")
	}
}

func strPtr(s string) *string { return &s }

func domainNoChange() error { return storeErrNoChange }
