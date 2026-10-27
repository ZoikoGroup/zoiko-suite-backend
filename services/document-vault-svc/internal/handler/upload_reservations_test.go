package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/scan"
)

// POST /v1/documents/{id}/upload-reservations and
// POST /v1/upload-reservations/{id}/complete (ZS-SVC-S-001 §3.4 / §3.6) over the
// real handlers. The state machine and its transactional guarantees are proved
// against a real database in internal/store/drc_reservations_test.go.

type resStub struct {
	reservations map[string]*domain.UploadReservation
	hashes       map[string]string // principal|key -> request hash
	byKey        map[string]string // principal|key -> reservation id
	versions     map[string]*domain.DocumentVersion
	seq          int
	commits      int
	completeErr  error
	createCalls  int
}

func newResStub() *resStub {
	return &resStub{reservations: map[string]*domain.UploadReservation{}, hashes: map[string]string{},
		byKey: map[string]string{}, versions: map[string]*domain.DocumentVersion{}}
}

func (s *resStub) CreateUploadReservation(_ context.Context, p domain.CreateUploadReservationParams) (*domain.UploadReservation, bool, error) {
	s.createCalls++
	k := p.ReservedByPrincipal + "|" + p.IdempotencyKey
	if id, ok := s.byKey[k]; ok {
		if s.hashes[k] != p.RequestHash {
			return nil, false, domain.ErrIdempotencyMismatch
		}
		return s.reservations[id], true, nil
	}
	s.seq++
	id := fmt.Sprintf("res-%d", s.seq)
	r := &domain.UploadReservation{ReservationID: id, DocumentID: p.DocumentID, ReservedBy: p.ReservedByPrincipal, Purpose: p.Purpose,
		ExpectedSizeBytes: p.ExpectedSizeBytes, DeclaredContentType: p.DeclaredContentType, CommitStatus: domain.ReservationReserved}
	s.reservations[id], s.byKey[k], s.hashes[k] = r, id, p.RequestHash
	return r, false, nil
}

func (s *resStub) GetUploadReservation(_ context.Context, id string) (*domain.UploadReservation, error) {
	if r, ok := s.reservations[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrUploadReservationNotFound
}

func (s *resStub) CompleteUploadReservation(_ context.Context, id string, v *domain.DocumentVersion, _ string) (*domain.UploadReservation, *domain.DocumentVersion, bool, error) {
	if s.completeErr != nil {
		return nil, nil, false, s.completeErr
	}
	r := s.reservations[id]
	if r.CommitStatus == domain.ReservationCommitted {
		return r, s.versions[*r.CommittedVersionID], true, nil
	}
	s.commits++
	v.DocumentVersionID = fmt.Sprintf("ver-new-%d", s.commits)
	v.DocumentID = r.DocumentID
	v.Version = 1 + s.commits
	s.versions[v.DocumentVersionID] = v
	r.CommitStatus = domain.ReservationCommitted
	r.CommittedVersionID = &v.DocumentVersionID
	return r, v, false, nil
}

func (s *resStub) CloseUploadReservation(_ context.Context, id, status, reason string) (*domain.UploadReservation, error) {
	r := s.reservations[id]
	if r.CommitStatus == domain.ReservationReserved {
		r.CommitStatus, r.FailureReason = status, reason
	}
	return r, nil
}

func (s *resStub) FindVersionByID(_ context.Context, id string) (*domain.DocumentVersion, error) {
	if v, ok := s.versions[id]; ok {
		return v, nil
	}
	return nil, domain.ErrDocumentVersionNotFound
}

type resEnv struct {
	r       chi.Router
	store   *stubStore
	res     *resStub
	scanner scan.Scanner
	stor    *countingStorage
	authz   *entityActionAuthz
}

func newResEnv(t *testing.T, sc scan.Scanner, az *entityActionAuthz, identity bool) *resEnv {
	t.Helper()
	e := &resEnv{store: newStubStore(), res: newResStub(), scanner: sc, stor: newCounting(t), authz: az}
	e.store.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-OWNER", CurrentVersion: 1, Status: domain.StatusActive}
	r := chi.NewRouter()
	if identity {
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
	}
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(e.store, e.stor, &stubResidency{}, az, sc, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterUploadReservationRoutes(r, h, e.res)
	e.r = r
	return e
}

func reserveBody(size int, ctype string) string {
	return fmt.Sprintf(`{"purpose":"contract amendment","expected_size_bytes":%d,"declared_content_type":%q}`, size, ctype)
}

func (e *resEnv) reserve(t *testing.T, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	hdr := map[string]string{}
	if key != "" {
		hdr["Idempotency-Key"] = key
	}
	return call(e.r, http.MethodPost, "/v1/documents/doc-1/upload-reservations", body, hdr)
}

func completeBody(content []byte) string {
	return `{"content_base64":"` + base64.StdEncoding.EncodeToString(content) + `"}`
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &m), body)
	return m
}

// ── reserve ──────────────────────────────────────────────────────────────────

func TestReserve_CreatesAReservation_ForAnExistingDocument(t *testing.T) {
	az := &entityActionAuthz{}
	e := newResEnv(t, newStubScanner(), az, true)
	w := call(e.r, http.MethodPost, "/v1/documents/doc-1/upload-reservations", reserveBody(len(pdfBytes), "application/pdf"), map[string]string{"Idempotency-Key": "k1"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	m := decode(t, w.Body.String())
	assert.Equal(t, "RESERVED", m["commit_status"])
	assert.Equal(t, "doc-1", m["document_id"])
	assert.Equal(t, "contract amendment", m["purpose"])
	assert.EqualValues(t, len(pdfBytes), m["expected_size_bytes"])
	up := m["upload"].(map[string]any)
	assert.Equal(t, "POST", up["method"])
	assert.Equal(t, "/v1/upload-reservations/"+m["reservation_id"].(string)+"/complete", up["path"])
	assert.Equal(t, "content_base64", up["content_field"])
	assert.Equal(t, []string{"entity-OWNER|DOCUMENT_VERSION_CREATE"}, az.asked, "the grant the direct version route needs, on the document's entity")
	assert.Zero(t, e.stor.puts, "reserving stores no bytes")
}

func TestReserve_ValidatesTheRequest(t *testing.T) {
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	post := func(key, body string) int {
		hdr := map[string]string{}
		if key != "" {
			hdr["Idempotency-Key"] = key
		}
		return call(e.r, http.MethodPost, "/v1/documents/doc-1/upload-reservations", body, hdr).Code
	}
	assert.Equal(t, http.StatusBadRequest, post("", reserveBody(10, "application/pdf")), "the Idempotency-Key is required")
	assert.Equal(t, http.StatusBadRequest, post(strings.Repeat("k", 256), reserveBody(10, "application/pdf")))
	assert.Equal(t, http.StatusBadRequest, post("k", `{"expected_size_bytes":10,"declared_content_type":"application/pdf"}`), "purpose")
	assert.Equal(t, http.StatusBadRequest, post("k", `{"purpose":"p","declared_content_type":"application/pdf"}`), "size")
	assert.Equal(t, http.StatusBadRequest, post("k", reserveBody(0, "application/pdf")))
	assert.Equal(t, http.StatusBadRequest, post("k", reserveBody(-5, "application/pdf")))
	assert.Equal(t, http.StatusBadRequest, post("k", reserveBody(1<<40, "application/pdf")), "larger than this service accepts")
	assert.Equal(t, http.StatusBadRequest, post("k", `{"purpose":"p","expected_size_bytes":10}`), "declared type")
	assert.Equal(t, http.StatusBadRequest, post("k", reserveBody(10, "not a media type")))
	assert.Equal(t, http.StatusBadRequest, post("k", `{"purpose":"p","expected_size_bytes":10,"declared_content_type":"application/pdf","bucket":"x"}`), "no client-chosen bucket or region")
	assert.Zero(t, e.res.createCalls, "nothing is reserved for an invalid request")
}

func TestReserve_FailsClosed(t *testing.T) {
	// not authorized / outage / anonymous / no such document: nothing is reserved
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{denyAll: true}, true)
	assert.Equal(t, http.StatusForbidden, e.reserve(t, "k", reserveBody(10, "application/pdf")).Code)
	e = newResEnv(t, newStubScanner(), &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, true)
	assert.Equal(t, http.StatusServiceUnavailable, e.reserve(t, "k", reserveBody(10, "application/pdf")).Code)
	assert.Zero(t, e.res.createCalls)
	e = newResEnv(t, newStubScanner(), &entityActionAuthz{}, false)
	assert.Equal(t, http.StatusUnauthorized, e.reserve(t, "k", reserveBody(10, "application/pdf")).Code)
	assert.Zero(t, e.res.createCalls)

	e = newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	w := call(e.r, http.MethodPost, "/v1/documents/doc-missing/upload-reservations", reserveBody(10, "application/pdf"), map[string]string{"Idempotency-Key": "k"})
	assert.Equal(t, http.StatusNotFound, w.Code, "an unknown or other-tenant document is not found")
	assert.Zero(t, e.res.createCalls)
}

func TestReserve_Idempotency(t *testing.T) {
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	first := e.reserve(t, "same-key", reserveBody(100, "application/pdf"))
	second := e.reserve(t, "same-key", reserveBody(100, "application/pdf"))
	require.Equal(t, http.StatusCreated, first.Code)
	require.Equal(t, http.StatusOK, second.Code, "a replay is not a new creation")
	assert.Equal(t, decode(t, first.Body.String())["reservation_id"], decode(t, second.Body.String())["reservation_id"])
	assert.Len(t, e.res.reservations, 1)

	other := e.reserve(t, "same-key", reserveBody(200, "application/pdf"))
	assert.Equal(t, http.StatusConflict, other.Code)
	code, _ := errorCode(t, other)
	assert.Equal(t, "idempotency_mismatch", code)
	assert.Len(t, e.res.reservations, 1)
}

// ── complete ─────────────────────────────────────────────────────────────────

func reserved(t *testing.T, e *resEnv, size int, ctype string) string {
	t.Helper()
	w := e.reserve(t, fmt.Sprintf("key-%d-%s", size, ctype), reserveBody(size, ctype))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	return decode(t, w.Body.String())["reservation_id"].(string)
}

func completePath(id string) string { return "/v1/upload-reservations/" + id + "/complete" }

func TestComplete_CommitsTheVersion_AfterEveryGate(t *testing.T) {
	sc := newStubScanner()
	e := newResEnv(t, sc, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes), "application/pdf")

	w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	m := decode(t, w.Body.String())
	assert.Equal(t, id, m["reservation_id"])
	assert.Equal(t, "COMMITTED", m["commit_status"])
	v := m["version"].(map[string]any)
	assert.Equal(t, "doc-1", v["document_id"])
	assert.Equal(t, "application/pdf", v["declared_mime_type"])
	assert.Equal(t, "application/pdf", v["detected_mime_type"], "the sniffed type is persisted")
	assert.Equal(t, "CLEAN", v["malware_status"])
	assert.Equal(t, "COMMITTED", v["commit_status"])
	assert.EqualValues(t, len(pdfBytes), v["byte_length"])
	assert.NotEmpty(t, v["content_hash"])

	assert.Equal(t, 1, sc.calls, "the malware scanner ran")
	assert.Equal(t, "application/pdf", sc.gotType, "and was given the DETECTED type")
	assert.Equal(t, 1, e.stor.puts)
	assert.Equal(t, 1, e.res.commits)
	assert.Equal(t, "application/pdf", e.res.versions[v["version_id"].(string)].DetectedContentType)
}

func TestComplete_RetriedReservation_CommitsOnce(t *testing.T) {
	sc := newStubScanner()
	e := newResEnv(t, sc, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes), "application/pdf")
	first := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	require.Equal(t, http.StatusCreated, first.Code)
	versionID := decode(t, first.Body.String())["version"].(map[string]any)["version_id"]

	for i := 0; i < 20; i++ {
		w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
		require.Equal(t, http.StatusOK, w.Code, "retry %d", i)
		assert.Equal(t, "true", w.Header().Get("Idempotency-Replay"))
		assert.Equal(t, versionID, decode(t, w.Body.String())["version"].(map[string]any)["version_id"])
	}
	assert.Equal(t, 1, e.res.commits, "one committed version after 21 completions (§11.2: retry 20 times)")
	assert.Equal(t, 1, e.stor.puts, "a replay does not re-scan or re-store")
	assert.Equal(t, 1, sc.calls)
}

func TestComplete_SizeMismatch_FailsTheReservation_AndStoresNothing(t *testing.T) {
	sc := newStubScanner()
	e := newResEnv(t, sc, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes)+5, "application/pdf")

	w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "DRC-002", code)
	assert.Contains(t, detail, "UPLOAD_VALIDATION_FAILED")
	assert.Equal(t, domain.ReservationFailed, e.res.reservations[id].CommitStatus)
	assert.Zero(t, e.stor.puts)
	assert.Zero(t, sc.calls, "no scan, no storage, no version for a size mismatch")
	assert.Zero(t, e.res.commits)

	// Terminal: completing again is refused, not retried.
	w = call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	code, detail = errorCode(t, w)
	assert.Equal(t, "reservation_closed", code)
	assert.Contains(t, detail, "DRC-002")
}

func TestComplete_ContentTypeMismatch_FailsTheReservation(t *testing.T) {
	sc := newStubScanner()
	e := newResEnv(t, sc, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pngBytes), "application/pdf") // declared PDF, bytes are a PNG

	w := call(e.r, http.MethodPost, completePath(id), completeBody(pngBytes), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "DRC-002", code)
	assert.Contains(t, detail, "UPLOAD_VALIDATION_FAILED")
	assert.Equal(t, domain.ReservationFailed, e.res.reservations[id].CommitStatus)
	assert.Zero(t, sc.calls)
	assert.Zero(t, e.stor.puts)
	assert.Zero(t, e.res.commits)
}

func TestComplete_MalwareVerdict_QuarantinesAndCommitsNothing(t *testing.T) {
	sc := newStubScanner()
	sc.clean, sc.reason = false, "EICAR test signature"
	e := newResEnv(t, sc, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes), "application/pdf")

	w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "upload_quarantined", code)
	assert.Contains(t, detail, "DRC-003")
	assert.Contains(t, detail, "EICAR test signature")
	assert.Equal(t, domain.ReservationQuarantined, e.res.reservations[id].CommitStatus)
	assert.Equal(t, []string{"doc-1"}, e.store.quarantineEvents, "the quarantine is recorded against the document")
	assert.Zero(t, e.stor.puts, "quarantined content is never stored")
	assert.Zero(t, e.res.commits, "ordinary consumers never see it (DRC-I18)")
}

func TestComplete_ScannerUnavailable_FailsClosed_AndStaysRetryable(t *testing.T) {
	e := newResEnv(t, scan.UnconfiguredScanner{}, &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes), "application/pdf")
	w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "an absent scanner is never a clean verdict")
	assert.Equal(t, domain.ReservationReserved, e.res.reservations[id].CommitStatus, "not a terminal failure: retry once the scanner is back")
	assert.Zero(t, e.stor.puts)
	assert.Zero(t, e.res.commits)
}

func TestComplete_AuthorizationAndIdentity_FailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		az       *entityActionAuthz
		identity bool
		want     int
	}{
		"denied":    {&entityActionAuthz{denyAll: true}, true, http.StatusForbidden},
		"outage":    {&entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, true, http.StatusServiceUnavailable},
		"anonymous": {&entityActionAuthz{}, false, http.StatusUnauthorized},
	} {
		// Reserve under a permissive environment, then complete under the restrictive one
		// against the same reservation store.
		permissive := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
		id := reserved(t, permissive, len(pdfBytes), "application/pdf")
		restricted := newResEnv(t, newStubScanner(), tc.az, tc.identity)
		restricted.res = permissive.res
		restricted.store = permissive.store
		r := chi.NewRouter()
		r.Use(svcmiddleware.TenantContext())
		h := handler.New(restricted.store, restricted.stor, &stubResidency{}, tc.az, newStubScanner(), zap.NewNop())
		handler.RegisterUploadReservationRoutes(r, h, permissive.res)
		hdr := map[string]string{}
		if tc.identity {
			hdr["X-Principal-Id"], hdr["X-Tenant-Id"] = testPrincipal, testTenant
		}
		w := call(r, http.MethodPost, completePath(id), completeBody(pdfBytes), hdr)
		assert.Equal(t, tc.want, w.Code, name)
		assert.Zero(t, permissive.res.commits, name)
		assert.Zero(t, restricted.stor.puts, name)
		assert.Equal(t, domain.ReservationReserved, permissive.res.reservations[id].CommitStatus, name)
	}
}

func TestComplete_AuthorizationIsReCheckedOnAReplay(t *testing.T) {
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	id := reserved(t, e, len(pdfBytes), "application/pdf")
	require.Equal(t, http.StatusCreated, call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil).Code)

	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(e.store, e.stor, &stubResidency{}, &entityActionAuthz{denyAll: true}, newStubScanner(), zap.NewNop())
	handler.RegisterUploadReservationRoutes(r, h, e.res)
	w := call(r, http.MethodPost, completePath(id), completeBody(pdfBytes), map[string]string{"X-Principal-Id": "p", "X-Tenant-Id": testTenant})
	assert.Equal(t, http.StatusForbidden, w.Code, "a stored result is not a way round authorization")
	assert.NotContains(t, w.Body.String(), "version_id")
}

func TestComplete_BadRequestsAndStoreFailures_LeaveItRetryable(t *testing.T) {
	// unknown reservation
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	assert.Equal(t, http.StatusNotFound, call(e.r, http.MethodPost, completePath("res-ghost"), completeBody(pdfBytes), nil).Code)

	// malformed base64 / unknown field: a bad request, not a terminal failure
	id := reserved(t, e, len(pdfBytes), "application/pdf")
	assert.Equal(t, http.StatusBadRequest, call(e.r, http.MethodPost, completePath(id), `{"content_base64":"!!!not base64"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(e.r, http.MethodPost, completePath(id), `{"content_base64":"AAAA","bucket":"x"}`, nil).Code)
	assert.Equal(t, domain.ReservationReserved, e.res.reservations[id].CommitStatus)

	// the database refuses the commit: the blob is removed and the reservation stays open
	e.res.completeErr = errors.New("document store unavailable: boom")
	w := call(e.r, http.MethodPost, completePath(id), completeBody(pdfBytes), nil)
	assert.GreaterOrEqual(t, w.Code, 500)
	assert.Equal(t, 1, e.stor.puts)
	assert.Equal(t, 1, e.stor.deletes, "no orphaned blob after a failed commit")
	assert.Equal(t, domain.ReservationReserved, e.res.reservations[id].CommitStatus)
}

func TestExistingUploadRoutes_AreUnchanged(t *testing.T) {
	e := newResEnv(t, newStubScanner(), &entityActionAuthz{}, true)
	req := newCreateReq(t, "application/pdf", pdfBytes, "")
	w := doReq(e.r, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "application/pdf", e.store.versions["doc-1"][0].DetectedContentType, "the direct route persists the detected type too")
	_ = authz.ActionDocumentCreate
}
