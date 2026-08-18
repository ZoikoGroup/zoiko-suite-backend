package handler_test

// Tests for the gaps closed in the 18 Aug pass. Each fails against the previous
// behaviour, which is the only reason to write it down.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/storage"
)

func createReq(t *testing.T, entity string) []byte {
	t.Helper()
	b, err := json.Marshal(domain.CreateDocumentRequest{
		TenantID: testTenant, LegalEntityID: entity, Title: "Board pack",
		Classification: domain.ClassificationRestricted,
		ContentType:    "text/plain",
		ContentBase64:  base64.StdEncoding.EncodeToString([]byte("restricted bytes")),
	})
	require.NoError(t, err)
	return b
}

// seedDoc puts one document in the stub and returns its id.
//
// It takes the storage backend rather than making its own: the bytes are
// written to storage during the create, so a test that later downloads them
// must read through the SAME backend. Minting a second one gave a 503
// storage_unavailable that looked like a service fault and was a harness bug.
func seedDoc(t *testing.T, s *stubStore, entity string, st storage.Backend) string {
	t.Helper()
	r := newRouter(s, &stubResidency{}, st)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createReq(t, entity))))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var doc domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	return doc.DocumentID
}

// ── GAP 1: no authorization on any route ─────────────────────────────────────

// The vault answered anything that could reach the port — including the route
// that returns the bytes of a RESTRICTED document.
func TestEveryRouteIsAuthorized(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)

	cases := []struct {
		name, method, path, action string
		body                       []byte
	}{
		{"create", http.MethodPost, "/v1/documents", authz.ActionDocumentCreate, createReq(t, "entity-1")},
		{"list", http.MethodGet, "/v1/documents?legal_entity_id=entity-1", authz.ActionDocumentRead, nil},
		{"read metadata", http.MethodGet, "/v1/documents/" + id, authz.ActionDocumentRead, nil},
		{"download", http.MethodGet, "/v1/documents/" + id + "/content", authz.ActionDocumentDownload, nil},
		{"list versions", http.MethodGet, "/v1/documents/" + id + "/versions", authz.ActionDocumentRead, nil},
		{"access log", http.MethodGet, "/v1/documents/" + id + "/access-log", authz.ActionDocumentAccessLogRead, nil},
		{"add version", http.MethodPost, "/v1/documents/" + id + "/versions", authz.ActionDocumentVersionCreate,
			[]byte(`{"content_type":"text/plain","content_base64":"aGk="}`)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			az := &stubAuthz{denied: map[string]bool{testPrincipal + "|" + c.action: true}}
			r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
			var body *bytes.Reader
			if c.body != nil {
				body = bytes.NewReader(c.body)
			} else {
				body = bytes.NewReader(nil)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, body))

			assert.Equal(t, http.StatusForbidden, rec.Code, "%s must be refused without %s: %s", c.name, c.action, rec.Body.String())
			assert.True(t, az.called(testPrincipal, c.action), "%s must consult %s", c.name, c.action)
		})
	}
}

// Reading a document's bytes is a different disclosure from knowing it exists.
// The access log has recorded them as different access types since day one;
// authorization now agrees, so a READ grant alone does not yield a download.
func TestDownloadNeedsItsOwnGrant(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)

	az := &stubAuthz{denied: map[string]bool{testPrincipal + "|" + authz.ActionDocumentDownload: true}}
	r := newRouterAuthz(s, &stubResidency{}, st, az)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code, "metadata read should still be allowed")

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "download must need DOCUMENT_DOWNLOAD")
}

// The access log is what an investigator reads. It should not fall out of
// ordinary read access to the document.
func TestAccessLogNeedsItsOwnGrant(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)

	az := &stubAuthz{denied: map[string]bool{testPrincipal + "|" + authz.ActionDocumentAccessLogRead: true}}
	r := newRouterAuthz(s, &stubResidency{}, st, az)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/access-log", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// An unreachable authorization-svc must refuse, not permit.
func TestAuthzUnavailableFailsClosed(t *testing.T) {
	s := newStubStore()
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), &stubAuthz{err: domain.ErrAuthzServiceUnavailable})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createReq(t, "entity-1"))))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── GAP 2: identity was optional, forgeable, and recorded as "unknown" ───────

func TestUnidentifiedCallerIsRefusedNotRecorded(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)
	raw := newRouterRaw(s, &stubResidency{}, st)

	for _, path := range []string{
		"/v1/documents?legal_entity_id=entity-1",
		"/v1/documents/" + id,
		"/v1/documents/" + id + "/content",
		"/v1/documents/" + id + "/versions",
		"/v1/documents/" + id + "/access-log",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", testTenant) // tenant present, identity absent
		rec := httptest.NewRecorder()
		raw.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s without a principal must be 401", path)
	}

	// And nothing was appended to the access log for those attempts — the old
	// behaviour recorded them as principal "unknown".
	for _, e := range s.accessLog {
		assert.NotEqual(t, "unknown", e.AccessedByPrincipalID)
	}
}

// X-Actor-Principal-ID took precedence over the gateway-verified header, so a
// caller could attribute their own download to somebody else. It is ignored.
func TestForgedActorHeaderIsIgnored(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)
	r := newRouter(s, &stubResidency{}, st)

	req := httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil)
	req.Header.Set("X-Principal-Id", "real-caller")
	req.Header.Set("X-Actor-Principal-ID", "someone-else")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var found bool
	for _, e := range s.accessLog {
		if e.AccessType == domain.AccessDownload {
			found = true
			assert.Equal(t, "real-caller", e.AccessedByPrincipalID,
				"the download must be attributed to the verified principal, not the forgeable header")
		}
	}
	assert.True(t, found, "a download must append an access-log entry")
}

// ── GAP 3: a missing tenant widened the query instead of refusing ────────────

func TestMissingTenantIsRefused(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)
	raw := newRouterRaw(s, &stubResidency{}, st)

	req := httptest.NewRequest(http.MethodGet, "/v1/documents/"+id, nil)
	req.Header.Set("X-Principal-Id", testPrincipal) // identity present, tenant absent
	rec := httptest.NewRecorder()
	raw.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant_missing")
}

// The body no longer decides which tenant a document lands in.
func TestBodyTenantMismatchIsRefused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, newTestStorage(t))

	body, err := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "99999999-9999-9999-9999-999999999999", LegalEntityID: "entity-1", Title: "x",
		Classification: domain.ClassificationInternal, ContentType: "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("d")),
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(body)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant_mismatch")
}

// ── GAP 4: there was no register at all ──────────────────────────────────────

func TestListDocumentsReturnsTheRegister(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	seedDoc(t, s, "entity-1", st)
	seedDoc(t, s, "entity-1", st)
	seedDoc(t, s, "entity-2", st)
	r := newRouter(s, &stubResidency{}, st)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents?legal_entity_id=entity-1", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var docs []domain.Document
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &docs))
	assert.Len(t, docs, 2, "the register must be scoped to the requested legal entity")
}

// legal_entity_id is required: this service authorizes per entity, so a
// register spanning all of them would have no scope to authorize against.
func TestListDocumentsRequiresLegalEntity(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "legal_entity_id")
}

func TestPagingValidation(t *testing.T) {
	s := newStubStore()
	st := newTestStorage(t)
	id := seedDoc(t, s, "entity-1", st)
	r := newRouter(s, &stubResidency{}, st)

	for _, path := range []string{
		"/v1/documents?legal_entity_id=entity-1&limit=abc",
		"/v1/documents?legal_entity_id=entity-1&limit=0",
		"/v1/documents?legal_entity_id=entity-1&limit=501",
		"/v1/documents?legal_entity_id=entity-1&offset=-1",
		"/v1/documents/" + id + "/access-log?limit=0",
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code, path)
	}
}

// ── GAP 5: an unknown field was discarded in silence ─────────────────────────

func TestUnknownFieldIsRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	// "classifcation" — a typo in the field that decides how the document is
	// protected. It used to be dropped, and the document stored with an empty
	// classification the caller believed they had set.
	body := `{"tenant_id":"` + testTenant + `","legal_entity_id":"e","title":"x",` +
		`"classifcation":"RESTRICTED","classification":"INTERNAL",` +
		`"content_type":"text/plain","content_base64":"aGk="}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/documents", strings.NewReader(body)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
