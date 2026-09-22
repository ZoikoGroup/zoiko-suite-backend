package handler_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"zoiko.io/document-vault-svc/internal/scan"
	"zoiko.io/document-vault-svc/internal/storage"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	docs              map[string]*domain.Document
	versions          map[string][]domain.DocumentVersion
	accessLog         []domain.DocumentAccessLog
	links             map[string][]domain.DocumentLink
	quarantineEvents  []string
	classifications   []*domain.RecordClassification
	seq               int
	linkSeq           int
	classificationSeq int
	createErr         error
	findErr           error
	recordErr         error
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

	// Mirrors the real store: the uploader's classification choice is
	// seeded as a CANDIDATE proposal, not trusted as final.
	s.classificationSeq++
	s.classifications = append(s.classifications, &domain.RecordClassification{
		ClassificationID: fmt.Sprintf("classification-%d", s.classificationSeq), DocumentID: doc.DocumentID,
		LegalEntityID: doc.LegalEntityID, ClassificationValue: doc.Classification,
		Status: domain.ClassificationStatusCandidate, Source: domain.ClassificationSourceHuman,
		ProposedByPrincipalID: doc.CreatedByPrincipalID, ProposedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
	})
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

func (s *stubStore) MoveToArchive(_ context.Context, p domain.MoveToArchiveParams, _ string) (*domain.Document, error) {
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if doc.Status != domain.StatusActive && doc.Status != domain.StatusSuperseded {
		return nil, domain.ErrDocumentNotArchivable
	}
	now := time.Now().UTC()
	doc.Status = domain.StatusArchived
	doc.ArchivedAt = &now
	doc.ArchivedByPrincipalID = &p.ArchivedByPrincipalID
	if p.Reason != "" {
		doc.ArchiveReason = &p.Reason
	}
	return doc, nil
}

func (s *stubStore) RequestDisposition(_ context.Context, p domain.RequestDispositionParams, _ string) (*domain.Document, error) {
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if doc.DispositionRequestedAt != nil {
		return nil, domain.ErrDispositionAlreadyRequested
	}
	now := time.Now().UTC()
	doc.Status = domain.StatusPurgePending
	doc.DispositionRequestedAt = &now
	doc.DispositionRequestedByPrincipalID = &p.RequestedByPrincipalID
	if p.Reason != "" {
		doc.DispositionReason = &p.Reason
	}
	return doc, nil
}

func (s *stubStore) GetAsOfDocument(_ context.Context, documentID string, asOf time.Time) (*domain.Document, *domain.DocumentVersion, error) {
	doc, ok := s.docs[documentID]
	if !ok {
		return nil, nil, domain.ErrDocumentNotFound
	}
	versions := s.versions[documentID]
	var found *domain.DocumentVersion
	for i := range versions {
		if versions[i].CreatedAt.After(asOf) {
			continue
		}
		if found == nil || versions[i].Version > found.Version {
			found = &versions[i]
		}
	}
	if found == nil {
		return nil, nil, domain.ErrDocumentVersionNotFound
	}
	return doc, found, nil
}

func (s *stubStore) LinkDocument(_ context.Context, p domain.LinkDocumentParams) (*domain.DocumentLink, error) {
	if _, ok := s.docs[p.DocumentID]; !ok {
		return nil, domain.ErrDocumentNotFound
	}
	for _, l := range s.links[p.DocumentID] {
		if l.LinkedObjectType == p.LinkedObjectType && l.LinkedObjectID == p.LinkedObjectID {
			return nil, domain.ErrDuplicateLink
		}
	}
	s.linkSeq++
	link := domain.DocumentLink{
		LinkID: fmt.Sprintf("link-%d", s.linkSeq), DocumentID: p.DocumentID,
		LinkedObjectType: p.LinkedObjectType, LinkedObjectID: p.LinkedObjectID,
		LinkedByPrincipalID: p.LinkedByPrincipalID, CreatedAt: time.Now().UTC(),
	}
	if s.links == nil {
		s.links = map[string][]domain.DocumentLink{}
	}
	s.links[p.DocumentID] = append(s.links[p.DocumentID], link)
	return &link, nil
}

func (s *stubStore) ListDocumentLinks(_ context.Context, documentID string) ([]domain.DocumentLink, error) {
	if _, ok := s.docs[documentID]; !ok {
		return nil, domain.ErrDocumentNotFound
	}
	return s.links[documentID], nil
}

func (s *stubStore) ClassifyRecord(_ context.Context, p domain.ClassifyRecordParams) (*domain.RecordClassification, error) {
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if p.Source != domain.ClassificationSourceHuman && p.Source != domain.ClassificationSourceAI {
		return nil, domain.ErrInvalidClassificationSource
	}
	if p.Source == domain.ClassificationSourceAI && p.Confidence == nil {
		return nil, domain.ErrAIConfidenceRequired
	}
	if p.Source == domain.ClassificationSourceHuman && p.Confidence != nil {
		return nil, domain.ErrHumanConfidenceNotAllowed
	}
	for _, c := range s.classifications {
		if c.DocumentID == p.DocumentID && (c.Status == domain.ClassificationStatusConfirmed || c.Status == domain.ClassificationStatusRestricted) {
			return nil, domain.ErrClassificationNotCandidate
		}
	}
	s.classificationSeq++
	c := &domain.RecordClassification{
		ClassificationID: fmt.Sprintf("classification-%d", s.classificationSeq), DocumentID: p.DocumentID,
		LegalEntityID: doc.LegalEntityID, ClassificationValue: p.ClassificationValue,
		Status: domain.ClassificationStatusCandidate, Source: p.Source, Confidence: p.Confidence,
		ProposedByPrincipalID: p.ProposedByPrincipalID, ProposedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
	}
	s.classifications = append(s.classifications, c)
	return c, nil
}

func (s *stubStore) FindClassificationByID(_ context.Context, classificationID string) (*domain.RecordClassification, error) {
	for _, c := range s.classifications {
		if c.ClassificationID == classificationID {
			return c, nil
		}
	}
	return nil, domain.ErrClassificationNotFound
}

func (s *stubStore) ConfirmClassification(_ context.Context, p domain.ConfirmClassificationParams) (*domain.RecordClassification, error) {
	for _, c := range s.classifications {
		if c.ClassificationID != p.ClassificationID {
			continue
		}
		if c.Source == domain.ClassificationSourceHuman && c.ProposedByPrincipalID == p.ConfirmedByPrincipalID {
			return nil, domain.ErrClassificationSelfConfirmation
		}
		if c.Status != domain.ClassificationStatusCandidate {
			return nil, domain.ErrClassificationNotCandidate
		}
		now := time.Now().UTC()
		c.Status = domain.ClassificationStatusConfirmed
		c.ConfirmedByPrincipalID = &p.ConfirmedByPrincipalID
		c.ConfirmedAt = &now
		if doc, ok := s.docs[c.DocumentID]; ok {
			doc.Classification = c.ClassificationValue
		}
		return c, nil
	}
	return nil, domain.ErrClassificationNotFound
}

func (s *stubStore) Reclassify(_ context.Context, p domain.ReclassifyParams) (*domain.RecordClassification, error) {
	doc, ok := s.docs[p.DocumentID]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if p.Source != domain.ClassificationSourceHuman && p.Source != domain.ClassificationSourceAI {
		return nil, domain.ErrInvalidClassificationSource
	}
	if p.Source == domain.ClassificationSourceAI && p.Confidence == nil {
		return nil, domain.ErrAIConfidenceRequired
	}
	if p.Source == domain.ClassificationSourceHuman && p.Confidence != nil {
		return nil, domain.ErrHumanConfidenceNotAllowed
	}
	var current *domain.RecordClassification
	for _, c := range s.classifications {
		if c.DocumentID == p.DocumentID && c.Status != domain.ClassificationStatusSuperseded {
			current = c
		}
	}
	if current == nil || !domain.CanReclassify(current) {
		return nil, domain.ErrClassificationNotConfirmed
	}
	s.classificationSeq++
	c := &domain.RecordClassification{
		ClassificationID: fmt.Sprintf("classification-%d", s.classificationSeq), DocumentID: p.DocumentID,
		LegalEntityID: doc.LegalEntityID, ClassificationValue: p.ClassificationValue,
		Status: domain.ClassificationStatusCandidate, Source: p.Source, Confidence: p.Confidence,
		ProposedByPrincipalID: p.ProposedByPrincipalID, ProposedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
	}
	s.classifications = append(s.classifications, c)
	return c, nil
}

func (s *stubStore) SupersedeClassification(_ context.Context, p domain.SupersedeClassificationParams) (*domain.RecordClassification, error) {
	var previous, next *domain.RecordClassification
	for _, c := range s.classifications {
		if c.ClassificationID == p.PreviousClassificationID {
			previous = c
		}
		if c.ClassificationID == p.NewClassificationID {
			next = c
		}
	}
	if previous == nil || next == nil {
		return nil, domain.ErrClassificationNotFound
	}
	if previous.SupersededByClassificationID != nil {
		return nil, domain.ErrClassificationAlreadySuperseded
	}
	if previous.Status != domain.ClassificationStatusConfirmed && previous.Status != domain.ClassificationStatusRestricted {
		return nil, domain.ErrClassificationNotConfirmed
	}
	if next.DocumentID != previous.DocumentID {
		return nil, domain.ErrClassificationDocumentMismatch
	}
	if next.Source == domain.ClassificationSourceHuman && next.ProposedByPrincipalID == p.ActorPrincipalID {
		return nil, domain.ErrClassificationSelfConfirmation
	}
	if next.Status != domain.ClassificationStatusCandidate {
		return nil, domain.ErrClassificationNotCandidate
	}
	now := time.Now().UTC()
	next.Status = domain.ClassificationStatusConfirmed
	next.ConfirmedByPrincipalID = &p.ActorPrincipalID
	next.ConfirmedAt = &now
	previous.Status = domain.ClassificationStatusSuperseded
	previous.SupersededByClassificationID = &next.ClassificationID
	if doc, ok := s.docs[next.DocumentID]; ok {
		doc.Classification = next.ClassificationValue
	}
	return next, nil
}

func (s *stubStore) GetClassification(_ context.Context, documentID string) (*domain.RecordClassification, error) {
	if _, ok := s.docs[documentID]; !ok {
		return nil, domain.ErrDocumentNotFound
	}
	var latest *domain.RecordClassification
	for _, c := range s.classifications {
		if c.DocumentID != documentID || c.Status == domain.ClassificationStatusSuperseded {
			continue
		}
		if latest == nil || c.ProposedAt.After(latest.ProposedAt) {
			latest = c
		}
	}
	if latest == nil {
		return nil, domain.ErrClassificationNotFound
	}
	return latest, nil
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

func (s *stubStore) RecordQuarantinedVersionUpload(_ context.Context, documentID, _, _, _ string) error {
	if _, ok := s.docs[documentID]; !ok {
		return domain.ErrDocumentNotFound
	}
	s.quarantineEvents = append(s.quarantineEvents, documentID)
	return nil
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

// ── stub scanner ─────────────────────────────────────────────────────────────

// stubScanner defaults to clean, same posture as the real NoOpScanner —
// tests that want a quarantine set clean=false explicitly.
type stubScanner struct {
	clean  bool
	reason string
	err    error
}

func newStubScanner() *stubScanner { return &stubScanner{clean: true} }

func (s *stubScanner) Scan(_ context.Context, _ []byte, _ string) (scan.Result, error) {
	if s.err != nil {
		return scan.Result{}, s.err
	}
	return scan.Result{Clean: s.clean, Reason: s.reason}, nil
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
	h := handler.New(s, st, res, az, newStubScanner(), zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newRouterScanner is newRouterAuthz with a caller-supplied scanner — for
// tests that need to control the quarantine gate specifically.
func newRouterScanner(s *stubStore, res residency.Validator, st storage.Backend, sc scan.Scanner) chi.Router {
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
	h := handler.New(s, st, res, &stubAuthz{}, sc, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newRouterRaw installs no identity and no tenant — the shape of a request that
// reached this service without passing the gateway.
func newRouterRaw(s *stubStore, res residency.Validator, st storage.Backend) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, st, res, &stubAuthz{}, newStubScanner(), zap.NewNop())
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

// ── Scan gate (BIZ-01 Wave 4) ────────────────────────────────────────────────

func TestCreateDocument_Quarantined_Returns422_NeverPersists(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouterScanner(s, &stubResidency{}, st, &stubScanner{clean: false, reason: "malware signature match"})

	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "malware signature match")
	assert.Empty(t, s.docs, "expected the quarantined upload to never create a document row")
}

func TestCreateDocument_ScanUnavailable_FailsClosed503(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouterScanner(s, &stubResidency{}, st, &stubScanner{err: errors.New("scanner down")})

	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, s.docs, "expected an unavailable scanner to fail closed, never persisting the document")
}

func TestAddVersion_Quarantined_Returns422_RecordsEventNeverPersistsVersion(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "v1 content"))))

	quarantineRouter := newRouterScanner(s, &stubResidency{}, st, &stubScanner{clean: false, reason: "type mismatch"})
	body, _ := json.Marshal(domain.CreateDocumentVersionRequest{
		ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("v2 content")),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/versions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	quarantineRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "type mismatch")
	assert.Equal(t, 1, s.docs["doc-1"].CurrentVersion, "expected the quarantined version to never bump current_version")
	assert.Equal(t, []string{"doc-1"}, s.quarantineEvents, "expected the quarantine event to be recorded against the existing document")
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

// ── MoveToArchive ────────────────────────────────────────────────────────────

func TestMoveToArchive_Valid_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	body, _ := json.Marshal(map[string]string{"reason": "no longer needed"})
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/archive", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.StatusArchived, got.Status)
	require.NotNil(t, got.ArchivedAt)
}

func TestMoveToArchive_NoBody_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/archive", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMoveToArchive_AlreadyArchived_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/archive", nil))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/archive", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// ── RequestDisposition ───────────────────────────────────────────────────────

func TestRequestDisposition_Valid_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/request-disposition", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.StatusPurgePending, got.Status)
	require.NotNil(t, got.DispositionRequestedAt)
}

func TestRequestDisposition_AlreadyRequested_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/request-disposition", nil))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/request-disposition", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// ── VerifyDigest ─────────────────────────────────────────────────────────────

func TestVerifyDigest_Valid_ReturnsVerifiedTrue(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/verify-digest", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.DigestVerification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Verified)
	assert.Equal(t, 1, got.Version)
}

func TestVerifyDigest_DocumentNotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/nope/verify-digest", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ── GetAsOfDocument ──────────────────────────────────────────────────────────

func TestGetAsOfDocument_MissingParam_Returns400(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/as-of", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetAsOfDocument_InvalidTimestamp_Returns400(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/as-of?as_of=not-a-timestamp", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetAsOfDocument_Valid_Returns200(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/as-of?as_of="+time.Now().UTC().Format(time.RFC3339), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Document domain.Document        `json:"document"`
		Version  domain.DocumentVersion `json:"version_as_of"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "doc-1", got.Document.DocumentID)
	assert.Equal(t, 1, got.Version.Version)
}

// ── LinkDocument / GetLinkedObjects ──────────────────────────────────────────

func linkBody(t *testing.T, objectType, objectID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]string{"linked_object_type": objectType, "linked_object_id": objectID})
	require.NoError(t, err)
	return body
}

func TestLinkDocument_Valid_Returns201(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "EXPENSE_CLAIM", "claim-1")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.DocumentLink
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "EXPENSE_CLAIM", got.LinkedObjectType)
	assert.Equal(t, "claim-1", got.LinkedObjectID)
}

func TestLinkDocument_MissingField_Returns400(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "EXPENSE_CLAIM", ""))))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLinkDocument_Duplicate_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "EXPENSE_CLAIM", "claim-1"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "EXPENSE_CLAIM", "claim-1"))))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestGetLinkedObjects_ReturnsAllLinks(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "EXPENSE_CLAIM", "claim-1"))))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/links", bytes.NewReader(linkBody(t, "WORKFLOW_INSTANCE", "wf-1"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/links", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got []domain.DocumentLink
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestGetLinkedObjects_NoLinks_ReturnsEmptyArray(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "INTERNAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/links", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, "[]", rec.Body.String())
}

func TestGetLinkedObjects_DocumentNotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/nope/links", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ── ClassifyRecord / ConfirmClassification / GetClassification (BIZ-02) ─────

func TestCreateDocument_SeedsCandidateClassification(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/doc-1/classification", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.RecordClassification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.ClassificationStatusCandidate, got.Status)
	assert.Equal(t, domain.Classification("CONFIDENTIAL"), got.ClassificationValue)
	assert.Nil(t, got.ConfirmedAt)
}

func classifyBody(t *testing.T, value, source string, confidence *float64) []byte {
	t.Helper()
	req := map[string]any{"classification_value": value, "source": source}
	if confidence != nil {
		req["confidence"] = *confidence
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return body
}

func TestConfirmClassification_Valid_Returns200_UpdatesDocument(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	proposal := s.classifications[0]

	req := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+proposal.ClassificationID+"/confirm", nil)
	req.Header.Set("X-Principal-Id", "steward-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.RecordClassification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.ClassificationStatusConfirmed, got.Status)
	assert.Equal(t, domain.Classification("CONFIDENTIAL"), s.docs["doc-1"].Classification)
}

func TestConfirmClassification_SelfConfirmation_Returns403(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	proposal := s.classifications[0]

	// createBody's default uploader is testPrincipal — confirming as the
	// same principal must be refused.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+proposal.ClassificationID+"/confirm", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestConfirmClassification_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/nope/confirm", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestClassifyRecord_AIWithoutConfidence_Returns400(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/classify", bytes.NewReader(classifyBody(t, "RESTRICTED", "AI", nil))))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestClassifyRecord_AIWithConfidence_Returns201_StaysCandidate(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))

	confidence := 0.95
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/classify", bytes.NewReader(classifyBody(t, "RESTRICTED", "AI", &confidence))))
	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.RecordClassification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.ClassificationStatusCandidate, got.Status, "expected no auto-confirm even at high confidence")
}

// ── Reclassify / SupersedeClassification (BIZ-02 Wave 2) ────────────────────

func confirmFirstClassification(t *testing.T, r chi.Router, s *stubStore) *domain.RecordClassification {
	t.Helper()
	proposal := s.classifications[0]
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+proposal.ClassificationID+"/confirm", nil)
	req.Header.Set("X-Principal-Id", "steward-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got domain.RecordClassification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return &got
}

func TestReclassify_WithoutConfirmedClassification_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "PUBLIC", "HUMAN", nil))))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestReclassify_ThenSupersede_ConfirmsNewAndSupersedesOld(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	original := confirmFirstClassification(t, r, s)

	reclassifyRec := httptest.NewRecorder()
	r.ServeHTTP(reclassifyRec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "RESTRICTED", "HUMAN", nil))))
	require.Equal(t, http.StatusCreated, reclassifyRec.Code)
	var proposed domain.RecordClassification
	require.NoError(t, json.Unmarshal(reclassifyRec.Body.Bytes(), &proposed))
	assert.Equal(t, domain.ClassificationStatusCandidate, proposed.Status)

	supersedeBody, err := json.Marshal(map[string]string{"new_classification_id": proposed.ClassificationID})
	require.NoError(t, err)
	supersedeReq := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+original.ClassificationID+"/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("X-Principal-Id", "steward-2")
	supersedeRec := httptest.NewRecorder()
	r.ServeHTTP(supersedeRec, supersedeReq)

	require.Equal(t, http.StatusOK, supersedeRec.Code)
	var confirmed domain.RecordClassification
	require.NoError(t, json.Unmarshal(supersedeRec.Body.Bytes(), &confirmed))
	assert.Equal(t, domain.ClassificationStatusConfirmed, confirmed.Status)
	assert.Equal(t, domain.Classification("RESTRICTED"), confirmed.ClassificationValue)
	assert.Equal(t, domain.ClassificationStatusSuperseded, s.classifications[0].Status)
	require.NotNil(t, s.classifications[0].SupersededByClassificationID)
	assert.Equal(t, proposed.ClassificationID, *s.classifications[0].SupersededByClassificationID)
	assert.Equal(t, domain.Classification("RESTRICTED"), s.docs["doc-1"].Classification)
}

func TestSupersedeClassification_SelfConfirmation_Returns403(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	original := confirmFirstClassification(t, r, s)

	reclassifyReq := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "RESTRICTED", "HUMAN", nil)))
	reclassifyReq.Header.Set("X-Principal-Id", "steward-3")
	reclassifyRec := httptest.NewRecorder()
	r.ServeHTTP(reclassifyRec, reclassifyReq)
	require.Equal(t, http.StatusCreated, reclassifyRec.Code)
	var proposed domain.RecordClassification
	require.NoError(t, json.Unmarshal(reclassifyRec.Body.Bytes(), &proposed))

	supersedeBody, err := json.Marshal(map[string]string{"new_classification_id": proposed.ClassificationID})
	require.NoError(t, err)
	supersedeReq := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+original.ClassificationID+"/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("X-Principal-Id", "steward-3")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, supersedeReq)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSupersedeClassification_AlreadySuperseded_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	original := confirmFirstClassification(t, r, s)

	firstReclassify := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "RESTRICTED", "HUMAN", nil)))
	firstReclassifyRec := httptest.NewRecorder()
	r.ServeHTTP(firstReclassifyRec, firstReclassify)
	var firstProposed domain.RecordClassification
	require.NoError(t, json.Unmarshal(firstReclassifyRec.Body.Bytes(), &firstProposed))

	firstSupersedeBody, err := json.Marshal(map[string]string{"new_classification_id": firstProposed.ClassificationID})
	require.NoError(t, err)
	firstSupersedeReq := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+original.ClassificationID+"/supersede", bytes.NewReader(firstSupersedeBody))
	firstSupersedeReq.Header.Set("X-Principal-Id", "steward-2")
	firstSupersedeRec := httptest.NewRecorder()
	r.ServeHTTP(firstSupersedeRec, firstSupersedeReq)
	require.Equal(t, http.StatusOK, firstSupersedeRec.Code)

	// Reclassify again so there's a fresh CANDIDATE to attempt a second
	// supersede of the now-already-superseded original with.
	secondReclassify := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "PUBLIC", "HUMAN", nil)))
	secondReclassifyRec := httptest.NewRecorder()
	r.ServeHTTP(secondReclassifyRec, secondReclassify)
	var secondProposed domain.RecordClassification
	require.NoError(t, json.Unmarshal(secondReclassifyRec.Body.Bytes(), &secondProposed))

	secondSupersedeBody, err := json.Marshal(map[string]string{"new_classification_id": secondProposed.ClassificationID})
	require.NoError(t, err)
	secondSupersedeReq := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+original.ClassificationID+"/supersede", bytes.NewReader(secondSupersedeBody))
	secondSupersedeReq.Header.Set("X-Principal-Id", "steward-4")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, secondSupersedeReq)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestClassifyRecord_AlreadyConfirmed_Returns409(t *testing.T) {
	st := newTestStorage(t)
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, st)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	proposal := s.classifications[0]
	confirmReq := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+proposal.ClassificationID+"/confirm", nil)
	confirmReq.Header.Set("X-Principal-Id", "steward-1")
	confirmRec := httptest.NewRecorder()
	r.ServeHTTP(confirmRec, confirmReq)
	require.Equal(t, http.StatusOK, confirmRec.Code)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/classify", bytes.NewReader(classifyBody(t, "PUBLIC", "HUMAN", nil))))
	assert.Equal(t, http.StatusConflict, rec.Code)
}
