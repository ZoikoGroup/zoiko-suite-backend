package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// API-4 cursor paging on the admin registers (AZ-1, 10 Nov 2026).
//
// Opt-in, so no caller breaks: a request that sends neither page_size nor
// cursor gets the register exactly as before (an array, or the existing
// wrapper object). One that sends either gets the estate's page shape,
//
//	{"records": [...], "next_cursor": "<opaque>"}   // next_cursor absent on the last page
//
// ordered by the record's id, ascending — a total order on a unique key, so a
// traversal neither repeats nor skips a record when others are added or
// removed between pages. The cursor names the last id served and the query it
// was served for (path, tenant and filters); replaying it against a different
// query is 400 invalid_cursor rather than a silently wrong page.
//
// The registers are tenant-scoped and bounded by what a tenant administers
// (roles, rules, limits, bindings, sessions), so the page is cut from the
// store's tenant read in memory. /v1/access-decisions, the one unbounded
// collection, pages in SQL (keyset, pg_store.go).

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

type pageCursor struct {
	Scope  string `json:"s"`
	LastID string `json:"k"`
}

// pageRequested reports whether the caller opted into cursor paging.
func pageRequested(r *http.Request) bool {
	q := r.URL.Query()
	return q.Has("page_size") || q.Has("cursor")
}

// pageScope identifies the query a cursor belongs to: path, tenant and every
// query parameter except the paging ones, in a canonical order.
func pageScope(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		if k != "cursor" && k != "page_size" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.URL.Path)
	b.WriteByte('|')
	b.WriteString(r.Header.Get("X-Tenant-Id"))
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.Join(q[k], ","))
	}
	return b.String()
}

// pageParams is a parsed paging request.
type pageParams struct {
	size  int
	after string
	scope string
}

// parsePage reads page_size and cursor. Writes the 400 and returns false on a
// bad value.
func parsePage(w http.ResponseWriter, r *http.Request) (pageParams, bool) {
	q := r.URL.Query()
	p := pageParams{size: defaultPageSize, scope: pageScope(r)}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageSize {
			writeError(w, r, http.StatusBadRequest, "invalid_page_size",
				"page_size must be an integer from 1 to "+strconv.Itoa(maxPageSize), map[string]any{"field": "page_size"})
			return p, false
		}
		p.size = n
	}
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		c, ok := decodePageCursor(raw)
		if !ok || c.Scope != p.scope {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor",
				"cursor is not one this service issued for this query — start from the first page", map[string]any{"field": "cursor"})
			return p, false
		}
		p.after = c.LastID
	}
	return p, true
}

// pageResponse is the estate's page shape. Truncated is set on the last page
// when the store's own row cap was reached, so a caller is told the register
// holds more than it could be given rather than reading the end as complete.
type pageResponse[T any] struct {
	Records    []T    `json:"records"`
	NextCursor string `json:"next_cursor,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// writePageOf writes page (already the rows after p.after, in id order, with at
// most p.size+1 rows: the extra one only says another page exists).
func writePageOf[T any](w http.ResponseWriter, p pageParams, page []T, id func(T) string, truncated bool) {
	resp := pageResponse[T]{Records: page}
	if len(page) > p.size {
		resp.Records = page[:p.size]
		resp.NextCursor = encodePageCursor(pageCursor{Scope: p.scope, LastID: id(resp.Records[p.size-1])})
	} else {
		resp.Truncated = truncated
	}
	if resp.Records == nil {
		resp.Records = []T{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// writePage pages a register the store returned whole (capped at storeCap
// rows). Writes the 400 for a bad page_size or cursor.
func writePage[T any](w http.ResponseWriter, r *http.Request, items []T, id func(T) string, storeCap int) {
	p, ok := parsePage(w, r)
	if !ok {
		return
	}
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return id(sorted[i]) < id(sorted[j]) })
	start := 0
	if p.after != "" {
		start = sort.Search(len(sorted), func(i int) bool { return id(sorted[i]) > p.after })
	}
	end := start + p.size + 1
	if end > len(sorted) {
		end = len(sorted)
	}
	writePageOf(w, p, sorted[start:end], id, storeCap > 0 && len(items) >= storeCap)
}

func encodePageCursor(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageCursor(raw string) (pageCursor, bool) {
	var c pageCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.LastID == "" {
		return pageCursor{}, false
	}
	return c, true
}
