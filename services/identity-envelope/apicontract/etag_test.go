package apicontract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFormatAndParseETag(t *testing.T) {
	if got := FormatETag(41); got != `"v41"` {
		t.Fatalf("etag = %q", got)
	}
	for _, raw := range []string{`"v41"`, `W/"v41"`, `41`, `v41`} {
		v, any, err := ParseETag(raw)
		if err != nil || any || v != 41 {
			t.Fatalf("parse %q = %d, %v, %v", raw, v, any, err)
		}
	}
	if v, any, err := ParseETag("*"); err != nil || !any || v != 0 {
		t.Fatalf("parse * = %d, %v, %v", v, any, err)
	}
	for _, raw := range []string{"", `"v0"`, `"v-1"`, `"vabc"`, `""`} {
		if _, _, err := ParseETag(raw); err == nil {
			t.Fatalf("parse %q unexpectedly succeeded", raw)
		}
	}
}

func TestCheckIfMatchRequiresHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/v1/invoices/1", nil)
	if CheckIfMatch(rec, req, 7) {
		t.Fatal("accepted a request without If-Match")
	}
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d", rec.Code)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p["code"] != "precondition_required" {
		t.Fatalf("code = %v", p["code"])
	}
}

func TestCheckIfMatchRejectsStaleVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/v1/invoices/1", nil)
	req.Header.Set("If-Match", FormatETag(6))
	if CheckIfMatch(rec, req, 7) {
		t.Fatal("accepted a stale ETag")
	}
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != FormatETag(7) {
		t.Fatalf("response etag = %q", got)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p["code"] != "stale_version" || p["current_version"] != float64(7) {
		t.Fatalf("problem = %v", p)
	}
}

func TestCheckIfMatchAcceptsCurrentAndWildcard(t *testing.T) {
	for _, header := range []string{FormatETag(7), "*"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PATCH", "/v1/invoices/1", nil)
		req.Header.Set("If-Match", header)
		if !CheckIfMatch(rec, req, 7) {
			t.Fatalf("rejected %q: %s", header, rec.Body.String())
		}
	}
}

func TestCheckIfMatchRejectsMalformed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/v1/invoices/1", nil)
	req.Header.Set("If-Match", `"vabc"`)
	if CheckIfMatch(rec, req, 7) {
		t.Fatal("accepted a malformed If-Match")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
