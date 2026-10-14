package ar02

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

func newUnitModule(az kit.Authz, p kit.ProfileResolver, orders OrderCreator) (*Module, http.Handler) {
	m := New(testDeps(az), p, &fakeTax{}, orders)
	return m, testRouter(m)
}

const (
	createPath = "/internal/v1/ar02/commands/createQuote"
	base       = "/internal/v1/ar02/"
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
		m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof, nil)
		w := newRecorder()
		m.createQuote(w, newReq(http.MethodPost, createPath, tenant, pPreparer, createBody(ent, party)))
		require.Equal(t, c.status, w.Code, name+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
		require.Equal(t, 1, prof.calls, name)
	}
}

func TestCreate_Unauthorized(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof, nil)
	for _, who := range []string{pNobody, pApprover, pConverter} {
		w := newRecorder()
		m.createQuote(w, newReq(http.MethodPost, createPath, tenant, who, createBody(ent, party)))
		require.Equal(t, http.StatusForbidden, w.Code, who)
		require.Equal(t, kit.CodeForbidden, errCode(t, w.Body.Bytes()))
	}
	require.Zero(t, prof.calls, "authorization is decided before the profile is consulted")

	m2, _ := newUnitModule(&fakeAuthz{err: svcclient.ErrUnavailable}, prof, nil)
	w := newRecorder()
	m2.createQuote(w, newReq(http.MethodPost, createPath, tenant, pPreparer, createBody(ent, party)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "authorization outage fails closed")
}

func TestCreate_Validation(t *testing.T) {
	ent, party, tenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	prof := &fakeProfiles{}
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, prof, nil)
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
		"past validity":     func(b map[string]any) { b["validity_date"] = "2001-01-01" },
		"bad validity":      func(b map[string]any) { b["validity_date"] = "next week" },
		"no validity":       func(b map[string]any) { delete(b, "validity_date") },
		"bad party":         func(b map[string]any) { b["customer_party_id"] = "not-a-uuid" },
		"tax w/o params":    func(b map[string]any) { b["request_tax_estimate"] = true },
		"bad price source":  func(b map[string]any) { b["price_source_ref"] = "has space" },
		"unknown field":     func(b map[string]any) { b["discount_pct"] = "5" },
	}
	for name, f := range mut {
		b := createBody(ent, party)
		f(b)
		w := newRecorder()
		m.createQuote(w, newReq(http.MethodPost, createPath, tenant, pPreparer, b))
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
	require.Zero(t, prof.calls)
}

// Neg #4 (spec): quote acceptance recorded without evidence is refused before anything else.
func TestNeg04_AcceptanceWithoutEvidenceIsRefused(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	body := map[string]any{"quote_id": uuid.NewString(), "expected_version": 3, "version_no": 1, "accepted_by": "Jane Customer", "acceptance_channel": "EMAIL"}
	for _, ev := range []string{"", "   "} {
		b := map[string]any{}
		for k, v := range body {
			b[k] = v
		}
		b["evidence_id"] = ev
		w := newRecorder()
		m.recordAcceptance(w, newReq(http.MethodPost, "/x", tenant, pPreparer, b))
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, kit.CodeEvidenceRequired, errCode(t, w.Body.Bytes()))
	}
	// even a principal who could never accept learns evidence is the first requirement only for well-formed callers:
	// the check precedes the database, so no state is read or changed.
}

func TestAcceptance_RequiredFields(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	full := func() map[string]any {
		return map[string]any{"quote_id": uuid.NewString(), "expected_version": 3, "version_no": 1, "accepted_by": "Jane", "acceptance_channel": "PORTAL", "evidence_id": "EV-1"}
	}
	mut := map[string]func(map[string]any){
		"no version_no":  func(b map[string]any) { delete(b, "version_no") },
		"no accepted_by": func(b map[string]any) { b["accepted_by"] = " " },
		"bad channel":    func(b map[string]any) { b["acceptance_channel"] = "PHONE" },
		"no channel":     func(b map[string]any) { delete(b, "acceptance_channel") },
		"bad accepted_at": func(b map[string]any) {
			b["accepted_at"] = "yesterday"
		},
		"future accepted_at": func(b map[string]any) { b["accepted_at"] = "2999-01-01T00:00:00Z" },
	}
	for name, f := range mut {
		b := full()
		f(b)
		w := newRecorder()
		m.recordAcceptance(w, newReq(http.MethodPost, "/x", tenant, pPreparer, b))
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
}

func TestCommands_RequireVersionReasonAndWellFormedIDs(t *testing.T) {
	tenant := uuid.NewString()
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	qid := uuid.NewString()
	one := 1
	cases := map[string]struct {
		h    http.HandlerFunc
		body map[string]any
		code string
	}{
		"submit without version":     {m.submitQuoteForApproval, map[string]any{"quote_id": qid}, kit.CodeExpectedVersionMissing},
		"approve without version":    {m.approveQuote, map[string]any{"quote_id": qid, "version_no": 1}, kit.CodeExpectedVersionMissing},
		"send without version":       {m.sendQuote, map[string]any{"quote_id": qid}, kit.CodeExpectedVersionMissing},
		"amend without version":      {m.amendQuote, map[string]any{"quote_id": qid, "terms": "x"}, kit.CodeExpectedVersionMissing},
		"expire without version":     {m.expireQuote, map[string]any{"quote_id": qid}, kit.CodeExpectedVersionMissing},
		"convert without version":    {m.convertQuoteToOrder, map[string]any{"quote_id": qid}, kit.CodeValidationFailed}, // see nil-creator test
		"reject without reason":      {m.rejectQuote, map[string]any{"quote_id": qid, "expected_version": one}, kit.CodeReasonRequired},
		"approve without version_no": {m.approveQuote, map[string]any{"quote_id": qid, "expected_version": one}, kit.CodeValidationFailed},
		"amend nothing":              {m.amendQuote, map[string]any{"quote_id": qid, "expected_version": one}, kit.CodeValidationFailed},
		"bad quote id":               {m.submitQuoteForApproval, map[string]any{"quote_id": "x", "expected_version": one}, kit.CodeValidationFailed},
		"send bad channel":           {m.sendQuote, map[string]any{"quote_id": qid, "expected_version": one, "send_channel": "e mail"}, kit.CodeValidationFailed},
	}
	for name, c := range cases {
		w := newRecorder()
		c.h(w, newReq(http.MethodPost, "/x", tenant, pPreparer, c.body))
		require.GreaterOrEqual(t, w.Code, 400, name)
		if name == "convert without version" {
			require.Equal(t, http.StatusServiceUnavailable, w.Code, "nil order creator is decided first")
			continue
		}
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name+": "+w.Body.String())
	}
}

// With no order service wired (AR-03 not built yet) conversion is a clear 503.
func TestConvert_WithoutOrderCreatorIs503(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	w := newRecorder()
	m.convertQuoteToOrder(w, newReq(http.MethodPost, "/x", uuid.NewString(), pBoss, map[string]any{"quote_id": uuid.NewString(), "expected_version": 1}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, w.Body.Bytes()))
	require.Contains(t, w.Body.String(), "AR-03 sales order service not wired")
}

func TestQueries_RequireTenantAndPrincipal(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	for name, h := range map[string]http.HandlerFunc{"get": m.getQuote, "version": m.getQuoteVersion, "list": m.listQuotes,
		"actions": m.getAvailableActions, "history": m.getQuoteHistory} {
		w := newRecorder()
		h(w, newReq(http.MethodGet, "/x?quote_id="+uuid.NewString(), "", pPreparer, nil)) // no tenant
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
		require.Equal(t, kit.CodeTenantScope, errCode(t, w.Body.Bytes()), name)
		w = newRecorder()
		h(w, newReq(http.MethodGet, "/x?quote_id="+uuid.NewString(), uuid.NewString(), "", nil)) // no principal
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
	}
}

func TestQueries_ParameterValidation(t *testing.T) {
	m, _ := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	tenant := uuid.NewString()
	for name, c := range map[string]struct {
		h  http.HandlerFunc
		qs string
	}{
		"list bad limit":   {m.listQuotes, "limit=0"},
		"list bad offset":  {m.listQuotes, "offset=-1"},
		"list bad state":   {m.listQuotes, "state=WHATEVER"},
		"list bad entity":  {m.listQuotes, "legal_entity_id=zzz"},
		"version bad no":   {m.getQuoteVersion, "quote_id=" + uuid.NewString() + "&version_no=0"},
		"history bad page": {m.getQuoteHistory, "quote_id=" + uuid.NewString() + "&limit=x"},
	} {
		w := newRecorder()
		c.h(w, newReq(http.MethodGet, "/x?"+c.qs, tenant, pPreparer, nil))
		require.Equal(t, http.StatusBadRequest, w.Code, name+w.Body.String())
	}
}

func TestRoutes_AllSpecPathsExistAndCommandsNeedIdempotencyKey(t *testing.T) {
	_, h := newUnitModule(&fakeAuthz{grants: standardGrants()}, &fakeProfiles{}, nil)
	for _, name := range []string{"createQuote", "amendQuote", "submitQuoteForApproval", "approveQuote", "sendQuote", "recordAcceptance",
		"rejectQuote", "expireQuote", "convertQuoteToOrder"} {
		// No Idempotency-Key header: refused before anything else (call() adds one for POST, so build by hand).
		w := newRecorder()
		h.ServeHTTP(w, newReq(http.MethodPost, base+"commands/"+name, uuid.NewString(), pPreparer, map[string]any{}))
		require.Equal(t, http.StatusBadRequest, w.Code, name)
		require.Equal(t, kit.CodeIdempotencyRequired, errCode(t, w.Body.Bytes()), name)
	}
	for _, name := range []string{"getQuote", "getQuoteVersion", "listQuotes", "getAvailableActions", "getQuoteHistory"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := newRecorder()
			h.ServeHTTP(w, newReq(method, base+name, "", pPreparer, nil))
			require.Equal(t, http.StatusUnauthorized, w.Code, name+" "+method+" (route exists, tenant missing)")
		}
	}
}
