package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/testdb"
)

// POST /privacy/decisions reads its body through a 1 MiB cap (the sibling
// privacy services' limit). An oversized body is refused as 400
// VALIDATION_FAILED ("unreadable body") before replay lookup, validation or
// evaluation, so nothing is evaluated, committed or consumed from the
// idempotency store.

const bodyLimit = 1 << 20

// countingPurposes records whether evaluation reached the first registry call.
type countingPurposes struct {
	*stubPurposeRegistry
	activityCalls int32
}

func (c *countingPurposes) ResolveActivity(ctx context.Context, tenantID, principalID, activityID string) (*purposeregistry.ActivityVersion, error) {
	atomic.AddInt32(&c.activityCalls, 1)
	return c.stubPurposeRegistry.ResolveActivity(ctx, tenantID, principalID, activityID)
}

// newLimitRouter wires the handler with any PurposeChecker (the shared
// newTestRouter takes the concrete stub).
func newLimitRouter(st *stubStore, purposes handler.PurposeChecker) chi.Router {
	h := handler.New(st, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, &stubTransferRegistry{}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func newCountingPurposes() *countingPurposes {
	p := newStubPurposeRegistry()
	p.activities[testActivity] = activeActivity()
	p.purposes[testPurpose] = publishedPurpose()
	return &countingPurposes{stubPurposeRegistry: p}
}

// bodyOfSize returns a valid decision request whose serialized length is
// exactly size bytes, padded through a free-text context field.
func bodyOfSize(t *testing.T, size int) []byte {
	t.Helper()
	sc := validSubjectContext()
	sc.Relationship = "a" // present in the base so the padded field adds no extra bytes
	req := domain.EvaluateDecisionRequest{
		SubjectRef: "user-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse, SubjectContext: sc,
	}
	base, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	pad := size - len(base)
	if pad < 0 {
		t.Fatalf("size %d is smaller than the base request (%d)", size, len(base))
	}
	sc.Relationship = "a" + strings.Repeat("a", pad)
	req.SubjectContext = sc
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != size {
		t.Fatalf("built %d bytes, want %d", len(b), size)
	}
	return b
}

func postRaw(r http.Handler, tenant string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/privacy/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	req.Header.Set("X-Correlation-ID", "corr-01")
	req.Header.Set("Idempotency-Key", "idem-"+uuid.NewString())
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not the JSON envelope: %q", w.Body.String())
	}
	return m["error_code"]
}

func TestBodyLimit_ExactlyOneMiB_IsProcessedNormally(t *testing.T) {
	st := newStubStore()
	purposes := newCountingPurposes()
	r := newLimitRouter(st, purposes)

	w := postRaw(r, testTenant, bodyOfSize(t, bodyLimit), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a body of exactly 1 MiB must be processed: %d %s", w.Code, w.Body.String())
	}
	if purposes.activityCalls == 0 || len(st.decisions) != 1 {
		t.Fatalf("expected evaluation and one persisted decision (calls=%d, decisions=%d)", purposes.activityCalls, len(st.decisions))
	}
}

func TestBodyLimit_OverOneMiB_IsRefusedBeforeEvaluationAndCommit(t *testing.T) {
	st := newStubStore()
	purposes := newCountingPurposes()
	r := newLimitRouter(st, purposes)

	for _, size := range []int{bodyLimit + 1, 4 * bodyLimit} {
		w := postRaw(r, testTenant, bodyOfSize(t, size), nil)
		if w.Code != http.StatusBadRequest || code(t, w) != domain.ErrCodeValidationFailed {
			t.Fatalf("%d bytes: want 400 %s, got %d %s", size, domain.ErrCodeValidationFailed, w.Code, w.Body.String())
		}
	}
	if purposes.activityCalls != 0 {
		t.Fatalf("an oversized request must never reach evaluation (%d registry calls)", purposes.activityCalls)
	}
	if st.commitCalls != 0 || len(st.decisions) != 0 || len(st.idempotency) != 0 {
		t.Fatalf("nothing may be committed: commits=%d decisions=%d keys=%d", st.commitCalls, len(st.decisions), len(st.idempotency))
	}
}

// A refused oversized request does not consume the Idempotency-Key.
func TestBodyLimit_OversizedRequestDoesNotConsumeTheKey(t *testing.T) {
	st := newStubStore()
	r := newLimitRouter(st, newCountingPurposes())
	hdr := map[string]string{"Idempotency-Key": "idem-reused"}

	if w := postRaw(r, testTenant, bodyOfSize(t, bodyLimit+1), hdr); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized: %d", w.Code)
	}
	w := postRaw(r, testTenant, bodyOfSize(t, 1024), hdr)
	if w.Code != http.StatusOK || w.Header().Get("Idempotency-Replay") != "" {
		t.Fatalf("the same key must be usable afterwards (first execution, not a replay): %d %s", w.Code, w.Body.String())
	}
}

// The header checks precede the body read: an oversized body with a missing
// header is refused for the header, in the documented order.
func TestBodyLimit_HeaderChecksStillPrecedeTheBodyRead(t *testing.T) {
	r := newLimitRouter(newStubStore(), newCountingPurposes())
	big := bodyOfSize(t, bodyLimit+1)
	cases := []struct {
		name    string
		tenant  string
		headers map[string]string
		status  int
		want    string
	}{
		{"no principal", testTenant, map[string]string{"X-Principal-Id": ""}, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed},
		{"no tenant", "", nil, http.StatusBadRequest, domain.ErrCodeTenantScopeRequired},
		{"no correlation id", testTenant, map[string]string{"X-Correlation-ID": ""}, http.StatusBadRequest, domain.ErrCodeCorrelationRequired},
		{"no idempotency key", testTenant, map[string]string{"Idempotency-Key": ""}, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired},
	}
	for _, tc := range cases {
		w := postRaw(r, tc.tenant, big, tc.headers)
		if w.Code != tc.status || code(t, w) != tc.want {
			t.Errorf("%s: want %d %s, got %d %s", tc.name, tc.status, tc.want, w.Code, w.Body.String())
		}
	}
}

// The cap is applied before the replay lookup, so an oversized body is refused
// even under a key that already has a stored response; the stored decision is
// untouched and an ordinary replay still works.
func TestBodyLimit_ReplayBehaviourUnchanged(t *testing.T) {
	st := newStubStore()
	r := newLimitRouter(st, newCountingPurposes())
	hdr := map[string]string{"Idempotency-Key": "idem-replay"}
	small := bodyOfSize(t, 2048)

	first := postRaw(r, testTenant, small, hdr)
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	if w := postRaw(r, testTenant, bodyOfSize(t, bodyLimit+1), hdr); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body under a used key: want 400, got %d", w.Code)
	}
	replay := postRaw(r, testTenant, small, hdr)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replay") != "true" || replay.Body.String() != first.Body.String() {
		t.Fatalf("ordinary replay changed: %d replay=%q", replay.Code, replay.Header().Get("Idempotency-Replay"))
	}
	if len(st.decisions) != 1 {
		t.Fatalf("exactly one decision expected, got %d", len(st.decisions))
	}
}

// Real PostgreSQL: an oversized request leaves no decision row and no
// idempotency row behind.
func TestBodyLimit_RealDB_NothingPersistedForOversizedBody(t *testing.T) {
	db := testdb.Setup(t)
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()
	r := newRealDBTestRouter(t, db, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)
	tenant := "00000000-0000-0000-0000-0000000000b1"

	if w := postRaw(r, tenant, bodyOfSize(t, bodyLimit+1), nil); w.Code != http.StatusBadRequest || code(t, w) != domain.ErrCodeValidationFailed {
		t.Fatalf("oversized: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"privacy_decisions", "decision_idempotency_keys"} {
		var n int
		if err := db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d row(s) after a refused oversized request", table, n)
		}
	}
	// A normal request under the same router still works and is persisted.
	if w := postRaw(r, tenant, bodyOfSize(t, 2048), nil); w.Code != http.StatusOK {
		t.Fatalf("normal request: %d %s", w.Code, w.Body.String())
	}
	var n int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM privacy_decisions`).Scan(&n)
	if n != 1 {
		t.Errorf("want 1 persisted decision, got %d", n)
	}
}
