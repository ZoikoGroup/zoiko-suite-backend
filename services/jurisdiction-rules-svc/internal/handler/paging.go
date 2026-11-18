package handler

// API-4 cursor paging for the list routes (identity-envelope/apicontract).
//
// A list answers in one of two shapes, chosen by the request:
//
//   - Paged (API-4), when the request names page_size, cursor or any
//     filter[name]: {"items": [...], "next_cursor": "...", "page_size": n}.
//     page_size defaults to 50, at most 200. next_cursor is an HMAC-signed
//     token bound to the caller's tenant ("platform" for a caller with none),
//     the route and the filter set, so a tampered cursor, or one replayed by
//     another tenant or against another query, is 400 invalid_cursor rather
//     than a page of something else. A bare legacy filter in a paged request
//     is 400 unknown_filter naming the filter[...] form to use.
//   - Legacy, when it names none of them: the bare JSON array the route has
//     always answered, with its original limit/offset and bare filters, so the
//     services and scripts that read these routes today keep working.

import (
	"crypto/rand"
	"net/http"
	"strings"

	"go.uber.org/zap"
	"zoiko.io/identity-envelope/apicontract"
)

// listResponse is the API-4 shape of every paged list.
type listResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	PageSize   int    `json:"page_size"`
}

// pageReq is a parsed list request.
type pageReq struct {
	paged     bool
	size      int // page size (paged) or limit (legacy; 0 = the route's default)
	offset    int
	filters   map[string]string
	route     string
	filterKey string
	tenant    string
}

// filter returns the value of a filter (or its legacy bare parameter).
func (p *pageReq) filter(name string) string { return p.filters[name] }

// fetch is how many rows to read: one more than the page in paged mode, to
// learn whether another page exists.
func (p *pageReq) fetch() int {
	if p.paged {
		return p.size + 1
	}
	return p.size
}

// WithCursors sets the key next_cursor tokens are signed with. New sets a
// random per-process key; main sets the configured one so cursors survive a
// restart and work on every replica.
func (h *Handler) WithCursors(c *apicontract.CursorSigner) *Handler {
	h.cursors = c
	return h
}

func randomCursorSigner() *apicontract.CursorSigner {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, _ := apicontract.NewCursorSigner(key)
	return c
}

func isPagedRequest(r *http.Request) bool {
	q := r.URL.Query()
	if q.Has("page_size") || q.Has("cursor") {
		return true
	}
	for k := range q {
		if strings.HasPrefix(k, "filter[") {
			return true
		}
	}
	return false
}

// beginList parses a list request. filters are the filter names the route
// supports; legacy maps each bare query parameter the route took before
// API-4 to the filter it means. It writes the 400 and returns false when the
// request is malformed.
func (h *Handler) beginList(w http.ResponseWriter, r *http.Request, filters []string, legacy map[string]string) (*pageReq, bool) {
	q := r.URL.Query()
	p := &pageReq{filters: map[string]string{}, route: apicontract.ScopedRoute(r.URL.Path), tenant: requestTenant(r)}
	if p.tenant == "" {
		p.tenant = "platform"
	}
	if !isPagedRequest(r) {
		limit, offset, ok := h.parsePaging(w, r, q)
		if !ok {
			return nil, false
		}
		p.size, p.offset = limit, offset
		for bare, name := range legacy {
			if v := q.Get(bare); v != "" {
				p.filters[name] = v
			}
		}
		return p, true
	}

	for bare, name := range legacy {
		if q.Has(bare) {
			writeError(w, r, http.StatusBadRequest, "unknown_filter",
				"use filter["+name+"]="+q.Get(bare)+": a paged list reads filters as filter[name]")
			return nil, false
		}
	}
	if q.Has("limit") || q.Has("offset") {
		writeError(w, r, http.StatusBadRequest, "invalid_paging",
			"a paged list takes page_size and cursor, not limit and offset")
		return nil, false
	}
	page, prob := apicontract.ParsePage(r, filters, nil, "")
	if prob != nil {
		apicontract.WriteProblem(w, r, *prob)
		return nil, false
	}
	p.paged, p.size, p.filterKey = true, page.PageSize, page.FilterKey()
	for k, v := range page.Filters {
		p.filters[k] = v
	}
	if page.Cursor != "" {
		c, err := h.cursors.Verify(page.Cursor, p.tenant, p.route, p.filterKey, page.SortKey())
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor",
				"the cursor is malformed, was tampered with, or belongs to another tenant or query")
			return nil, false
		}
		p.offset = c.Offset
	}
	return p, true
}

// finishList writes a page read with p.fetch() rows from p.offset.
func finishList[T any](h *Handler, w http.ResponseWriter, r *http.Request, p *pageReq, items []T) {
	if items == nil {
		items = []T{}
	}
	if !p.paged {
		writeJSON(w, http.StatusOK, items)
		return
	}
	resp := listResponse[T]{Items: items, PageSize: p.size}
	if len(items) > p.size {
		resp.Items = items[:p.size]
		next, err := h.cursors.Sign(apicontract.Cursor{TenantID: p.tenant, Route: p.route, Filter: p.filterKey,
			Offset: p.offset + p.size})
		if err != nil {
			h.log.Error("sign next cursor failed", zap.Error(err))
			writeError(w, r, http.StatusInternalServerError, "list_paging_failed", "")
			return
		}
		resp.NextCursor = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// pageOf pages a list the store returns whole (the small registries: regimes,
// rings, regions, keys, calendars): the rows from p.offset, p.fetch() of them.
func pageOf[T any](p *pageReq, all []T) []T {
	if p.offset >= len(all) {
		return []T{}
	}
	rest := all[p.offset:]
	if n := p.fetch(); n > 0 && len(rest) > n {
		rest = rest[:n]
	}
	return rest
}

// finishListAs is finishList for a route whose legacy answer wraps the list
// in an object under key (e.g. {"sources": [...]}).
func finishListAs[T any](h *Handler, w http.ResponseWriter, r *http.Request, p *pageReq, key string, items []T) {
	if !p.paged {
		if items == nil {
			items = []T{}
		}
		writeJSON(w, http.StatusOK, map[string]any{key: items})
		return
	}
	finishList(h, w, r, p, items)
}
