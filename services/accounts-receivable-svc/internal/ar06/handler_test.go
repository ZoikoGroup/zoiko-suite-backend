package ar06

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// These handler tests run without a database: every path asserted here is decided before the command transaction opens.

const createPath = base + "commands/createCorrectionDraft"

type unit struct {
	m      *Module
	inv    *fakeInvoices
	tenant string
	entity string
	party  string
	src    *kit.IssuedInvoice
}

func newUnit(t *testing.T) *unit {
	t.Helper()
	u := &unit{inv: newFakeInvoices(), tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString()}
	u.m = New(testDeps(&fakeAuthz{grants: standardGrants()}), u.inv, newFakeOpenItems(), newFakeTax(), &fakePeriods{}, nil)
	u.src = sourceInvoice(u.entity, u.party)
	u.inv.put(u.tenant, u.src)
	return u
}

func (u *unit) create(principal string, body map[string]any) (int, []byte) {
	w := newRecorder()
	u.m.createCorrectionDraft(w, newReq(http.MethodPost, createPath, u.tenant, principal, body))
	return w.Code, w.Body.Bytes()
}

func TestCreate_Unauthorized(t *testing.T) {
	u := newUnit(t)
	for _, who := range []string{pNobody, pApprover, pIssuer, pLinker} {
		code, body := u.create(who, creditBody(u.entity, u.src, "100.00", nil))
		require.Equal(t, http.StatusForbidden, code, who)
		require.Equal(t, kit.CodeForbidden, errCode(t, body))
	}
	m2 := New(testDeps(&fakeAuthz{err: svcclient.ErrUnavailable}), u.inv, nil, nil, nil, nil)
	w := newRecorder()
	m2.createCorrectionDraft(w, newReq(http.MethodPost, createPath, u.tenant, pCreator, creditBody(u.entity, u.src, "100.00", nil)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "an authorization outage fails closed")
}

func TestCreate_Validation(t *testing.T) {
	u := newUnit(t)
	mut := map[string]struct {
		f    func(map[string]any)
		code string
	}{
		"no reason":     {func(b map[string]any) { delete(b, "reason_code") }, kit.CodeReasonRequired},
		"bad kind":      {func(b map[string]any) { b["kind"] = "REFUND" }, kit.CodeValidationFailed},
		"no lines":      {func(b map[string]any) { b["lines"] = []map[string]any{} }, kit.CodeValidationFailed},
		"bad date":      {func(b map[string]any) { b["supply_date"] = "yesterday" }, kit.CodeValidationFailed},
		"bad source":    {func(b map[string]any) { b["source_invoice_id"] = "not-a-uuid" }, kit.CodeValidationFailed},
		"zero net":      {func(b map[string]any) { b["lines"].([]map[string]any)[0]["unit_price"] = "0" }, kit.CodeValidationFailed},
		"unknown key":   {func(b map[string]any) { b["tax_amount"] = "5" }, kit.CodeValidationFailed},
		"tax on a line": {func(b map[string]any) { b["lines"].([]map[string]any)[0]["tax_amount"] = "5" }, kit.CodeValidationFailed},
	}
	for name, c := range mut {
		b := creditBody(u.entity, u.src, "100.00", nil)
		c.f(b)
		code, body := u.create(pCreator, b)
		require.GreaterOrEqual(t, code, 400, name)
		require.Equal(t, c.code, errCode(t, body), name+string(body))
	}
}

// Spec section 18 #18 (decidable without a DB): a correction against an unissued / unknown / foreign source is refused.
func TestCreate_WrongSource(t *testing.T) {
	u := newUnit(t)
	// unknown or unissued (AR-04 answers NOT_FOUND): never an authoritative document
	b := creditBody(u.entity, u.src, "100.00", map[string]any{"source_invoice_id": uuid.NewString()})
	code, body := u.create(pCreator, b)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, kit.CodeNotFound, errCode(t, body))
	// another tenant's invoice looks identical to an absent one
	other := sourceInvoice(u.entity, u.party)
	u.inv.put(uuid.NewString(), other)
	code, _ = u.create(pCreator, creditBody(u.entity, other, "100.00", nil))
	require.Equal(t, http.StatusNotFound, code)
	// another legal entity in the same tenant
	foreign := sourceInvoice(uuid.NewString(), u.party)
	u.inv.put(u.tenant, foreign)
	code, body = u.create(pCreator, creditBody(u.entity, foreign, "100.00", nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body))
	// another customer / currency
	code, body = u.create(pCreator, creditBody(u.entity, u.src, "100.00", map[string]any{"customer_party_id": uuid.NewString()}))
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body), string(body))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	_, body = u.create(pCreator, creditBody(u.entity, u.src, "100.00", map[string]any{"currency": "EUR"}))
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body))
	// a credit line naming a line the source does not have
	b = creditBody(u.entity, u.src, "100.00", nil)
	b["lines"].([]map[string]any)[0]["source_line_no"] = 7
	_, body = u.create(pCreator, b)
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body))
}

func TestCreate_CreditLineCannotExceedSourceLine(t *testing.T) {
	u := newUnit(t)
	code, body := u.create(pCreator, creditBody(u.entity, u.src, "1000.01", nil)) // source line 1 net is 1000.00
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body))
}

func TestCreate_EvidenceRequiredAboveThreshold(t *testing.T) {
	u := newUnit(t)
	big := sourceInvoice(u.entity, u.party)
	big.Lines[0].Net = kit.MustAmount("5000.00")
	u.inv.put(u.tenant, big)
	code, body := u.create(pCreator, creditBody(u.entity, big, "1000.01", nil))
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body))
}

func TestCreate_SourcePortNotWiredFailsClosed(t *testing.T) {
	u := newUnit(t)
	m := New(testDeps(&fakeAuthz{grants: standardGrants()}), nil, nil, nil, nil, nil)
	w := newRecorder()
	m.createCorrectionDraft(w, newReq(http.MethodPost, createPath, u.tenant, pCreator, creditBody(u.entity, u.src, "100.00", nil)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCommandsRequireIdempotencyKey(t *testing.T) {
	u := newUnit(t)
	h := testRouter(u.m)
	for _, name := range []string{"createCorrectionDraft", "submitCorrectionForApproval", "approveCorrection", "issueCreditNote", "issueDebitNote", "cancelUnissuedCorrection", "linkReplacementInvoice"} {
		req := newReq(http.MethodPost, base+"commands/"+name, u.tenant, pCreator, map[string]any{})
		req.Header.Del("Idempotency-Key")
		w := newRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code, name)
		require.Equal(t, kit.CodeIdempotencyRequired, errCode(t, w.Body.Bytes()), name)
	}
}

func TestQueriesRequireTenant(t *testing.T) {
	u := newUnit(t)
	h := testRouter(u.m)
	for _, name := range []string{"getCorrection", "getCorrectableAmount", "getCorrectionChain", "getAvailableActions", "getCorrectionAccountingStatus"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := newReq(method, base+name, "", pCreator, nil)
			w := newRecorder()
			h.ServeHTTP(w, req)
			require.Equal(t, http.StatusUnauthorized, w.Code, name)
		}
	}
}

func TestGetCorrectableAmount_UnissuedSourceRefused(t *testing.T) {
	u := newUnit(t)
	h := testRouter(u.m)
	code, _, _ := call(t, h, http.MethodGet, base+"getCorrectableAmount?source_invoice_id="+uuid.NewString(), u.tenant, pCreator, "", nil)
	require.Equal(t, http.StatusNotFound, code)
}
