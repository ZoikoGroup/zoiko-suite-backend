package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// API-4 cursor paging on the admin registers (AZ-1): opt-in, id-ordered,
// scope-bound cursors, and the unpaged shape unchanged for existing callers.

const pageTenant = "11111111-1111-4111-8111-111111111111"

func pageGet(t *testing.T, s *stubStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", pageTenant)
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

type rolePage struct {
	Records    []domain.Role `json:"records"`
	NextCursor string        `json:"next_cursor"`
	Truncated  bool          `json:"truncated"`
}

func fiveRoles() []domain.Role {
	// Deliberately not in id order: the page orders by id.
	return []domain.Role{{RoleID: "r-4"}, {RoleID: "r-1"}, {RoleID: "r-5"}, {RoleID: "r-2"}, {RoleID: "r-3"}}
}

func TestPaging_TraversesInIDOrderWithoutRepeatsOrGaps(t *testing.T) {
	s := &stubStore{roles: fiveRoles()}
	var seen []string
	path := "/v1/admin/roles?page_size=2"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("traversal did not terminate")
		}
		w := pageGet(t, s, path)
		if w.Code != http.StatusOK {
			t.Fatalf("code %d: %s", w.Code, w.Body.String())
		}
		var p rolePage
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Records {
			seen = append(seen, r.RoleID)
		}
		if p.NextCursor == "" {
			break
		}
		path = "/v1/admin/roles?page_size=2&cursor=" + url.QueryEscape(p.NextCursor)
	}
	want := []string{"r-1", "r-2", "r-3", "r-4", "r-5"}
	if len(seen) != len(want) {
		t.Fatalf("seen %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen %v, want %v", seen, want)
		}
	}
}

// A record removed between pages does not shift the next page (keyset, not
// offset).
func TestPaging_DeletionBetweenPagesSkipsNothing(t *testing.T) {
	s := &stubStore{roles: fiveRoles()}
	var first rolePage
	_ = json.Unmarshal(pageGet(t, s, "/v1/admin/roles?page_size=2").Body.Bytes(), &first)
	s.roles = []domain.Role{{RoleID: "r-3"}, {RoleID: "r-4"}, {RoleID: "r-5"}} // r-1 and r-2 removed
	var second rolePage
	_ = json.Unmarshal(pageGet(t, s, "/v1/admin/roles?page_size=2&cursor="+url.QueryEscape(first.NextCursor)).Body.Bytes(), &second)
	if len(second.Records) != 2 || second.Records[0].RoleID != "r-3" {
		t.Fatalf("second page %+v, want r-3, r-4", second.Records)
	}
}

func TestPaging_CursorIsBoundToItsQuery(t *testing.T) {
	s := &stubStore{roles: fiveRoles()}
	var first rolePage
	_ = json.Unmarshal(pageGet(t, s, "/v1/admin/roles?page_size=2").Body.Bytes(), &first)
	// Same cursor, different filter: refused, not silently applied.
	w := pageGet(t, s, "/v1/admin/roles?page_size=2&active_only=true&cursor="+url.QueryEscape(first.NextCursor))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cursor replayed on another query: got %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := pageGet(t, s, "/v1/admin/roles?cursor=not-a-cursor"); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage cursor: got %d, want 400", w.Code)
	}
}

func TestPaging_PageSizeBounds(t *testing.T) {
	s := &stubStore{roles: fiveRoles()}
	for _, bad := range []string{"0", "-1", "201", "x"} {
		if w := pageGet(t, s, "/v1/admin/roles?page_size="+bad); w.Code != http.StatusBadRequest {
			t.Errorf("page_size=%s: got %d, want 400", bad, w.Code)
		}
	}
	if w := pageGet(t, s, "/v1/admin/roles?page_size=200"); w.Code != http.StatusOK {
		t.Errorf("page_size=200: got %d, want 200", w.Code)
	}
}

// Existing callers (the console, access-control-svc) send no paging
// parameters and still get the bare array.
func TestPaging_UnpagedShapeUnchanged(t *testing.T) {
	w := pageGet(t, &stubStore{roles: fiveRoles()}, "/v1/admin/roles")
	var arr []domain.Role
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil || len(arr) != 5 {
		t.Fatalf("unpaged: want a 5-element array, got %s (%v)", w.Body.String(), err)
	}
}

// The store caps a register at 500 rows. Reaching the cap marks the last page
// truncated rather than presenting the end as complete.
func TestPaging_StoreCapMarksTheLastPageTruncated(t *testing.T) {
	roles := make([]domain.Role, 500)
	for i := range roles {
		roles[i] = domain.Role{RoleID: "r-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676))}
	}
	var p rolePage
	_ = json.Unmarshal(pageGet(t, &stubStore{roles: roles}, "/v1/admin/roles?page_size=200").Body.Bytes(), &p)
	if p.Truncated || p.NextCursor == "" {
		t.Fatalf("first page: truncated=%v next=%q, want a cursor and no truncation yet", p.Truncated, p.NextCursor)
	}
	var second, last rolePage
	path := "/v1/admin/roles?page_size=200&cursor=" + url.QueryEscape(p.NextCursor)
	_ = json.Unmarshal(pageGet(t, &stubStore{roles: roles}, path).Body.Bytes(), &second)
	_ = json.Unmarshal(pageGet(t, &stubStore{roles: roles}, "/v1/admin/roles?page_size=200&cursor="+url.QueryEscape(second.NextCursor)).Body.Bytes(), &last)
	if last.NextCursor != "" || !last.Truncated {
		t.Fatalf("last page: truncated=%v next=%q, want truncated and no cursor", last.Truncated, last.NextCursor)
	}
}
