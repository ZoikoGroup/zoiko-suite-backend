package handler_test

// Regression tests for the service-only remediation of document-vault-svc:
// fail-closed scanning, content-type validation (DRC-I19), idempotent upload
// commands, authorization-before-validation, download headers, the DRC codes
// the specification defines, the legal-hold disposition block, and the records
// handler's authorization scope and residency gate.

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

// ── fixtures ────────────────────────────────────────────────────────────────

// countingStorage wraps a real backend and counts writes/deletes, so a test can
// prove that a refused upload stored nothing and a replay wrote nothing.
type countingStorage struct {
	storage.Backend
	puts, deletes int
	getErr        error
}

func (c *countingStorage) Put(ctx context.Context, key string, data []byte) (string, error) {
	c.puts++
	return c.Backend.Put(ctx, key, data)
}
func (c *countingStorage) Delete(ctx context.Context, key string) error {
	c.deletes++
	return c.Backend.Delete(ctx, key)
}
func (c *countingStorage) Get(ctx context.Context, key, sum string) ([]byte, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.Backend.Get(ctx, key, sum)
}

func newCounting(t *testing.T) *countingStorage { return &countingStorage{Backend: newTestStorage(t)} }

// fullRouter builds the real handlers over stubs, with the identity injector the
// other tests use. Records and retention routes are mounted on stubs whose
// unimplemented methods are never reached.
func fullRouter(s *stubStore, st storage.Backend, az authz.Client, sc scan.Scanner, res residency.Validator, rec handler.RecordsStore) chi.Router {
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
	h := handler.New(s, st, res, az, sc, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterEvidenceRoutes(r, h, stubEvidence{})
	if rec != nil {
		handler.RegisterRecordsRoutes(r, h, rec)
	}
	handler.RegisterRetentionRoutes(r, h, stubRetention{})
	return r
}

type stubEvidence struct{ handler.EvidenceStore }
type stubRetention struct{ handler.RetentionStore }

var (
	pdfBytes = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n")
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
)

func newCreateReq(t *testing.T, contentType string, content []byte, key string) *http.Request {
	t.Helper()
	body, err := json.Marshal(domain.CreateDocumentRequest{
		TenantID: testTenant, LegalEntityID: "entity-1", Title: "Contract",
		Classification: domain.ClassificationConfidential, ContentType: contentType,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

func doReq(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) (code, detail string) {
	t.Helper()
	var e struct{ Error, Detail string }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &e), rec.Body.String())
	return e.Error, e.Detail
}

// ── GAP A: the malware scanner fails closed ─────────────────────────────────

func TestUnconfiguredScanner_RefusesEveryUpload_AndStoresNothing(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, scan.UnconfiguredScanner{}, &stubResidency{}, nil)

	rec := doReq(r, newCreateReq(t, "text/plain", []byte("harmless text"), ""))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "an absent scanner is not a clean verdict")
	code, _ := errorCode(t, rec)
	assert.Equal(t, "scan_unavailable", code)
	assert.Zero(t, st.puts, "nothing was written to storage")
	assert.Zero(t, s.createCalls, "no document row was created")

	// A new version is refused the same way.
	good := newStubStore()
	good.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive}
	r2 := fullRouter(good, st, &stubAuthz{}, scan.UnconfiguredScanner{}, &stubResidency{}, nil)
	vb, _ := json.Marshal(domain.CreateDocumentVersionRequest{ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("v2"))})
	rec = doReq(r2, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/versions", bytes.NewReader(vb)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Zero(t, st.puts)
	assert.Zero(t, good.addVersionCalls)

	assert.ErrorIs(t, errUnconfigured(), scan.ErrNotConfigured)
}

func errUnconfigured() error {
	_, err := scan.UnconfiguredScanner{}.Scan(context.Background(), []byte("x"), "text/plain")
	return err
}

func TestConfiguredScanner_AcceptsCleanContent_AndIsGivenTheDetectedType(t *testing.T) {
	s, st, sc := newStubStore(), newCounting(t), newStubScanner()
	r := fullRouter(s, st, &stubAuthz{}, sc, &stubResidency{}, nil)

	rec := doReq(r, newCreateReq(t, "application/pdf", pdfBytes, ""))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, 1, sc.calls)
	assert.Equal(t, "application/pdf", sc.gotType, "the scanner receives the type detected from the bytes")
	assert.Equal(t, 1, st.puts)

	rec = doReq(r, newCreateReq(t, "application/pdf; version=1.7", pdfBytes, ""))
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/pdf", sc.gotType, "parameters on the declared header are not what is scanned")
}

func TestScannerFailure_FailsClosed(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	sc := newStubScanner()
	sc.err = errors.New("clamd unreachable")
	r := fullRouter(s, st, &stubAuthz{}, sc, &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("x"), ""))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Zero(t, st.puts)
	assert.Zero(t, s.createCalls)
}

func TestInfectedUpload_IsQuarantined_AndNotStored(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	sc := newStubScanner()
	sc.clean, sc.reason = false, "Eicar-Test-Signature"
	r := fullRouter(s, st, &stubAuthz{}, sc, &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("x"), ""))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	code, _ := errorCode(t, rec)
	assert.Equal(t, "upload_quarantined", code)
	assert.Zero(t, st.puts)
}

// ── DRC-I19: declared MIME is validated against the bytes ───────────────────

func TestCreateDocument_ContentTypeMismatch_Is422_DRC002_BeforeScanOrStorage(t *testing.T) {
	s, st, sc := newStubStore(), newCounting(t), newStubScanner()
	r := fullRouter(s, st, &stubAuthz{}, sc, &stubResidency{}, nil)

	for name, tc := range map[string]struct {
		declared string
		content  []byte
	}{
		"png declared as pdf":     {"application/pdf", pngBytes},
		"text declared as pdf":    {"application/pdf", []byte("not a pdf at all")},
		"pdf declared as text":    {"text/plain", pdfBytes},
		"text declared as docx":   {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("plain")},
		"opaque declared as zip":  {"application/zip", []byte{0x00, 0x01, 0x02, 0xfe, 0xff}},
		"png declared as generic": {"image/jpeg", pngBytes},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doReq(r, newCreateReq(t, tc.declared, tc.content, ""))
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			code, detail := errorCode(t, rec)
			assert.Equal(t, "DRC-002", code)
			assert.Contains(t, detail, "UPLOAD_VALIDATION_FAILED")
		})
	}
	assert.Zero(t, sc.calls, "the scanner is not reached for content the declared type contradicts")
	assert.Zero(t, st.puts)
	assert.Zero(t, s.createCalls)
}

func TestCreateDocument_InvalidDeclaredContentType_Is400(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "not a media type", []byte("x"), ""))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorCode(t, rec)
	assert.Equal(t, "invalid_content_type", code)
	assert.Zero(t, st.puts)
}

func TestAddVersion_ContentTypeMismatch_Is422_AndValidTypePasses(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive}
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	post := func(ct string, content []byte) *httptest.ResponseRecorder {
		b, _ := json.Marshal(domain.CreateDocumentVersionRequest{ContentType: ct, ContentBase64: base64.StdEncoding.EncodeToString(content)})
		return doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/versions", bytes.NewReader(b)))
	}
	rec := post("application/pdf", pngBytes)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	code, _ := errorCode(t, rec)
	assert.Equal(t, "DRC-002", code)
	assert.Zero(t, st.puts)
	assert.Zero(t, s.addVersionCalls)

	require.Equal(t, http.StatusCreated, post("application/pdf", pdfBytes).Code)
	assert.Equal(t, 1, s.addVersionCalls)
}

// ── idempotent upload commands (ZS-SVC-S-001 §9.1, DRC-I30) ─────────────────

func TestCreateDocument_SameKeySameRequest_ReplaysTheOriginal(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)

	first := doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1"))
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get("Idempotency-Replay"))

	again := doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1"))
	require.Equal(t, http.StatusCreated, again.Code)
	assert.Equal(t, "true", again.Header().Get("Idempotency-Replay"))
	assert.JSONEq(t, first.Body.String(), again.Body.String(), "the original document is returned")
	assert.Equal(t, 1, s.createCalls, "one logical operation")
	assert.Equal(t, 1, st.puts, "the replay wrote no blob")
}

func TestCreateDocument_SameKeyDifferentRequest_Is409_NothingCreated(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	require.Equal(t, http.StatusCreated, doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1")).Code)

	rec := doReq(r, newCreateReq(t, "text/plain", []byte("something else"), "key-1"))
	require.Equal(t, http.StatusConflict, rec.Code)
	code, _ := errorCode(t, rec)
	assert.Equal(t, "idempotency_mismatch", code)
	assert.Equal(t, 1, s.createCalls)
	assert.Equal(t, 1, st.puts)
}

func TestCreateDocument_DifferentKeys_AreIndependent_AndNoKeyIsUnkeyed(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	require.Equal(t, http.StatusCreated, doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-a")).Code)
	require.Equal(t, http.StatusCreated, doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-b")).Code)
	require.Equal(t, http.StatusCreated, doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "")).Code)
	assert.Equal(t, 3, s.createCalls)
}

func TestCreateDocument_IdempotencyLookupFailure_FailsClosed(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	s.idemLookupErr = errors.New("db down")
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1"))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "never proceed without replay protection")
	assert.Zero(t, s.createCalls)
	assert.Zero(t, st.puts)
}

func TestCreateDocument_ReplayIsNeverAWayAroundAuthorization(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	az := &stubAuthz{}
	r := fullRouter(s, st, az, newStubScanner(), &stubResidency{}, nil)
	require.Equal(t, http.StatusCreated, doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1")).Code)

	az.denied = map[string]bool{testPrincipal + "|" + authz.ActionDocumentCreate: true} // grant revoked
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "a revoked grant must not replay a cached success")
}

func TestAddVersion_SameKeyReplays_OneVersion(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive}
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	send := func(content, key string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(domain.CreateDocumentVersionRequest{ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte(content))})
		req := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/versions", bytes.NewReader(b))
		req.Header.Set("Idempotency-Key", key)
		return doReq(r, req)
	}
	first := send("v2", "vk-1")
	require.Equal(t, http.StatusCreated, first.Code)
	again := send("v2", "vk-1")
	require.Equal(t, http.StatusCreated, again.Code)
	assert.Equal(t, "true", again.Header().Get("Idempotency-Replay"))
	assert.JSONEq(t, first.Body.String(), again.Body.String())
	assert.Equal(t, 1, s.addVersionCalls)
	assert.Equal(t, 1, st.puts)

	assert.Equal(t, http.StatusConflict, send("different", "vk-1").Code)
	assert.Equal(t, 1, s.addVersionCalls)
}

// A failed store write removes the blob it had already written.
func TestCreateDocument_StoreFailure_DeletesTheOrphanBlob(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	s.createErr = errors.New("db down")
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("hello"), "key-1"))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, 1, st.puts)
	assert.Equal(t, 1, st.deletes, "the orphan blob was compensated")
}

// ── authorization precedes validation ───────────────────────────────────────

func TestCreateDocument_UnauthorizedCallerGets403_NotAValidationError(t *testing.T) {
	deny := &stubAuthz{denied: map[string]bool{testPrincipal + "|" + authz.ActionDocumentCreate: true}}
	r := fullRouter(newStubStore(), newCounting(t), deny, newStubScanner(), &stubResidency{}, nil)

	// legal_entity_id present (the authorization scope) but the document is
	// otherwise invalid: no title, bad classification, undecodable content.
	bad := `{"tenant_id":"` + testTenant + `","legal_entity_id":"entity-1","classification":"TOP_SECRET","content_type":"???","content_base64":"%%%"}`
	rec := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewBufferString(bad)))
	assert.Equal(t, http.StatusForbidden, rec.Code, "an unauthorized caller learns nothing about what is valid")

	// An authorized caller sending the same body gets the validation error.
	ok := fullRouter(newStubStore(), newCounting(t), &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec = doReq(ok, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewBufferString(bad)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegisterEvidence_UnauthorizedCallerGets403_BeforeFieldValidation(t *testing.T) {
	body := `{"legal_entity_id":"entity-1"}` // evidence_source / acquisition_method / correlation_id all missing
	deny := &stubAuthz{denied: map[string]bool{testPrincipal + "|AUDIT_EVIDENCE_REGISTER": true}}
	r := fullRouter(newStubStore(), newCounting(t), deny, newStubScanner(), &stubResidency{}, nil)
	rec := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/audit-evidence", bytes.NewBufferString(body)))
	assert.Equal(t, http.StatusForbidden, rec.Code)

	ok := fullRouter(newStubStore(), newCounting(t), &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec = doReq(ok, httptest.NewRequest(http.MethodPost, "/v1/audit-evidence", bytes.NewBufferString(body)))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "authorized: the missing field is reported")
}

func TestRetentionCommand_UnauthorizedCallerGets403_BeforeBodyValidation(t *testing.T) {
	deny := &stubAuthz{denied: map[string]bool{testPrincipal + "|RETENTION_STATE_MANAGE": true}}
	r := fullRouter(newStubStore(), newCounting(t), deny, newStubScanner(), &stubResidency{}, nil)
	rec := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/retention-states/abc/evaluate-due", bytes.NewBufferString(`{}`)))
	assert.Equal(t, http.StatusForbidden, rec.Code, "as_of is missing, but the caller is not allowed to ask")

	ok := fullRouter(newStubStore(), newCounting(t), &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	rec = doReq(ok, httptest.NewRequest(http.MethodPost, "/v1/retention-states/abc/evaluate-due", bytes.NewBufferString(`{}`)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ── download controls and the DRC codes the specification defines ───────────

func uploadAndGetID(t *testing.T, r http.Handler) string {
	t.Helper()
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("download me"), ""))
	require.Equal(t, http.StatusCreated, rec.Code)
	var d domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	return d.DocumentID
}

func TestDownload_SetsAttachmentNoSniffNoStoreHeaders(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	id := uploadAndGetID(t, r)

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "download me", rec.Body.String())
	assert.Equal(t, fmt.Sprintf(`attachment; filename="%s-v1"`, id), rec.Header().Get("Content-Disposition"),
		"the filename carries no title or party name")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	assert.NotEmpty(t, rec.Header().Get("X-Checksum-SHA256"))
}

func TestDownload_IntegrityFailure_IsDRC011(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	id := uploadAndGetID(t, r)
	st.getErr = storage.ErrIntegrityFailure

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	require.Equal(t, http.StatusConflict, rec.Code)
	code, detail := errorCode(t, rec)
	assert.Equal(t, "integrity_check_failed", code, "the existing machine code is preserved")
	assert.Contains(t, detail, "DRC-011")
	assert.Empty(t, rec.Header().Get("Content-Disposition"), "no bytes are served")
}

func TestDownload_MissingObject_IsDRC019_NotASilentEmptyBody(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	id := uploadAndGetID(t, r)
	st.getErr = storage.ErrObjectNotFound

	for _, path := range []string{"/content", "/verify-digest"} {
		rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+path, nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		code, detail := errorCode(t, rec)
		assert.Equal(t, "object_missing", code, path)
		assert.Contains(t, detail, "DRC-019", path)
	}
}

// ── DRC-008: a legal hold blocks a disposition request ──────────────────────

func TestRequestDisposition_UnderLegalHold_Is409_DRC008(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	id := uploadAndGetID(t, r)
	s.dispositionErr = fmt.Errorf("%w (hold hold-77)", domain.ErrRecordUnderLegalHold)

	rec := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents/"+id+"/request-disposition", bytes.NewBufferString(`{"reason":"cleanup"}`)))
	require.Equal(t, http.StatusConflict, rec.Code)
	code, detail := errorCode(t, rec)
	assert.Equal(t, "legal_hold_block", code)
	assert.Contains(t, detail, "DRC-008")
	assert.Contains(t, detail, "hold-77", "the block names the hold")
}

// ── DRC-02 record declaration ───────────────────────────────────────────────

type stubRecords struct {
	handler.RecordsStore
	declared []domain.DeclareRecordV2Params
}

func (s *stubRecords) DeclareRecordV2(_ context.Context, p domain.DeclareRecordV2Params) (*domain.Record, bool, error) {
	s.declared = append(s.declared, p)
	return &domain.Record{RecordID: "rec-1", DocumentID: p.DocumentID, LegalEntityID: p.LegalEntityID}, true, nil
}

// scopeAuthz records the legal entity each authorization was asked about.
type scopeAuthz struct {
	denyAll bool
	scopes  []string
}

func (a *scopeAuthz) CheckAllowed(_ context.Context, _, legalEntityID, _ string) error {
	a.scopes = append(a.scopes, legalEntityID)
	if a.denyAll {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func declareBody(legalEntity string) string {
	le := ""
	if legalEntity != "" {
		le = `"legal_entity_id":"` + legalEntity + `",`
	}
	return `{"document_id":"doc-1","document_version_id":"ver-1",` + le + `"record_class":"LEGAL_CONTRACT","jurisdiction_scope":"US-DE","business_context":"msa","declaration_reason":"executed"}`
}

func recordsRouter(t *testing.T, az authz.Client, res residency.Validator, region *string) (chi.Router, *stubRecords) {
	s := newStubStore()
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-OWNER", CurrentVersion: 1, Status: domain.StatusActive, ResidencyRegionCode: region}
	rec := &stubRecords{}
	return fullRouter(s, newCounting(t), az, newStubScanner(), res, rec), rec
}

func TestDeclareRecord_IsAuthorizedAgainstTheDocumentsOwningEntity(t *testing.T) {
	az := &scopeAuthz{}
	r, rec := recordsRouter(t, az, &stubResidency{}, nil)

	// No legal_entity_id in the request: the owning entity is used.
	resp := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody(""))))
	require.Equal(t, http.StatusCreated, resp.Code, resp.Body.String())
	assert.Equal(t, []string{"entity-OWNER"}, az.scopes)
	require.Len(t, rec.declared, 1)
	assert.Equal(t, "entity-OWNER", rec.declared[0].LegalEntityID)

	// A caller-asserted entity that is NOT the owner is refused — it can no
	// longer be used to pick an entity the caller holds a grant on.
	az.scopes = nil
	resp = doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody("entity-MINE"))))
	require.Equal(t, http.StatusBadRequest, resp.Code)
	code, _ := errorCode(t, resp)
	assert.Equal(t, "legal_entity_mismatch", code)
	assert.Equal(t, []string{"entity-OWNER"}, az.scopes, "authorization was decided on the owner, not on the caller's claim")
	assert.Len(t, rec.declared, 1, "nothing was declared")

	// Naming the true owner is accepted.
	resp = doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody("entity-OWNER"))))
	assert.Equal(t, http.StatusCreated, resp.Code)
}

func TestDeclareRecord_UnauthorizedCallerGets403_BeforeFieldValidation(t *testing.T) {
	r, rec := recordsRouter(t, &scopeAuthz{denyAll: true}, &stubResidency{}, nil)
	// record_class, jurisdiction_scope, document_version_id all missing.
	resp := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(`{"document_id":"doc-1"}`)))
	assert.Equal(t, http.StatusForbidden, resp.Code)
	assert.Empty(t, rec.declared)
}

func TestDeclareRecord_ResidencyGate(t *testing.T) {
	eu := "eu"
	// Region disallowed for the tenant: blocked, nothing declared.
	r, rec := recordsRouter(t, &scopeAuthz{}, &stubResidency{err: residency.ErrMismatch}, &eu)
	resp := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody(""))))
	assert.Equal(t, http.StatusConflict, resp.Code)
	assert.Empty(t, rec.declared)

	// Registry unreachable: fail closed.
	r, rec = recordsRouter(t, &scopeAuthz{}, &stubResidency{err: residency.ErrServiceUnavailable}, &eu)
	resp = doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody(""))))
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	assert.Empty(t, rec.declared)

	// Allowed region: declared.
	r, rec = recordsRouter(t, &scopeAuthz{}, &stubResidency{}, &eu)
	resp = doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody(""))))
	assert.Equal(t, http.StatusCreated, resp.Code)
	assert.Len(t, rec.declared, 1)

	// A document with no declared region has nothing to check.
	r, rec = recordsRouter(t, &scopeAuthz{}, &stubResidency{err: residency.ErrMismatch}, nil)
	resp = doReq(r, httptest.NewRequest(http.MethodPost, "/v1/records", bytes.NewBufferString(declareBody(""))))
	assert.Equal(t, http.StatusCreated, resp.Code)
	assert.Len(t, rec.declared, 1)
}
