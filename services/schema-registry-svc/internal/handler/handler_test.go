package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/domain"
	"zoiko.io/schema-registry-svc/internal/handler"
)

// ── stub store ────────────────────────────────────────────────────────────────

type stubStore struct {
	latest    *domain.EventSchema
	latestErr error

	version    *domain.EventSchema
	versionErr error

	versions    []*domain.EventSchema
	versionsErr error

	names    []string
	namesErr error

	insertErr   error
	insertedArg *domain.EventSchema
	// insertedExpectedVersion records the optimistic-concurrency token the
	// handler passed down, so tests can prove the compatibility baseline and
	// the write guard are the same version.
	insertedExpectedVersion int

	// gotLimit/gotOffset record the paging bounds the handler applied, so a
	// test can prove the reads are bounded rather than assuming it.
	gotLimit, gotOffset int
}

func (s *stubStore) LatestVersion(_ context.Context, _ string) (*domain.EventSchema, error) {
	return s.latest, s.latestErr
}
func (s *stubStore) Version(_ context.Context, _ string, _ int) (*domain.EventSchema, error) {
	return s.version, s.versionErr
}
func (s *stubStore) Versions(_ context.Context, _ string, limit, offset int) ([]*domain.EventSchema, error) {
	s.gotLimit, s.gotOffset = limit, offset
	return s.versions, s.versionsErr
}
func (s *stubStore) EventNames(_ context.Context, limit, offset int) ([]string, error) {
	s.gotLimit, s.gotOffset = limit, offset
	return s.names, s.namesErr
}
func (s *stubStore) Insert(_ context.Context, sch *domain.EventSchema, expectedVersion int) (*domain.EventSchema, error) {
	s.insertedArg = sch
	s.insertedExpectedVersion = expectedVersion
	if s.insertErr != nil {
		return nil, s.insertErr
	}
	// The real store assigns the version inside the INSERT and returns the
	// stored row; mirror that so the handler's response body is exercised.
	stored := *sch
	stored.Version = expectedVersion + 1
	return &stored, nil
}

// ── stub authz client ──────────────────────────────────────────────────────

type stubAuthz struct {
	err          error
	gotPrincipal string
	// gotScope records the legal entity the handler authorized against, so a
	// test can prove an entity-less registration falls back to the platform
	// scope rather than sending "" (which authorization-svc rejects outright).
	gotScope string
}

func (a *stubAuthz) CheckSchemaPublishAllowed(_ context.Context, principalID, legalEntityID, _ string) error {
	a.gotPrincipal = principalID
	a.gotScope = legalEntityID
	return a.err
}

// newRouter builds a router whose authz client always GRANTS — the default
// for tests focused on store/compat behavior. Register requests must carry
// the X-Principal-Id header the gateway would set (see withIdentity).
func newRouter(s *stubStore) chi.Router {
	return newRouterWithAuthz(s, &stubAuthz{})
}

// testPlatformScope stands in for AUTHZ_PLATFORM_SCOPE_ID.
const testPlatformScope = "00000000-0000-0000-0000-00000000f001"

func newRouterWithAuthz(s *stubStore, a *stubAuthz) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, a, testPlatformScope, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// getWithIdentity is a GET carrying the identity headers the gateway sets.
// Reads used to require none at all — anything that could reach the port could
// enumerate the platform's entire event-contract catalogue.
func getWithIdentity(path string) *http.Request {
	return withIdentity(httptest.NewRequest(http.MethodGet, path, nil))
}

// withIdentity stamps the identity headers gateway-auth-svc sets on a
// verified request, so a register call clears the authorization gate.
func withIdentity(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", "principal-admin-001")
	req.Header.Set("X-Legal-Entity-Id", "entity-001")
	return req
}

// ── RegisterVersion ──────────────────────────────────────────────────────────

func TestRegisterVersion_FirstVersion_Returns201WithVersion1(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.EventSchema
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, "identity.context.resolved", got.EventName)
	require.NotNil(t, s.insertedArg)
	assert.Equal(t, "principal-admin-001", s.insertedArg.RegisteredBy)
	// The handler no longer computes the version — the store assigns it inside
	// the INSERT so concurrent registrations cannot collide. What the handler
	// is responsible for is the baseline it checked against, which doubles as
	// the optimistic-concurrency guard. 0 means "no version registered yet".
	assert.Equal(t, 0, s.insertedExpectedVersion,
		"a first registration must be guarded against a baseline of 0")
	// An omitted mode is stored as BACKWARD, not left blank — §17.2 requires
	// the mode to be declared, and a blank column would record nothing.
	assert.Equal(t, domain.CompatibilityBackward, s.insertedArg.CompatibilityMode)
}

func TestRegisterVersion_CompatibleEvolution_Returns201WithNextVersion(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName:  "identity.context.resolved",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`),
	}}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"principal_id":{"type":"string"},"session_id":{"type":"string"}},"required":["principal_id"]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.EventSchema
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 2, got.Version)
}

func TestRegisterVersion_BreakingChange_Returns409WithViolations(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName:  "identity.context.resolved",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id","tenant_id"]}`),
	}}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Contains(t, got, "violations")
	assert.Nil(t, s.insertedArg, "must not persist a rejected schema")
}

func TestRegisterVersion_MissingSchema_Returns400(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegisterVersion_MalformedJSONSchema_Returns400(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(`{"json_schema": not-json}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegisterVersion_StoreUnavailable_Returns503(t *testing.T) {
	s := &stubStore{latestErr: assertErr}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{},"required":[]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── Authorization gate (chunk 2) ────────────────────────────────────────────

func TestRegisterVersion_NoIdentityHeader_Returns401(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{},"required":[]}}`
	// No withIdentity — simulates a request that never passed the gateway.
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, s.insertedArg, "must not persist without a resolved identity")
}

func TestRegisterVersion_AuthorizationDenied_Returns403(t *testing.T) {
	s := &stubStore{}
	a := &stubAuthz{err: domain.ErrPublishDenied}
	r := newRouterWithAuthz(s, a)

	body := `{"json_schema":{"properties":{},"required":[]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "principal-admin-001", a.gotPrincipal)
	assert.Nil(t, s.insertedArg, "must not persist a denied mutation")
}

func TestRegisterVersion_AuthorizationServiceUnavailable_Returns503FailClosed(t *testing.T) {
	s := &stubStore{}
	a := &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}
	r := newRouterWithAuthz(s, a)

	body := `{"json_schema":{"properties":{},"required":[]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.event.name/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, s.insertedArg, "must fail closed when authorization-svc is unreachable")
}

// ── GetLatest / GetVersion / ListVersions / ListEventNames ──────────────────

func TestGetLatest_Found_Returns200(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{EventName: "probe.event.read", Version: 3, JSONSchema: json.RawMessage(`{}`)}}
	r := newRouter(s)

	req := getWithIdentity("/v1/schemas/probe.event.read/versions/latest")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetLatest_NotFound_Returns404(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)

	req := getWithIdentity("/v1/schemas/probe.event.read/versions/latest")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetVersion_NonIntegerVersion_Returns400(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	req := getWithIdentity("/v1/schemas/probe.event.read/versions/abc")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListVersions_None_Returns404(t *testing.T) {
	s := &stubStore{versions: nil}
	r := newRouter(s)

	req := getWithIdentity("/v1/schemas/probe.event.read/versions")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListEventNames_Returns200(t *testing.T) {
	s := &stubStore{names: []string{"a", "b"}}
	r := newRouter(s)

	req := getWithIdentity("/v1/schemas/")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

var assertErr = &testError{"store unavailable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// ── compatibility_mode (04-data-model.md §17.2) ─────────────────────────────

// TestRegisterVersion_DeclaredNoneSkipsCompatibilityCheck covers the
// controlled-rollout case §17.2 allows for. Before the mode was declarable
// every schema was checked for backward compatibility whatever its author
// intended, so a contract that was legitimately allowed a breaking change
// could not be evolved at all except by registering it under a new name.
func TestRegisterVersion_DeclaredNoneSkipsCompatibilityCheck(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName:  "internal.probe",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"old_field":{"type":"string"}},"required":["old_field"]}`),
	}}
	r := newRouter(s)

	// Removes a required field — unambiguously breaking under BACKWARD.
	body := `{"json_schema":{"properties":{"new_field":{"type":"string"}},"required":["new_field"]},"compatibility_mode":"NONE"}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/internal.probe/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code,
		"a breaking change must be accepted when NONE is declared")
	assert.Equal(t, domain.CompatibilityNone, s.insertedArg.CompatibilityMode,
		"the declared mode must be recorded on the row, so the exemption is visible in the register")
}

// TestRegisterVersion_BreakingChangeStillRefusedUnderBackward is the other
// half: NONE must be an explicit opt-out, not a weakening of the default.
func TestRegisterVersion_BreakingChangeStillRefusedUnderBackward(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName:  "public.probe",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"old_field":{"type":"string"}},"required":["old_field"]}`),
	}}
	r := newRouter(s)

	// Same breaking body, mode omitted — must default to BACKWARD and refuse.
	body := `{"json_schema":{"properties":{"new_field":{"type":"string"}},"required":["new_field"]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/public.probe/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Nil(t, s.insertedArg, "a refused schema must never reach the store")
}

// TestRegisterVersion_UnknownCompatibilityModeRefused — an unrecognised mode
// is rejected rather than quietly treated as BACKWARD. Defaulting it would
// record a discipline the service is not applying, which is worse than a 400.
func TestRegisterVersion_UnknownCompatibilityModeRefused(t *testing.T) {
	for _, mode := range []string{"FORWARD", "FULL", "backward", "ANYTHING"} {
		t.Run(mode, func(t *testing.T) {
			s := &stubStore{latest: nil}
			r := newRouter(s)

			body := `{"json_schema":{"properties":{}},"compatibility_mode":"` + mode + `"}`
			req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/x.probe/versions", bytes.NewBufferString(body)))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code,
				"mode %q is not enforceable by this service and must be refused", mode)
			assert.Nil(t, s.insertedArg, "nothing may be written for an unenforceable mode")
		})
	}
}

// TestRegisterVersion_OwningServiceRecorded — §17.1 lists owning_service on
// SchemaRegistryArtifact. Without it the registry can say a contract changed
// but not who is responsible for it.
func TestRegisterVersion_OwningServiceRecorded(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{}},"owning_service":"identity-context-svc"}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/y.probe/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "identity-context-svc", s.insertedArg.OwningService)
}

// ── version race (was reported as a 503) ────────────────────────────────────

// TestRegisterVersion_LostRaceIs409Not503 is the regression test for a
// concurrent registration being reported as a database outage.
//
// The handler used to compute the next version itself, so a race ended in a
// primary-key collision that surfaced as ErrStoreUnavailable — a 503, which
// sends the reader to look for a broken database when nothing is broken. It
// is a 409, and the message tells the caller to re-read and retry rather than
// simply retry: its schema was checked against a version that is no longer
// latest.
func TestRegisterVersion_LostRaceIs409Not503(t *testing.T) {
	s := &stubStore{
		latest: &domain.EventSchema{
			EventName:  "raced.probe",
			Version:    3,
			JSONSchema: json.RawMessage(`{"properties":{},"required":[]}`),
		},
		insertErr: domain.ErrVersionRaced,
	}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"extra":{"type":"string"}},"required":[]}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/raced.probe/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code,
		"a lost version race is a conflict, not a store outage")
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "retry")
	assert.Equal(t, 3, s.insertedExpectedVersion,
		"the guard passed to the store must be the version the compatibility check ran against")
}

// TestRegisterVersion_GenuineStoreFailureIsStill503 — the 409 above must not
// have swallowed real database failures.
func TestRegisterVersion_GenuineStoreFailureIsStill503(t *testing.T) {
	s := &stubStore{latest: nil, insertErr: errors.New("connection refused")}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{}}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/z.probe/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
