package ar05

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

// These handler tests run without a database: every path asserted here is decided before the command transaction opens.

const (
	base         = "/internal/v1/ar05/"
	prepPath     = base + "commands/prepareEInvoice"
	validatePath = base + "commands/validateEInvoice"
)

type unitEnv struct {
	m      *Module
	inv    *fakeInvoices
	prof   *fakeProfiles
	ent    *fakeEntJur
	tenant string
	entity string
	doc    *kit.IssuedInvoice
}

func newUnitEnv(t *testing.T, authz kit.Authz, cat ProfileCatalog) *unitEnv {
	t.Helper()
	e := &unitEnv{inv: newFakeInvoices(), prof: &fakeProfiles{profile: defaultProfile()}, ent: &fakeEntJur{code: "GB"}, tenant: uuid.NewString(), entity: uuid.NewString()}
	e.m = New(testDeps(authz, func() time.Time { return testNow }), Collaborators{Invoices: e.inv, Profiles: e.prof, Catalog: cat, Entities: e.ent})
	e.doc = sampleInvoice(e.entity)
	e.inv.put(e.tenant, e.doc)
	return e
}

func newReq(method, path, tenant, principal string, body any) *http.Request {
	rdr := bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
		req = req.WithContext(svcmiddleware.WithTenant(req.Context(), tenant))
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	return req
}

func (e *unitEnv) prepare(principal string, extra map[string]any) *httptest.ResponseRecorder {
	b := map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "profile_code": "PEPPOL_INVOICE"}
	for k, v := range extra {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	w := httptest.NewRecorder()
	e.m.prepareEInvoice(w, newReq(http.MethodPost, prepPath, e.tenant, principal, b))
	return w
}

func TestPrepare_Authorization(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	for _, who := range []string{pNobody, pReconciler, pAdmin} {
		w := e.prepare(who, nil)
		require.Equal(t, http.StatusForbidden, w.Code, who)
		require.Equal(t, kit.CodeForbidden, errCode(t, w.Body.Bytes()))
	}
	require.Zero(t, e.prof.calls, "authorization is decided before the profile registry is consulted")

	m2 := New(testDeps(&fakeAuthz{err: errTest}, func() time.Time { return testNow }), Collaborators{Invoices: e.inv, Profiles: e.prof})
	w := httptest.NewRecorder()
	m2.prepareEInvoice(w, newReq(http.MethodPost, prepPath, e.tenant, pSubmitter, map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "profile_code": "X"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "an authorization outage fails closed")
}

var errTest = &kit.Problem{Status: 503, Code: kit.CodeDependencyUnavailable, Msg: "down"}

func TestPrepare_Validation(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	cases := map[string]map[string]any{
		"no jurisdiction":   {"jurisdiction_id": nil},
		"blank document id": {"document_id": " "},
		"not a uuid":        {"document_id": "12345"},
		"unknown doc type":  {"document_type": "RECEIPT"},
		"overlong network":  {"network": string(make([]byte, 65))},
		"alias disagrees":   {"invoice_id": uuid.NewString()},
	}
	for name, extra := range cases {
		w := e.prepare(pSubmitter, extra)
		require.Equal(t, http.StatusBadRequest, w.Code, name+" "+w.Body.String())
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()), name)
	}
	// unknown body fields are rejected (a misspelled key is a refused request)
	w := httptest.NewRecorder()
	e.m.prepareEInvoice(w, newReq(http.MethodPost, prepPath, e.tenant, pSubmitter, map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "jurisdiction": "GB"}))
	require.Equal(t, http.StatusBadRequest, w.Code)
	// no tenant: 401
	w = httptest.NewRecorder()
	e.m.prepareEInvoice(w, newReq(http.MethodPost, prepPath, "", pSubmitter, map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"}))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, e.prof.calls)
}

func TestPrepare_OnlyIssuedDocumentsOfTheTenant(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	w := e.prepare(pSubmitter, map[string]any{"document_id": uuid.NewString()})
	require.Equal(t, http.StatusNotFound, w.Code, "a draft / unknown invoice is NOT_FOUND")
	require.Equal(t, kit.CodeNotFound, errCode(t, w.Body.Bytes()))

	other := httptest.NewRecorder()
	e.m.prepareEInvoice(other, newReq(http.MethodPost, prepPath, uuid.NewString(), pSubmitter, map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "profile_code": "X"}))
	require.Equal(t, http.StatusNotFound, other.Code, "another tenant's invoice looks identical to an absent one")

	w = e.prepare(pSubmitter, map[string]any{"legal_entity_id": uuid.NewString()})
	require.Equal(t, http.StatusForbidden, w.Code, "a body entity that disagrees with the invoice is refused")
	require.Equal(t, kit.CodeTenantScope, errCode(t, w.Body.Bytes()))

	w = e.prepare(pSubmitter, map[string]any{"document_type": "CREDIT_NOTE"})
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, "no credit note source is wired yet (AR-06)")
	require.Equal(t, kit.CodeEInvoiceValidationFailed, errCode(t, w.Body.Bytes()))
}

func TestPrepare_ProfileCodeMustAgreeWithConfiguration(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, StaticProfileCatalog{"GB|INVOICE": "PEPPOL_INVOICE"})
	w := e.prepare(pSubmitter, map[string]any{"profile_code": "XRECHNUNG"})
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	require.Equal(t, kit.CodeEInvoiceValidationFailed, errCode(t, w.Body.Bytes()), "a profile that is not the configured one for the jurisdiction is a wrong-profile refusal")
	require.Zero(t, e.prof.calls)

	e2 := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	w = e2.prepare(pSubmitter, map[string]any{"profile_code": nil})
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, "no configured and no stated profile: nothing is guessed")
	require.Zero(t, e2.prof.calls)
}

func TestCommandsRequireStatefulInputs(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	sub := uuid.NewString()
	type cmd struct {
		name string
		h    http.HandlerFunc
	}
	cmds := []cmd{{"validate", e.m.validateEInvoice}, {"submit", e.m.submitEInvoice}, {"retry", e.m.retrySubmission}}
	for _, c := range cmds {
		w := httptest.NewRecorder()
		c.h(w, newReq(http.MethodPost, validatePath, e.tenant, pSubmitter, map[string]any{"submission_id": sub}))
		require.Equal(t, http.StatusBadRequest, w.Code, c.name)
		require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, w.Body.Bytes()), c.name+": expected_version is required")

		w = httptest.NewRecorder()
		c.h(w, newReq(http.MethodPost, validatePath, e.tenant, pSubmitter, map[string]any{"submission_id": "nope", "expected_version": 1}))
		require.Equal(t, http.StatusBadRequest, w.Code, c.name)
	}
	w := httptest.NewRecorder()
	e.m.cancelSubmission(w, newReq(http.MethodPost, validatePath, e.tenant, pSubmitter, map[string]any{"submission_id": sub, "expected_version": 1}))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, kit.CodeReasonRequired, errCode(t, w.Body.Bytes()), "cancelling needs a reason")
}

func TestReconcile_ManualPathNeedsReasonEvidenceAndReceipt(t *testing.T) {
	e := newUnitEnv(t, &fakeAuthz{grants: standardGrants()}, nil)
	sub := uuid.NewString()
	post := func(body map[string]any) *httptest.ResponseRecorder {
		body["submission_id"] = sub
		body["expected_version"] = 3
		w := httptest.NewRecorder()
		e.m.reconcileSubmissionState(w, newReq(http.MethodPost, base+"commands/reconcileSubmissionState", e.tenant, pAdmin, body))
		return w
	}
	w := post(map[string]any{"method": "ADMIN_EVIDENCE", "resolved_state": "ACCEPTED", "receipt_id": "r"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, kit.CodeReasonRequired, errCode(t, w.Body.Bytes()))

	w = post(map[string]any{"method": "ADMIN_EVIDENCE", "resolved_state": "ACCEPTED", "receipt_id": "r", "reason_code": "PROVIDER_PORTAL_CONFIRMED"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, w.Body.Bytes()))

	w = post(map[string]any{"method": "ADMIN_EVIDENCE", "resolved_state": "SUBMITTED", "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusBadRequest, w.Code, "only ACCEPTED, REJECTED or NOT_RECEIVED can be stated")

	w = post(map[string]any{"method": "ADMIN_EVIDENCE", "resolved_state": "ACCEPTED", "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusBadRequest, w.Code, "no acceptance without a receipt")

	w = post(map[string]any{"method": "MAGIC"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ── callback security (decided before any database access) ────────────────────

func callbackReq(secret []byte, provider string, ts time.Time, nonce string, body []byte, mutate func(*http.Request)) *http.Request {
	req := httptest.NewRequest(http.MethodPost, CallbackPath, bytes.NewReader(body))
	tss := strconv.FormatInt(ts.Unix(), 10)
	req.Header.Set(HeaderProvider, provider)
	req.Header.Set(HeaderTimestamp, tss)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, Sign(secret, tss, nonce, body))
	if mutate != nil {
		mutate(req)
	}
	return req
}

func TestCallback_ForgedOrUnverifiableIsRefused(t *testing.T) {
	secret := []byte("provider-secret")
	m := New(testDeps(&fakeAuthz{grants: standardGrants()}, func() time.Time { return testNow }), Collaborators{Secrets: MapSecrets{"acme": secret}})
	body := []byte(`{"provider_event_id":"e1","external_attempt_id":"` + uuid.NewString() + `","document_id":"` + uuid.NewString() + `","status":"ACCEPTED","receipt_id":"r","occurred_at":"2026-10-12T10:00:00Z"}`)

	cases := map[string]struct {
		req  *http.Request
		code string
		http int
	}{
		"signed with the wrong secret": {callbackReq([]byte("attacker"), "acme", testNow, "n1", body, nil), CodeCallbackUnauthenticated, 401},
		"body tampered after signing": {callbackReq(secret, "acme", testNow, "n2", body, func(r *http.Request) {
			r.Body = http.NoBody
			r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(body, []byte("ACCEPTED"), []byte("REJECTED"), 1)))
		}), CodeCallbackUnauthenticated, 401},
		"no signature":        {callbackReq(secret, "acme", testNow, "n3", body, func(r *http.Request) { r.Header.Del(HeaderSignature) }), CodeCallbackUnauthenticated, 401},
		"no nonce":            {callbackReq(secret, "acme", testNow, "n4", body, func(r *http.Request) { r.Header.Del(HeaderNonce) }), CodeCallbackUnauthenticated, 401},
		"unknown provider":    {callbackReq(secret, "mallory", testNow, "n5", body, nil), CodeCallbackUnauthenticated, 401},
		"invalid provider id": {callbackReq(secret, "../etc", testNow, "n6", body, nil), CodeCallbackUnauthenticated, 401},
		"stale":               {callbackReq(secret, "acme", testNow.Add(-10*time.Minute), "n7", body, nil), CodeCallbackStale, 401},
		"future":              {callbackReq(secret, "acme", testNow.Add(10*time.Minute), "n8", body, nil), CodeCallbackStale, 401},
	}
	for name, c := range cases {
		w := httptest.NewRecorder()
		m.recordProviderCallback(w, c.req)
		require.Equal(t, c.http, w.Code, name+" "+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), name)
	}
}

func TestCallback_AuthenticButMalformedIsBadRequest(t *testing.T) {
	secret := []byte("provider-secret")
	m := New(testDeps(&fakeAuthz{grants: standardGrants()}, func() time.Time { return testNow }), Collaborators{Secrets: MapSecrets{"acme": secret}})
	att, doc := uuid.NewString(), uuid.NewString()
	ok := func(mut func(map[string]any)) []byte {
		b := map[string]any{"provider_event_id": "e1", "external_attempt_id": att, "document_id": doc, "status": "ACCEPTED", "receipt_id": "r", "occurred_at": "2026-10-12T10:00:00Z"}
		mut(b)
		raw, _ := json.Marshal(b)
		return raw
	}
	bodies := map[string][]byte{
		"not json":         []byte(`{`),
		"unknown field":    ok(func(b map[string]any) { b["status_override"] = "ACCEPTED" }),
		"missing event id": ok(func(b map[string]any) { b["provider_event_id"] = "" }),
		"bad attempt id":   ok(func(b map[string]any) { b["external_attempt_id"] = "x" }),
		"bad status":       ok(func(b map[string]any) { b["status"] = "DONE" }),
		"bad occurred_at":  ok(func(b map[string]any) { b["occurred_at"] = "yesterday" }),
		"overlong receipt": ok(func(b map[string]any) { b["receipt_id"] = string(make([]byte, 300)) }),
	}
	for name, body := range bodies {
		w := httptest.NewRecorder()
		m.recordProviderCallback(w, callbackReq(secret, "acme", testNow, "n-"+name, body, nil))
		require.Equal(t, http.StatusBadRequest, w.Code, name+" "+w.Body.String())
	}
}
