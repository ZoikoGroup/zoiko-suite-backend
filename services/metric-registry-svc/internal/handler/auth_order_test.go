package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	authzpkg "zoiko.io/metric-registry-svc/internal/authz"
	"zoiko.io/metric-registry-svc/internal/domain"
)

// On both write routes identity and authorization come before any read of
// stored state. A replay returns a stored definition, and the publish route's
// existing-definition lookup answers 404 for an absent code, so reaching either
// without identity would let an anonymous caller read a definition or tell an
// existing metric_code from an absent one.

// spyStore counts the stateful lookups the write handlers make.
type spyStore struct {
	*stubStore
	idemLookups, activeLookups int
}

func (s *spyStore) FindByIdempotencyKey(ctx context.Context, code, key string) (*domain.ReportMetricDefinition, error) {
	s.idemLookups++
	return s.stubStore.FindByIdempotencyKey(ctx, code, key)
}

func (s *spyStore) GetActiveMetricDefinition(ctx context.Context, code string) (*domain.ReportMetricDefinition, error) {
	s.activeLookups++
	return s.stubStore.GetActiveMetricDefinition(ctx, code)
}

func newSpyRouter(az AuthzChecker) (*spyStore, http.Handler) {
	st := &spyStore{stubStore: newStubStore()}
	h := New(st, &stubPublisher{}, az, zap.NewNop())
	return st, newTestRouter(h)
}

func call(r http.Handler, method, path string, body interface{}, principal, idemKey string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createBody(code string) domain.CreateReportMetricRequest {
	return domain.CreateReportMetricRequest{
		MetricCode: code, MetricName: "n", FormulaDescription: "f",
		OwnerPrincipalID: "owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}
}

func publishBody() domain.PublishMetricVersionRequest {
	return domain.PublishMetricVersionRequest{
		MetricName: "n2", FormulaDescription: "f2", OwnerPrincipalID: "owner-1", EffectiveFrom: "2026-02-01T00:00:00Z",
	}
}

// switchAuthz lets a test flip authorization after setup.
type switchAuthz struct{ err error }

func (a *switchAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// ── Create ───────────────────────────────────────────────────────────────────

func TestCreate_NoPrincipal_NeverGetsAReplay_KnownOrUnknownKey(t *testing.T) {
	st, r := newSpyRouter(&stubAuthz{})
	first := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "creator-1", "key-known")
	if first.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201, got %d — %s", first.Code, first.Body.String())
	}
	st.idemLookups = 0

	for name, key := range map[string]string{"known key": "key-known", "unknown key": "key-unknown"} {
		t.Run(name, func(t *testing.T) {
			w := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "", key)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d — %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("metric_definition_id")) {
				t.Fatalf("an unauthenticated caller must not receive the stored definition: %s", w.Body.String())
			}
		})
	}
	if st.idemLookups != 0 {
		t.Fatalf("the replay lookup must not run before identity, ran %d times", st.idemLookups)
	}
}

func TestCreate_Unauthorized_NeverGetsAReplay(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"denied":      {authzpkg.ErrAuthorizationDenied, http.StatusForbidden},
		"unavailable": {errors.New("authorization-svc down"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			az := &switchAuthz{}
			st, r := newSpyRouter(az)
			if w := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "creator-1", "key-known"); w.Code != http.StatusCreated {
				t.Fatalf("setup: %d", w.Code)
			}
			st.idemLookups = 0
			az.err = tc.err
			w := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "someone", "key-known")
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d — %s", tc.want, w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("metric_definition_id")) || st.idemLookups != 0 {
				t.Fatalf("no replay may be disclosed or looked up: lookups=%d body=%s", st.idemLookups, w.Body.String())
			}
		})
	}
}

func TestCreate_Authorized_ReplayStillReturnsOriginal201_AndNewKeyCreates(t *testing.T) {
	st, r := newSpyRouter(&stubAuthz{})
	first := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "creator-1", "key-1")
	replay := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_A"), "creator-1", "key-1")
	if first.Code != http.StatusCreated || replay.Code != http.StatusCreated {
		t.Fatalf("expected 201/201, got %d/%d", first.Code, replay.Code)
	}
	var a, b domain.ReportMetricDefinition
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(replay.Body.Bytes(), &b)
	if a.MetricDefinitionID == "" || a.MetricDefinitionID != b.MetricDefinitionID {
		t.Fatalf("replay must return the original row: %q vs %q", a.MetricDefinitionID, b.MetricDefinitionID)
	}
	if w := call(r, http.MethodPost, "/v1/report-metrics", createBody("CODE_B"), "creator-1", "key-new"); w.Code != http.StatusCreated {
		t.Fatalf("a new key must create: %d — %s", w.Code, w.Body.String())
	}
	if len(st.versions["CODE_A"]) != 1 || len(st.versions["CODE_B"]) != 1 {
		t.Fatalf("expected exactly one row per code")
	}
}

// ── Publish ──────────────────────────────────────────────────────────────────

func TestPublish_NoPrincipal_CannotTellExistingFromAbsentCode(t *testing.T) {
	st, r := newSpyRouter(&stubAuthz{})
	call(r, http.MethodPost, "/v1/report-metrics", createBody("EXISTS"), "creator-1", "key-create")
	st.idemLookups, st.activeLookups = 0, 0

	existing := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "", "key-p1")
	absent := call(r, http.MethodPost, "/v1/report-metrics/ABSENT/versions", publishBody(), "", "key-p2")
	if existing.Code != http.StatusUnauthorized || absent.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401/401, got %d/%d", existing.Code, absent.Code)
	}
	if existing.Body.String() != absent.Body.String() {
		t.Fatalf("responses must be indistinguishable: %q vs %q", existing.Body.String(), absent.Body.String())
	}
	if st.idemLookups != 0 || st.activeLookups != 0 {
		t.Fatalf("no stored state may be read before identity: idem=%d active=%d", st.idemLookups, st.activeLookups)
	}
}

func TestPublish_NoPrincipal_NeverGetsAReplay(t *testing.T) {
	st, r := newSpyRouter(&stubAuthz{})
	call(r, http.MethodPost, "/v1/report-metrics", createBody("EXISTS"), "creator-1", "key-create")
	if w := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "publisher-1", "key-pub"); w.Code != http.StatusCreated {
		t.Fatalf("setup: %d — %s", w.Code, w.Body.String())
	}
	st.idemLookups = 0
	w := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "", "key-pub")
	if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte("metric_definition_id")) {
		t.Fatalf("expected a bare 401, got %d — %s", w.Code, w.Body.String())
	}
	if st.idemLookups != 0 {
		t.Fatalf("replay lookup ran before identity")
	}
}

func TestPublish_Unauthorized_CannotTellExistingFromAbsentCode(t *testing.T) {
	az := &switchAuthz{}
	st, r := newSpyRouter(az)
	call(r, http.MethodPost, "/v1/report-metrics", createBody("EXISTS"), "creator-1", "key-create")
	st.idemLookups, st.activeLookups = 0, 0
	az.err = authzpkg.ErrAuthorizationDenied

	existing := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "someone", "")
	absent := call(r, http.MethodPost, "/v1/report-metrics/ABSENT/versions", publishBody(), "someone", "")
	if existing.Code != http.StatusForbidden || absent.Code != http.StatusForbidden {
		t.Fatalf("expected 403/403, got %d/%d", existing.Code, absent.Code)
	}
	if existing.Body.String() != absent.Body.String() {
		t.Fatalf("responses must be indistinguishable: %q vs %q", existing.Body.String(), absent.Body.String())
	}
	if st.idemLookups != 0 || st.activeLookups != 0 {
		t.Fatalf("no stored state may be read before authorization: idem=%d active=%d", st.idemLookups, st.activeLookups)
	}
}

func TestPublish_Authorized_UnknownCodeStill404(t *testing.T) {
	_, r := newSpyRouter(&stubAuthz{})
	w := call(r, http.MethodPost, "/v1/report-metrics/ABSENT/versions", publishBody(), "publisher-1", "key-x")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected the documented 404, got %d — %s", w.Code, w.Body.String())
	}
}

func TestPublish_Authorized_PublishAndReplayUnchanged(t *testing.T) {
	st, r := newSpyRouter(&stubAuthz{})
	call(r, http.MethodPost, "/v1/report-metrics", createBody("EXISTS"), "creator-1", "key-create")

	first := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "publisher-1", "key-pub")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", first.Code, first.Body.String())
	}
	var v2 domain.ReportMetricDefinition
	_ = json.Unmarshal(first.Body.Bytes(), &v2)
	if v2.Version != 2 {
		t.Fatalf("expected version 2, got %d", v2.Version)
	}

	replay := call(r, http.MethodPost, "/v1/report-metrics/EXISTS/versions", publishBody(), "publisher-1", "key-pub")
	var again domain.ReportMetricDefinition
	_ = json.Unmarshal(replay.Body.Bytes(), &again)
	if replay.Code != http.StatusCreated || again.MetricDefinitionID != v2.MetricDefinitionID {
		t.Fatalf("replay must return the original version: %d — %s", replay.Code, replay.Body.String())
	}
	if len(st.versions["EXISTS"]) != 2 {
		t.Fatalf("a replay must not create a third version, got %d", len(st.versions["EXISTS"]))
	}
}
