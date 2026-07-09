package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
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
}

func (s *stubStore) LatestVersion(_ context.Context, _ string) (*domain.EventSchema, error) {
	return s.latest, s.latestErr
}
func (s *stubStore) Version(_ context.Context, _ string, _ int) (*domain.EventSchema, error) {
	return s.version, s.versionErr
}
func (s *stubStore) Versions(_ context.Context, _ string) ([]*domain.EventSchema, error) {
	return s.versions, s.versionsErr
}
func (s *stubStore) EventNames(_ context.Context) ([]string, error) {
	return s.names, s.namesErr
}
func (s *stubStore) Insert(_ context.Context, sch *domain.EventSchema) error {
	s.insertedArg = sch
	return s.insertErr
}

func newRouter(s *stubStore) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// ── RegisterVersion ──────────────────────────────────────────────────────────

func TestRegisterVersion_FirstVersion_Returns201WithVersion1(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.EventSchema
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, "identity.context.resolved", got.EventName)
	require.NotNil(t, s.insertedArg)
	assert.Equal(t, 1, s.insertedArg.Version)
}

func TestRegisterVersion_CompatibleEvolution_Returns201WithNextVersion(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName:  "identity.context.resolved",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`),
	}}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{"principal_id":{"type":"string"},"session_id":{"type":"string"}},"required":["principal_id"]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/identity.context.resolved/versions", bytes.NewBufferString(body))
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

	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/foo/versions", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegisterVersion_MalformedJSONSchema_Returns400(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/foo/versions", bytes.NewBufferString(`{"json_schema": not-json}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegisterVersion_StoreUnavailable_Returns503(t *testing.T) {
	s := &stubStore{latestErr: assertErr}
	r := newRouter(s)

	body := `{"json_schema":{"properties":{},"required":[]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/foo/versions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── GetLatest / GetVersion / ListVersions / ListEventNames ──────────────────

func TestGetLatest_Found_Returns200(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{EventName: "foo", Version: 3, JSONSchema: json.RawMessage(`{}`)}}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/foo/versions/latest", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetLatest_NotFound_Returns404(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/foo/versions/latest", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetVersion_NonIntegerVersion_Returns400(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/foo/versions/abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListVersions_None_Returns404(t *testing.T) {
	s := &stubStore{versions: nil}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/foo/versions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListEventNames_Returns200(t *testing.T) {
	s := &stubStore{names: []string{"a", "b"}}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

var assertErr = &testError{"store unavailable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
