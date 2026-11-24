package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
)

// API-4 keyset paging for GET /v1/config and GET /v1/flags.
//
// Both lists are read from the environment's latest snapshot and sorted by the
// store on (key, environment, tenant), the global default ("" tenant) before
// the tenant's own row. The cursor is the sort key of the last row returned;
// the next page starts strictly after it, so a row added or removed between
// pages neither repeats nor shifts the rest. The body stays a JSON array; the
// next page is announced in X-Next-Cursor and a Link rel="next" header.
//
// The cursor only positions the read: the tenant filter is applied before
// paging, so a forged cursor can skip rows but never reveal another tenant's.

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// listKey is a list row's sort key.
type listKey struct {
	Key, Environment, Tenant string
}

func (a listKey) less(b listKey) bool {
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	if a.Environment != b.Environment {
		return a.Environment < b.Environment
	}
	return a.Tenant < b.Tenant
}

func encodeListCursor(k listKey) string {
	raw, _ := json.Marshal([3]string{k.Key, k.Environment, k.Tenant})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeListCursor(s string) (listKey, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listKey{}, false
	}
	var parts [3]string
	if err := json.Unmarshal(raw, &parts); err != nil || parts[0] == "" || parts[1] == "" {
		return listKey{}, false
	}
	return listKey{Key: parts[0], Environment: parts[1], Tenant: parts[2]}, true
}

// parseListPage reads ?limit= (1..500, default 100) and ?cursor=.
//
//	400 invalid_limit  -> not an integer in range
//	400 invalid_cursor -> not a cursor this service issued
func parseListPage(w http.ResponseWriter, r *http.Request) (limit int, after *listKey, ok bool) {
	q := r.URL.Query()
	limit = defaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to 500")
			return 0, nil, false
		}
		limit = n
	}
	if v := q.Get("cursor"); v != "" {
		k, good := decodeListCursor(v)
		if !good {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is not one this service issued")
			return 0, nil, false
		}
		after = &k
	}
	return limit, after, true
}

// pageRows returns the rows strictly after the cursor, at most limit of them,
// and the cursor of the next page ("" when this is the last one). rows must be
// sorted by keyOf.
func pageRows[T any](rows []T, keyOf func(T) listKey, limit int, after *listKey) ([]T, string) {
	start := 0
	if after != nil {
		for start < len(rows) && !after.less(keyOf(rows[start])) {
			start++
		}
	}
	rows = rows[start:]
	if len(rows) <= limit {
		return rows, ""
	}
	page := rows[:limit]
	return page, encodeListCursor(keyOf(page[len(page)-1]))
}

// writeNextPage announces the next page, if there is one.
func writeNextPage(w http.ResponseWriter, r *http.Request, next string) {
	if next == "" {
		return
	}
	q := r.URL.Query()
	q.Set("cursor", next)
	w.Header().Set("X-Next-Cursor", next)
	w.Header().Set("Link", "<"+r.URL.Path+"?"+q.Encode()+`>; rel="next"`)
}

func tenantOrEmpty(t *string) string {
	if t == nil {
		return ""
	}
	return *t
}
