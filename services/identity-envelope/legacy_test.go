package idenv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteLegacyErrorRendersProblemAndKeepsTheCode(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	r.Header.Set(HeaderXRequestID, "req_0123456789abcdef")
	r.Header.Set("X-Correlation-ID", "corr-1")
	w := httptest.NewRecorder()

	ok := WriteLegacyError(w, r, http.StatusConflict, map[string]string{
		"error": "version_mismatch", "detail": "expected 3", "obligation_id": "o-1"})
	if !ok {
		t.Fatal("a legacy error body was not converted")
	}
	if ct := w.Header().Get("Content-Type"); ct != ContentTypeProblem {
		t.Fatalf("Content-Type = %q, want %q", ct, ContentTypeProblem)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type":           ProblemBase + "version-mismatch",
		"title":          "Conflict",
		"status":         float64(409),
		"detail":         "expected 3",
		"code":           "version_mismatch",
		"error":          "version_mismatch",
		"error_class":    "CONCURRENCY_CONFLICT",
		"obligation_id":  "o-1",
		"request_id":     "req_0123456789abcdef",
		"correlation_id": "corr-1",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d", w.Code)
	}
}

func TestWriteLegacyErrorKeepsAnExistingCodeAndMessage(t *testing.T) {
	w := httptest.NewRecorder()
	type legacy struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if !WriteLegacyError(w, nil, http.StatusNotFound, legacy{Error: "not_found", Code: "POLICY_NOT_EFFECTIVE", Message: "none in force"}) {
		t.Fatal("struct error body was not converted")
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "POLICY_NOT_EFFECTIVE" || body["error"] != "not_found" || body["detail"] != "none in force" {
		t.Fatalf("body = %v", body)
	}
	if body["error_class"] != "POLICY_NOT_EFFECTIVE" {
		t.Fatalf("error_class = %v, want the class the code names", body["error_class"])
	}
}

func TestWriteLegacyErrorLeavesOtherBodiesAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		v      any
	}{
		{"success body", http.StatusOK, map[string]string{"error": "x"}},
		{"no code", http.StatusBadRequest, map[string]string{"field": "x"}},
		{"array", http.StatusBadRequest, []string{"a"}},
	} {
		w := httptest.NewRecorder()
		if WriteLegacyError(w, nil, tc.status, tc.v) {
			t.Errorf("%s: converted, want untouched", tc.name)
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s: wrote %q", tc.name, w.Body.String())
		}
	}
}

func TestErrorClassTable(t *testing.T) {
	cases := map[string]string{
		"forbidden":                "AUTHORIZATION_DENIED",
		"IDEMPOTENCY_KEY_CONFLICT": "IDEMPOTENCY_MISMATCH",
		"tenant_missing":           "CONTEXT_UNRESOLVED",
		"self_approval_forbidden":  "SOD_CONFLICT",
		"missing_field":            "",
	}
	for code, want := range cases {
		if got := ErrorClass(code); got != want {
			t.Errorf("ErrorClass(%q) = %q, want %q", code, got, want)
		}
	}
	RegisterErrorClasses(map[string]string{"Obligation_Conflict": "CONCURRENCY_CONFLICT"})
	if got := ErrorClass("obligation_conflict"); got != "CONCURRENCY_CONFLICT" {
		t.Errorf("registered class = %q", got)
	}
}

func TestRetryableOnUnavailable(t *testing.T) {
	p, ok := ProblemFromLegacy(http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	if !ok || !p.Retryable {
		t.Fatalf("503 problem retryable = %v (ok %v)", p.Retryable, ok)
	}
	p, _ = ProblemFromLegacy(http.StatusBadRequest, map[string]string{"error": "missing_field"})
	if p.Retryable {
		t.Fatal("a 400 is not retryable")
	}
}
