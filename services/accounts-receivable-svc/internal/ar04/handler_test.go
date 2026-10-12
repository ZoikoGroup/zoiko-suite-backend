package ar04

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// These handler tests run without a database: every path asserted here is decided before the
// command transaction opens.

func newUnitModule(az kit.Authz, p kit.ProfileResolver) (*Module, http.Handler) {
	m := New(testDeps(az), p, nil, nil, nil, nil, nil)
	return m, testRouter(m)
}

const (
	createPath = "/internal/v1/ar04/commands/createInvoiceDraft"
	base       = "/internal/v1/ar04/"
)

func TestCreate_ProfileMustBeActive(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"inactive":    {kit.Unprocessable(kit.CodeCustomerProfileInactive, "suspended"), http.StatusUnprocessableEntity, kit.CodeCustomerProfileInactive},
		"unavailable": {svcclient.ErrUnavailable, http.StatusServiceUnavailable, kit.CodeDependencyUnavailable},
	}
	for name, c := range cases {
		prof := &fakeProfiles{err: c.err}
		m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof)
		w := newRecorder()
		m.createInvoiceDraft(w, newReq(http.MethodPost, createPath, tenant, pCreator, createBody(ent, party)))
		require.Equal(t, c.status, w.Code, name+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
		require.Equal(t, 1, prof.calls, name)
		require.Equal(t, testNow, prof.at[0], "the profile is resolved at the (injected) clock")
	}
}

func TestCreate_Unauthorized(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof)
	for _, who := range []string{pNobody, pApprover, pIssuer, pDelivery, pCancelReq, pCancelApp, pLinker} {
		w := newRecorder()
		m.createInvoiceDraft(w, newReq(http.MethodPost, createPath, tenant, who, createBody(ent, party)))
		require.Equal(t, http.StatusForbidden, w.Code, who)
		require.Equal(t, kit.CodeForbidden, errCode(t, w.Body.Bytes()))
	}
	require.Zero(t, prof.calls, "authorization is decided before the profile is consulted")

	m2, _ := newUnitModule(&fakeAuthz{err: svcclient.ErrUnavailable}, prof)
	w := newRecorder()
	m2.createInvoiceDraft(w, newReq(http.MethodPost, createPath, tenant, pCreator, createBody(ent, party)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "authorization outage fails closed")
}

func TestCreate_Validation(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof)
	line := func(mm map[string]any) []map[string]any { return []map[string]any{mm} }
	mut := map[string]func(map[string]any){
		"bad currency": func(b map[string]any) { b["currency"] = "dollars" },
		"no lines":     func(b map[string]any) { b["lines"] = []map[string]any{} },
		"zero quantity": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "x", "quantity": "0", "unit_price": "1"})
		},
		"negative price": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "x", "quantity": "1", "unit_price": "-1"})
		},
		"blank description": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "", "quantity": "1", "unit_price": "1"})
		},
		"five decimals": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "x", "quantity": "1", "unit_price": "1.00001"})
		},
		"bad party":       func(b map[string]any) { b["customer_party_id"] = "not-a-uuid" },
		"no supply date":  func(b map[string]any) { delete(b, "supply_date") },
		"bad supply date": func(b map[string]any) { b["supply_date"] = "yesterday" },
		"bad due date":    func(b map[string]any) { b["due_date"] = "soon" },
		"order w/o mile":  func(b map[string]any) { b["order_id"] = uuid.NewString() },
		"bad supply type": func(b map[string]any) {
			b["tax_context"] = map[string]any{"jurisdiction_id": "GB", "supply_type": "B2X"}
		},
		"unknown field": func(b map[string]any) { b["discount_pct"] = "5" },
		"tax_total":     func(b map[string]any) { b["tax_total"] = "10.00" },
		"tax_amount":    func(b map[string]any) { b["tax_amount"] = "10.00" },
		"tax_exempt":    func(b map[string]any) { b["tax_exempt"] = true },
		"line tax_amount": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "x", "quantity": "1", "unit_price": "1", "tax_amount": "5.00"})
		},
		"line tax_rate": func(b map[string]any) {
			b["lines"] = line(map[string]any{"description": "x", "quantity": "1", "unit_price": "1", "tax_rate": "20"})
		},
	}
	for name, f := range mut {
		b := createBody(ent, party)
		f(b)
		w := newRecorder()
		m.createInvoiceDraft(w, newReq(http.MethodPost, createPath, tenant, pCreator, b))
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
	require.Zero(t, prof.calls)
}

func TestCreate_OrderBasisIsCheckedBeforeAnyWrite(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	withOrder := func() map[string]any {
		b := createBody(ent, party)
		b["order_id"], b["milestone_id"] = uuid.NewString(), uuid.NewString()
		return b
	}
	run := func(m *Module, b map[string]any) *http2 {
		w := newRecorder()
		m.createInvoiceDraft(w, newReq(http.MethodPost, createPath, tenant, pCreator, b))
		return &http2{w.Code, w.Body.Bytes()}
	}
	basis := func(party string, ccy string) *fakeOrders {
		return &fakeOrders{basis: &kit.BillingBasis{CustomerParty: party, Currency: ccy, EligibleNet: kit.MustAmount("1250.50")}}
	}
	m := New(testDeps(&fakeAuthz{grants: standardGrants()}), &fakeProfiles{}, nil, nil, nil, nil, nil)
	r := run(m, withOrder())
	require.Equal(t, http.StatusServiceUnavailable, r.code, "an order basis with no AR-03 wired fails closed")

	m = New(testDeps(&fakeAuthz{grants: standardGrants()}), &fakeProfiles{}, &fakeOrders{eligErr: kit.Unprocessable(kit.CodeOrderNotBillable, "cancelled")}, nil, nil, nil, nil)
	r = run(m, withOrder())
	require.Equal(t, http.StatusUnprocessableEntity, r.code)
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, r.body))

	m = New(testDeps(&fakeAuthz{grants: standardGrants()}), &fakeProfiles{}, basis(uuid.NewString(), "USD"), nil, nil, nil, nil)
	r = run(m, withOrder())
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, r.body), "another customer's order is not billable here")

	m = New(testDeps(&fakeAuthz{grants: standardGrants()}), &fakeProfiles{}, basis(party, "EUR"), nil, nil, nil, nil)
	r = run(m, withOrder())
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, r.body), "an order in another currency is not billable here")
}

type http2 struct {
	code int
	body []byte
}

func TestCommands_RequireVersionReasonEvidenceAndWellFormedInput(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	iid := uuid.NewString()
	one := 1
	cases := map[string]struct {
		h    http.HandlerFunc
		body map[string]any
		code string
	}{
		"submit without version":  {m.submitInvoiceForApproval, map[string]any{"invoice_id": iid}, kit.CodeExpectedVersionMissing},
		"approve without version": {m.approveInvoice, map[string]any{"invoice_id": iid, "content_hash": "h"}, kit.CodeExpectedVersionMissing},
		"issue without version":   {m.issueInvoice, map[string]any{"invoice_id": iid}, kit.CodeExpectedVersionMissing},
		"amend without version":   {m.amendInvoiceDraft, map[string]any{"invoice_id": iid, "notes": "x"}, kit.CodeExpectedVersionMissing},
		"cancel without version":  {m.cancelUnissuedInvoice, map[string]any{"invoice_id": iid, "reason_code": "R"}, kit.CodeExpectedVersionMissing},
		"request w/o version": {m.requestLegalCancellation, map[string]any{"invoice_id": iid, "reason_code": "R", "evidence_id": "E"},
			kit.CodeExpectedVersionMissing},
		"decide w/o version": {m.decideLegalCancellation, map[string]any{"invoice_id": iid, "reason_code": "R", "decision": "APPROVE"}, kit.CodeExpectedVersionMissing},
		"link w/o version": {m.linkCorrectionDocument, map[string]any{"invoice_id": iid, "reason_code": "R", "evidence_id": "E", "correction_id": "CN-1", "kind": "CREDIT_NOTE"},
			kit.CodeExpectedVersionMissing},

		"cancel without reason":     {m.cancelUnissuedInvoice, map[string]any{"invoice_id": iid, "expected_version": one}, kit.CodeReasonRequired},
		"request without reason":    {m.requestLegalCancellation, map[string]any{"invoice_id": iid, "expected_version": one, "evidence_id": "E"}, kit.CodeReasonRequired},
		"request without evidence":  {m.requestLegalCancellation, map[string]any{"invoice_id": iid, "expected_version": one, "reason_code": "R"}, kit.CodeEvidenceRequired},
		"decide without reason":     {m.decideLegalCancellation, map[string]any{"invoice_id": iid, "expected_version": one, "decision": "REJECT"}, kit.CodeReasonRequired},
		"decide bad decision":       {m.decideLegalCancellation, map[string]any{"invoice_id": iid, "expected_version": one, "reason_code": "R", "decision": "MAYBE"}, kit.CodeValidationFailed},
		"link without reason":       {m.linkCorrectionDocument, map[string]any{"invoice_id": iid, "expected_version": one, "evidence_id": "E", "correction_id": "C", "kind": "CREDIT_NOTE"}, kit.CodeReasonRequired},
		"link without evidence":     {m.linkCorrectionDocument, map[string]any{"invoice_id": iid, "expected_version": one, "reason_code": "R", "correction_id": "C", "kind": "CREDIT_NOTE"}, kit.CodeEvidenceRequired},
		"approve without hash":      {m.approveInvoice, map[string]any{"invoice_id": iid, "expected_version": one}, kit.CodeValidationFailed},
		"amend nothing":             {m.amendInvoiceDraft, map[string]any{"invoice_id": iid, "expected_version": one}, kit.CodeValidationFailed},
		"amend tax_amount":          {m.amendInvoiceDraft, map[string]any{"invoice_id": iid, "expected_version": one, "tax_amount": "1.00"}, kit.CodeValidationFailed},
		"amend tax_total":           {m.amendInvoiceDraft, map[string]any{"invoice_id": iid, "expected_version": one, "tax_total": "1.00"}, kit.CodeValidationFailed},
		"amend line tax":            {m.amendInvoiceDraft, map[string]any{"invoice_id": iid, "expected_version": one, "lines": []map[string]any{{"description": "x", "quantity": "1", "unit_price": "1", "tax_amount": "1"}}}, kit.CodeValidationFailed},
		"bad invoice id":            {m.submitInvoiceForApproval, map[string]any{"invoice_id": "x", "expected_version": one}, kit.CodeValidationFailed},
		"delivery bad result":       {m.markDeliveryResult, map[string]any{"invoice_id": iid, "result": "NOT_SENT"}, kit.CodeValidationFailed},
		"delivery failed no reason": {m.markDeliveryResult, map[string]any{"invoice_id": iid, "result": "DELIVERY_FAILED"}, kit.CodeValidationFailed},
		"delivery unknown field":    {m.markDeliveryResult, map[string]any{"invoice_id": iid, "result": "SENT", "void": true}, kit.CodeValidationFailed},
		"delivery bad id":           {m.markDeliveryResult, map[string]any{"invoice_id": "x", "result": "SENT"}, kit.CodeValidationFailed},
	}
	for name, c := range cases {
		w := newRecorder()
		c.h(w, newReq(http.MethodPost, "/x", tenant, pCreator, c.body))
		require.GreaterOrEqual(t, w.Code, 400, name+": "+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
	}
}

func TestQueries_RequireTenantAndPrincipal(t *testing.T) {
	m, h := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	_ = m
	for _, q := range []string{"getInvoice", "getInvoiceAsIssued", "getInvoiceStateDimensions", "getAvailableActions", "getInvoiceHistory", "getInvoiceAccountingStatus"} {
		code, body, _ := call(t, h, http.MethodGet, base+q+"?invoice_id="+uuid.NewString(), "", pCreator, "", nil)
		require.Equal(t, http.StatusUnauthorized, code, q+string(body))
		code, body, _ = call(t, h, http.MethodGet, base+q+"?invoice_id="+uuid.NewString(), uuid.NewString(), "", "", nil)
		require.Equal(t, http.StatusUnauthorized, code, q+string(body))
	}
}

func TestQueries_ParameterValidation(t *testing.T) {
	_, h := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	tenant := uuid.NewString()
	for _, qs := range []string{"limit=0", "limit=abc", "after_version=-1", "after_version=x"} {
		code, body, _ := call(t, h, http.MethodGet, base+"getInvoiceHistory?invoice_id="+uuid.NewString()+"&"+qs, tenant, pNobody, "", nil)
		require.Equal(t, http.StatusBadRequest, code, qs+string(body))
		require.Equal(t, kit.CodeValidationFailed, errCode(t, body))
	}
}

func TestRoutes_AllSpecPathsExistAndCommandsNeedIdempotencyKey(t *testing.T) {
	_, h := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	tenant := uuid.NewString()
	for _, name := range []string{"createInvoiceDraft", "amendInvoiceDraft", "submitInvoiceForApproval", "approveInvoice", "issueInvoice", "cancelUnissuedInvoice",
		"requestLegalCancellation", "decideLegalCancellation", "markDeliveryResult", "linkCorrectionDocument"} {
		req := newReq(http.MethodPost, base+"commands/"+name, tenant, pCreator, map[string]any{})
		req.Header.Del("Idempotency-Key")
		w := newRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code, name)
		require.Equal(t, kit.CodeIdempotencyRequired, errCode(t, w.Body.Bytes()), name)
	}
	for _, name := range []string{"getInvoice", "getInvoiceAsIssued", "getInvoiceStateDimensions", "getAvailableActions", "getInvoiceHistory", "getInvoiceAccountingStatus"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			code, _, _ := call(t, h, method, base+name, "", "", "k", nil)
			require.NotEqual(t, http.StatusNotFound, code, name+" "+method)
			require.NotEqual(t, http.StatusMethodNotAllowed, code, name+" "+method)
		}
	}
	code, _, _ := call(t, h, http.MethodGet, base+"commands/issueInvoice", tenant, pCreator, "", nil)
	require.Equal(t, http.StatusMethodNotAllowed, code, "commands are POST only")
}

func TestPortValidationPrecedesAnyDatabaseAccess(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	c := kit.Caller{TenantID: uuid.NewString(), PrincipalID: "ar05", LegalEntityID: uuid.NewString()}
	ctx := context.Background()
	for name, err := range map[string]error{
		"einvoice state":   m.SetEInvoiceState(ctx, nil, c, uuid.NewString(), "PAID"),
		"settlement state": m.SetSettlementState(ctx, nil, c, uuid.NewString(), "ACCEPTED"),
		"dispute state":    m.SetDisputeState(ctx, nil, c, uuid.NewString(), "UNPAID"),
		"link kind":        m.LinkCorrection(ctx, nil, c, uuid.NewString(), "CN-1", "VOID"),
		"link no id":       m.LinkCorrection(ctx, nil, c, uuid.NewString(), " ", "CREDIT_NOTE"),
	} {
		require.Equal(t, kit.CodeValidationFailed, mustProblem(t, err).Code, name)
	}
}
