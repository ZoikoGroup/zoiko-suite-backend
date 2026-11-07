package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 point-in-time reads and the evidence on retirement, at the HTTP layer.
// The SQL is proven on a real database in internal/store.

// historyStub adds the history capabilities to the shared stub.
type historyStub struct {
	*stubStore

	asOf        *domain.JurisdictionAsOf
	hierarchy   *domain.Hierarchy
	readErr     error
	gotID       string
	gotAt       time.Time
	gotDepth    int
	retireCalls []domain.RetireJurisdictionParams
	retired     *domain.Jurisdiction
	retireErr   error
}

func (s *historyStub) GetJurisdictionAsOf(_ context.Context, id string, at time.Time) (*domain.JurisdictionAsOf, error) {
	s.gotID, s.gotAt = id, at
	return s.asOf, s.readErr
}

func (s *historyStub) GetHierarchy(_ context.Context, id string, at time.Time, depth int) (*domain.Hierarchy, error) {
	s.gotID, s.gotAt, s.gotDepth = id, at, depth
	return s.hierarchy, s.readErr
}

func (s *historyStub) RetireJurisdictionWithQuerier(_ context.Context, _ store.Querier, p domain.RetireJurisdictionParams) (*domain.Jurisdiction, bool, error) {
	s.retireCalls = append(s.retireCalls, p)
	return s.retired, s.retireErr == nil, s.retireErr
}

func newHistoryStub() *historyStub {
	return &historyStub{
		stubStore: &stubStore{},
		asOf:      &domain.JurisdictionAsOf{JurisdictionID: "j-1", JurisdictionCode: "GB", VersionNo: 2},
		hierarchy: &domain.Hierarchy{Count: 1, Root: &domain.HierarchyNode{JurisdictionID: "j-1", Children: []*domain.HierarchyNode{}}},
		retired:   &domain.Jurisdiction{JurisdictionID: "j-1", ActiveFlag: false},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func problemCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &b)
	c, _ := b["code"].(string)
	return c
}

func TestAsOf_ParsesTheInstantAndAnswersTheVersion(t *testing.T) {
	for raw, want := range map[string]time.Time{
		"2026-03-31":           time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		"2026-03-31T10:30:00Z": time.Date(2026, 3, 31, 10, 30, 0, 0, time.UTC),
	} {
		st := newHistoryStub()
		rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/as-of?as_of="+raw)
		if rr.Code != http.StatusOK {
			t.Fatalf("as_of=%s: %d %s", raw, rr.Code, rr.Body.String())
		}
		if st.gotID != "j-1" || !st.gotAt.Equal(want) {
			t.Errorf("as_of=%s: store asked for %q at %v, want j-1 at %v", raw, st.gotID, st.gotAt, want)
		}
		var out domain.JurisdictionAsOf
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		if out.JurisdictionCode != "GB" || out.VersionNo != 2 {
			t.Errorf("body = %s", rr.Body.String())
		}
	}
}

// A date nobody can have meant is refused rather than read as "now".
func TestAsOf_AMalformedInstantIs400AndNeverReachesTheStore(t *testing.T) {
	for _, path := range []string{"/v1/jurisdictions/j-1/as-of?as_of=last-tuesday", "/v1/jurisdictions/j-1/hierarchy?as_of=31/03/2026"} {
		st := newHistoryStub()
		rr := get(t, newTestRouter(st), path)
		if rr.Code != http.StatusBadRequest || problemCode(t, rr) != "invalid_as_of" {
			t.Errorf("%s: %d %q, want 400 invalid_as_of", path, rr.Code, problemCode(t, rr))
		}
		if st.gotID != "" {
			t.Errorf("%s: the store was queried for an unparseable date", path)
		}
	}
}

func TestAsOf_DefaultsToNow(t *testing.T) {
	st := newHistoryStub()
	before := time.Now().UTC()
	rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/as-of")
	if rr.Code != http.StatusOK || st.gotAt.Before(before.Add(-time.Second)) || st.gotAt.After(time.Now().Add(time.Second)) {
		t.Errorf("status %d, as-of %v, want about now", rr.Code, st.gotAt)
	}
}

func TestAsOf_RefusalsAreExplicit(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unknown id", domain.ErrJurisdictionNotFound, http.StatusNotFound, "jurisdiction_not_found"},
		{"not in force then", &domain.NotEffectiveError{JurisdictionID: "j-1", AsOf: time.Now(), EffectiveFrom: time.Now().Add(time.Hour)}, http.StatusNotFound, "jurisdiction_not_effective"},
		{"store down", domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
	}
	for _, c := range cases {
		st := newHistoryStub()
		st.readErr = c.err
		rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/as-of")
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q (%s)", c.name, rr.Code, problemCode(t, rr), c.status, c.code, rr.Body.String())
		}
	}
}

// Not-in-force says WHEN it was, so a caller resolving an old document learns
// the code was real rather than mistyped.
func TestAsOf_NotEffectiveSaysWhen(t *testing.T) {
	st := newHistoryStub()
	from := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	st.readErr = &domain.NotEffectiveError{JurisdictionID: "j-1", AsOf: from.Add(-time.Hour), EffectiveFrom: from}
	rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/as-of")
	if !strings.Contains(rr.Body.String(), "2030-01-01") {
		t.Errorf("body = %s, want the effective-from date", rr.Body.String())
	}
}

func TestHierarchy_DepthDefaultsAndIsParsed(t *testing.T) {
	st := newHistoryStub()
	if rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/hierarchy"); rr.Code != http.StatusOK || st.gotDepth != 5 {
		t.Errorf("default: %d depth %d, want 200 and 5", rr.Code, st.gotDepth)
	}
	st = newHistoryStub()
	if rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/hierarchy?depth=2"); rr.Code != http.StatusOK || st.gotDepth != 2 {
		t.Errorf("depth=2: %d depth %d", rr.Code, st.gotDepth)
	}
	st = newHistoryStub()
	rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/hierarchy?depth=deep")
	if rr.Code != http.StatusBadRequest || problemCode(t, rr) != "invalid_depth" || st.gotID != "" {
		t.Errorf("depth=deep: %d %q reached store=%v", rr.Code, problemCode(t, rr), st.gotID != "")
	}
}

func TestHierarchy_StoreRefusalsAreMapped(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"depth out of range": {domain.ErrInvalidDepth, http.StatusBadRequest, "invalid_depth"},
		"too large":          {domain.ErrHierarchyTooLarge, http.StatusUnprocessableEntity, "hierarchy_too_large"},
		"unknown root":       {domain.ErrJurisdictionNotFound, http.StatusNotFound, "jurisdiction_not_found"},
	} {
		st := newHistoryStub()
		st.readErr = c.err
		rr := get(t, newTestRouter(st), "/v1/jurisdictions/j-1/hierarchy")
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q", name, rr.Code, problemCode(t, rr), c.status, c.code)
		}
	}
}

func TestRetire_EvidenceInTheBodyReachesTheStore(t *testing.T) {
	st := newHistoryStub()
	succ := "j-2"
	rr := postJSON(t, newTestRouter(st), "/v1/admin/jurisdictions/j-1/deactivate", map[string]any{
		"reason": "  state dissolved ", "evidence_ref": "iso-newsletter-3", "superseded_by_jurisdiction_id": succ,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if len(st.retireCalls) != 1 {
		t.Fatalf("%d retire calls", len(st.retireCalls))
	}
	p := st.retireCalls[0]
	if p.JurisdictionID != "j-1" || p.ActorID != testPrincipal || p.Reason != "state dissolved" ||
		p.EvidenceRef != "iso-newsletter-3" || p.SupersededBy == nil || *p.SupersededBy != succ {
		t.Errorf("params = %+v, want the verified caller as actor and the trimmed evidence", p)
	}
}

// The body is optional: the endpoint that existed before still works with none.
func TestRetire_NoBodyStillWorks(t *testing.T) {
	st := newHistoryStub()
	rr := postJSON(t, newTestRouter(st), "/v1/admin/jurisdictions/j-1/deactivate", nil)
	if rr.Code != http.StatusOK || len(st.retireCalls) != 1 || st.retireCalls[0].Reason != "" {
		t.Errorf("%d, calls %+v", rr.Code, st.retireCalls)
	}
}

func TestRetire_RefusalsAreMapped(t *testing.T) {
	st := newHistoryStub()
	st.retireErr = domain.ErrInvalidSuccessor
	rr := postJSON(t, newTestRouter(st), "/v1/admin/jurisdictions/j-1/deactivate", map[string]any{"superseded_by_jurisdiction_id": "j-1"})
	if rr.Code != http.StatusBadRequest || problemCode(t, rr) != "invalid_successor" {
		t.Errorf("%d %q, want 400 invalid_successor", rr.Code, problemCode(t, rr))
	}

	st = newHistoryStub()
	rr = postJSON(t, newTestRouter(st), "/v1/admin/jurisdictions/j-1/deactivate", map[string]any{"reason": strings.Repeat("x", 2001)})
	if rr.Code != http.StatusBadRequest || problemCode(t, rr) != "input_too_long" || len(st.retireCalls) != 0 {
		t.Errorf("over-long reason: %d %q calls %d", rr.Code, problemCode(t, rr), len(st.retireCalls))
	}
}

// If-Match (API-3) reaches the retirement as the expected version, and the
// store's stale-version refusal is 412, not an outage.
func TestRetire_IfMatchIsTheExpectedVersion(t *testing.T) {
	st := newHistoryStub()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions/j-1/deactivate", nil)
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("If-Match", `"4"`)
	rr := executeRequest(newTestRouter(st), req)
	if rr.Code != http.StatusOK || len(st.retireCalls) != 1 {
		t.Fatalf("%d, calls %+v: %s", rr.Code, st.retireCalls, rr.Body.String())
	}
	if v := st.retireCalls[0].ExpectedRowVersion; v == nil || *v != 4 {
		t.Errorf("expected row version = %v, want 4", v)
	}

	st = newHistoryStub()
	st.retireErr = domain.ErrVersionConflict
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions/j-1/deactivate", nil)
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("If-Match", `"3"`)
	if rr := executeRequest(newTestRouter(st), req); rr.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match: %d, want 412: %s", rr.Code, rr.Body.String())
	}
}
