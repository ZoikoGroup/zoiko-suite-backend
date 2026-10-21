package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// ── stub ─────────────────────────────────────────────────────────────────────

type stubWFPStore struct {
	profiles        map[string]*domain.SourceTrustProfile // by source_system_id
	manifests       map[string]*domain.PayrollRuleManifest
	refs            map[string]*domain.PayrollRunRef // by identity fingerprint
	events          []string
	lastTenantOfRef string
}

func newStubWFP() *stubWFPStore {
	return &stubWFPStore{
		profiles:  map[string]*domain.SourceTrustProfile{},
		manifests: map[string]*domain.PayrollRuleManifest{},
		refs:      map[string]*domain.PayrollRunRef{},
	}
}

func (s *stubWFPStore) emit(typ string) { s.events = append(s.events, typ) }

func (s *stubWFPStore) CreateSourceTrustProfile(_ context.Context, p *domain.SourceTrustProfile, mk store.EventBuilder[domain.SourceTrustProfile]) (bool, error) {
	if ex, ok := s.profiles[p.SourceSystemID]; ok {
		*p = *ex
		return false, nil
	}
	p.Status, p.Version = domain.TrustPendingCertification, 1
	s.profiles[p.SourceSystemID] = p
	if mk != nil {
		if ev, err := mk(p); err == nil {
			s.emit(ev.EventType)
		}
	}
	return true, nil
}

func (s *stubWFPStore) GetSourceTrustProfile(_ context.Context, id string) (*domain.SourceTrustProfile, error) {
	for _, p := range s.profiles {
		if p.ProfileID == id {
			return p, nil
		}
	}
	return nil, domain.ErrTrustProfileNotFound
}

func (s *stubWFPStore) GetSourceTrustProfileBySource(_ context.Context, src string) (*domain.SourceTrustProfile, error) {
	if p, ok := s.profiles[src]; ok {
		return p, nil
	}
	return nil, domain.ErrTrustProfileNotFound
}

func (s *stubWFPStore) ListSourceTrustProfiles(context.Context, string) ([]domain.SourceTrustProfile, error) {
	return nil, nil
}

func (s *stubWFPStore) TransitionSourceTrustProfile(_ context.Context, id string, expected int, to, cert string, mk store.EventBuilder[domain.SourceTrustProfile]) (*domain.SourceTrustProfile, error) {
	p, err := s.GetSourceTrustProfile(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if p.Version != expected {
		return nil, domain.ErrVersionConflict
	}
	if !domain.TrustTransitionAllowed(p.Status, to) {
		return nil, domain.ErrInvalidTrustTransition
	}
	if to == domain.TrustActive {
		if cert == "" && p.CertificationRef == nil {
			return nil, domain.ErrCertificationRequired
		}
		if cert != "" {
			p.CertificationRef = &cert
		}
	}
	p.Status = to
	p.Version++
	if mk != nil {
		if ev, err := mk(p); err == nil {
			s.emit(ev.EventType)
		}
	}
	return p, nil
}

func (s *stubWFPStore) RegisterRuleManifest(_ context.Context, m *domain.PayrollRuleManifest, mk store.EventBuilder[domain.PayrollRuleManifest]) (bool, error) {
	s.manifests[m.ManifestID] = m
	if mk != nil {
		if ev, err := mk(m); err == nil {
			s.emit(ev.EventType)
		}
	}
	return true, nil
}

func (s *stubWFPStore) GetRuleManifest(_ context.Context, id string) (*domain.PayrollRuleManifest, error) {
	if m, ok := s.manifests[id]; ok {
		return m, nil
	}
	return nil, domain.ErrManifestNotFound
}

func (s *stubWFPStore) ResolveRuleManifest(_ context.Context, src, jur, asOf string) (*domain.PayrollRuleManifest, error) {
	for _, m := range s.manifests {
		if m.SourceSystemID == src && m.JurisdictionID == jur && m.EffectiveOn(asOf) {
			return m, nil
		}
	}
	return nil, domain.ErrManifestNotFound
}

func (s *stubWFPStore) RegisterPayrollRunRef(_ context.Context, r *domain.PayrollRunRef, mk store.EventBuilder[domain.PayrollRunRef]) (bool, error) {
	if ex, ok := s.refs[r.IdentityFingerprint]; ok {
		*r = *ex
		return false, nil
	}
	r.AcceptanceState, r.Version = domain.AcceptanceReceived, 1
	s.refs[r.IdentityFingerprint] = r
	if mk != nil {
		if ev, err := mk(r); err == nil {
			s.emit(ev.EventType)
			var env map[string]any
			_ = json.Unmarshal(ev.Payload, &env)
			s.lastTenantOfRef, _ = env["tenant_id"].(string)
		}
	}
	return true, nil
}

func (s *stubWFPStore) GetPayrollRunRef(_ context.Context, id string) (*domain.PayrollRunRef, error) {
	for _, r := range s.refs {
		if r.RunRefID == id {
			return r, nil
		}
	}
	return nil, domain.ErrRunRefNotFound
}

func (s *stubWFPStore) ListPayrollRunRefs(context.Context, store.RunRefFilter) ([]domain.PayrollRunRef, error) {
	return nil, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

func newWFPRouter(s *stubWFPStore, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPRoutes(r, handler.NewWFP(s, authz, zap.NewNop()))
	return r
}

type reqOpts struct {
	principal, entity, ifMatch string
}

func wfpReq(r chi.Router, method, path string, body any, o reqOpts) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if o.principal != "" {
		req.Header.Set("X-Principal-Id", o.principal)
	}
	if o.entity != "" {
		req.Header.Set("X-Legal-Entity-Id", o.entity)
	}
	if o.ifMatch != "" {
		req.Header.Set("If-Match", o.ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

var wfpCaller = reqOpts{principal: "p-1", entity: "le-1"}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	return b["error_code"]
}

func activeProfile(s *stubWFPStore, source string, caps ...string) {
	cert := "cert-1"
	s.profiles[source] = &domain.SourceTrustProfile{
		ProfileID: "prof-" + source, SourceSystemID: source, Status: domain.TrustActive,
		AllowedCapabilities: caps, CertificationRef: &cert, Version: 2,
	}
}

func manifest(s *stubWFPStore, id, source, from string, to *string) {
	s.manifests[id] = &domain.PayrollRuleManifest{
		ManifestID: id, SourceSystemID: source, JurisdictionID: "GB", EffectiveFrom: from, EffectiveTo: to,
	}
}

func runRefBody(manifestID string) map[string]any {
	return map[string]any{
		"legal_entity_id": "le-1", "source_system_id": "zp", "source_run_id": "RUN-1", "source_run_version": 1,
		"run_kind": "REGULAR", "payroll_period_id": "2026-09", "pay_date": "2026-09-30", "rule_manifest_id": manifestID,
	}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP_RequiresPrincipalEntityAndAuthorization(t *testing.T) {
	body := map[string]any{"source_system_id": "zp", "source_kind": "ZOIKO_PAYROLL", "allowed_capabilities": []string{"PAYROLL_RESULT"}}

	r := newWFPRouter(newStubWFP(), &stubAuthZ{})
	assert.Equal(t, http.StatusUnauthorized, wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles", body, reqOpts{entity: "le-1"}).Code)
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles", body, reqOpts{principal: "p-1"}).Code)

	denied := newWFPRouter(newStubWFP(), &stubAuthZ{err: domain.ErrAuthorizationDenied})
	assert.Equal(t, http.StatusForbidden, wfpReq(denied, "POST", "/v1/payroll/wfp/source-trust-profiles", body, wfpCaller).Code)

	down := newWFPRouter(newStubWFP(), &stubAuthZ{err: domain.ErrAuthzServiceUnavailable})
	assert.Equal(t, http.StatusServiceUnavailable, wfpReq(down, "POST", "/v1/payroll/wfp/source-trust-profiles", body, wfpCaller).Code, "fail closed")
}

func TestWFP_TenantIsNeverTakenFromTheBody(t *testing.T) {
	r := newWFPRouter(newStubWFP(), &stubAuthZ{})
	body := runRefBody("m-1")
	body["tenant_id"] = "someone-elses-tenant"
	rr := wfpReq(r, "POST", "/v1/payroll/wfp/run-refs", body, wfpCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code, "a forged tenant field is rejected outright, not ignored")
	assert.Equal(t, "invalid_json", errCode(t, rr))
}

// ── trust profiles ───────────────────────────────────────────────────────────

func TestWFP_TrustProfile_CreateReplayAndTransitions(t *testing.T) {
	s := newStubWFP()
	r := newWFPRouter(s, &stubAuthZ{})
	body := map[string]any{"source_system_id": "zp", "source_kind": "ZOIKO_PAYROLL", "allowed_capabilities": []string{"PAYROLL_RESULT"}}

	rr := wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles", body, wfpCaller)
	require.Equal(t, http.StatusCreated, rr.Code)
	var p domain.SourceTrustProfile
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &p))
	assert.Equal(t, domain.TrustPendingCertification, p.Status)

	assert.Equal(t, http.StatusOK, wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles", body, wfpCaller).Code, "replay is 200, not a second profile")
	assert.Equal(t, []string{domain.EventSourceTrustProfileChanged}, s.events)

	path := "/v1/payroll/wfp/source-trust-profiles/" + p.ProfileID
	// expected_version is mandatory.
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "POST", path+"/activate", map[string]any{"certification_ref": "c"}, wfpCaller).Code)
	// Activation without evidence is refused.
	rr = wfpReq(r, "POST", path+"/activate", map[string]any{"expected_version": 1}, wfpCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "CERTIFICATION_REQUIRED", errCode(t, rr))
	// Stale version conflicts; If-Match is honoured over the body.
	rr = wfpReq(r, "POST", path+"/activate", map[string]any{"expected_version": 9, "certification_ref": "c"}, wfpCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "VERSION_CONFLICT", errCode(t, rr))
	withHeader := wfpCaller
	withHeader.ifMatch = `"1"`
	rr = wfpReq(r, "POST", path+"/activate", map[string]any{"certification_ref": "c"}, withHeader)
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = wfpReq(r, "POST", path+"/retire", map[string]any{"expected_version": 2}, wfpCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	rr = wfpReq(r, "POST", path+"/activate", map[string]any{"expected_version": 3, "certification_ref": "c"}, wfpCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "INVALID_TRANSITION", errCode(t, rr))
}

func TestWFP_TrustProfile_LookupBySource(t *testing.T) {
	s := newStubWFP()
	activeProfile(s, "hris", domain.CapWorkerProfile)
	r := newWFPRouter(s, &stubAuthZ{})

	rr := wfpReq(r, "GET", "/v1/payroll/wfp/source-trust-profiles/by-source/hris", nil, wfpCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	var p domain.SourceTrustProfile
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &p))
	assert.True(t, p.Allows(domain.CapWorkerProfile))

	assert.Equal(t, http.StatusNotFound, wfpReq(r, "GET", "/v1/payroll/wfp/source-trust-profiles/by-source/unknown", nil, wfpCaller).Code)
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "GET", "/v1/payroll/wfp/source-trust-profiles/by-source/hris", nil, reqOpts{principal: "p-1"}).Code)
}

func TestWFP_TrustProfile_ValidationRejectsUnknownKindAndCapability(t *testing.T) {
	r := newWFPRouter(newStubWFP(), &stubAuthZ{})
	rr := wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles",
		map[string]any{"source_system_id": "zp", "source_kind": "SPREADSHEET", "allowed_capabilities": []string{"PAYROLL_RESULT"}}, wfpCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	rr = wfpReq(r, "POST", "/v1/payroll/wfp/source-trust-profiles",
		map[string]any{"source_system_id": "zp", "source_kind": "ZOIKO_PAYROLL", "allowed_capabilities": []string{"PAYMENT"}}, wfpCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ── run refs: trust and provenance gates ─────────────────────────────────────

func TestWFP_RunRef_BlockedUnlessSourceTrustedForPayrollResults(t *testing.T) {
	cases := map[string]func(*stubWFPStore){
		"no trust profile": func(s *stubWFPStore) {},
		"suspended": func(s *stubWFPStore) {
			activeProfile(s, "zp", domain.CapPayrollResult)
			s.profiles["zp"].Status = domain.TrustSuspended
		},
		"pending certification": func(s *stubWFPStore) {
			activeProfile(s, "zp", domain.CapPayrollResult)
			s.profiles["zp"].Status = domain.TrustPendingCertification
		},
		"trusted for a different capability only": func(s *stubWFPStore) { activeProfile(s, "zp", domain.CapTimeLeave) },
	}
	for name, setup := range cases {
		s := newStubWFP()
		manifest(s, "m-1", "zp", "2026-01-01", nil)
		setup(s)
		rr := wfpReq(newWFPRouter(s, &stubAuthZ{}), "POST", "/v1/payroll/wfp/run-refs", runRefBody("m-1"), wfpCaller)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, name)
		assert.Equal(t, "SOURCE_NOT_TRUSTED", errCode(t, rr), name)
		assert.Empty(t, s.refs, name)
	}
}

func TestWFP_RunRef_BlockedWithoutValidRuleProvenance(t *testing.T) {
	future := func(s *stubWFPStore) { manifest(s, "m-1", "zp", "2026-10-01", nil) } // starts after the pay date
	expired := func(s *stubWFPStore) { to := "2026-09-30"; manifest(s, "m-1", "zp", "2026-01-01", &to) }
	wrongSrc := func(s *stubWFPStore) { manifest(s, "m-1", "other-engine", "2026-01-01", nil) }

	for name, c := range map[string]struct {
		setup  func(*stubWFPStore)
		status int
		code   string
	}{
		"missing manifest":        {func(*stubWFPStore) {}, http.StatusNotFound, "not_found"},
		"future-effective":        {future, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE"},
		"expired (end exclusive)": {expired, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE"},
		"another source's":        {wrongSrc, http.StatusUnprocessableEntity, "RULE_MANIFEST_SOURCE_MISMATCH"},
	} {
		s := newStubWFP()
		activeProfile(s, "zp", domain.CapPayrollResult)
		c.setup(s)
		rr := wfpReq(newWFPRouter(s, &stubAuthZ{}), "POST", "/v1/payroll/wfp/run-refs", runRefBody("m-1"), wfpCaller)
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, errCode(t, rr), name)
		assert.Empty(t, s.refs, name)
	}
}

func TestWFP_RunRef_RegisterAndReplay(t *testing.T) {
	s := newStubWFP()
	activeProfile(s, "zp", domain.CapPayrollResult)
	manifest(s, "m-1", "zp", "2026-01-01", nil)
	r := newWFPRouter(s, &stubAuthZ{})

	rr := wfpReq(r, "POST", "/v1/payroll/wfp/run-refs", runRefBody("m-1"), wfpCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var ref domain.PayrollRunRef
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ref))
	assert.Equal(t, domain.AcceptanceReceived, ref.AcceptanceState)
	assert.Len(t, ref.IdentityFingerprint, 64)
	assert.Equal(t, domain.RunIdentityFingerprint("tenant-abc", domain.RegisterPayrollRunRefRequest{
		LegalEntityID: "le-1", SourceSystemID: "zp", SourceRunID: "RUN-1", SourceRunVersion: 1,
		RunKind: "REGULAR", PayrollPeriodID: "2026-09", PayDate: "2026-09-30", RuleManifestID: "m-1",
	}), ref.IdentityFingerprint, "the fingerprint binds the authenticated tenant")
	assert.Equal(t, "tenant-abc", s.lastTenantOfRef, "the event envelope carries the authenticated tenant")

	// The same run delivered again is the same result, not a second one.
	rr = wfpReq(r, "POST", "/v1/payroll/wfp/run-refs", runRefBody("m-1"), wfpCaller)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Len(t, s.refs, 1)
	assert.Equal(t, []string{domain.EventPayrollResultReceived}, s.events, "a replay emits no second event")

	// Reads round-trip.
	assert.Equal(t, http.StatusOK, wfpReq(r, "GET", "/v1/payroll/wfp/run-refs/"+ref.RunRefID, nil, wfpCaller).Code)
	assert.Equal(t, http.StatusNotFound, wfpReq(r, "GET", "/v1/payroll/wfp/run-refs/nope", nil, wfpCaller).Code)
}

func TestWFP_RunRef_RunKindAndVersionValidated(t *testing.T) {
	s := newStubWFP()
	activeProfile(s, "zp", domain.CapPayrollResult)
	manifest(s, "m-1", "zp", "2026-01-01", nil)
	r := newWFPRouter(s, &stubAuthZ{})

	b := runRefBody("m-1")
	b["run_kind"] = "WEEKLY"
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "POST", "/v1/payroll/wfp/run-refs", b, wfpCaller).Code)
	b = runRefBody("m-1")
	b["source_run_version"] = 0
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "POST", "/v1/payroll/wfp/run-refs", b, wfpCaller).Code)
}

// ── rule manifests ───────────────────────────────────────────────────────────

func TestWFP_Manifest_RegisterAndResolveRequiresAsOf(t *testing.T) {
	s := newStubWFP()
	r := newWFPRouter(s, &stubAuthZ{})

	body := map[string]any{
		"source_system_id": "zp", "jurisdiction_id": "GB", "manifest_version": "2026-27.1",
		"manifest_digest": strings.Repeat("a", 64), "effective_from": "2026-04-06", "evidence_ref": "doc://gb",
	}
	require.Equal(t, http.StatusCreated, wfpReq(r, "POST", "/v1/payroll/wfp/rule-manifests", body, wfpCaller).Code)
	assert.Equal(t, []string{domain.EventRuleManifestRegistered}, s.events)

	body["manifest_digest"] = "short"
	assert.Equal(t, http.StatusBadRequest, wfpReq(r, "POST", "/v1/payroll/wfp/rule-manifests", body, wfpCaller).Code)

	// "Current rules" by omission is not a thing: as_of is mandatory.
	rr := wfpReq(r, "GET", "/v1/payroll/wfp/rule-manifests/resolve?source_system_id=zp&jurisdiction_id=GB", nil, wfpCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	rr = wfpReq(r, "GET", "/v1/payroll/wfp/rule-manifests/resolve?source_system_id=zp&jurisdiction_id=GB&as_of=2026-09-30", nil, wfpCaller)
	assert.Equal(t, http.StatusOK, rr.Code)
	rr = wfpReq(r, "GET", "/v1/payroll/wfp/rule-manifests/resolve?source_system_id=zp&jurisdiction_id=GB&as_of=2025-01-01", nil, wfpCaller)
	assert.Equal(t, http.StatusNotFound, rr.Code, "no rules in force for that date fails closed")
}
