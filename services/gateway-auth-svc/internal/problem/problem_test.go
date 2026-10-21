package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

func TestWriteShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Spec{Code: CodeRateLimited, Status: 429, Detail: "slow down", Instance: "/v1/x",
		Retryable: true, RetryAfter: 3, CorrelationID: "c-1", DecisionID: "d-1"})
	if rec.Code != 429 || rec.Header().Get("Content-Type") != ContentType || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "urn:zoikosuite:problem:INT-RATE-007" || p.Title != "Rate limited" || !p.Retryable ||
		p.Instance != "/v1/x" || p.CorrelationID != "c-1" || p.DecisionID != "d-1" || p.Status != 429 {
		t.Fatalf("%+v", p)
	}
}

func TestNoRetryAfterUnlessAdvised(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Spec{Code: CodeAuthDenied, Status: http.StatusUnauthorized})
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("no Retry-After was advised")
	}
}

func TestUnknownCodeFallsBackToStatusText(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Spec{Code: "INT-X-999", Status: 500})
	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Title != "Internal Server Error" {
		t.Fatalf("title = %q", p.Title)
	}
}

// The canonical taxonomy is zoiko.io/eventing/integration/errors.go. Every code
// and title this package carries must match it exactly, so the gateway cannot
// answer with a code the platform does not define or reword one it does.
func TestCodesAndTitlesMatchCanonicalTaxonomy(t *testing.T) {
	src, err := os.ReadFile("../../../eventing/integration/errors.go")
	if err != nil {
		t.Skipf("canonical taxonomy not present in this checkout: %v", err)
	}
	codeRe := regexp.MustCompile(`Code\w+\s+Code = "(INT-[A-Z]+-\d{3})"`)
	titleRe := regexp.MustCompile(`Code\w+:\s*\{"([^"]+)",`)
	codes := codeRe.FindAllStringSubmatch(string(src), -1)
	titles := titleRe.FindAllStringSubmatch(string(src), -1)
	if len(codes) != 16 || len(titles) != 16 {
		t.Fatalf("expected 16 canonical codes and titles, found %d and %d", len(codes), len(titles))
	}
	canon := map[Code]string{}
	for i := range codes {
		canon[Code(codes[i][1])] = titles[i][1]
	}
	for code, title := range Titles {
		want, ok := canon[code]
		if !ok {
			t.Errorf("%s is not a canonical code", code)
			continue
		}
		if title != want {
			t.Errorf("%s title %q, canonical %q", code, title, want)
		}
	}
}
