package ar01

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// These handler tests run without a database: every path asserted here is
// decided before the command transaction opens.

func createBody(entity, party string) map[string]any {
	return map[string]any{"legal_entity_id": entity, "customer_party_id": party, "payment_terms_code": "NET30", "payment_terms_days": 30,
		"credit_currency": "USD"}
}

func newUnitModule(az kit.Authz, parties PartyDirectory) (*Module, http.Handler) {
	m := New(testDeps(az), parties, nil)
	return m, testRouter(m)
}

const createPath = "/internal/v1/ar01/commands/createFinancialProfile"

func TestNeg01_PartyInactiveBlocksCreate(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cases := map[string]struct {
		parties *fakeParties
		status  int
		code    string
	}{
		"suspended customer":  {&fakeParties{party: &Party{Type: "CUSTOMER", Status: "SUSPENDED"}}, http.StatusUnprocessableEntity, kit.CodeCustomerProfileInactive},
		"terminated":          {&fakeParties{party: &Party{Type: "CUSTOMER", Status: "TERMINATED"}}, http.StatusUnprocessableEntity, kit.CodeCustomerProfileInactive},
		"vendor not customer": {&fakeParties{party: &Party{Type: "VENDOR", Status: "ACTIVE"}}, http.StatusUnprocessableEntity, kit.CodeCustomerProfileInactive},
		"missing party":       {&fakeParties{err: ErrPartyNotFound}, http.StatusUnprocessableEntity, kit.CodeCustomerProfileInactive},
		"master unavailable":  {&fakeParties{err: svcclient.ErrUnavailable}, http.StatusServiceUnavailable, kit.CodeDependencyUnavailable},
	}
	for name, c := range cases {
		m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, c.parties)
		w := newRecorder()
		r := newReq(http.MethodPost, createPath, tenant, pMaker, createBody(ent, party))
		m.createFinancialProfile(w, r)
		require.Equal(t, c.status, w.Code, name)
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
		require.Equal(t, 1, c.parties.calls, name)
	}
}

func TestNeg02_UnauthorizedPrincipalCannotCreateOrSetLimit(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	parties := &fakeParties{party: activeCustomer()}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, parties)

	// No profile-manage grant at all.
	w := newRecorder()
	m.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pNobody, createBody(ent, party)))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, kit.CodeForbidden, errCode(t, w.Body.Bytes()))
	require.Zero(t, parties.calls, "authorization is decided before the party master is consulted")

	// Profile manage but no credit-limit manage: a non-zero initial limit is refused.
	body := createBody(ent, party)
	body["credit_limit"] = "5000.00"
	body["reason_code"] = "NEW_CUSTOMER"
	az := standardGrants()
	az["pm-only"] = map[string]bool{ActionProfileManage: true}
	m2, _ := newUnitModule(&fakeAuthz{grants: az}, parties)
	w = newRecorder()
	m2.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, "pm-only", body))
	require.Equal(t, http.StatusForbidden, w.Code)

	// A policy ceiling can only be set by someone holding the approve action.
	body = createBody(ent, party)
	body["credit_limit_policy_max"] = "10000"
	w = newRecorder()
	m.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pMaker, body))
	require.Equal(t, http.StatusForbidden, w.Code)

	// Authorization outage fails closed.
	m3, _ := newUnitModule(&fakeAuthz{err: svcclient.ErrUnavailable}, parties)
	w = newRecorder()
	m3.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pMaker, createBody(ent, party)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCreate_Validation(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	parties := &fakeParties{party: activeCustomer()}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, parties)
	mut := map[string]func(map[string]any){
		"bad terms code": func(b map[string]any) { b["payment_terms_code"] = "net 30" },
		"days too large": func(b map[string]any) { b["payment_terms_days"] = 999 },
		"bad currency":   func(b map[string]any) { b["credit_currency"] = "dollars" },
		"negative limit": func(b map[string]any) { b["credit_limit"] = "-5" },
		"jpy fractional": func(b map[string]any) { b["credit_currency"] = "JPY"; b["credit_limit"] = "10.5" },
		"risk without source": func(b map[string]any) {
			b["risk_attributes"] = map[string]any{"rating": "HIGH"}
			b["reason_code"] = "R"
		},
		"past effective_from":  func(b map[string]any) { b["effective_from"] = "2001-01-01" },
		"bad party id":         func(b map[string]any) { b["customer_party_id"] = "not-a-uuid" },
		"bad effective format": func(b map[string]any) { b["effective_from"] = "01/02/2026" },
	}
	for name, f := range mut {
		b := createBody(ent, party)
		f(b)
		w := newRecorder()
		m.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pMaker, b))
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
	require.Zero(t, parties.calls)

	// Over-ceiling initial limit needs a reason; a risk assertion too.
	b := createBody(ent, party)
	b["credit_limit"] = "100"
	w := newRecorder()
	m.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pMaker, b))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, kit.CodeReasonRequired, errCode(t, w.Body.Bytes()))

	// Unknown fields are rejected, not ignored.
	b = createBody(ent, party)
	b["credit_limt"] = "1"
	w = newRecorder()
	m.createFinancialProfile(w, newReq(http.MethodPost, createPath, tenant, pMaker, b))
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCommands_RequireVersionReasonAndWellFormedIDs(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeParties{party: activeCustomer()})
	pid := uuid.NewString()
	one := 1
	cases := map[string]struct {
		h    http.HandlerFunc
		body map[string]any
		code string
	}{
		"hold without reason":    {m.placeCreditHold, map[string]any{"profile_id": pid, "expected_version": one}, kit.CodeReasonRequired},
		"release without reason": {m.releaseCreditHold, map[string]any{"profile_id": pid, "expected_version": one}, kit.CodeReasonRequired},
		"retire without reason":  {m.retireFinancialProfile, map[string]any{"profile_id": pid, "expected_version": one}, kit.CodeReasonRequired},
		"hold without version":   {m.placeCreditHold, map[string]any{"profile_id": pid, "reason_code": "R"}, kit.CodeExpectedVersionMissing},
		"limit without version":  {m.setCreditLimit, map[string]any{"profile_id": pid, "credit_limit": "1", "credit_currency": "USD", "reason_code": "R"}, kit.CodeExpectedVersionMissing},
		"terms without version":  {m.changePaymentTerms, map[string]any{"profile_id": pid, "payment_terms_code": "NET60", "payment_terms_days": 60}, kit.CodeExpectedVersionMissing},
		"amend nothing":          {m.amendFinancialProfile, map[string]any{"profile_id": pid, "expected_version": one}, kit.CodeValidationFailed},
		"suspend without reason": {m.amendFinancialProfile, map[string]any{"profile_id": pid, "expected_version": one, "transition": "SUSPEND"}, kit.CodeReasonRequired},
		"unknown transition":     {m.amendFinancialProfile, map[string]any{"profile_id": pid, "expected_version": one, "transition": "DELETE"}, kit.CodeValidationFailed},
		"bad profile id":         {m.placeCreditHold, map[string]any{"profile_id": "x", "expected_version": one, "reason_code": "R"}, kit.CodeValidationFailed},
		"limit no reason":        {m.setCreditLimit, map[string]any{"profile_id": pid, "expected_version": one, "credit_limit": "1", "credit_currency": "USD"}, kit.CodeReasonRequired},
		"bad decision":           {m.approveCreditLimitChange, map[string]any{"profile_id": pid, "expected_version": one, "request_id": uuid.NewString(), "decision": "MAYBE", "reason_code": "R"}, kit.CodeValidationFailed},
	}
	for name, c := range cases {
		w := newRecorder()
		c.h(w, newReq(http.MethodPost, "/x", tenant, pMaker, c.body))
		require.GreaterOrEqual(t, w.Code, 400, name)
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name+": "+w.Body.String())
	}
}

func TestQueries_RequireTenantAndPrincipal(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeParties{party: activeCustomer()})
	for name, h := range map[string]http.HandlerFunc{"get": m.getFinancialProfile, "asOf": m.getFinancialProfileAsOf, "actions": m.getAvailableActions,
		"credit": m.getCreditStatus, "history": m.listProfileHistory} {
		w := newRecorder()
		r := newReq(http.MethodGet, "/x?profile_id="+uuid.NewString(), "", pMaker, nil) // no tenant
		h(w, r)
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
		require.Equal(t, kit.CodeTenantScope, errCode(t, w.Body.Bytes()), name)
	}
}

func TestParseAsOf(t *testing.T) {
	d, ts, err := parseAsOf("2026-03-04")
	require.NoError(t, err)
	require.Equal(t, "2026-03-04", d)
	require.Equal(t, "2026-03-04T23:59:59Z", ts.Format("2006-01-02T15:04:05Z07:00"))
	d, _, err = parseAsOf("2026-03-04T10:00:00+02:00")
	require.NoError(t, err)
	require.Equal(t, "2026-03-04", d)
	_, _, err = parseAsOf("")
	require.Error(t, err)
	_, _, err = parseAsOf("yesterday")
	require.Error(t, err)
}
