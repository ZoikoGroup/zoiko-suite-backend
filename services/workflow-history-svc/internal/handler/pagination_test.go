package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/workflow-history-svc/internal/authz"
	"zoiko.io/workflow-history-svc/internal/store"
)

// Deterministic ordering and cursor pagination (ZS-API-001 §15.1, §15.2).

var t0 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// eventsAt builds n events for one instance that ALL share the same
// recorded_at, inserted in descending event_id order so insertion order is the
// opposite of the required order.
func eventsAt(tenant, entity, instance string, n int, at time.Time) []store.WorkflowHistoryEvent {
	out := make([]store.WorkflowHistoryEvent, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, store.WorkflowHistoryEvent{
			EventID: fmt.Sprintf("e%03d", i), WorkflowInstanceID: instance, EventType: "approval.granted",
			TenantID: tenant, LegalEntityID: entity, Payload: json.RawMessage(`{}`), RecordedAt: at,
		})
	}
	return out
}

func get(t *testing.T, h http.Handler, path, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type pageBody struct {
	Data []struct {
		EventID string `json:"event_id"`
	} `json:"data"`
	Meta struct {
		PageSize   int    `json:"page_size"`
		NextCursor string `json:"next_cursor"`
	} `json:"meta"`
}

func decodePage(t *testing.T, w *httptest.ResponseRecorder) pageBody {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var p pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
	return p
}

func eventIDs(p pageBody) []string {
	out := []string{}
	for _, e := range p.Data {
		out = append(out, e.EventID)
	}
	return out
}

func arrayIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var arr []struct {
		EventID string `json:"event_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &arr), "the response is a bare array")
	out := []string{}
	for _, e := range arr {
		out = append(out, e.EventID)
	}
	return out
}

const window = "legal_entity_id=le1&from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z"

// ── ordering ────────────────────────────────────────────────────────────────

func TestOrdering_EqualTimestamps_AreOrderedByEventID_OnBothRoutes(t *testing.T) {
	r := newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 5, t0)}, &stubAuthz{})
	want := []string{"e000", "e001", "e002", "e003", "e004"}

	assert.Equal(t, want, arrayIDs(t, get(t, r, "/v1/workflows/wf-1/history", "t1")),
		"instance route: one stable order for rows sharing recorded_at")
	assert.Equal(t, want, arrayIDs(t, get(t, r, "/v1/workflows/history?"+window, "t1")),
		"cross-workflow route: same order")
}

func TestOrdering_RecordedAtStillComesFirst(t *testing.T) {
	evs := eventsAt("t1", "le1", "wf-1", 2, t0.Add(time.Hour)) // e000, e001 later
	evs = append(evs, store.WorkflowHistoryEvent{EventID: "zzz", WorkflowInstanceID: "wf-1", EventType: "workflow.started",
		TenantID: "t1", LegalEntityID: "le1", Payload: json.RawMessage(`{}`), RecordedAt: t0}) // earlier, but its id sorts last
	r := newRouter(&fakeReadStore{events: evs}, &stubAuthz{})
	p := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=10", "t1"))
	assert.Equal(t, []string{"zzz", "e000", "e001"}, eventIDs(p), "chronology wins; event_id only breaks ties")
}

// ── pagination ──────────────────────────────────────────────────────────────

func TestPagination_Instance_WalksEveryRowOnce_AcrossTies(t *testing.T) {
	// 120 rows, all with the same recorded_at: every page boundary falls inside
	// a run of ties.
	r := newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 120, t0)}, &stubAuthz{})

	var seen []string
	var sizes []int
	path := "/v1/workflows/wf-1/history?page_size=50"
	for i := 0; i < 10; i++ {
		p := decodePage(t, get(t, r, path, "t1"))
		assert.Equal(t, 50, p.Meta.PageSize)
		sizes = append(sizes, len(p.Data))
		seen = append(seen, eventIDs(p)...)
		if p.Meta.NextCursor == "" {
			break
		}
		path = "/v1/workflows/wf-1/history?page_size=50&cursor=" + p.Meta.NextCursor
	}
	assert.Equal(t, []int{50, 50, 20}, sizes)
	require.Len(t, seen, 120)
	for i, id := range seen {
		assert.Equal(t, fmt.Sprintf("e%03d", i), id, "no skips, no repeats, stable order across pages")
	}
}

func TestPagination_DefaultPageSizeIs50_WhenOnlyCursorIsGiven(t *testing.T) {
	r := newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 120, t0)}, &stubAuthz{})
	p := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=1", "t1"))
	require.NotEmpty(t, p.Meta.NextCursor)
	p = decodePage(t, get(t, r, "/v1/workflows/wf-1/history?cursor="+p.Meta.NextCursor, "t1"))
	assert.Equal(t, 50, p.Meta.PageSize)
	assert.Len(t, p.Data, 50)
}

func TestPagination_MaxPageSize200_AndInvalidInputIs400(t *testing.T) {
	r := newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 250, t0)}, &stubAuthz{})
	p := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=200", "t1"))
	assert.Len(t, p.Data, 200)
	assert.NotEmpty(t, p.Meta.NextCursor)

	for _, bad := range []string{"201", "0", "-1", "abc", ""} {
		w := get(t, r, "/v1/workflows/wf-1/history?page_size="+bad, "t1")
		assert.Equal(t, http.StatusBadRequest, w.Code, "page_size=%q", bad)
		assert.Contains(t, w.Body.String(), "page_size")
	}
}

func TestPagination_InvalidOrForeignCursor_Is400(t *testing.T) {
	fake := &fakeReadStore{events: append(eventsAt("t1", "le1", "wf-1", 5, t0), eventsAt("t1", "le1", "wf-2", 5, t0)...)}
	r := newRouter(fake, &stubAuthz{})

	for _, bad := range []string{"not-base64!!", "e30", "bm90LWpzb24"} { // garbage, {}, "not-json"
		w := get(t, r, "/v1/workflows/wf-1/history?cursor="+bad, "t1")
		assert.Equal(t, http.StatusBadRequest, w.Code, "cursor=%q", bad)
	}

	// A real cursor issued for wf-1 is refused for wf-2, and in another tenant.
	p := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=2", "t1"))
	require.NotEmpty(t, p.Meta.NextCursor)
	assert.Equal(t, http.StatusBadRequest, get(t, r, "/v1/workflows/wf-2/history?cursor="+p.Meta.NextCursor, "t1").Code)
	assert.Equal(t, http.StatusBadRequest, get(t, r, "/v1/workflows/wf-1/history?cursor="+p.Meta.NextCursor, "t2").Code,
		"a cursor cannot be replayed in another tenant")
}

func TestPagination_EmptyPageAfterTheEnd_Is200_FirstPageOfUnknownInstanceIs404(t *testing.T) {
	fake := &fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 3, t0)}
	r := newRouter(fake, &stubAuthz{})

	full := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=3", "t1"))
	assert.Empty(t, full.Meta.NextCursor, "exactly one full page: no next cursor")

	// Walk to a cursor positioned on e001, then make e002 disappear: the page
	// after the end is empty, not a 404.
	p1 := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=1", "t1"))
	p2 := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=1&cursor="+p1.Meta.NextCursor, "t1"))
	require.Equal(t, []string{"e001"}, eventIDs(p2))
	require.NotEmpty(t, p2.Meta.NextCursor)
	var kept []store.WorkflowHistoryEvent
	for _, e := range fake.events {
		if e.EventID != "e002" {
			kept = append(kept, e)
		}
	}
	fake.events = kept
	p3 := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=1&cursor="+p2.Meta.NextCursor, "t1"))
	assert.Empty(t, p3.Data)
	assert.Empty(t, p3.Meta.NextCursor)

	assert.Equal(t, http.StatusNotFound, get(t, r, "/v1/workflows/missing/history?page_size=5", "t1").Code)
}

type authzFunc func() error

func (f authzFunc) CheckAllowed(_ context.Context, _, _, _ string) error { return f() }

func TestPagination_TenantIsolationAndAuthorization_ApplyToEveryPage(t *testing.T) {
	evs := append(eventsAt("t1", "le1", "wf-1", 5, t0), eventsAt("t2", "le2", "wf-1", 5, t0)...)
	fake := &fakeReadStore{events: evs}

	r := newRouter(fake, &stubAuthz{})
	p := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=3", "t2"))
	assert.Len(t, p.Data, 3)
	assert.Equal(t, "t2", fake.gotTenantID, "every page is scoped to the verified tenant")

	// Authorization is evaluated on page 2 as well as page 1.
	calls := 0
	az := authzFunc(func() error {
		calls++
		if calls >= 2 {
			return authz.ErrAuthorizationDenied
		}
		return nil
	})
	r = newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 5, t0)}, az)
	first := decodePage(t, get(t, r, "/v1/workflows/wf-1/history?page_size=2", "t1"))
	w := get(t, r, "/v1/workflows/wf-1/history?page_size=2&cursor="+first.Meta.NextCursor, "t1")
	assert.Equal(t, http.StatusForbidden, w.Code, "denial on a later page is honoured")
	assert.NotContains(t, w.Body.String(), "e002")

	r = newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 5, t0)}, authzFunc(func() error { return errors.New("down") }))
	assert.Equal(t, http.StatusServiceUnavailable, get(t, r, "/v1/workflows/wf-1/history?page_size=2", "t1").Code)
}

func TestPagination_CrossWorkflow_WalksTheWindow_AndRejectsAForeignCursor(t *testing.T) {
	var evs []store.WorkflowHistoryEvent
	for i := 0; i < 7; i++ {
		evs = append(evs, store.WorkflowHistoryEvent{EventID: fmt.Sprintf("e%02d", i), WorkflowInstanceID: fmt.Sprintf("wf-%d", i%3),
			EventType: "approval.granted", TenantID: "t1", LegalEntityID: "le1", Payload: json.RawMessage(`{}`), RecordedAt: t0})
	}
	r := newRouter(&fakeReadStore{events: evs}, &stubAuthz{})

	var seen []string
	path := "/v1/workflows/history?" + window + "&page_size=3"
	for i := 0; i < 5; i++ {
		p := decodePage(t, get(t, r, path, "t1"))
		seen = append(seen, eventIDs(p)...)
		if p.Meta.NextCursor == "" {
			break
		}
		path = "/v1/workflows/history?" + window + "&page_size=3&cursor=" + p.Meta.NextCursor
	}
	assert.Equal(t, []string{"e00", "e01", "e02", "e03", "e04", "e05", "e06"}, seen)

	p := decodePage(t, get(t, r, "/v1/workflows/history?"+window+"&page_size=3", "t1"))
	other := "legal_entity_id=le1&from=2026-07-01T00:00:00Z&to=2026-07-03T00:00:00Z"
	assert.Equal(t, http.StatusBadRequest, get(t, r, "/v1/workflows/history?"+other+"&cursor="+p.Meta.NextCursor, "t1").Code,
		"a cursor is bound to its window")
}

// ── compatibility with evidence-manifest-svc ────────────────────────────────

// evidence-manifest-svc's aggregator sends neither page_size nor cursor and
// decodes a bare JSON array. That exact request must keep returning the whole
// list, even past the paged default of 50 — a silent cut-off would produce a
// manifest that looks complete and is not.
func TestCompat_NoPaginationParameters_ReturnsEveryRowAsABareArray(t *testing.T) {
	r := newRouter(&fakeReadStore{events: eventsAt("t1", "le1", "wf-1", 120, t0)}, &stubAuthz{})
	for _, path := range []string{
		"/v1/workflows/wf-1/history",
		"/v1/workflows/history?" + window,
	} {
		ids := arrayIDs(t, get(t, r, path, "t1"))
		assert.Len(t, ids, 120, path)
		assert.Equal(t, "e000", ids[0])
	}
}
