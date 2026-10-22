package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// ZS-SVC-S-001 routes added as exact-contract paths over existing behaviour:
//   GET  /v1/document-versions/{version_id}   §3.6
//   POST /v1/records:declare                  §9.2
//   POST /v1/records/{id}:supersede           §9.2
//   POST /v1/legal-holds/{id}:release         §9.2
// Each test also pins that the pre-existing equivalent route still behaves.

// ── stubs ────────────────────────────────────────────────────────────────────

// stubVersions serves FindVersionByID from an explicit map keyed by tenant, so a
// version of another tenant is exactly "not found", as the real store reports.
type stubVersions struct {
	byTenant map[string]map[string]domain.DocumentVersion // tenant -> version id -> version
	err      error
}

func (s *stubVersions) FindVersionByID(ctx context.Context, versionID string) (*domain.DocumentVersion, error) {
	if s.err != nil {
		return nil, s.err
	}
	tenant := svcmiddleware.TenantFromContext(ctx)
	if v, ok := s.byTenant[tenant][versionID]; ok {
		return &v, nil
	}
	return nil, domain.ErrDocumentVersionNotFound
}

type relationStub struct {
	handler.RecordsStore
	records map[string]*domain.Record
	created []domain.CreateRecordRelationshipParams
	relErr  error
}

func (s *relationStub) GetRecord(_ context.Context, id string) (*domain.Record, error) {
	if r, ok := s.records[id]; ok {
		return r, nil
	}
	return nil, domain.ErrRecordNotFound
}

func (s *relationStub) CreateRecordRelationship(_ context.Context, p domain.CreateRecordRelationshipParams) (*domain.RecordRelationship, error) {
	if s.relErr != nil {
		return nil, s.relErr
	}
	s.created = append(s.created, p)
	return &domain.RecordRelationship{
		RelationshipID: "rel-1", SourceRecordID: p.SourceRecordID, TargetRecordID: p.TargetRecordID,
		RelationshipType: domain.RelationshipType(p.RelationshipType),
	}, nil
}

type releaseStub struct {
	handler.RetentionStore
	released []domain.ReleaseLegalHoldParams
	err      error
}

func (s *releaseStub) ReleaseLegalHold(_ context.Context, p domain.ReleaseLegalHoldParams) (*domain.LegalHold, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.released = append(s.released, p)
	return &domain.LegalHold{HoldID: p.HoldID, Status: "RELEASED"}, nil
}

// drcRouter mounts the real handlers over stubs. withIdentity=false mounts no
// identity injector, so a request without headers is genuinely anonymous.
func drcRouter(t *testing.T, s *stubStore, az authz.Client, vs handler.VersionStore, rec handler.RecordsStore, ret handler.RetentionStore, withIdentity bool) chi.Router {
	r := chi.NewRouter()
	if withIdentity {
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
	h := handler.New(s, newCounting(t), &stubResidency{}, az, newStubScanner(), zap.NewNop())
	handler.RegisterRoutes(r, h)
	if vs != nil {
		handler.RegisterVersionRoutes(r, h, vs)
	}
	if rec != nil {
		handler.RegisterRecordsRoutes(r, h, rec)
	}
	if ret != nil {
		handler.RegisterRetentionRoutes(r, h, ret)
	}
	return r
}

func call(r http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return doReq(r, req)
}

// ── GET /v1/document-versions/{version_id} ───────────────────────────────────

func versionFixture() (*stubStore, *stubVersions) {
	region := "eu-west-1"
	s := newStubStore()
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-OWNER", CurrentVersion: 2, Status: domain.StatusActive, ResidencyRegionCode: &region}
	t1 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	// v1 is a "legacy" row: no detected type was persisted for it.
	v1 := domain.DocumentVersion{DocumentVersionID: "ver-1", DocumentID: "doc-1", Version: 1, ChecksumSHA256: "hash-one", StorageKey: "obj-1", SizeBytes: 10, ContentType: "application/pdf", CreatedAt: t1}
	v2 := domain.DocumentVersion{DocumentVersionID: "ver-2", DocumentID: "doc-1", Version: 2, ChecksumSHA256: "hash-two", StorageKey: "obj-2", SizeBytes: 20, ContentType: "application/pdf", DetectedContentType: "application/pdf", CreatedAt: t2}
	s.versions["doc-1"] = []domain.DocumentVersion{v1, v2}
	vs := &stubVersions{byTenant: map[string]map[string]domain.DocumentVersion{testTenant: {"ver-1": v1, "ver-2": v2}}}
	return s, vs
}

func TestGetDocumentVersion_ReturnsTheExactVersionNotTheLatest(t *testing.T) {
	s, vs := versionFixture()
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)

	w := call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"version_id":"ver-1"`)
	assert.Contains(t, w.Body.String(), `"content_hash":"hash-one"`)
	assert.NotContains(t, w.Body.String(), "hash-two", "the older version must not be substituted by the latest")

	w = call(r, http.MethodGet, "/v1/document-versions/ver-2", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content_hash":"hash-two"`)
	assert.Len(t, s.accessLog, 2, "each read is access-logged against the exact version")
	require.NotNil(t, s.accessLog[0].DocumentVersionID)
	assert.Equal(t, "ver-1", *s.accessLog[0].DocumentVersionID)
}

// §3.3: every field of the immutable version contract that this service can
// supply, under the specification's own names.
func TestGetDocumentVersion_UsesTheSection33FieldNames(t *testing.T) {
	s, vs := versionFixture()
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)

	var v2 map[string]any
	w := call(r, http.MethodGet, "/v1/document-versions/ver-2", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v2))
	assert.Equal(t, "ver-2", v2["version_id"])
	assert.Equal(t, "doc-1", v2["document_id"])
	assert.EqualValues(t, 2, v2["version_number"])
	assert.Equal(t, "obj-2", v2["content_object_id"])
	assert.Equal(t, "hash-two", v2["content_hash"])
	assert.EqualValues(t, 20, v2["byte_length"])
	assert.Equal(t, "application/pdf", v2["declared_mime_type"])
	assert.Equal(t, "application/pdf", v2["detected_mime_type"])
	assert.Equal(t, "CLEAN", v2["malware_status"])
	assert.Equal(t, "eu-west-1", v2["region"], "the document's residency region")
	assert.Equal(t, "ver-1", v2["created_from_version_id"], "lineage to the previous version")
	assert.Equal(t, "COMMITTED", v2["commit_status"])
	assert.Equal(t, "2026-03-02T09:00:00Z", v2["committed_at"])
	assert.NotContains(t, v2, "encryption_key_ref", "no per-object key reference exists, so none is invented")
	assert.NotContains(t, v2, "document_version_id", "the legacy field names are not repeated")
	assert.NotContains(t, v2, "checksum_sha256")

	// Version 1: nothing precedes it, and it predates the persisted content sniff.
	var v1 map[string]any
	w = call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v1))
	assert.Nil(t, v1["created_from_version_id"])
	assert.Nil(t, v1["detected_mime_type"], "a legacy row has no detected type and none is fabricated")
	assert.Contains(t, v1, "detected_mime_type", "the field is present, as null")
}

func TestGetDocumentVersion_NoRegionIsNull(t *testing.T) {
	s, vs := versionFixture()
	s.docs["doc-1"].ResidencyRegionCode = nil
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)
	var v map[string]any
	require.NoError(t, json.Unmarshal(call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil).Body.Bytes(), &v))
	assert.Nil(t, v["region"])
}

func TestGetDocumentVersion_UnknownAndOtherTenantAreIndistinguishable(t *testing.T) {
	s, vs := versionFixture()
	vs.byTenant["22222222-2222-2222-2222-222222222222"] = map[string]domain.DocumentVersion{
		"ver-foreign": {DocumentVersionID: "ver-foreign", DocumentID: "doc-foreign", ChecksumSHA256: "secret"}}
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)

	unknown := call(r, http.MethodGet, "/v1/document-versions/ver-nope", "", nil)
	foreign := call(r, http.MethodGet, "/v1/document-versions/ver-foreign", "", nil)
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.Equal(t, http.StatusNotFound, foreign.Code)
	assert.Equal(t, unknown.Body.String(), foreign.Body.String(), "another tenant's version reads exactly like a missing one")
	assert.NotContains(t, foreign.Body.String(), "secret")
}

func TestGetDocumentVersion_AuthorizationFailsClosed_OnTheOwningEntity(t *testing.T) {
	s, vs := versionFixture()

	az := &scopeAuthz{}
	r := drcRouter(t, s, az, vs, nil, nil, true)
	require.Equal(t, http.StatusOK, call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil).Code)
	assert.Equal(t, []string{"entity-OWNER"}, az.scopes, "decided on the document's own legal entity")

	denied := drcRouter(t, s, &scopeAuthz{denyAll: true}, vs, nil, nil, true)
	w := call(denied, http.MethodGet, "/v1/document-versions/ver-1", "", nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "hash-one")

	down := drcRouter(t, s, &stubAuthz{err: domain.ErrAuthzServiceUnavailable}, vs, nil, nil, true)
	w = call(down, http.MethodGet, "/v1/document-versions/ver-1", "", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "an authorization outage is never a permit")
	assert.NotContains(t, w.Body.String(), "hash-one")
}

func TestGetDocumentVersion_RequiresIdentity(t *testing.T) {
	s, vs := versionFixture()
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, false)

	assert.Equal(t, http.StatusUnauthorized, call(r, http.MethodGet, "/v1/document-versions/ver-1", "", map[string]string{"X-Principal-Id": "p"}).Code, "no tenant")
	assert.Equal(t, http.StatusUnauthorized, call(r, http.MethodGet, "/v1/document-versions/ver-1", "", map[string]string{"X-Tenant-Id": testTenant}).Code, "no principal")
	assert.Equal(t, http.StatusUnauthorized, call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil).Code, "neither")
	assert.Empty(t, s.accessLog)
}

func TestGetDocumentVersion_StoreFailureIsNotASuccess(t *testing.T) {
	s, vs := versionFixture()
	vs.err = context.DeadlineExceeded
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)
	assert.GreaterOrEqual(t, call(r, http.MethodGet, "/v1/document-versions/ver-1", "", nil).Code, 500)
}

func TestVersionsListRoute_IsUnchanged(t *testing.T) {
	s, vs := versionFixture()
	r := drcRouter(t, s, &stubAuthz{}, vs, nil, nil, true)
	w := call(r, http.MethodGet, "/v1/documents/doc-1/versions", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "hash-one")
	assert.Contains(t, w.Body.String(), "hash-two")
}

// ── POST /v1/records:declare ─────────────────────────────────────────────────

func TestRecordsDeclare_ColonRouteIsTheSameDeclarationPathAsTheOldRoute(t *testing.T) {
	az := &scopeAuthz{}
	r, rec := recordsRouter(t, az, &stubResidency{}, nil)

	viaNew := call(r, http.MethodPost, "/v1/records:declare", declareBody(""), nil)
	viaOld := call(r, http.MethodPost, "/v1/records", declareBody(""), nil)
	require.Equal(t, http.StatusCreated, viaNew.Code, viaNew.Body.String())
	require.Equal(t, http.StatusCreated, viaOld.Code, viaOld.Body.String())
	assert.Equal(t, viaOld.Body.String(), viaNew.Body.String(), "one handler, one response")
	require.Len(t, rec.declared, 2)
	assert.Equal(t, rec.declared[0], rec.declared[1], "identical parameters reach the store")
	assert.Equal(t, []string{"entity-OWNER", "entity-OWNER"}, az.scopes)
}

func TestRecordsDeclare_ColonRoute_KeepsEveryGate(t *testing.T) {
	// unauthorized
	r, rec := recordsRouter(t, &scopeAuthz{denyAll: true}, &stubResidency{}, nil)
	assert.Equal(t, http.StatusForbidden, call(r, http.MethodPost, "/v1/records:declare", declareBody(""), nil).Code)
	assert.Empty(t, rec.declared, "a denied caller declares nothing")

	// authorization outage
	r, rec = recordsRouter(t, &stubAuthz{err: domain.ErrAuthzServiceUnavailable}, &stubResidency{}, nil)
	assert.Equal(t, http.StatusServiceUnavailable, call(r, http.MethodPost, "/v1/records:declare", declareBody(""), nil).Code)
	assert.Empty(t, rec.declared)

	// exact version required: no "latest" substitution
	r, rec = recordsRouter(t, &scopeAuthz{}, &stubResidency{}, nil)
	w := call(r, http.MethodPost, "/v1/records:declare", `{"document_id":"doc-1","record_class":"LEGAL_CONTRACT","jurisdiction_scope":"US-DE"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "missing_field", code)
	assert.Equal(t, "document_version_id", detail)
	assert.Empty(t, rec.declared)

	// wrong tenant: the document is not visible to this tenant
	r, rec = recordsRouter(t, &scopeAuthz{}, &stubResidency{}, nil)
	w = call(r, http.MethodPost, "/v1/records:declare", `{"document_id":"doc-of-another-tenant","document_version_id":"v","record_class":"LEGAL_CONTRACT","jurisdiction_scope":"US-DE"}`, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, rec.declared)
}

// ── POST /v1/records/{id}:supersede ──────────────────────────────────────────

func supersedeFixture() (*stubStore, *relationStub) {
	rec := &relationStub{records: map[string]*domain.Record{
		"rec-new":   {RecordID: "rec-new", LegalEntityID: "entity-NEW"},
		"rec-prior": {RecordID: "rec-prior", LegalEntityID: "entity-PRIOR"},
	}}
	return newStubStore(), rec
}

func TestRecordsSupersede_CreatesTheSupersessionRelationship(t *testing.T) {
	s, rec := supersedeFixture()
	az := &scopeAuthz{}
	r := drcRouter(t, s, az, nil, rec, nil, true)

	w := call(r, http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new"}`, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.Len(t, rec.created, 1)
	got := rec.created[0]
	assert.Equal(t, "rec-new", got.SourceRecordID, "the new record is the acting record")
	assert.Equal(t, "rec-prior", got.TargetRecordID, "the record in the URL is the one superseded")
	assert.Equal(t, "SUPERSEDES", got.RelationshipType)
	assert.Equal(t, testPrincipal, got.CreatedByPrincipalID)
	assert.Equal(t, []string{"entity-NEW"}, az.scopes, "RECORD_RELATE is decided on the acting record's entity, as for /relationships")

	// Equivalence with the existing route: same store call for the same pair.
	s2, rec2 := supersedeFixture()
	r2 := drcRouter(t, s2, &scopeAuthz{}, nil, rec2, nil, true)
	old := call(r2, http.MethodPost, "/v1/records/rec-new/relationships", `{"target_record_id":"rec-prior","relationship_type":"SUPERSEDES"}`, nil)
	require.Equal(t, http.StatusCreated, old.Code)
	assert.Equal(t, rec2.created[0], rec.created[0], "both routes call the same store method with the same parameters")
}

func TestRecordsSupersede_RefusalsAndErrors(t *testing.T) {
	// missing new_record_id
	s, rec := supersedeFixture()
	r := drcRouter(t, s, &scopeAuthz{}, nil, rec, nil, true)
	w := call(r, http.MethodPost, "/v1/records/rec-prior:supersede", `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, rec.created)

	// unknown fields are refused, not ignored
	w = call(r, http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new","relationship_type":"CORRECTS"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, "a caller cannot smuggle another relationship type through this route")
	assert.Empty(t, rec.created)

	// the new record must already exist
	w = call(r, http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, rec.created)

	// unauthorized / outage fail closed
	s, rec = supersedeFixture()
	assert.Equal(t, http.StatusForbidden, call(drcRouter(t, s, &scopeAuthz{denyAll: true}, nil, rec, nil, true),
		http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new"}`, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, call(drcRouter(t, s, &stubAuthz{err: domain.ErrAuthzServiceUnavailable}, nil, rec, nil, true),
		http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new"}`, nil).Code)
	assert.Empty(t, rec.created)

	// already superseded -> 409, self-reference -> 400 (the store's own rules)
	s, rec = supersedeFixture()
	rec.relErr = domain.ErrRecordAlreadySuperseded
	assert.Equal(t, http.StatusConflict, call(drcRouter(t, s, &scopeAuthz{}, nil, rec, nil, true),
		http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new"}`, nil).Code)
	rec.relErr = domain.ErrRecordRelationshipSelfReference
	assert.Equal(t, http.StatusBadRequest, call(drcRouter(t, s, &scopeAuthz{}, nil, rec, nil, true),
		http.MethodPost, "/v1/records/rec-new:supersede", `{"new_record_id":"rec-new"}`, nil).Code)

	// identity required
	s, rec = supersedeFixture()
	anon := drcRouter(t, s, &scopeAuthz{}, nil, rec, nil, false)
	assert.Equal(t, http.StatusUnauthorized, call(anon, http.MethodPost, "/v1/records/rec-prior:supersede", `{"new_record_id":"rec-new"}`, nil).Code)
	assert.Empty(t, rec.created)
}

func TestRecordsGetRoute_IsNotShadowedByTheSupersedeRoute(t *testing.T) {
	s, rec := supersedeFixture()
	r := drcRouter(t, s, &scopeAuthz{}, nil, rec, nil, true)
	w := call(r, http.MethodGet, "/v1/records/rec-prior", "", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "rec-prior")
}

// ── POST /v1/legal-holds/{id}:release ────────────────────────────────────────

const releaseBody = `{"release_reason":"matter closed","release_approved_by_principal_id":"approver-9"}`

func TestLegalHoldRelease_ColonRouteIsTheSameReleasePathAsTheOldRoute(t *testing.T) {
	ret := &releaseStub{}
	r := drcRouter(t, newStubStore(), &stubAuthz{}, nil, nil, ret, true)

	viaNew := call(r, http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil)
	viaOld := call(r, http.MethodPost, "/v1/legal-holds/hold-7/release", releaseBody, nil)
	require.Equal(t, http.StatusOK, viaNew.Code, viaNew.Body.String())
	require.Equal(t, http.StatusOK, viaOld.Code)
	assert.Equal(t, viaOld.Body.String(), viaNew.Body.String())
	require.Len(t, ret.released, 2)
	assert.Equal(t, "hold-7", ret.released[0].HoldID, "the id before ':release' is the hold id")
	assert.Equal(t, ret.released[0], ret.released[1], "identical parameters reach the store")
	assert.Equal(t, "approver-9", ret.released[0].ReleaseApprovedByPrincipalID)
}

func TestLegalHoldRelease_ColonRoute_KeepsTheTwoPersonRuleAndEveryGate(t *testing.T) {
	// self-release forbidden
	ret := &releaseStub{}
	r := drcRouter(t, newStubStore(), &stubAuthz{}, nil, nil, ret, true)
	w := call(r, http.MethodPost, "/v1/legal-holds/hold-7:release",
		`{"release_reason":"x","release_approved_by_principal_id":"`+testPrincipal+`"}`, nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	code, _ := errorCode(t, w)
	assert.Equal(t, "self_release_forbidden", code)
	assert.Empty(t, ret.released)

	// missing approval reference / reason
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/legal-holds/hold-7:release", `{"release_reason":"x"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/legal-holds/hold-7:release", `{"release_approved_by_principal_id":"approver-9"}`, nil).Code)
	assert.Empty(t, ret.released)

	// the caller is not authorized
	ret = &releaseStub{}
	denied := &stubAuthz{denied: map[string]bool{testPrincipal + "|LEGAL_HOLD_MANAGE": true}}
	assert.Equal(t, http.StatusForbidden, call(drcRouter(t, newStubStore(), denied, nil, nil, ret, true),
		http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil).Code)
	assert.Empty(t, ret.released)

	// the named approver is independently authorized
	ret = &releaseStub{}
	approverDenied := &stubAuthz{denied: map[string]bool{"approver-9|LEGAL_HOLD_MANAGE": true}}
	assert.Equal(t, http.StatusForbidden, call(drcRouter(t, newStubStore(), approverDenied, nil, nil, ret, true),
		http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil).Code)
	assert.Empty(t, ret.released)

	// authorization outage
	assert.Equal(t, http.StatusServiceUnavailable, call(drcRouter(t, newStubStore(), &stubAuthz{err: domain.ErrAuthzServiceUnavailable}, nil, nil, ret, true),
		http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil).Code)
	assert.Empty(t, ret.released)

	// identity required
	assert.Equal(t, http.StatusUnauthorized, call(drcRouter(t, newStubStore(), &stubAuthz{}, nil, nil, ret, false),
		http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil).Code)
	assert.Empty(t, ret.released)
}

func TestLegalHoldRelease_ColonRoute_StateErrorsKeepTheirStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"hold not found":     {domain.ErrLegalHoldNotFound, http.StatusNotFound},
		"hold not active":    {domain.ErrLegalHoldNotActive, http.StatusConflict},
		"self release store": {domain.ErrLegalHoldSelfRelease, http.StatusForbidden},
	} {
		ret := &releaseStub{err: tc.err}
		w := call(drcRouter(t, newStubStore(), &stubAuthz{}, nil, nil, ret, true),
			http.MethodPost, "/v1/legal-holds/hold-7:release", releaseBody, nil)
		assert.Equal(t, tc.want, w.Code, name)
	}
}
