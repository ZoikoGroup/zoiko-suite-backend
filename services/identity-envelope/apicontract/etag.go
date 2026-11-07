package apicontract

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func FormatETag(version int64) string {
	return `"v` + strconv.FormatInt(version, 10) + `"`
}

func ParseETag(value string) (int64, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, fmt.Errorf("empty etag")
	}
	if value == "*" {
		return 0, true, nil
	}
	value = strings.TrimPrefix(value, "W/")
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	value = strings.TrimPrefix(value, "v")
	if value == "" {
		return 0, false, fmt.Errorf("malformed etag")
	}
	v, err := strconv.ParseInt(value, 10, 64)
	if err != nil || v < 1 {
		return 0, false, fmt.Errorf("malformed etag")
	}
	return v, false, nil
}

func IfMatchVersion(r *http.Request) (version int64, any bool, present bool, err error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false, false, nil
	}
	version, any, err = ParseETag(raw)
	if err != nil {
		return 0, false, true, err
	}
	return version, any, true, nil
}

func CheckIfMatch(w http.ResponseWriter, r *http.Request, current int64) bool {
	version, any, present, err := IfMatchVersion(r)
	if !present {
		WriteProblem(w, r, ProblemFor(http.StatusPreconditionRequired, "precondition_required",
			"If-Match is required for a mutation of an existing aggregate"))
		return false
	}
	if err != nil {
		WriteProblem(w, r, ProblemFor(http.StatusBadRequest, "invalid_if_match",
			"If-Match must be a single ETag from the latest representation"))
		return false
	}
	if any || version == current {
		return true
	}
	p := ProblemFor(http.StatusPreconditionFailed, "stale_version", "The aggregate has changed since the supplied ETag")
	p.CurrentVersion = &current
	w.Header().Set("ETag", FormatETag(current))
	WriteProblem(w, r, p)
	return false
}

func WriteETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", FormatETag(version))
}

func RequireIfMatch(next http.HandlerFunc, current func(*http.Request) (int64, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := current(r)
		if !ok {
			WriteProblem(w, r, ProblemFor(http.StatusNotFound, "not_found", "The aggregate does not exist"))
			return
		}
		if !CheckIfMatch(w, r, v) {
			return
		}
		next(w, r)
	}
}
