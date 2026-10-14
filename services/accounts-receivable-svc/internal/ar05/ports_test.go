package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

func TestParseProfileCatalog(t *testing.T) {
	c, err := ParseProfileCatalog(" GB|INVOICE=PEPPOL_INVOICE , de|invoice = XRECHNUNG ,, ")
	require.NoError(t, err)
	code, ok := c.CodeFor("gb", "INVOICE")
	require.True(t, ok)
	require.Equal(t, "PEPPOL_INVOICE", code)
	code, ok = c.CodeFor("DE", "invoice")
	require.True(t, ok)
	require.Equal(t, "XRECHNUNG", code)
	_, ok = c.CodeFor("FR", "INVOICE")
	require.False(t, ok)
	_, ok = c.CodeFor("GB", "CREDIT_NOTE")
	require.False(t, ok)
	for _, bad := range []string{"GB=X", "GB|INVOICE", "|INVOICE=X", "GB|=X", "GB|INVOICE="} {
		_, err := ParseProfileCatalog(bad)
		require.Error(t, err, bad)
	}
	empty, err := ParseProfileCatalog("")
	require.NoError(t, err)
	require.Empty(t, empty)
}

// jurisdictionStub answers like jurisdiction-rules-svc (handler/submission.go).
func jurisdictionStub(t *testing.T, status int, body string, seen *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const resolvedBody = `{"decision_id":"d-1","replayed":false,"profile":{"outcome":"PROFILE_RESOLVED",
 "resolution":{"outcome":"RESOLVED","pack":{"pack_ref":"gb","version":"2026.3","pack_version_id":"pv-9","artifact_digest":"sha256:aa"},
   "rule":{"rule_id":"r-1","jurisdiction_code":"GB","content_digest":"sha256:bb"}},
 "profile":{"channel":"PEPPOL","document_type":"INVOICE","profile_version":"1.0","syntax":"JSON","transmission_mode":"EXCHANGE","corrections":["CREDIT_NOTE"],
   "dependencies":[{"kind":"SCHEMA","ref":"s","version":"1","valid_from":"2026-01-01","valid_to":null}],
   "semantic_mapping":[{"from":"invoice.number","to":"cbc:ID"}],
   "lifecycle":{"initial":"SUBMITTED","terminal":["ACCEPTED","REJECTED"],"transitions":[["SUBMITTED","PROCESSING"],["SUBMITTED","ACCEPTED"],["PROCESSING","REJECTED"]],"receipt_required":["ACCEPTED"]}}}}`

func TestHTTPProfileResolver(t *testing.T) {
	var seen []string
	srv := jurisdictionStub(t, 200, resolvedBody, &seen)
	defer srv.Close()
	r := NewHTTPProfileResolver(srv.URL)
	p, err := r.Resolve(context.Background(), kit.Caller{TenantID: "t", PrincipalID: "p"}, ProfileQuery{Jurisdiction: "GB", ProfileCode: "PEPPOL_INVOICE", At: testNow})
	require.NoError(t, err)
	require.Equal(t, []string{"POST /v1/submission-profiles:resolve"}, seen)
	require.Equal(t, "PEPPOL", p.Network)
	require.Equal(t, SyntaxJSON, p.Syntax)
	require.Equal(t, "EXCHANGE", p.Mode)
	require.Equal(t, "1.0", p.ProfileVersion)
	require.Equal(t, "GB", p.Jurisdiction)
	require.Equal(t, "pv-9", p.PackVersionID)
	require.Equal(t, "r-1", p.RuleID)
	require.Equal(t, "d-1", p.DecisionID)
	require.Equal(t, testNow, p.EffectiveAt)
	require.True(t, p.HasLifecycleState("ACCEPTED"))
	require.True(t, p.HasLifecycleState("PROCESSING"))
	require.False(t, p.HasLifecycleState("CLEARED"))
	require.Len(t, p.SemanticMapping, 1)
}

func TestHTTPProfileResolverFailureClasses(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"no rule in force":         {200, `{"profile":{"outcome":"NO_RULE"}}`, ErrNoProfile},
		"unsupported jurisdiction": {422, `{"profile":{"outcome":"UNSUPPORTED_JURISDICTION"}}`, ErrNoProfile},
		"no parameters":            {422, `{"profile":{"outcome":"NO_PARAMETERS"}}`, ErrProfileInvalid},
		"dependency expired":       {422, `{"profile":{"outcome":"DEPENDENCY_EXPIRED"}}`, ErrProfileInvalid},
		"ambiguous":                {409, `{"profile":{"outcome":"AMBIGUOUS"}}`, ErrProfileUnavailable},
		"pack unverified":          {503, `{"error":"pack_unverified"}`, ErrProfileUnavailable},
		"forbidden":                {403, `{"error":"unauthorized"}`, ErrProfileUnavailable},
		"garbage":                  {200, `not json`, ErrProfileUnavailable},
		"resolved without profile": {200, `{"profile":{"outcome":"PROFILE_RESOLVED"}}`, ErrProfileInvalid},
	}
	for name, c := range cases {
		srv := jurisdictionStub(t, c.status, c.body, nil)
		_, err := NewHTTPProfileResolver(srv.URL).Resolve(context.Background(), kit.Caller{TenantID: "t"}, ProfileQuery{Jurisdiction: "GB", ProfileCode: "X", At: testNow})
		require.ErrorIs(t, err, c.want, name)
		srv.Close()
	}
	// transport failure
	srv := jurisdictionStub(t, 200, "{}", nil)
	url := srv.URL
	srv.Close()
	_, err := NewHTTPProfileResolver(url).Resolve(context.Background(), kit.Caller{TenantID: "t"}, ProfileQuery{Jurisdiction: "GB", ProfileCode: "X", At: testNow})
	require.ErrorIs(t, err, ErrProfileUnavailable)
}

func TestHTTPRegistry(t *testing.T) {
	var gotKey, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("Idempotency-Key"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"replayed":false,"submission":{"submission_id":"reg-77","status":"SUBMITTED"}}`))
	}))
	defer srv.Close()
	reg := NewHTTPRegistry(srv.URL)
	id, err := reg.Register(context.Background(), kit.Caller{TenantID: "t", PrincipalID: "p"}, RegistryRegistration{Jurisdiction: "GB", ProfileCode: "PEPPOL_INVOICE", EffectiveAt: testNow,
		SubjectRef: "INVOICE:abc", PayloadHash: "sha256:" + Sha256Hex([]byte("x")), Key: "k-1"})
	require.NoError(t, err)
	require.Equal(t, "reg-77", id)
	require.Equal(t, "k-1", gotKey)
	require.Equal(t, "/v1/regulatory-submissions", gotPath)
	require.Equal(t, "EINVOICE", gotBody["kind"])
	require.Equal(t, "INVOICE:abc", gotBody["subject_ref"])
	require.Equal(t, "GB", gotBody["jurisdiction"])

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer srv2.Close()
	require.NoError(t, NewHTTPRegistry(srv2.URL).RecordEvent(context.Background(), kit.Caller{TenantID: "t"}, "reg-77",
		RegistryEvent{ProviderEventID: "e1", Status: "ACCEPTED", ReceiptID: "rc", OccurredAt: testNow}))
	require.Equal(t, "/v1/regulatory-submissions/reg-77/events", gotPath)
	require.Equal(t, "ACCEPTED", gotBody["status"])

	refuse := jurisdictionStub(t, 422, `{"profile":{"outcome":"DEPENDENCY_EXPIRED"}}`, nil)
	defer refuse.Close()
	_, err = NewHTTPRegistry(refuse.URL).Register(context.Background(), kit.Caller{TenantID: "t"}, RegistryRegistration{Key: "k"})
	require.Error(t, err, "a registry refusal is an error, never a registration")
}

func TestHTTPEntityJurisdictions(t *testing.T) {
	ent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/entities/E1", r.URL.Path)
		_, _ = w.Write([]byte(`{"legal_entity_id":"E1","tenant_id":"T1","primary_jurisdiction_id":"J-uuid"}`))
	}))
	defer ent.Close()
	rules := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/jurisdictions/J-uuid", r.URL.Path)
		_, _ = w.Write([]byte(`{"jurisdiction_id":"J-uuid","jurisdiction_code":"GB"}`))
	}))
	defer rules.Close()
	code, err := NewHTTPEntityJurisdictions(ent.URL, rules.URL).PrimaryJurisdiction(context.Background(), "T1", "E1")
	require.NoError(t, err)
	require.Equal(t, "GB", code)

	// An entity of another tenant is never answered (the registry itself does not scope its read).
	_, err = NewHTTPEntityJurisdictions(ent.URL, rules.URL).PrimaryJurisdiction(context.Background(), "OTHER", "E1")
	require.ErrorIs(t, err, svcclient.ErrUnavailable)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer down.Close()
	_, err = NewHTTPEntityJurisdictions(down.URL, rules.URL).PrimaryJurisdiction(context.Background(), "T1", "E1")
	require.ErrorIs(t, err, svcclient.ErrUnavailable)
	_, err = NewHTTPEntityJurisdictions(ent.URL, down.URL).PrimaryJurisdiction(context.Background(), "T1", "E1")
	require.ErrorIs(t, err, svcclient.ErrUnavailable)
}

func TestTransportResolvers(t *testing.T) {
	p := testProfile()
	_, err := UnconfiguredTransports{}.For(p)
	var np *NoProviderError
	require.ErrorAs(t, err, &np)
	require.Equal(t, "PEPPOL", np.Network)

	ft := NewFakeTransport("acme")
	tr, err := NetworkTransports{"PEPPOL": ft}.For(p)
	require.NoError(t, err)
	require.Equal(t, "acme", tr.Name())
	_, err = NetworkTransports{"SDI": ft}.For(p)
	require.ErrorAs(t, err, &np)
}

func TestFakeTransportIsAHonestProvider(t *testing.T) {
	ft := NewFakeTransport("acme")
	ft.Script(FakeSubmit{Err: errors.New("timeout"), Delivered: true}, FakeSubmit{Err: errors.New("timeout")}, FakeSubmit{Err: ErrNotSent})
	ctx := context.Background()
	_, err := ft.Submit(ctx, SubmitRequest{ExternalAttemptID: "a1"})
	require.Error(t, err)
	_, err = ft.Submit(ctx, SubmitRequest{ExternalAttemptID: "a2"})
	require.Error(t, err)
	_, err = ft.Submit(ctx, SubmitRequest{ExternalAttemptID: "a3"})
	require.ErrorIs(t, err, ErrNotSent)
	res, err := ft.Submit(ctx, SubmitRequest{ExternalAttemptID: "a4"})
	require.NoError(t, err)
	require.Equal(t, DispAcknowledged, res.Disposition)

	for id, want := range map[string]string{"a1": ProviderReceived, "a2": ProviderNotReceived, "a3": ProviderNotReceived, "a4": ProviderReceived} {
		q, err := ft.QueryStatus(ctx, QueryRequest{ExternalAttemptID: id})
		require.NoError(t, err)
		require.Equal(t, want, q.State, id)
	}
	ft.SetProviderState("a4", ProviderAccepted)
	q, _ := ft.QueryStatus(ctx, QueryRequest{ExternalAttemptID: "a4"})
	require.Equal(t, ProviderAccepted, q.State)
	require.NotEmpty(t, q.ReceiptID)
	require.Equal(t, 4, ft.SubmitCount())
}

// ── kit.EInvoiceRequirement ──────────────────────────────────────────────────

func newRequirementModule(cat ProfileCatalog, pr ProfileResolver) *Module {
	return New(testDeps(&fakeAuthz{grants: standardGrants()}, func() time.Time { return testNow }), Collaborators{Profiles: pr, Catalog: cat})
}

func TestRequired(t *testing.T) {
	ctx := context.Background()
	cat := StaticProfileCatalog{"GB|INVOICE": "PEPPOL_INVOICE"}
	var _ kit.EInvoiceRequirement = newRequirementModule(nil, nil)

	fp := &fakeProfiles{profile: defaultProfile()}
	m := newRequirementModule(cat, fp)
	ok, err := m.Required(ctx, "t", "e", "GB", kit.DocInvoice)
	require.NoError(t, err)
	require.True(t, ok, "a profile in force makes the e-invoice required")
	require.Equal(t, "PEPPOL_INVOICE", fp.last.ProfileCode)
	require.Equal(t, testNow, fp.last.At)

	ok, err = m.Required(ctx, "t", "e", "FR", kit.DocInvoice)
	require.NoError(t, err)
	require.False(t, ok, "no profile configured for the jurisdiction: false")
	require.Equal(t, 1, fp.calls, "no registry call when nothing is configured")

	ok, err = m.Required(ctx, "t", "e", "GB", kit.DocCreditNote)
	require.NoError(t, err)
	require.False(t, ok)

	fp.setErr(ErrNoProfile)
	ok, err = m.Required(ctx, "t", "e", "GB", kit.DocInvoice)
	require.NoError(t, err)
	require.False(t, ok, "no rule in force: not required")

	for _, bad := range []error{ErrProfileUnavailable, ErrProfileInvalid, errors.New("boom")} {
		fp.setErr(bad)
		ok, err = m.Required(ctx, "t", "e", "GB", kit.DocInvoice)
		require.Error(t, err, "an unanswerable requirement is an ERROR so AR-04 fails closed")
		require.False(t, ok)
	}

	fp.setErr(nil)
	fp.set(func(p *Profile) { p.DocumentType = kit.DocCreditNote })
	ok, _ = m.Required(ctx, "t", "e", "GB", kit.DocInvoice)
	require.False(t, ok, "a profile for another document type does not make an invoice e-invoiceable")

	ok, err = newRequirementModule(nil, fp).Required(ctx, "t", "e", "GB", kit.DocInvoice)
	require.NoError(t, err)
	require.False(t, ok, "no catalog: nothing is required")

	_, err = newRequirementModule(cat, nil).Required(ctx, "t", "e", "GB", kit.DocInvoice)
	require.Error(t, err, "a configured jurisdiction with no resolver wired fails closed")
}
