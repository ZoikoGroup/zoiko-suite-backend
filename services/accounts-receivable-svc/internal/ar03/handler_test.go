package ar03

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// These handler tests run without a database: every path asserted here is decided before
// the command transaction opens.

func newUnitModule(az kit.Authz, p kit.ProfileResolver) (*Module, http.Handler) {
	m := New(testDeps(az), p, nil)
	return m, testRouter(m)
}

const (
	createPath = "/internal/v1/ar03/commands/createSalesOrder"
	base       = "/internal/v1/ar03/"
)

func TestCreate_ProfileMustBeActive_CreditHoldDoesNotBlockDrafting(t *testing.T) {
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
		m.createSalesOrder(w, newReq(http.MethodPost, createPath, tenant, pCreator, createBody(ent, party)))
		require.Equal(t, c.status, w.Code, name+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
		require.Equal(t, 1, prof.calls, name)
	}
}

func TestCreate_Unauthorized(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof)
	for _, who := range []string{pNobody, pApprover, pConfirmer, pCanceller} {
		w := newRecorder()
		m.createSalesOrder(w, newReq(http.MethodPost, createPath, tenant, who, createBody(ent, party)))
		require.Equal(t, http.StatusForbidden, w.Code, who)
		require.Equal(t, kit.CodeForbidden, errCode(t, w.Body.Bytes()))
	}
	require.Zero(t, prof.calls, "authorization is decided before the profile is consulted")

	m2, _ := newUnitModule(&fakeAuthz{err: svcclient.ErrUnavailable}, prof)
	w := newRecorder()
	m2.createSalesOrder(w, newReq(http.MethodPost, createPath, tenant, pCreator, createBody(ent, party)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "authorization outage fails closed")
}

func TestCreate_Validation(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof)
	line := func(m map[string]any) []map[string]any { return []map[string]any{m} }
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
		"discount over 100": func(b map[string]any) { b["discount_percent"] = "100.5" },
		"negative discount": func(b map[string]any) { b["discount_percent"] = "-1" },
		"bad party":         func(b map[string]any) { b["customer_party_id"] = "not-a-uuid" },
		"bad price source":  func(b map[string]any) { b["price_source_ref"] = "has space" },
		"bad po ref":        func(b map[string]any) { b["customer_po_ref"] = "has space" },
		"unknown field":     func(b map[string]any) { b["discount_pct"] = "5" },
	}
	for name, f := range mut {
		b := createBody(ent, party)
		f(b)
		w := newRecorder()
		m.createSalesOrder(w, newReq(http.MethodPost, createPath, tenant, pCreator, b))
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
	require.Zero(t, prof.calls)
}

func TestCommands_RequireVersionReasonEvidenceAndWellFormedInput(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	oid := uuid.NewString()
	one := 1
	ms := func(mut func(map[string]any)) map[string]any {
		b := map[string]any{"order_id": oid, "expected_version": one, "external_ref": "FUL-1", "line_no": 1, "scheduled_qty": "1", "scheduled_amount": "10",
			"fulfillment_source": "LOGISTICS", "fulfillment_evidence_ref": "SHIP-9"}
		mut(b)
		return b
	}
	cases := map[string]struct {
		h    http.HandlerFunc
		body map[string]any
		code string
	}{
		"submit without version":     {m.submitOrder, map[string]any{"order_id": oid}, kit.CodeExpectedVersionMissing},
		"approve without version":    {m.approveOrder, map[string]any{"order_id": oid, "version_no": 1}, kit.CodeExpectedVersionMissing},
		"confirm without version":    {m.confirmOrder, map[string]any{"order_id": oid}, kit.CodeExpectedVersionMissing},
		"amend without version":      {m.amendSalesOrder, map[string]any{"order_id": oid, "terms": "x"}, kit.CodeExpectedVersionMissing},
		"close without version":      {m.closeOrder, map[string]any{"order_id": oid}, kit.CodeExpectedVersionMissing},
		"hold without version":       {m.placeOrderHold, map[string]any{"order_id": oid, "reason_code": "R", "evidence_id": "E", "hold_kind": "OTHER"}, kit.CodeExpectedVersionMissing},
		"release without version":    {m.releaseOrderHold, map[string]any{"order_id": oid, "reason_code": "R", "evidence_id": "E"}, kit.CodeExpectedVersionMissing},
		"cancel without version":     {m.cancelOrder, map[string]any{"order_id": oid, "reason_code": "R"}, kit.CodeExpectedVersionMissing},
		"milestone w/o version":      {m.recordBillableMilestone, ms(func(b map[string]any) { delete(b, "expected_version") }), kit.CodeExpectedVersionMissing},
		"cancel without reason":      {m.cancelOrder, map[string]any{"order_id": oid, "expected_version": one}, kit.CodeReasonRequired},
		"hold without reason":        {m.placeOrderHold, map[string]any{"order_id": oid, "expected_version": one, "evidence_id": "E", "hold_kind": "OTHER"}, kit.CodeReasonRequired},
		"hold without evidence":      {m.placeOrderHold, map[string]any{"order_id": oid, "expected_version": one, "reason_code": "R", "hold_kind": "OTHER"}, kit.CodeEvidenceRequired},
		"hold bad kind":              {m.placeOrderHold, map[string]any{"order_id": oid, "expected_version": one, "reason_code": "R", "evidence_id": "E", "hold_kind": "X"}, kit.CodeValidationFailed},
		"release without reason":     {m.releaseOrderHold, map[string]any{"order_id": oid, "expected_version": one, "evidence_id": "E"}, kit.CodeReasonRequired},
		"release without evidence":   {m.releaseOrderHold, map[string]any{"order_id": oid, "expected_version": one, "reason_code": "R"}, kit.CodeEvidenceRequired},
		"approve without version_no": {m.approveOrder, map[string]any{"order_id": oid, "expected_version": one}, kit.CodeValidationFailed},
		"amend nothing":              {m.amendSalesOrder, map[string]any{"order_id": oid, "expected_version": one}, kit.CodeValidationFailed},
		"bad order id":               {m.submitOrder, map[string]any{"order_id": "x", "expected_version": one}, kit.CodeValidationFailed},
		"milestone no ref":           {m.recordBillableMilestone, ms(func(b map[string]any) { b["external_ref"] = "" }), kit.CodeValidationFailed},
		"milestone bad ref":          {m.recordBillableMilestone, ms(func(b map[string]any) { b["external_ref"] = "a b" }), kit.CodeValidationFailed},
		"milestone line 0":           {m.recordBillableMilestone, ms(func(b map[string]any) { b["line_no"] = 0 }), kit.CodeValidationFailed},
		"milestone zero amount":      {m.recordBillableMilestone, ms(func(b map[string]any) { b["scheduled_amount"] = "0" }), kit.CodeValidationFailed},
		"milestone negative qty":     {m.recordBillableMilestone, ms(func(b map[string]any) { b["scheduled_qty"] = "-1" }), kit.CodeValidationFailed},
		"milestone no source":        {m.recordBillableMilestone, ms(func(b map[string]any) { b["fulfillment_source"] = "" }), kit.CodeValidationFailed},
		"milestone no evidence":      {m.recordBillableMilestone, ms(func(b map[string]any) { delete(b, "fulfillment_evidence_ref") }), kit.CodeValidationFailed},
		"milestone lower-case src":   {m.recordBillableMilestone, ms(func(b map[string]any) { b["fulfillment_source"] = "logistics" }), kit.CodeValidationFailed},
	}
	for name, c := range cases {
		w := newRecorder()
		c.h(w, newReq(http.MethodPost, "/x", tenant, pCreator, c.body))
		require.GreaterOrEqual(t, w.Code, 400, name)
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name+": "+w.Body.String())
	}
}

func TestQueries_RequireTenantAndPrincipal(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	for name, h := range map[string]http.HandlerFunc{"get": m.getSalesOrder, "billing": m.getOrderBillingStatus, "eligibility": m.getInvoiceEligibility,
		"actions": m.getAvailableActions, "history": m.getOrderHistory} {
		w := newRecorder()
		h(w, newReq(http.MethodGet, "/x?order_id="+uuid.NewString(), "", pCreator, nil)) // no tenant
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
		require.Equal(t, kit.CodeTenantScope, errCode(t, w.Body.Bytes()), name)
		w = newRecorder()
		h(w, newReq(http.MethodGet, "/x?order_id="+uuid.NewString(), uuid.NewString(), "", nil)) // no principal
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
	}
}

func TestQueries_ParameterValidation(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	tenant := uuid.NewString()
	for name, c := range map[string]struct {
		h  http.HandlerFunc
		qs string
	}{
		"get bad version":   {m.getSalesOrder, "order_id=" + uuid.NewString() + "&version_no=0"},
		"history bad limit": {m.getOrderHistory, "order_id=" + uuid.NewString() + "&limit=x"},
		"history bad after": {m.getOrderHistory, "order_id=" + uuid.NewString() + "&after_version=-1"},
	} {
		w := newRecorder()
		c.h(w, newReq(http.MethodGet, "/x?"+c.qs, tenant, pCreator, nil))
		require.Equal(t, http.StatusBadRequest, w.Code, name+w.Body.String())
	}
}

func TestRoutes_AllSpecPathsExistAndCommandsNeedIdempotencyKey(t *testing.T) {
	_, h := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{})
	for _, name := range []string{"createSalesOrder", "amendSalesOrder", "submitOrder", "approveOrder", "confirmOrder", "placeOrderHold",
		"releaseOrderHold", "cancelOrder", "recordBillableMilestone", "closeOrder"} {
		w := newRecorder()
		h.ServeHTTP(w, newReq(http.MethodPost, base+"commands/"+name, uuid.NewString(), pCreator, map[string]any{}))
		require.Equal(t, http.StatusBadRequest, w.Code, name)
		require.Equal(t, kit.CodeIdempotencyRequired, errCode(t, w.Body.Bytes()), name)
	}
	for _, name := range []string{"getSalesOrder", "getOrderBillingStatus", "getInvoiceEligibility", "getAvailableActions", "getOrderHistory"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := newRecorder()
			h.ServeHTTP(w, newReq(method, base+name, "", pCreator, nil))
			require.Equal(t, http.StatusUnauthorized, w.Code, name+" "+method+" (route exists, tenant missing)")
		}
	}
}

func TestPortValidationPrecedesAnyDatabaseAccess(t *testing.T) {
	m := New(testDeps(&fakeAuthz{}), &fakeProfiles{}, nil)
	_, err := m.InvoiceEligibility(context.Background(), uuid.NewString(), "not-a-uuid", "")
	var p *kit.Problem
	require.ErrorAs(t, err, &p)
	require.Equal(t, kit.CodeValidationFailed, p.Code)
}
