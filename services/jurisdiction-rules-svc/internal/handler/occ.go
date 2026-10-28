package handler

import (
	"net/http"
	"strconv"
	"strings"
)

// Optimistic concurrency (API standard; GCP §16). A rule's ETag is its
// row_version, which increases on every update (000020). A write that names
// one in If-Match is applied only to that version: 412 precondition_failed if
// the rule has moved on. If-Match is optional, so existing callers are
// unaffected; "*" means "any version".

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func setETag(w http.ResponseWriter, version int64) {
	if version > 0 {
		w.Header().Set("ETag", etag(version))
	}
}

// parseIfMatch returns the asserted row_version, nil when there is none (or
// "*"), and false after writing 400 when the header is not a single version.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (*int64, bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" || v == "*" {
		return nil, true
	}
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		writeError(w, r, http.StatusBadRequest, "invalid_if_match",
			`If-Match must be a single ETag from this service, e.g. "3"`)
		return nil, false
	}
	return &n, true
}

