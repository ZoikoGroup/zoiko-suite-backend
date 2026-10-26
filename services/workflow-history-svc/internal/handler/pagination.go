package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"zoiko.io/workflow-history-svc/internal/store"
)

// Cursor pagination per ZS-API-001 §15.2: an opaque cursor, default page_size
// 50, maximum 200, over a deterministic total order (recorded_at, event_id —
// §15.1 "sort must have a deterministic tie-breaker").
//
// Pagination is OPT-IN: it engages only when the request carries `page_size`
// or `cursor`. A request with neither keeps the original response — a bare JSON
// array of every matching event. That is a deliberate compatibility rule, not
// an omission: evidence-manifest-svc's aggregator calls both routes with no
// pagination parameters and decodes a bare array, and a default page of 50
// would silently truncate an evidence manifest (a partial manifest that looks
// complete). See the service README for the exact impact.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// pageMeta is the `meta` block of a paged response.
type pageMeta struct {
	PageSize   int    `json:"page_size"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// pagedResponse is the response of a paged request: ZS-API-001 §7's `data` /
// `meta` envelope, with the continuation cursor in meta. next_cursor is absent
// on the last page.
type pagedResponse struct {
	Data []historyEventResponse `json:"data"`
	Meta pageMeta               `json:"meta"`
}

// pageRequest is the parsed pagination input of one request.
type pageRequest struct {
	Paged bool
	Size  int
	After *store.PageCursor
}

var errBadPagination = errors.New("invalid pagination")

// cursorBody is the decoded content of a cursor.
type cursorBody struct {
	V int    `json:"v"`
	S string `json:"s"` // scope: binds the cursor to the query it was issued for
	T string `json:"t"` // recorded_at of the last row, RFC 3339 nano UTC
	E string `json:"e"` // event_id of the last row
}

// instanceScope and windowScope identify the query a cursor belongs to, so a
// cursor issued for one tenant/instance/entity/window cannot be replayed against
// another. Tenant is part of the scope, and every page is re-scoped and
// re-authorized from the request anyway; the scope only stops accidental or
// hostile cursor reuse from producing a confusing answer.
func instanceScope(tenantID, instanceID string) string {
	return scopeHash("instance", tenantID, instanceID)
}

func windowScope(tenantID, legalEntityID string, from, to time.Time) string {
	return scopeHash("window", tenantID, legalEntityID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
}

func scopeHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func encodeCursor(scope string, e store.WorkflowHistoryEvent) string {
	b, _ := json.Marshal(cursorBody{V: 1, S: scope, T: e.RecordedAt.UTC().Format(time.RFC3339Nano), E: e.EventID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw, scope string) (*store.PageCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errBadPagination
	}
	var c cursorBody
	if err := json.Unmarshal(b, &c); err != nil || c.V != 1 || c.S != scope || c.E == "" {
		return nil, errBadPagination
	}
	t, err := time.Parse(time.RFC3339Nano, c.T)
	if err != nil {
		return nil, errBadPagination
	}
	return &store.PageCursor{RecordedAt: t, EventID: c.E}, nil
}

// parsePage reads page_size and cursor. It writes the 400 itself and returns
// false when the input is invalid, using the service's existing {error} body.
func parsePage(w http.ResponseWriter, r *http.Request, scope string) (pageRequest, bool) {
	q := r.URL.Query()
	rawSize, hasSize := q["page_size"]
	rawCursor, hasCursor := q["cursor"]
	if !hasSize && !hasCursor {
		return pageRequest{}, true
	}
	p := pageRequest{Paged: true, Size: defaultPageSize}
	if hasSize {
		n, err := strconv.Atoi(rawSize[0])
		if err != nil || n < 1 || n > maxPageSize {
			writeError(w, http.StatusBadRequest, "page_size must be an integer between 1 and "+strconv.Itoa(maxPageSize))
			return pageRequest{}, false
		}
		p.Size = n
	}
	if hasCursor {
		after, err := decodeCursor(rawCursor[0], scope)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor is invalid or was not issued for this query")
			return pageRequest{}, false
		}
		p.After = after
	}
	return p, true
}

// pageOut converts a fetched page (fetched with limit size+1) into the response:
// it trims the look-ahead row and, if there was one, sets next_cursor from the
// last returned row.
func pageOut(events []store.WorkflowHistoryEvent, p pageRequest, scope string) pagedResponse {
	resp := pagedResponse{Data: []historyEventResponse{}, Meta: pageMeta{PageSize: p.Size}}
	more := len(events) > p.Size
	if more {
		events = events[:p.Size]
	}
	for _, e := range events {
		resp.Data = append(resp.Data, toResponse(e))
	}
	if more {
		resp.Meta.NextCursor = encodeCursor(scope, events[len(events)-1])
	}
	return resp
}
