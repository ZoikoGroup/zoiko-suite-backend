package handler_test

import (
	"bytes"
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
	"zoiko.io/document-vault-svc/internal/envelope"
	"zoiko.io/document-vault-svc/internal/handler"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/storage"
)

// ── Gap 1: a download is never served without its access-log record ───────────

func downloadSetup(t *testing.T) (*stubStore, *countingStorage, chi.Router, string) {
	t.Helper()
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	return s, st, r, uploadAndGetID(t, r)
}

func TestDownload_Success_IsLogged_AndServed(t *testing.T) {
	s, _, r, id := downloadSetup(t)

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "download me", rec.Body.String())
	require.Len(t, s.accessLog, 1, "exactly one access record for the download")
	assert.Equal(t, domain.AccessDownload, s.accessLog[0].AccessType)
	assert.Equal(t, id, s.accessLog[0].DocumentID)
	assert.Equal(t, testPrincipal, s.accessLog[0].AccessedByPrincipalID)
}

func TestDownload_AccessLogWriteFails_NothingIsServed(t *testing.T) {
	s, _, r, id := downloadSetup(t)
	s.recordErr = errors.New("access log insert failed")

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	code, detail := errorCode(t, rec)
	assert.Equal(t, "store_unavailable", code, "the service's existing store-failure code")
	assert.Contains(t, detail, "no content was served")

	assert.NotContains(t, rec.Body.String(), "download me", "the document bytes must not be in the response")
	assert.Empty(t, rec.Header().Get("Content-Disposition"))
	assert.Empty(t, rec.Header().Get("X-Checksum-SHA256"))
	assert.Empty(t, s.accessLog)
}

func TestDownload_AuthorizationStillComesFirst_WhenAccessLogIsBroken(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	az := &stubAuthz{}
	r := fullRouter(s, st, az, newStubScanner(), &stubResidency{}, nil)
	id := uploadAndGetID(t, r)
	s.recordErr = errors.New("access log insert failed")
	az.denied = map[string]bool{testPrincipal + "|" + authz.ActionDocumentDownload: true}

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "denial is 403, not the access-log failure")
	assert.Empty(t, s.accessLog)
}

func TestDownload_IntegrityAndMissingObject_KeepTheirCodes_WhenAccessLogIsBroken(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"integrity failure": {storage.ErrIntegrityFailure, http.StatusConflict, "integrity_check_failed"},
		"missing object":    {storage.ErrObjectNotFound, http.StatusServiceUnavailable, "object_missing"},
	} {
		t.Run(name, func(t *testing.T) {
			s, st, r, id := downloadSetup(t)
			st.getErr = tc.err
			s.recordErr = errors.New("access log insert failed")

			rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id+"/content", nil))
			require.Equal(t, tc.status, rec.Code)
			code, _ := errorCode(t, rec)
			assert.Equal(t, tc.code, code, "the integrity / object check runs before the access record and keeps its own error")
			assert.Empty(t, rec.Header().Get("Content-Disposition"))
		})
	}
}

// The documented guarantee is about downloads. A metadata read keeps its
// earlier best-effort logging, so this does not change unrelated behaviour.
func TestMetadataRead_StillServed_WhenAccessLogFails(t *testing.T) {
	s, _, r, id := downloadSetup(t)
	s.recordErr = errors.New("access log insert failed")

	rec := doReq(r, httptest.NewRequest(http.MethodGet, "/v1/documents/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ── Gap 2: bulk-classify is atomic ───────────────────────────────────────────

func bulkSetup(t *testing.T, n int) (*stubStore, chi.Router, []string) {
	t.Helper()
	s := newStubStore()
	r := newRouter(s, &stubResidency{}, newTestStorage(t))
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		rec := doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", fmt.Sprintf("doc %d", i)))))
		require.Equal(t, http.StatusCreated, rec.Code)
		var d domain.Document
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
		ids[i] = d.DocumentID
	}
	return s, r, ids
}

func bulk(r http.Handler, t *testing.T, ids []string) *httptest.ResponseRecorder {
	return doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents/bulk-classify", bytes.NewReader(bulkClassifyBody(t, ids, "RESTRICTED"))))
}

func classificationCount(s *stubStore, docID string) int {
	n := 0
	for _, c := range s.classifications {
		if c.DocumentID == docID {
			n++
		}
	}
	return n
}

func assertRolledBack(t *testing.T, rec *httptest.ResponseRecorder, status int, code, failedDoc string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var f map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &f))
	assert.Equal(t, code, f["error"])
	assert.Equal(t, failedDoc, f["failed_document_id"])
	assert.Equal(t, true, f["rolled_back"])
}

func TestBulkClassify_AllValid_CommitsEveryClassification(t *testing.T) {
	s, r, ids := bulkSetup(t, 3)
	before := map[string]int{}
	for _, id := range ids {
		before[id] = classificationCount(s, id)
	}

	rec := bulk(r, t, ids)
	require.Equal(t, http.StatusOK, rec.Code)
	var results []bulkClassifyResultDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &results))
	require.Len(t, results, 3)
	for i, res := range results {
		assert.Equal(t, ids[i], res.DocumentID, "results keep the request order")
		require.NotNil(t, res.Classification)
		assert.Equal(t, before[ids[i]]+1, classificationCount(s, ids[i]))
	}
}

func TestBulkClassify_FailureAtFirstMiddleOrLast_AppliesNothing(t *testing.T) {
	for name, position := range map[string]int{"first": 0, "middle": 1, "last": 2} {
		t.Run(name, func(t *testing.T) {
			s, r, ids := bulkSetup(t, 2)
			batch := make([]string, 0, 3)
			batch = append(batch, ids...)
			// insert the missing document at the requested position
			batch = append(batch[:position], append([]string{"missing-doc"}, batch[position:]...)...)
			before := len(s.classifications)

			assertRolledBack(t, bulk(r, t, batch), http.StatusNotFound, "document_not_found", "missing-doc")
			assert.Len(t, s.classifications, before, "no partial classification may remain")
		})
	}
}

// The rule failure can only be seen inside the transaction (a document that
// already has a confirmed classification), AFTER earlier proposals were made —
// so this is the case that proves rollback and not just the up-front checks.
func TestBulkClassify_RuleFailureAfterEarlierSuccess_RollsBackTheEarlierProposals(t *testing.T) {
	s, r, ids := bulkSetup(t, 3)
	s.classifications = append(s.classifications, &domain.RecordClassification{
		ClassificationID: "already-confirmed", DocumentID: ids[1], Status: domain.ClassificationStatusConfirmed,
	})
	before := map[string]int{}
	for _, id := range ids {
		before[id] = classificationCount(s, id)
	}

	assertRolledBack(t, bulk(r, t, ids), http.StatusConflict, "not_candidate", ids[1])
	for _, id := range ids {
		assert.Equal(t, before[id], classificationCount(s, id), "doc %s must be unchanged, including the one processed before the failure", id)
	}
}

func TestBulkClassify_PerDocumentAuthorization_Preserved_AndDenialAppliesNothing(t *testing.T) {
	s := newStubStore()
	az := &stubAuthz{}
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
	for i := 0; i < 2; i++ {
		doReq(r, httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", fmt.Sprintf("d%d", i)))))
	}
	az.calls = nil
	before := len(s.classifications)

	require.Equal(t, http.StatusOK, bulk(r, t, []string{"doc-1", "doc-2"}).Code)
	assert.Equal(t, []string{
		testPrincipal + "|" + authz.ActionClassifyRecord,
		testPrincipal + "|" + authz.ActionClassifyRecord,
	}, az.calls, "every document is authorized individually")
	assert.Len(t, s.classifications, before+2)

	az.denied = map[string]bool{testPrincipal + "|" + authz.ActionClassifyRecord: true}
	after := len(s.classifications)
	assertRolledBack(t, bulk(r, t, []string{"doc-1", "doc-2"}), http.StatusForbidden, "forbidden", "doc-1")
	assert.Len(t, s.classifications, after)
}

func TestBulkClassify_ExactlyFiveHundredIDs_StillAccepted_FiveHundredAndOne_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubResidency{}, newTestStorage(t))
	mk := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("x-%d", i)
		}
		return ids
	}
	// 500 passes the size check (and then stops at the first unknown id).
	assert.Equal(t, http.StatusNotFound, bulk(r, t, mk(500)).Code)
	assert.Equal(t, http.StatusBadRequest, bulk(r, t, mk(501)).Code)
}

// ── Gap 4: the envelope status contract ──────────────────────────────────────

func TestEnvelope_MissingTenantOrActor_Is401_OtherViolations400(t *testing.T) {
	r := chi.NewRouter()
	r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), envelope.ModeStrict, nil))
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(newStubStore(), newTestStorage(t), &stubResidency{}, &stubAuthz{}, newStubScanner(), zap.NewNop())
	handler.RegisterRoutes(r, h)

	full := map[string]string{
		"X-Tenant-Id": testTenant, "X-Principal-Id": testPrincipal, "X-Request-Id": "req-1", "X-Correlation-ID": "corr-1",
		"X-Source-Channel": "api", "Idempotency-Key": "key-1", "X-Legal-Entity-Id": "entity-1", "X-Purpose-Context": "test",
	}
	send := func(drop string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader([]byte(`{}`)))
		for k, v := range full {
			if k != drop {
				req.Header.Set(k, v)
			}
		}
		return doReq(r, req)
	}

	for _, missing := range []string{"X-Tenant-Id", "X-Principal-Id"} {
		rec := send(missing)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "missing %s is an authentication failure", missing)
		assert.Contains(t, rec.Body.String(), "envelope_incomplete")
	}
	for _, missing := range []string{"X-Request-Id", "Idempotency-Key", "X-Purpose-Context", "X-Source-Channel"} {
		rec := send(missing)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "missing %s is a contract error", missing)
		assert.Contains(t, rec.Body.String(), "envelope_incomplete")
	}
}
