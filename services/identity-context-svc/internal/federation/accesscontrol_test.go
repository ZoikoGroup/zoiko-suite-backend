package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type acStub struct {
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  []string
	handler func(w http.ResponseWriter, r *http.Request)
}

func (s *acStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	s.bodies = append(s.bodies, string(b))
	s.mu.Unlock()
	s.handler(w, r)
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func newAC(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*ACClient, *acStub) {
	t.Helper()
	stub := &acStub{handler: h}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return NewACClient(srv.URL, "svc-identity", 2*time.Second), stub
}

var asg = AssignmentRequest{TenantID: "t-1", LegalEntityID: "le-1", TargetPrincipalID: "p-1", RoleDefinitionID: "r-1", Justification: "j", CorrelationID: "fed-abc"}

func TestACClientRequestShape(t *testing.T) {
	c, stub := newAC(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 201, `{"request_id":"req-1","status":"PROVISIONED"}`)
	})
	out, err := c.RequestAssignment(context.Background(), asg)
	if err != nil || out.RequestID != "req-1" || out.Status != "PROVISIONED" {
		t.Fatalf("%+v %v", out, err)
	}
	r, body := stub.reqs[0], stub.bodies[0]
	if r.Method != "POST" || r.URL.Path != "/v1/iam/access-assignments/" {
		t.Fatalf("%s %s", r.Method, r.URL.Path)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(body), &sent)
	if sent["origin"] != "FEDERATION" || sent["target_principal_id"] != "p-1" || sent["role_definition_id"] != "r-1" || sent["correlation_id"] != "fed-abc" {
		t.Fatalf("the request carries origin FEDERATION and the fixed correlation: %s", body)
	}
	for k, want := range map[string]string{
		"X-Tenant-Id": "t-1", "X-Principal-Id": "svc-identity", "X-Legal-Entity-Id": "le-1", "X-Correlation-ID": "fed-abc",
		"Idempotency-Key": "fed-abc", "X-Source-Channel": "system", "X-Workload-Id": "identity-context-svc"} {
		if r.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, r.Header.Get(k), want)
		}
	}
	if r.Header.Get("X-Request-Id") == "" {
		t.Error("a request id is sent")
	}
}

func isRefused(e error, code string) bool {
	var rr *RequestRefused
	return errors.As(e, &rr) && rr.Code == code && !errors.Is(e, ErrAccessControlUnavailable)
}

func isRetry(e error) bool { return errors.Is(e, ErrAccessControlUnavailable) }

func TestACClientRequestAnswers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(AssignmentOutcome, error) bool
	}{
		{"created", 201, `{"request_id":"r1","status":"PROVISIONED"}`, func(o AssignmentOutcome, e error) bool { return e == nil && o.RequestID == "r1" }},
		{"replay", 200, `{"request_id":"r1","status":"PROVISIONED"}`, func(o AssignmentOutcome, e error) bool { return e == nil && o.RequestID == "r1" }},
		{"queued for approval is reported as such", 202, `{"request_id":"r2","status":"PENDING_APPROVAL"}`, func(o AssignmentOutcome, e error) bool { return e == nil && o.Status == "PENDING_APPROVAL" }},
		{"privileged", 409, `{"error_code":"federation_privileged_refused"}`, func(o AssignmentOutcome, e error) bool { return errors.Is(e, ErrPrivilegedRefused) }},
		{"sod conflict is final", 403, `{"error_code":"sod_conflict"}`, func(o AssignmentOutcome, e error) bool { return isRefused(e, "sod_conflict") }},
		{"retired role is final", 409, `{"error_code":"role_retired"}`, func(o AssignmentOutcome, e error) bool { return isRefused(e, "role_retired") }},
		{"unknown role is final", 404, `{"error_code":"not_found"}`, func(o AssignmentOutcome, e error) bool { return isRefused(e, "not_found") }},
		{"a missing grant is the operator's to fix: retried", 403, `{"error_code":"forbidden"}`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"no identity: retried", 401, `{"error_code":"identity_missing"}`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"authz down: retried", 503, `{"error_code":"authz_unavailable"}`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"server error: retried", 500, `oops`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"rate limited: retried", 429, `{}`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"success with an unreadable body is not a success", 201, `not json`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
		{"success with no id is not a success", 201, `{"status":"PROVISIONED"}`, func(o AssignmentOutcome, e error) bool { return isRetry(e) }},
	}
	for _, tc := range cases {
		c, _ := newAC(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.status, tc.body) })
		out, err := c.RequestAssignment(context.Background(), asg)
		if !tc.check(out, err) {
			t.Errorf("%s: %+v %v", tc.name, out, err)
		}
	}
	// no answer at all
	c := NewACClient("http://127.0.0.1:1", "svc", 300*time.Millisecond)
	if _, err := c.RequestAssignment(context.Background(), asg); !isRetry(err) {
		t.Fatalf("%v", err)
	}
}

func TestACClientEligibility(t *testing.T) {
	c, stub := newAC(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("role_definition_id") {
		case "ok":
			reply(w, 200, `{"role_definition_id":"ok","eligible":true,"risk_tier":"STANDARD","reason":"baseline"}`)
		case "priv":
			reply(w, 200, `{"role_definition_id":"priv","eligible":false,"risk_tier":"CRITICAL","reason":"privileged"}`)
		case "gone":
			reply(w, 404, `{"error_code":"not_found"}`)
		case "down":
			reply(w, 503, `{"error_code":"taxonomy_unavailable"}`)
		case "nogrant":
			reply(w, 403, `{"error_code":"forbidden"}`)
		default:
			reply(w, 200, `garbage`)
		}
	})
	if e, err := c.RoleEligibility(context.Background(), "t-1", "ok"); err != nil || !e.Eligible || e.RiskTier != "STANDARD" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err := c.RoleEligibility(context.Background(), "t-1", "priv"); err != nil || e.Eligible || e.RiskTier != "CRITICAL" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err := c.RoleEligibility(context.Background(), "t-1", "gone"); err != nil || e.Eligible {
		t.Fatalf("a role that does not exist is not eligible: %+v %v", e, err)
	}
	for _, id := range []string{"down", "nogrant", "other"} {
		if e, err := c.RoleEligibility(context.Background(), "t-1", id); err == nil || e.Eligible {
			t.Errorf("%s: an unclear answer is an error and never eligible: %+v %v", id, e, err)
		}
	}
	if !strings.Contains(stub.reqs[0].URL.RawQuery, "role_definition_id=ok") || stub.reqs[0].Header.Get("X-Tenant-Id") != "t-1" {
		t.Fatalf("%v", stub.reqs[0].URL)
	}
	// the id is escaped, never interpreted as more query
	_, _ = c.RoleEligibility(context.Background(), "t-1", "a&inject=1")
	last := stub.reqs[len(stub.reqs)-1].URL.Query()
	if last.Get("inject") != "" || last.Get("role_definition_id") != "a&inject=1" {
		t.Fatalf("the role id must be query-escaped: %v", last)
	}
}

func TestACClientRevoke(t *testing.T) {
	var getStatus string
	c, stub := newAC(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			reply(w, 200, `{"request_id":"r1","status":"`+getStatus+`"}`)
		case strings.Contains(r.URL.Path, "r-ok"):
			reply(w, 200, `{}`)
		case strings.Contains(r.URL.Path, "r-gone"):
			reply(w, 404, `{"error_code":"not_found"}`)
		case strings.Contains(r.URL.Path, "r-state"):
			reply(w, 409, `{"error_code":"invalid_state"}`)
		case strings.Contains(r.URL.Path, "r-nogrant"):
			reply(w, 403, `{"error_code":"forbidden"}`)
		case strings.Contains(r.URL.Path, "r-400"):
			reply(w, 400, `{"error_code":"missing_fields"}`)
		default:
			reply(w, 500, `x`)
		}
	})
	do := func(id string) error {
		return c.RevokeAssignment(context.Background(), "t-1", "le-1", id, "fed-revoke-1", "no longer entitled")
	}
	if err := do("r-ok"); err != nil {
		t.Fatal(err)
	}
	if p := stub.reqs[0].URL.Path; p != "/v1/iam/access-assignments/r-ok:revoke" {
		t.Fatalf("the documented colon form: %s", p)
	}
	if err := do("r-gone"); err != nil {
		t.Fatalf("something already gone is revoked: %v", err)
	}
	getStatus = "REVOKED"
	if err := do("r-state"); err != nil {
		t.Fatalf("already revoked is success: %v", err)
	}
	getStatus = "PENDING_APPROVAL"
	if err := do("r-state"); !isRefused(err, "invalid_state") {
		t.Fatalf("not revocable and not revoked is a refusal: %v", err)
	}
	if err := do("r-nogrant"); !isRetry(err) {
		t.Fatalf("a missing grant is retried: %v", err)
	}
	if err := do("r-500"); !isRetry(err) {
		t.Fatalf("%v", err)
	}
	if err := do("r-400"); !isRefused(err, "missing_fields") {
		t.Fatalf("%v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(stub.bodies[0]), &sent)
	if sent["legal_entity_id"] != "le-1" || sent["reason"] == "" || sent["correlation_id"] != "fed-revoke-1" {
		t.Fatalf("%v", sent)
	}
}

func TestOperatorFault(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   bool
	}{{401, "", true}, {403, "forbidden", true}, {403, "authz_unavailable", true}, {403, "provisioning_forbidden", true}, {403, "sod_conflict", false},
		{409, "role_retired", false}, {400, "", false}, {404, "", false}} {
		if got := operatorFault(tc.status, tc.code); got != tc.want {
			t.Errorf("%d %s: %v", tc.status, tc.code, got)
		}
	}
}
