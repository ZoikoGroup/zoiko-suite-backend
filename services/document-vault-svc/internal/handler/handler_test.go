package handler_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/residency"
	"zoiko.io/document-vault-svc/internal/storage"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	docs      map[string]*domain.Document
	versions  map[string][]domain.DocumentVersion
	accessLog []domain.DocumentAccessLog
	seq       int
	createErr error
	findErr   error
	recordErr error
}

func newStubStore() *stubStore {
	return &stubStore{docs: map[string]*domain.Document{}, versions: map[string][]domain.DocumentVersion{}}
}

func (s *stubStore) CreateDocument(_ context.Context, doc *domain.Document, v *domain.DocumentVersion, _ string) error {
	if s.createErr != nil {
		return s.createErr
	}
	// Unique per create. This used to hardcode "doc-1", so the stub could only
	// ever hold ONE document — every create overwrote the last. That was
	// invisible while no endpoint listed documents; it makes a register
	// untestable, and would have let a broken list endpoint pass.
	s.seq++
	doc.DocumentID = fmt.Sprintf("doc-%d", s.seq)
	doc.CurrentVersion = 1
	doc.Status = domain.StatusActive
	v.DocumentID = doc.DocumentID
	v.DocumentVersionID = fmt.Sprintf("ver-%d-1", s.seq)
	v.Version = 1
	s.docs[doc.DocumentID] = doc
	s.versions[doc.DocumentID] = []domain.DocumentVersion{*v}
	return nil
}

func (s *stubStore) AddVersion(_ context.Context, documentID string, v *domain.DocumentVersion, _ string) (*domain.Document, error) {
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

func (s *stubStore) DeclareRecord(_ context.Context, p domain.DeclareRecordParams, _ string) (*domain.Document, error) {
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if doc.Status != domain.StatusActive {
		return nil, domain.ErrDocumentNotActive
	}
	if doc.DeclaredAt != nil {
		return nil, domain.ErrDocumentAlreadyDeclared
	}
	v := doc.CurrentVersion
	now := time.Now().UTC()
	doc.DeclaredVersion = &v
	doc.DeclaredAt = &now
	doc.DeclaredByPrincipalID = &p.DeclaredByPrincipalID
	return doc, nil
}

func (s *stubStore) SupersedeDocument(_ context.Context, p domain.SupersedeDocumentParams, _ string) (*domain.Document, error) {
	if p.DocumentID == p.SupersededByDocumentID {
		return nil, domain.ErrCannotSupersedeSelf
	}
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if _, ok := s.docs[p.SupersededByDocumentID]; !ok {
		return nil, domain.ErrSupersedingDocumentNotFound
	}
	if doc.SupersededByDocumentID != nil {
		return nil, domain.ErrDocumentAlreadySuperseded
	}
	doc.Status = domain.StatusSuperseded
	doc.SupersededByDocumentID = &p.SupersededByDocumentID
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

func (s *stubStore) ListAccessLog(_ context.Context, documentID string, limit, offset int) ([]domain.DocumentAccessLog, error) {
	out := s.accessLog
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *stubStore) ListDocuments(_ context.Context, legalEntityID string, limit, offset int) ([]domain.Document, error) {
	var out []domain.Document
	for _, d := range s.docs {
		if legalEntityID == "" || d.LegalEntityID == legalEntityID {
			out = append(out, *d)
		}
	}
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ── stub authorization client ────────────────────────────────────────────────

// stubAuthz GRANTS by default. Tests that care about the gate set err, or deny
// a specific (principal, action) pair.
type stubAuthz struct {
	err    error
	denied map[string]bool // "principal|action"
	calls  []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, _, actionType string) error {
	a.calls = append(a.calls, principalID+"|"+actionType)
	if a.denied[principalID+"|"+actionType] {
		return domain.ErrAuthorizationDenied
	}
	return a.err
}

func (a *stubAuthz) called(principal, action string) bool {
	for _, c := range a.calls {
		if c == principal+"|"+action {
			return true
		}
	}
	return false
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

const (
	testTenant    = "11111111-1111-1111-1111-111111111111"
	testPrincipal = "principal-1"
)

// newRouter stands in for the deployed stack: TenantContext reads X-Tenant-Id,
// and the gateway has already stamped an identity by the time a request
// arrives. The injector fills both in when a test has not set them, so the
// tests below stay about document behaviour.
//
// It does NOT paper over the refusals: newRouterRaw omits the injector, and
// gaps_test.go uses it to assert that a request without identity or tenant is
// refused rather than served.
func newRouter(s *stubStore, res residency.Validator, st storage.Backend) chi.Router {
	return newRouterAuthz(s, res, st, &stubAuthz{})
}

func newRouterAuthz(s *stubStore, res residency.Validator, st storage.Backend, az authz.Client) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("X-Principal-Id") == "" {
				req.Header.Set("X-Principal-Id", testPrincipal)
			}
			if req.Header.Get("X-Tenant-Id") == "" {
				req.Header.Set("X-Tenant-Id", testTenant)
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, st, res, az, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newRouterRaw installs no identity and no tenant — the shape of a request that
// reached this service without passing the gateway.
func newRouterRaw(s *stubStore, res residency.Validator, st storage.Backend) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, st, res, &stubAuthz{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func createBody(t *testing.T, classification, content string) []byte {
	t.Helper()
	body, err := json.Marshal(domain.CreateDocumentRequest{
		// Must agree with the request's X-Tenant-Id. The body no longer decides
		// which tenant a document lands in — the verified header does — and a
		// body naming a different one is refused rather than ignored.
		TenantID: testTenant, LegalEntityID: "entity-1", Title: "Contract",
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
		TenantID: testTenant, LegalEntityID: "e", Title: "x", Classification: domain.ClassificationRestricted,
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
		TenantID: testTenant, LegalEntityID: "e", Title: "x", Classification: domain.ClassificationRestricted,
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

// ── DeclareRecord ────────────────────────────────────────────────────────────

func TestDeclareRecord_Valid_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content")))
	r.ServeHTTP(httptest.NewRecorder(), createReq)

	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/declare-record", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.DeclaredAt)
	require.NotNil(t, got.DeclaredVersion)
	assert.Equal(t, 1, *got.DeclaredVersion)
	require.NotNil(t, got.DeclaredByPrincipalID)
	assert.Equal(t, testPrincipal, *got.DeclaredByPrincipalID)
}

func TestDeclareRecord_AlreadyDeclared_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content")))
	r.ServeHTTP(httptest.NewRecorder(), createReq)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/declare-record", nil))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/declare-record", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestDeclareRecord_DocumentNotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/nope/declare-record", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeclareRecord_AuthorizationDenied_Returns403(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	az := &stubAuthz{denied: map[string]bool{testPrincipal + "|" + authz.ActionDocumentDeclareRecord: true}}
	r := newRouterAuthz(s, &stubResidency{}, st, az)

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/declare-record", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ── SupersedeDocument ────────────────────────────────────────────────────────

func supersedeBody(t *testing.T, supersededByID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]string{"superseded_by_document_id": supersededByID})
	require.NoError(t, err)
	return body
}

func TestSupersedeDocument_Valid_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "original"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "replacement"))))

	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/supersede", bytes.NewReader(supersedeBody(t, "doc-2")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.StatusSuperseded, got.Status)
	require.NotNil(t, got.SupersededByDocumentID)
	assert.Equal(t, "doc-2", *got.SupersededByDocumentID)
}

func TestSupersedeDocument_MissingField_Returns400(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/supersede", bytes.NewReader(supersedeBody(t, ""))))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSupersedeDocument_AlreadySuperseded_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "original"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "replacement-1"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "replacement-2"))))

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/supersede", bytes.NewReader(supersedeBody(t, "doc-2"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/supersede", bytes.NewReader(supersedeBody(t, "doc-3"))))
	assert.Equal(t, http.StatusConflict, rec.Code)
}
