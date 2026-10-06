package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/access-control-svc/internal/domain"
)

// fakeSoDValidate serves POST /v1/sod/validate the way authorization-svc does
// (internal/handler/validation.go): candidate_actions is mandatory (400
// missing_field otherwise), principal and tenant headers are mandatory (401),
// and the verdict is ALWAYS a 200 — there is no 409. Any other route is 404.
// A client written against a different contract fails here, which is the
// point: the previous client sent no candidate_actions and was only ever
// tested against a stub that accepted anything.
func fakeSoDValidate(t *testing.T, verdict func(candidates []string) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sod/validate" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Principal-Id") == "" || r.Header.Get("X-Tenant-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var candidates []string
		_ = json.Unmarshal(body["candidate_actions"], &candidates)
		if len(candidates) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing_field","field":"candidate_actions"}`))
			return
		}
		if _, has := body["principal_id"]; has {
			t.Errorf("a role definition assigns nobody; principal_id must not be sent")
		}
		var tenant string
		_ = json.Unmarshal(body["tenant_id"], &tenant)
		if tenant != r.Header.Get("X-Tenant-Id") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(verdict(candidates)))
	}))
}

func sodReq(actions ...string) domain.SoDCheckRequest {
	return domain.SoDCheckRequest{TenantID: "t-1", CallerID: "p-1", CorrelationID: "c-1", CandidateActions: actions}
}

func TestSoDClient_Contract(t *testing.T) {
	srv := fakeSoDValidate(t, func(c []string) string {
		if len(c) == 2 {
			return `{"conflict_free":false,"conflicts":[{"candidate_action":"B","conflicts_with":"A","source":"candidate"}]}`
		}
		return `{"conflict_free":true,"conflicts":[]}`
	})
	defer srv.Close()
	c := NewSoDClient(srv.URL)

	if err := c.CheckConflict(context.Background(), sodReq("A")); err != nil {
		t.Fatalf("conflict-free set: %v", err)
	}

	err := c.CheckConflict(context.Background(), sodReq("A", "B"))
	if !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("a 200 conflict_free:false verdict is a conflict, got %v", err)
	}
	var ce *domain.SoDConflictError
	if !errors.As(err, &ce) || len(ce.Conflicts) != 1 || ce.Conflicts[0].ConflictsWith != "A" {
		t.Fatalf("the conflicting pair is lost: %v", err)
	}
	if errors.Is(err, domain.ErrSoDUnavailable) {
		t.Fatal("a conflict is not an outage")
	}
}

func TestSoDClient_FailsClosedWithoutAVerdict(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"400 malformed request": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
		"409 (not in contract)": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) },
		"503":                   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"200 without verdict":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) },
		"200 not JSON":          func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>`)) },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		err := NewSoDClient(srv.URL).CheckConflict(context.Background(), sodReq("A"))
		srv.Close()
		if !errors.Is(err, domain.ErrSoDUnavailable) || errors.Is(err, domain.ErrSoDConflict) {
			t.Errorf("%s: want ErrSoDUnavailable only, got %v", name, err)
		}
	}
	err := NewSoDClient("http://127.0.0.1:1").CheckConflict(context.Background(), sodReq("A"))
	if !errors.Is(err, domain.ErrSoDUnavailable) {
		t.Errorf("unreachable: want ErrSoDUnavailable, got %v", err)
	}
}

func TestAuthzAdmin_403IsARefusalNotAnOutage(t *testing.T) {
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"forbidden","required_action":"iam.role.manage"}`))
	}))
	defer srv.Close()
	c := NewAuthzAdminClient(srv.URL)
	s := Scope{PrincipalID: "p", TenantID: "t", LegalEntityID: "le", CorrelationID: "c"}

	err := c.CreateRole(context.Background(), "r", "CODE", "Name", "TENANT", s)
	if !errors.Is(err, domain.ErrProvisioningForbidden) || !strings.Contains(err.Error(), "iam.role.manage") {
		t.Fatalf("403 must be ErrProvisioningForbidden and keep the reason, got %v", err)
	}
	err = c.SetPermissionBundleActive(context.Background(), "r", "B", false, s)
	if !errors.Is(err, domain.ErrProvisioningForbidden) {
		t.Fatalf("403 on the bundle list read: got %v", err)
	}

	status = http.StatusInternalServerError
	err = c.CreateRole(context.Background(), "r", "CODE", "Name", "TENANT", s)
	if err == nil || errors.Is(err, domain.ErrProvisioningForbidden) {
		t.Fatalf("500 is an outage, got %v", err)
	}
}
