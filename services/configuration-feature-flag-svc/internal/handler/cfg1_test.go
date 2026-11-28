package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// CFG-1 (API-3): every mutation of an existing aggregate needs If-Match; the
// handler hands it to the store, which compares it under the row lock.

type cfg1Route struct {
	name, path, body string
	allowNone        bool
}

var cfg1Routes = []cfg1Route{
	{name: "approve change", path: "/v1/config/changes/c-1/approve", body: `{"approved":true}`},
	{name: "rollback change", path: "/v1/config/changes/c-1/rollback", body: `{}`},
	{name: "flag removal", path: "/v1/flags/legacy/removal", body: `{"environment":"production","consumer_scan_evidence":{"references":0}}`},
	{name: "release plan", path: "/v1/flags/new_ui/release-plans", body: `{"environment":"production","strategy":"ALL_OR_NOTHING"}`, allowNone: true},
}

func cfg1Store() *stubStore {
	return &stubStore{
		approvedChange: &domain.ConfigChange{ChangeID: "c-1", Status: domain.ChangeStatusApproved, RecordVersion: 3},
		rollback:       &domain.ConfigChange{ChangeID: "c-2", Status: domain.ChangeStatusProposed},
		retirement:     &domain.FlagRetirement{RetirementID: "r-1", RecordVersion: 2},
		releasePlan:    &domain.ReleasePlan{ReleasePlanID: "rp-1", Version: 4},
	}
}

func cfg1Post(t *testing.T, s *stubStore, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := authed(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	return w
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.Code
}

func TestCFG1_MutationWithoutIfMatchIs428AndNeverReachesTheStore(t *testing.T) {
	for _, rt := range cfg1Routes {
		t.Run(rt.name, func(t *testing.T) {
			s := cfg1Store()
			w := cfg1Post(t, s, rt.path, rt.body, nil)
			if w.Code != http.StatusPreconditionRequired || problemCode(t, w) != "precondition_required" {
				t.Fatalf("want 428 precondition_required, got %d: %s", w.Code, w.Body.String())
			}
			if s.gotPrecondition != nil || s.approveChangeID != "" || s.rollbackOf != "" ||
				s.gotRemoval.Key != "" || s.gotReleasePlan.FlagKey != "" {
				t.Fatal("the store was called without a precondition")
			}
		})
	}
}

func TestCFG1_MalformedIfMatchIs400(t *testing.T) {
	for _, rt := range cfg1Routes {
		t.Run(rt.name, func(t *testing.T) {
			s := cfg1Store()
			w := cfg1Post(t, s, rt.path, rt.body, map[string]string{"If-Match": `"not-a-version"`})
			if w.Code != http.StatusBadRequest || problemCode(t, w) != "invalid_if_match" {
				t.Fatalf("want 400 invalid_if_match, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCFG1_IfMatchReachesTheStore(t *testing.T) {
	for _, rt := range cfg1Routes {
		t.Run(rt.name, func(t *testing.T) {
			s := cfg1Store()
			w := cfg1Post(t, s, rt.path, rt.body, map[string]string{"If-Match": `"v3"`})
			if w.Code >= 300 {
				t.Fatalf("want 2xx, got %d: %s", w.Code, w.Body.String())
			}
			if s.gotPrecondition == nil || s.gotPrecondition.Version != 3 || s.gotPrecondition.Any || s.gotPrecondition.NoneMatch {
				t.Fatalf("store got precondition %+v, want Version 3", s.gotPrecondition)
			}
		})
	}
}

func TestCFG1_StaleVersionIs412WithTheCurrentETag(t *testing.T) {
	for _, rt := range cfg1Routes {
		t.Run(rt.name, func(t *testing.T) {
			s := cfg1Store()
			stale := &domain.StaleVersionError{Current: 5}
			s.approveErr, s.rollbackErr, s.retireErr, s.releasePlanErr = stale, stale, stale, stale
			w := cfg1Post(t, s, rt.path, rt.body, map[string]string{"If-Match": `"v4"`})
			if w.Code != http.StatusPreconditionFailed || problemCode(t, w) != "stale_version" {
				t.Fatalf("want 412 stale_version, got %d: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("ETag"); got != `"v5"` {
				t.Fatalf("ETag = %q, want \"v5\"", got)
			}
			var p struct {
				CurrentVersion *int64 `json:"current_version"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &p)
			if p.CurrentVersion == nil || *p.CurrentVersion != 5 {
				t.Fatalf("current_version = %v, want 5: %s", p.CurrentVersion, w.Body.String())
			}
		})
	}
}

func TestCFG1_ReleasePlan_IfNoneMatchStarPublishesTheFirst(t *testing.T) {
	rt := cfg1Routes[3]
	s := cfg1Store()
	w := cfg1Post(t, s, rt.path, rt.body, map[string]string{"If-None-Match": "*"})
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotPrecondition == nil || !s.gotPrecondition.NoneMatch {
		t.Fatalf("store got %+v, want NoneMatch", s.gotPrecondition)
	}
	if got := w.Header().Get("ETag"); got != `"v4"` {
		t.Fatalf("release plan ETag = %q, want the plan version \"v4\"", got)
	}

	both := cfg1Post(t, cfg1Store(), rt.path, rt.body, map[string]string{"If-None-Match": "*", "If-Match": `"v1"`})
	if both.Code != http.StatusBadRequest || problemCode(t, both) != "invalid_if_match" {
		t.Fatalf("both headers: want 400 invalid_if_match, got %d: %s", both.Code, both.Body.String())
	}

	// If-None-Match: * is a release-plan form only: elsewhere the aggregate
	// already exists, so it is not a precondition.
	ap := cfg1Post(t, cfg1Store(), cfg1Routes[0].path, cfg1Routes[0].body, map[string]string{"If-None-Match": "*"})
	if ap.Code != http.StatusPreconditionRequired {
		t.Fatalf("approve with If-None-Match: want 428, got %d: %s", ap.Code, ap.Body.String())
	}
}

func TestCFG1_RetireAndRemovalAnswerTheRetirementETag(t *testing.T) {
	s := cfg1Store()
	w := cfg1Post(t, s, "/v1/flags/legacy/retire", `{"environment":"production","final_enabled":false}`, nil)
	if w.Code != http.StatusCreated || w.Header().Get("ETag") != `"v2"` {
		t.Fatalf("retire: want 201 with ETag \"v2\", got %d %q: %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
	s.retirement = &domain.FlagRetirement{RetirementID: "r-1", RecordVersion: 3, Reusable: true}
	w = cfg1Post(t, s, "/v1/flags/legacy/removal", cfg1Routes[2].body, map[string]string{"If-Match": `"v2"`})
	if w.Code != http.StatusOK || w.Header().Get("ETag") != `"v3"` {
		t.Fatalf("removal: want 200 with ETag \"v3\", got %d %q: %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
}

// CFG-1 (API-4): GET /v1/config and GET /v1/flags page by keyset cursor.

func cfg1Get(t *testing.T, s *stubStore, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := authed(httptest.NewRequest(http.MethodGet, url, nil))
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

func TestCFG1_ConfigListPagesByCursor(t *testing.T) {
	tenant := testTenant
	s := &stubStore{listConfigResult: []*domain.ConfigEntry{
		{Key: "a.key", Environment: "production"},
		{Key: "a.key", Environment: "production", TenantID: &tenant},
		{Key: "b.key", Environment: "production"},
		{Key: "c.key", Environment: "staging"},
		{Key: "d.key", Environment: "production"},
	}}
	// Every limit, so a page boundary also falls between a key's global
	// default and the tenant's own row.
	for limit := 1; limit <= 3; limit++ {
		cfg1PageThroughConfig(t, s, limit)
	}
}

func cfg1PageThroughConfig(t *testing.T, s *stubStore, limit int) {
	t.Helper()
	var seen []string
	url := "/v1/config?limit=" + strconv.Itoa(limit)
	for pages := 0; pages < 6; pages++ {
		w := cfg1Get(t, s, url)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: want 200, got %d: %s", pages, w.Code, w.Body.String())
		}
		var rows []domain.ConfigEntry
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("body is not an array: %s", w.Body.String())
		}
		if len(rows) > limit {
			t.Fatalf("page %d has %d rows, limit %d", pages, len(rows), limit)
		}
		for _, e := range rows {
			id := e.Key + "/" + e.Environment
			if e.TenantID != nil {
				id += "/" + *e.TenantID
			}
			seen = append(seen, id)
		}
		next := w.Header().Get("X-Next-Cursor")
		if next == "" {
			if w.Header().Get("Link") != "" {
				t.Fatal("last page still sends Link rel=next")
			}
			break
		}
		if !strings.Contains(w.Header().Get("Link"), `rel="next"`) {
			t.Fatalf("Link header missing: %q", w.Header().Get("Link"))
		}
		url = "/v1/config?limit=" + strconv.Itoa(limit) + "&cursor=" + next
	}
	want := []string{"a.key/production", "a.key/production/" + testTenant, "b.key/production", "c.key/staging", "d.key/production"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("limit %d, rows across pages:\n got %v\nwant %v", limit, seen, want)
	}
}

func TestCFG1_FlagListPagesByCursor(t *testing.T) {
	s := &stubStore{listFlagResult: []*domain.FeatureFlag{
		{Key: "f1", Environment: "production"},
		{Key: "f2", Environment: "production"},
		{Key: "f3", Environment: "production"},
	}}
	w := cfg1Get(t, s, "/v1/flags?limit=2")
	next := w.Header().Get("X-Next-Cursor")
	if w.Code != http.StatusOK || next == "" {
		t.Fatalf("first page: want 200 with a cursor, got %d %q", w.Code, next)
	}
	w = cfg1Get(t, s, "/v1/flags?limit=2&cursor="+next)
	var rows []domain.FeatureFlag
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].Key != "f3" || w.Header().Get("X-Next-Cursor") != "" {
		t.Fatalf("second page: got %+v cursor %q", rows, w.Header().Get("X-Next-Cursor"))
	}
	// No limit: the default page (100) holds all three.
	w = cfg1Get(t, s, "/v1/flags")
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 3 || w.Header().Get("X-Next-Cursor") != "" {
		t.Fatalf("default page: got %d rows, cursor %q", len(rows), w.Header().Get("X-Next-Cursor"))
	}
}

func TestCFG1_ListPagingRefusesBadInput(t *testing.T) {
	for _, tc := range []struct{ url, code string }{
		{"/v1/config?limit=0", "invalid_limit"},
		{"/v1/config?limit=501", "invalid_limit"},
		{"/v1/config?limit=x", "invalid_limit"},
		{"/v1/config?cursor=%21%21", "invalid_cursor"},
		{"/v1/flags?cursor=bm90LWpzb24", "invalid_cursor"},
	} {
		w := cfg1Get(t, &stubStore{}, tc.url)
		if w.Code != http.StatusBadRequest || problemCode(t, w) != tc.code {
			t.Errorf("%s: want 400 %s, got %d: %s", tc.url, tc.code, w.Code, w.Body.String())
		}
	}
}
