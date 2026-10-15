package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/employee-master-svc/internal/clients"
	"zoiko.io/employee-master-svc/internal/domain"
	"zoiko.io/employee-master-svc/internal/handler"
	"zoiko.io/employee-master-svc/internal/middleware"
	"zoiko.io/employee-master-svc/internal/store"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubWFPStore struct {
	syncResult  *store.SyncResult
	syncErr     error
	lastProfile *domain.WorkerFinancialProfile
	lastPre     string
	built       []domain.OutboxEvent
	asOf        []domain.WorkerFinancialProfile
	facts       domain.WorkerFacts
	profile     *domain.WorkerFinancialProfile
	err         error
}

func (s *stubWFPStore) SyncWorkerFinancialProfile(_ context.Context, p *domain.WorkerFinancialProfile, pre string, mk store.EventBuilder) (*store.SyncResult, error) {
	s.lastProfile, s.lastPre = p, pre
	if s.syncErr != nil {
		return nil, s.syncErr
	}
	if s.syncResult != nil {
		if mk != nil {
			evs, err := mk(s.syncResult.Profile, s.syncResult.Superseded)
			if err != nil {
				return nil, err
			}
			s.built = evs
		}
		return s.syncResult, nil
	}
	p.State, p.Version = domain.ProfileActive, 1
	if pre != "" {
		p.State, p.QuarantineReason = domain.ProfileQuarantined, &pre
	}
	return &store.SyncResult{Profile: p, Created: true}, nil
}

func (s *stubWFPStore) RevalidateWorkerFinancialProfile(_ context.Context, _ string, _ int, pre string, _ store.EventBuilder) (*store.SyncResult, error) {
	s.lastPre = pre
	if s.err != nil {
		return nil, s.err
	}
	return s.syncResult, nil
}

func (s *stubWFPStore) DeactivateWorkerFinancialProfile(_ context.Context, _ string, _ int, mk store.EventBuilder) (*domain.WorkerFinancialProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	p := *s.profile
	p.State = domain.ProfileDeactivated
	if mk != nil {
		evs, err := mk(&p, nil)
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &p, nil
}

func (s *stubWFPStore) GetWorkerFinancialProfile(context.Context, string) (*domain.WorkerFinancialProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.profile == nil {
		return nil, domain.ErrProfileNotFound
	}
	return s.profile, nil
}

func (s *stubWFPStore) ListProfileVersions(context.Context, string, string) ([]domain.WorkerFinancialProfile, error) {
	return nil, s.err
}

func (s *stubWFPStore) GetAsOfProfiles(context.Context, string, string, string) ([]domain.WorkerFinancialProfile, error) {
	return s.asOf, s.err
}

func (s *stubWFPStore) WorkerFactsFor(context.Context, string) (domain.WorkerFacts, error) {
	return s.facts, s.err
}

type stubTrust struct {
	err    error
	called int
	source string
	caller clients.CallerContext
}

func (t *stubTrust) Check(_ context.Context, c clients.CallerContext, source, _ string) error {
	t.called++
	t.source, t.caller = source, c
	return t.err
}

type stubJur struct {
	exists bool
	err    error
}

func (j *stubJur) Exists(context.Context, string, string) (bool, error) { return j.exists, j.err }

// ── harness ──────────────────────────────────────────────────────────────────

func newWFPRouter(st *stubWFPStore, authz *stubAuthZ, tr *stubTrust, j *stubJur) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPRoutes(r, handler.NewWFP(st, authz, tr, j, zap.NewNop()))
	return r
}

type caller struct{ principal, entity, ifMatch string }

var wfpCaller = caller{principal: "p-1", entity: "le-1"}

func wfpDo(r chi.Router, method, path string, body any, c caller) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if c.principal != "" {
		req.Header.Set("X-Principal-Id", c.principal)
	}
	if c.entity != "" {
		req.Header.Set("X-Legal-Entity-Id", c.entity)
	}
	if c.ifMatch != "" {
		req.Header.Set("If-Match", c.ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func code(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	s, _ := b["error_code"].(string)
	return s
}

const base = "/v1/wfp/worker-financial-profiles"

func syncBody() map[string]any {
	return map[string]any{
		"worker_id": uuid.NewString(), "assignment_id": "A1", "legal_entity_id": "le-1", "source_system_id": "hris",
		"source_version": 1, "payroll_jurisdiction_id": "GB", "pay_currency": "GBP",
		"employment_classification": "EMPLOYEE", "effective_from": "2026-01-01",
		"payee_ref": "SECRET-PAYEE-REF-123",
	}
}

func okRouter() (chi.Router, *stubWFPStore, *stubTrust) {
	st, tr := &stubWFPStore{}, &stubTrust{}
	return newWFPRouter(st, &stubAuthZ{}, tr, &stubJur{exists: true}), st, tr
}

// ── identity, authorization, trust ───────────────────────────────────────────

func TestWFP01_IdentityAndAuthorization(t *testing.T) {
	r, _, _ := okRouter()
	assert.Equal(t, http.StatusUnauthorized, wfpDo(r, "POST", base, syncBody(), caller{entity: "le-1"}).Code)

	denied := newWFPRouter(&stubWFPStore{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubTrust{}, &stubJur{exists: true})
	assert.Equal(t, http.StatusForbidden, wfpDo(denied, "POST", base, syncBody(), wfpCaller).Code)

	down := newWFPRouter(&stubWFPStore{}, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, &stubTrust{}, &stubJur{exists: true})
	assert.Equal(t, http.StatusServiceUnavailable, wfpDo(down, "POST", base, syncBody(), wfpCaller).Code, "fail closed")

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "GET", base+"/"+uuid.NewString(), nil, caller{principal: "p-1"}).Code, "reads need an entity scope")
}

func TestWFP01_ForgedTenantIsRejected(t *testing.T) {
	r, st, _ := okRouter()
	b := syncBody()
	b["tenant_id"] = "someone-elses-tenant"
	rr := wfpDo(r, "POST", base, b, wfpCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", code(t, rr))
	assert.Nil(t, st.lastProfile, "nothing reaches the store")
}

func TestWFP01_UntrustedSourceBlocksBeforeAnyWrite(t *testing.T) {
	for name, err := range map[string]error{"not trusted": domain.ErrSourceNotTrusted} {
		st, tr := &stubWFPStore{}, &stubTrust{err: err}
		r := newWFPRouter(st, &stubAuthZ{}, tr, &stubJur{exists: true})
		rr := wfpDo(r, "POST", base, syncBody(), wfpCaller)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, name)
		assert.Equal(t, "SOURCE_NOT_TRUSTED", code(t, rr))
		assert.Nil(t, st.lastProfile, name)
	}

	// An unverifiable source is a 503, never a pass.
	st := &stubWFPStore{}
	r := newWFPRouter(st, &stubAuthZ{}, &stubTrust{err: domain.ErrSourceTrustUnavailable}, &stubJur{exists: true})
	rr := wfpDo(r, "POST", base, syncBody(), wfpCaller)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Nil(t, st.lastProfile)
}

func TestWFP01_TrustCheckCarriesRealCallerContext(t *testing.T) {
	r, _, tr := okRouter()
	require.Equal(t, http.StatusCreated, wfpDo(r, "POST", base, syncBody(), wfpCaller).Code)
	assert.Equal(t, 1, tr.called)
	assert.Equal(t, "hris", tr.source)
	assert.Equal(t, "tenant-abc", tr.caller.TenantID, "the authenticated tenant, not a body value")
	assert.Equal(t, "p-1", tr.caller.PrincipalID)
	assert.Equal(t, "le-1", tr.caller.LegalEntityID)
}

// ── jurisdiction ─────────────────────────────────────────────────────────────

func TestWFP01_JurisdictionUnsupportedQuarantinesUnreachableFailsClosed(t *testing.T) {
	st := &stubWFPStore{}
	r := newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{exists: false})
	rr := wfpDo(r, "POST", base, syncBody(), wfpCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "PROFILE_QUARANTINED", code(t, rr))
	assert.Equal(t, domain.ReasonUnsupportedJurisdiction, st.lastPre, "the profile is kept as evidence with its reason")

	st = &stubWFPStore{}
	r = newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{err: domain.ErrJurisdictionUnavailable})
	rr = wfpDo(r, "POST", base, syncBody(), wfpCaller)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Nil(t, st.lastProfile, "an unreachable registry never defaults to accepted")
}

// ── sync outcomes ────────────────────────────────────────────────────────────

func TestWFP01_SyncStatusCodes(t *testing.T) {
	r, st, _ := okRouter()
	rr := wfpDo(r, "POST", base, syncBody(), wfpCaller)
	require.Equal(t, http.StatusCreated, rr.Code)
	assert.NotEmpty(t, st.lastProfile.SourceFingerprint)

	// replay → 200
	p := &domain.WorkerFinancialProfile{ProfileID: "p", State: domain.ProfileActive}
	st.syncResult = &store.SyncResult{Profile: p, Created: false}
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", base, syncBody(), wfpCaller).Code)

	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"stale version":   {domain.ErrStaleSourceVersion, http.StatusConflict, "STALE_SOURCE_VERSION"},
		"changed content": {domain.ErrSourceVersionConflict, http.StatusConflict, "SOURCE_VERSION_CONFLICT"},
		"partial overlap": {domain.ErrPartialOverlap, http.StatusConflict, "PARTIAL_OVERLAP_UNSUPPORTED"},
	} {
		st := &stubWFPStore{syncErr: c.err}
		rr := wfpDo(newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{exists: true}), "POST", base, syncBody(), wfpCaller)
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, code(t, rr), name)
	}
}

func TestWFP01_ValidationFailuresAre400(t *testing.T) {
	r, st, _ := okRouter()
	b := syncBody()
	b["pay_currency"] = "pounds"
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", base, b, wfpCaller).Code)
	b = syncBody()
	b["source_version"] = 0
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", base, b, wfpCaller).Code)
	assert.Nil(t, st.lastProfile)
}

func TestWFP01_EventsNeverCarryThePayeeReference(t *testing.T) {
	p := &domain.WorkerFinancialProfile{
		ProfileID: "pr-1", WorkerID: uuid.NewString(), AssignmentID: "A1", LegalEntityID: "le-1",
		PayrollJurisdictionID: "GB", PayCurrency: "GBP", EffectiveFrom: "2026-01-01", SourceSystemID: "hris",
		SourceVersion: 1, State: domain.ProfileActive, Version: 1, PayeeRefChanged: true,
	}
	secret := "SECRET-PAYEE-REF-123"
	p.PayeeRef = &secret
	sup := domain.WorkerFinancialProfile{ProfileID: "old", WorkerID: p.WorkerID, AssignmentID: "A1", LegalEntityID: "le-1", PayrollJurisdictionID: "GB"}
	st := &stubWFPStore{syncResult: &store.SyncResult{Profile: p, Created: true, Superseded: []domain.WorkerFinancialProfile{sup}}}
	r := newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{exists: true})

	require.Equal(t, http.StatusCreated, wfpDo(r, "POST", base, syncBody(), wfpCaller).Code)
	require.Len(t, st.built, 2, "synced + superseded")
	assert.Equal(t, domain.EventProfileSynced, st.built[0].EventType)
	assert.Equal(t, domain.EventProfileSuperseded, st.built[1].EventType)
	for _, ev := range st.built {
		assert.NotContains(t, string(ev.Payload), secret, "payee_ref must not travel on the bus")
		assert.Equal(t, p.WorkerID, ev.ObjectID, "keyed by worker so one worker's events stay ordered")
		assert.Equal(t, "tenant-abc", ev.TenantID)
	}
	assert.Contains(t, string(st.built[0].Payload), `"payee_ref_changed":true`)
}

func TestWFP01_QuarantineEmitsRejectedEvent(t *testing.T) {
	reason := domain.ReasonCrossEntityWorker
	p := &domain.WorkerFinancialProfile{ProfileID: "pr-1", WorkerID: uuid.NewString(), LegalEntityID: "le-1",
		State: domain.ProfileQuarantined, QuarantineReason: &reason}
	st := &stubWFPStore{syncResult: &store.SyncResult{Profile: p, Created: true}}
	r := newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{exists: true})

	rr := wfpDo(r, "POST", base, syncBody(), wfpCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "PROFILE_QUARANTINED", code(t, rr))
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventProfileRejected, st.built[0].EventType)
}

// ── manage / query ───────────────────────────────────────────────────────────

func TestWFP01_DeactivateAndRevalidateRequireExpectedVersion(t *testing.T) {
	p := &domain.WorkerFinancialProfile{ProfileID: "pr-1", WorkerID: uuid.NewString(), LegalEntityID: "le-1", State: domain.ProfileActive, Version: 1}
	st := &stubWFPStore{profile: p, syncResult: &store.SyncResult{Profile: p, Created: true}}
	r := newWFPRouter(st, &stubAuthZ{}, &stubTrust{}, &stubJur{exists: true})

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", base+"/pr-1/deactivate", map[string]any{"reason": "left"}, wfpCaller).Code)
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", base+"/pr-1/deactivate", map[string]any{"expected_version": 1}, wfpCaller).Code, "a reason is required")
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", base+"/pr-1/deactivate", map[string]any{"expected_version": 1, "reason": "left"}, wfpCaller).Code)

	viaHeader := wfpCaller
	viaHeader.ifMatch = `"1"`
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", base+"/pr-1/deactivate", map[string]any{"reason": "left"}, viaHeader).Code, "If-Match is honoured")

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", base+"/pr-1/revalidate", map[string]any{}, wfpCaller).Code)

	st.err = domain.ErrProfileVersionConflict
	rr := wfpDo(r, "POST", base+"/pr-1/deactivate", map[string]any{"expected_version": 1, "reason": "x"}, wfpCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "VERSION_CONFLICT", code(t, rr))
}

func TestWFP01_RevalidateRechecksTrustAndJurisdiction(t *testing.T) {
	p := &domain.WorkerFinancialProfile{ProfileID: "pr-1", WorkerID: uuid.NewString(), LegalEntityID: "le-1",
		PayrollJurisdictionID: "ZZ", SourceSystemID: "hris", State: domain.ProfileQuarantined, Version: 2}
	st := &stubWFPStore{profile: p, syncResult: &store.SyncResult{Profile: p}}
	tr := &stubTrust{}
	r := newWFPRouter(st, &stubAuthZ{}, tr, &stubJur{exists: false})

	rr := wfpDo(r, "POST", base+"/pr-1/revalidate", map[string]any{"expected_version": 2}, wfpCaller)
	assert.Equal(t, 1, tr.called, "the source must still be trusted")
	assert.Equal(t, domain.ReasonUnsupportedJurisdiction, st.lastPre)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "still quarantined is reported, not hidden")
}

func TestWFP01_AsOfRequiresDateAndFailsClosed(t *testing.T) {
	r, st, _ := okRouter()
	w := uuid.NewString()
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "GET", base+"/as-of?worker_id="+w, nil, wfpCaller).Code, "as_of is mandatory")
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "GET", base+"/as-of?worker_id=x&as_of=2026-01-01", nil, wfpCaller).Code)
	assert.Equal(t, http.StatusNotFound, wfpDo(r, "GET", base+"/as-of?worker_id="+w+"&as_of=2026-01-01", nil, wfpCaller).Code, "no profile in force is a 404, not a default")

	st.asOf = []domain.WorkerFinancialProfile{{ProfileID: "pr-1", State: domain.ProfileActive}}
	assert.Equal(t, http.StatusOK, wfpDo(r, "GET", base+"/as-of?worker_id="+w+"&as_of=2026-01-01", nil, wfpCaller).Code)
}

func TestWFP01_ValidateContextReportsEveryReason(t *testing.T) {
	r, st, _ := okRouter()
	w := uuid.NewString()
	vcPath := base + "/validate-context?worker_id=" + w + "&legal_entity_id=le-1&as_of=2026-06-01"

	// worker unknown and no profile: both reasons surface.
	rr := wfpDo(r, "GET", vcPath, nil, wfpCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	var out struct {
		Valid         bool     `json:"valid"`
		Reasons       []string `json:"reasons"`
		PayCurrencies []string `json:"pay_currencies"`
		Jurisdictions []string `json:"payroll_jurisdictions"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.False(t, out.Valid)
	assert.ElementsMatch(t, []string{domain.ReasonWorkerNotFound, "NO_ACTIVE_PROFILE"}, out.Reasons)

	// valid worker, but the only profile belongs to another entity.
	st.facts = domain.WorkerFacts{Found: true, LegalEntityID: "le-1", Status: "ACTIVE"}
	st.asOf = []domain.WorkerFinancialProfile{{ProfileID: "x", LegalEntityID: "le-OTHER", State: domain.ProfileActive}}
	rr = wfpDo(r, "GET", vcPath, nil, wfpCaller)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.False(t, out.Valid)
	assert.Equal(t, []string{"NO_ACTIVE_PROFILE"}, out.Reasons)

	st.asOf = []domain.WorkerFinancialProfile{
		{ProfileID: "x", LegalEntityID: "le-1", State: domain.ProfileActive, PayCurrency: "GBP", PayrollJurisdictionID: "GB"},
		{ProfileID: "y", LegalEntityID: "le-1", State: domain.ProfileActive, PayCurrency: "GBP", PayrollJurisdictionID: "GB"},
	}
	rr = wfpDo(r, "GET", vcPath, nil, wfpCaller)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.True(t, out.Valid)
	assert.Empty(t, out.Reasons)
	assert.Equal(t, []string{"GBP"}, out.PayCurrencies, "distinct pay currencies are reported")
	assert.Equal(t, []string{"GB"}, out.Jurisdictions)
	assert.NotContains(t, rr.Body.String(), "payee")
}
