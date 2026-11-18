package recordvault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type call struct {
	method, path string
	hdr          http.Header
	body         map[string]any
}

func fakeVault(t *testing.T, calls *[]call, fail map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		*calls = append(*calls, call{r.Method, r.URL.Path, r.Header.Clone(), b})
		if code := fail[r.Method+" "+r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/documents":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"document_id":"doc-1","current_version":2}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/documents/doc-1/versions":
			_, _ = w.Write([]byte(`[{"document_version_id":"v-old","version":1},{"document_version_id":"v-cur","version":2}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/records":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"record_id":"rec-7"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func decl() Declaration {
	return Declaration{TenantID: "t1", PrincipalID: "alice", CorrelationID: "c1", LegalEntityID: "le-1", Title: "pkg", Package: []byte(`{"a":1}`),
		RecordClass: "COMPLIANCE_EVIDENCE", JurisdictionScope: "GB", RetentionScheduleRef: "ret-1", BusinessContext: "notice n1",
		DeclarationReason: "because", IdempotencyKey: "ncd-notice-n1-v1"}
}

func TestDeclare_StoresThePackageThenDeclaresTheCurrentVersion(t *testing.T) {
	var calls []call
	srv := fakeVault(t, &calls, nil)
	defer srv.Close()
	id, err := NewClient(srv.URL, 0, nil).Declare(context.Background(), decl())
	if err != nil || id != "rec-7" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	doc := calls[0].body
	if raw, _ := base64.StdEncoding.DecodeString(doc["content_base64"].(string)); string(raw) != `{"a":1}` || doc["classification"] != "CONFIDENTIAL" || doc["content_type"] != "application/json" {
		t.Fatalf("the package is stored as JSON, confidential: %+v", doc)
	}
	rec := calls[2].body
	if rec["document_version_id"] != "v-cur" || rec["document_id"] != "doc-1" || rec["record_class"] != "COMPLIANCE_EVIDENCE" || rec["jurisdiction_scope"] != "GB" {
		t.Fatalf("the CURRENT version is the one declared: %+v", rec)
	}
	for _, c := range calls {
		if c.hdr.Get("X-Tenant-Id") != "t1" || c.hdr.Get("X-Principal-Id") != "alice" || c.hdr.Get("Idempotency-Key") == "" {
			t.Fatalf("every call carries tenant, principal and an idempotency key: %s %s %v", c.method, c.path, c.hdr)
		}
	}
	if calls[0].hdr.Get("Idempotency-Key") == calls[2].hdr.Get("Idempotency-Key") {
		t.Fatal("each step needs its own key, derived from the notice's")
	}
}

func TestDeclare_FailuresAreErrorsAndNeverARecordReference(t *testing.T) {
	cases := map[string]struct {
		fail map[string]int
		want error
	}{
		"upload 5xx":     {map[string]int{"POST /v1/documents": 503}, ErrUnavailable},
		"versions 500":   {map[string]int{"GET /v1/documents/doc-1/versions": 500}, ErrUnavailable},
		"declare 400":    {map[string]int{"POST /v1/records": 400}, ErrRejected},
		"declare 403":    {map[string]int{"POST /v1/records": 403}, ErrRejected},
		"declare 5xx":    {map[string]int{"POST /v1/records": 502}, ErrUnavailable},
		"upload 429":     {map[string]int{"POST /v1/documents": 429}, ErrUnavailable},
		"upload refused": {map[string]int{"POST /v1/documents": 422}, ErrRejected},
	}
	for name, tc := range cases {
		var calls []call
		srv := fakeVault(t, &calls, tc.fail)
		id, err := NewClient(srv.URL, 0, nil).Declare(context.Background(), decl())
		srv.Close()
		if id != "" || !errors.Is(err, tc.want) {
			t.Errorf("%s: id=%q err=%v, want %v", name, id, err, tc.want)
		}
	}
	if id, err := NewClient("http://127.0.0.1:1", 0, nil).Declare(context.Background(), decl()); id != "" || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unreachable vault is unavailable: %q %v", id, err)
	}
}
