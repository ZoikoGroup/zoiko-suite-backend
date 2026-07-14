package handler_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
	"zoiko.io/document-vault-svc/internal/residency"
	"zoiko.io/document-vault-svc/internal/storage"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	docs      map[string]*domain.Document
	versions  map[string][]domain.DocumentVersion
	accessLog []domain.DocumentAccessLog
	createErr error
	findErr   error
	recordErr error
}

func newStubStore() *stubStore {
	return &stubStore{docs: map[string]*domain.Document{}, versions: map[string][]domain.DocumentVersion{}}
}

func (s *stubStore) CreateDocument(_ context.Context, doc *domain.Document, v *domain.DocumentVersion) error {
	if s.createErr != nil {
		return s.createErr
	}
	doc.DocumentID = "doc-1"
	doc.CurrentVersion = 1
	doc.Status = domain.StatusActive
	v.DocumentID = doc.DocumentID
	v.DocumentVersionID = "ver-1"
	v.Version = 1
	s.docs[doc.DocumentID] = doc
	s.versions[doc.DocumentID] = []domain.DocumentVersion{*v}
	return nil
}

func (s *stubStore) AddVersion(_ context.Context, documentID string, v *domain.DocumentVersion) (*domain.Document, error) {
	doc, ok := s.docs[documentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	doc.CurrentVersion++
	v.DocumentID = documentID
	v.Version = doc.CurrentVersion
	v.DocumentVersionID = "ver-new"
	s.versions[documentID] = append(s.versions[documentID], *v)
	return doc, nil
}

func (s *stubStore) FindDocumentByID(_ context.Context, documentID string) (*domain.Document, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	doc, ok := s.docs[documentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	return doc, nil
}

func (s *stubStore) FindVersion(_ context.Context, documentID string, version int) (*domain.DocumentVersion, error) {
	for _, v := range s.versions[documentID] {
		if v.Version == version {
			return &v, nil
		}
	}
	return nil, domain.ErrDocumentVersionNotFound
}

func (s *stubStore) ListVersions(_ context.Context, documentID string) ([]domain.DocumentVersion, error) {
	return s.versions[documentID], nil
}

func (s *stubStore) RecordAccess(_ context.Context, log *domain.DocumentAccessLog) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.accessLog = append(s.accessLog, *log)
	return nil
}

func (s *stubStore) ListAccessLog(_ context.Context, documentID string) ([]domain.DocumentAccessLog, error) {
	return s.accessLog, nil
}

// ── stub residency validator ─────────────────────────────────────────────────

type stubResidency struct {
	err error
}

func (r *stubResidency) CheckRegion(_ context.Context, _, _ string) error { return r.err }

// ── in-memory storage backend (real crypto, no disk) ────────────────────────

func newTestStorage(t *testing.T) storage.Backend {
	t.Helper()
	dir := t.TempDir()
	b, err := storage.NewLocalFileBackend(dir, "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f")
	require.NoError(t, err)
	return b
}

func newRouter(s *stubStore, res residency.Validator, st storage.Backend) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, st, res, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func createBody(t *testing.T, classification, content string) []byte {
	t.Helper()
	body, err := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "tenant-1", LegalEntityID: "entity-1", Title: "Contract",
		Classification: domain.Classification(classification),
		ContentType:    "text/plain",
		ContentBase64:  base64.StdEncoding.EncodeToString([]byte(content)),
	})
	require.NoError(t, err)
	return body
}

// ── CreateDocument ───────────────────────────────────────────────────────────

func TestCreateDocument_Valid_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "hello world")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "doc-1", got.DocumentID)
	assert.Equal(t, 1, got.CurrentVersion)
}

func TestCreateDocument_InvalidClassification_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "TOP_SECRET", "x")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateDocument_EmptyContent_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	body, _ := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "t", LegalEntityID: "e", Title: "x", Classification: domain.ClassificationPublic,
		ContentType: "text/plain", ContentBase64: "",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateDocument_ResidencyMismatch_Returns409(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{err: residency.ErrMismatch}, newTestStorage(t))
	body, _ := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "t", LegalEntityID: "e", Title: "x", Classification: domain.ClassificationRestricted,
		ResidencyRegionCode: strPtr("eu"), ContentType: "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("data")),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestCreateDocument_ResidencyServiceUnavailable_FailsClosed503(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{err: residency.ErrServiceUnavailable}, newTestStorage(t))
	body, _ := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "t", LegalEntityID: "e", Title: "x", Classification: domain.ClassificationRestricted,
		ResidencyRegionCode: strPtr("eu"), ContentType: "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("data")),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── GetDocument / GetContent — real round-trip through real crypto storage ──

func TestGetContent_RoundTrip_MatchesUploadedBytes(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "the real content")))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusCreated, createRec.Code)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/content", nil)
	getRec := httptest.NewRecorder()
	r.ServeHTTP(getRec, getReq)

	require.Equal(t, http.StatusOK, getRec.Code)
	assert.Equal(t, "the real content", getRec.Body.String())
	assert.NotEmpty(t, getRec.Header().Get("X-Checksum-SHA256"))

	// Access must have been logged.
	require.Len(t, s.accessLog, 1)
	assert.Equal(t, domain.AccessDownload, s.accessLog[0].AccessType)
}

func TestGetDocument_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	req := httptest.NewRequest(http.MethodGet, "/v1/documents/nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetDocument_RecordsMetadataAccess(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "PUBLIC", "x")))
	r.ServeHTTP(httptest.NewRecorder(), createReq)

	req := httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, s.accessLog, 1)
	assert.Equal(t, domain.AccessMetadata, s.accessLog[0].AccessType)
}

// ── AddVersion ───────────────────────────────────────────────────────────────

func TestAddVersion_Valid_BumpsCurrentVersion(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "v1 content")))
	r.ServeHTTP(httptest.NewRecorder(), createReq)

	body, _ := json.Marshal(domain.CreateDocumentVersionRequest{
		ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("v2 content")),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/versions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 2, got.CurrentVersion)
}

func TestAddVersion_DocumentNotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	body, _ := json.Marshal(domain.CreateDocumentVersionRequest{
		ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/nope/versions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func strPtr(s string) *string { return &s }
