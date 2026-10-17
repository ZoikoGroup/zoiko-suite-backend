package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/whout"
)

// whAuthz records every authorization question and answers per (principal, action).
type whAuthz struct {
	mu    sync.Mutex
	asked []string
	deny  map[string]bool
	err   error
}

func (a *whAuthz) CheckAllowed(_ context.Context, principal, legalEntity, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, principal+"|"+legalEntity+"|"+action)
	if a.err != nil {
		return a.err
	}
	if a.deny[principal+"|"+action] || a.deny["*|"+action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func (a *whAuthz) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 {
		return ""
	}
	return a.asked[len(a.asked)-1]
}

type whAPI struct {
	e     *whEnv
	authz *whAuthz
	h     http.Handler
}

func newWhAPI(t *testing.T) *whAPI {
	e := newWhEnv(t)
	az := &whAuthz{deny: map[string]bool{}}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterWebhookRoutes(r, handler.NewWebhookHandler(e.svc, az, zap.NewNop()))
	return &whAPI{e: e, authz: az, h: r}
}

type whResp struct {
	Code   int
	Header http.Header
	Raw    string
	JSON   map[string]any
}

func (a *whAPI) do(method, path, principal string, body any, hdr ...string) whResp {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", "corr-http")
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	req.Header.Set("X-Tenant-Id", a.e.tenant)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	out := whResp{Code: w.Code, Header: w.Header(), Raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.JSON)
	return out
}

func createBody() map[string]any {
	return map[string]any{
		"legal_entity_id": "le-1", "name": "ERP feed", "destination_url": whURL, "event_types": []string{"invoice.paid"},
		"allowed_fields": []string{"invoice_id", "amount"}, "signing_key_path": "kv/webhooks/erp", "destination_region": "uk", "max_attempts": 3,
	}
}

func asInt(v any) int { f, _ := v.(float64); return int(f) }

func TestWebhookAPI_FullLifecycleOverHTTP(t *testing.T) {
	a := newWhAPI(t)
	c := a.do("POST", "/v1/webhook-subscriptions", whMaker, createBody())
	if c.Code != 201 || c.JSON["status"] != "DRAFT" || !strings.HasPrefix(c.Header.Get("Location"), "/v1/webhook-subscriptions/") {
		t.Fatalf("create: %d %s", c.Code, c.Raw)
	}
	if got := a.authz.last(); got != whMaker+"|le-1|"+handler.ActionWebhookSubscriptionCreate {
		t.Fatalf("create was authorized as %q", got)
	}
	id := c.JSON["subscription_id"].(string)
	base := "/v1/webhook-subscriptions/" + id

	s := a.do("POST", base+"/submit", whMaker, map[string]any{"expected_version": 1})
	if s.Code != 200 || s.JSON["status"] != "PENDING_APPROVAL" {
		t.Fatalf("submit: %d %s", s.Code, s.Raw)
	}
	if got := a.authz.last(); got != whMaker+"|le-1|"+handler.ActionWebhookSubscriptionSubmit {
		t.Fatalf("submit was authorized as %q", got)
	}
	// the maker cannot approve their own subscription, over HTTP either
	self := a.do("POST", base+"/approve", whMaker, map[string]any{"expected_version": 2})
	if self.Code != 403 || self.JSON["code"] != "INT-AUTH-003" || self.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("self-approval: %d %s", self.Code, self.Raw)
	}
	// version in the header works, and must agree with the body when both are present
	if r := a.do("POST", base+"/approve", whChecker, map[string]any{"expected_version": 2}, "X-Expected-Version", "3"); r.Code != 400 {
		t.Fatalf("disagreeing versions: %d %s", r.Code, r.Raw)
	}
	ap := a.do("POST", base+"/approve", whChecker, map[string]any{"reason": "verified"}, "X-Expected-Version", "2")
	if ap.Code != 200 || ap.JSON["status"] != "ACTIVE" || ap.JSON["approved_by"] != whChecker {
		t.Fatalf("approve: %d %s", ap.Code, ap.Raw)
	}
	if got := a.authz.last(); got != whChecker+"|le-1|"+handler.ActionWebhookSubscriptionApprove {
		t.Fatalf("approve was authorized as %q", got)
	}

	g := a.do("GET", base, whMaker, nil)
	if g.Code != 200 || g.JSON["config_fingerprint"] != ap.JSON["config_fingerprint"] {
		t.Fatalf("get: %d %s", g.Code, g.Raw)
	}
	l := a.do("GET", "/v1/webhook-subscriptions", whMaker, nil)
	if l.Code != 200 || len(l.JSON["subscriptions"].([]any)) != 1 {
		t.Fatalf("list: %d %s", l.Code, l.Raw)
	}

	// deliver one event, then read its evidence over the API
	a.e.emit("evt-h1", "invoice.paid", a.e.invoice("inv-h1"))
	a.e.drain()
	dl := a.do("GET", base+"/deliveries", whMaker, nil)
	ds := dl.JSON["deliveries"].([]any)
	if dl.Code != 200 || len(ds) != 1 {
		t.Fatalf("deliveries: %d %s", dl.Code, dl.Raw)
	}
	did := ds[0].(map[string]any)["delivery_id"].(string)
	ev := a.do("GET", "/v1/webhook-deliveries/"+did, whMaker, nil)
	if ev.Code != 200 {
		t.Fatalf("evidence: %d %s", ev.Code, ev.Raw)
	}
	at := ev.JSON["attempts"].([]any)
	if len(at) != 1 || at[0].(map[string]any)["outcome"] != "SUCCEEDED" || at[0].(map[string]any)["destination_host"] != whHost {
		t.Fatalf("attempts: %s", ev.Raw)
	}
	// the API never returns the payload body or anything key-shaped
	for _, leak := range []string{"internal_note", "never leaves", "account_number", string(whKey1), `"body"`} {
		if strings.Contains(ev.Raw, leak) || strings.Contains(dl.Raw, leak) {
			t.Fatalf("the API leaks %q", leak)
		}
	}
	h := a.do("GET", base+"/health", whMaker, nil)
	if h.Code != 200 || asInt(h.JSON["succeeded"]) != 1 || h.JSON["breaker_open"] != false {
		t.Fatalf("health: %d %s", h.Code, h.Raw)
	}
	lg := a.do("GET", base+"/log", whMaker, nil)
	if lg.Code != 200 || len(lg.JSON["log"].([]any)) != 3 {
		t.Fatalf("log: %d %s", lg.Code, lg.Raw)
	}

	// suspend / resume / rotate / revoke
	cur := asInt(h2(a, base)["version"])
	sp := a.do("POST", base+"/suspend", whChecker, map[string]any{"expected_version": cur, "reason": "customer asked"})
	if sp.Code != 200 || sp.JSON["status"] != "SUSPENDED" {
		t.Fatalf("suspend: %d %s", sp.Code, sp.Raw)
	}
	if r := a.do("POST", base+"/suspend", whChecker, map[string]any{"expected_version": cur + 1}); r.Code != 400 {
		t.Fatalf("suspend without a reason must be refused: %d %s", r.Code, r.Raw)
	}
	rs := a.do("POST", base+"/resume", whChecker, map[string]any{"expected_version": cur + 1, "reason": "back"})
	if rs.Code != 200 || rs.JSON["status"] != "ACTIVE" {
		t.Fatalf("resume: %d %s", rs.Code, rs.Raw)
	}
	rot := a.do("POST", base+"/rotate-signing-key", whMaker, map[string]any{"expected_version": cur + 2, "new_signing_key_path": "kv/webhooks/erp-2", "grace_period_seconds": 3600})
	if rot.Code != 200 || rot.JSON["signing_key_path"] != "kv/webhooks/erp-2" || rot.JSON["previous_signing_key_path"] != "kv/webhooks/erp" {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Raw)
	}
	rv := a.do("POST", base+"/revoke", whChecker, map[string]any{"expected_version": cur + 3, "reason": "contract ended"})
	if rv.Code != 200 || rv.JSON["status"] != "REVOKED" {
		t.Fatalf("revoke: %d %s", rv.Code, rv.Raw)
	}
	if r := a.do("PATCH", base, whMaker, map[string]any{"expected_version": cur + 4, "name": "x"}); r.Code != 409 || r.JSON["code"] != "INT-SCOPE-013" {
		t.Fatalf("editing a revoked subscription: %d %s", r.Code, r.Raw)
	}
}

func h2(a *whAPI, base string) map[string]any {
	return a.do("GET", base, whMaker, nil).JSON
}

func TestWebhookAPI_AuthorizationAndIdentity(t *testing.T) {
	a := newWhAPI(t)
	if r := a.do("POST", "/v1/webhook-subscriptions", "", createBody()); r.Code != 401 {
		t.Errorf("no principal: %d", r.Code)
	}
	if r := a.do("POST", "/v1/webhook-subscriptions", whMaker, createBody(), "X-Tenant-Id", ""); r.Code != 401 {
		t.Errorf("no tenant: %d", r.Code)
	}
	a.authz.deny["*|"+handler.ActionWebhookSubscriptionCreate] = true
	if r := a.do("POST", "/v1/webhook-subscriptions", whMaker, createBody()); r.Code != 403 || r.JSON["code"] != "INT-AUTH-003" {
		t.Errorf("denied: %d %s", r.Code, r.Raw)
	}
	var count int
	_ = a.e.admin.QueryRow(whBG, `SELECT count(*) FROM webhook_subscriptions WHERE tenant_id = $1`, a.e.tenant).Scan(&count)
	if count != 0 {
		t.Errorf("a denied create must create nothing; found %d", count)
	}
	delete(a.authz.deny, "*|"+handler.ActionWebhookSubscriptionCreate)

	a.authz.err = domain.ErrAuthzServiceUnavailable
	if r := a.do("POST", "/v1/webhook-subscriptions", whMaker, createBody()); r.Code != 503 || r.Header.Get("Retry-After") == "" {
		t.Errorf("authz outage must fail closed with a retry hint: %d %s", r.Code, r.Raw)
	}
	a.authz.err = nil
	sub := a.e.active()
	for name, c := range map[string]struct{ method, path, action string }{
		"update":  {"PATCH", "", handler.ActionWebhookSubscriptionUpdate},
		"suspend": {"POST", "/suspend", handler.ActionWebhookSubscriptionSuspend},
		"revoke":  {"POST", "/revoke", handler.ActionWebhookSubscriptionRevoke},
		"rotate":  {"POST", "/rotate-signing-key", handler.ActionWebhookSubscriptionRotate},
		"resume":  {"POST", "/resume", handler.ActionWebhookSubscriptionResume},
		"submit":  {"POST", "/submit", handler.ActionWebhookSubscriptionSubmit},
		"approve": {"POST", "/approve", handler.ActionWebhookSubscriptionApprove},
		"reject":  {"POST", "/reject", handler.ActionWebhookSubscriptionApprove},
		"read":    {"GET", "", handler.ActionWebhookSubscriptionRead},
		"log":     {"GET", "/log", handler.ActionWebhookDeliveryRead},
		"health":  {"GET", "/health", handler.ActionWebhookDeliveryRead},
	} {
		a.authz.deny["*|"+c.action] = true
		r := a.do(c.method, "/v1/webhook-subscriptions/"+sub.SubscriptionID+c.path, whChecker, map[string]any{"expected_version": sub.Version, "reason": "x"})
		if r.Code != 403 {
			t.Errorf("%s without %s: %d %s", name, c.action, r.Code, r.Raw)
		}
		delete(a.authz.deny, "*|"+c.action)
	}
	if got := a.e.sub(sub.SubscriptionID); got.Status != whout.StatusActive || got.Version != sub.Version {
		t.Fatalf("denied commands must change nothing: %+v", got)
	}
}

func TestWebhookAPI_InputErrors(t *testing.T) {
	a := newWhAPI(t)
	for name, c := range map[string]struct {
		body any
		want int
		code string
	}{
		"not json":          {`{`, 400, "INT-SCHEMA-004"},
		"unknown field":     {`{"legal_entity_id":"le-1","surprise":1}`, 400, "INT-SCHEMA-004"},
		"missing fields":    {map[string]any{"legal_entity_id": "le-1"}, 400, "INT-SCHEMA-004"},
		"restricted field":  {mut(createBody(), "allowed_fields", []string{"invoice_id", "api_key"}), 422, "INT-SCOPE-013"},
		"private ip":        {mut(createBody(), "destination_url", "https://10.0.0.5/x"), 403, "INT-REG-002"},
		"internal host":     {mut(createBody(), "destination_url", "https://db.internal/x"), 403, "INT-REG-002"},
		"url with secret":   {mut(createBody(), "destination_url", "https://example.com/x?token=abc"), 400, "INT-SCHEMA-004"},
		"disallowed region": {mut(createBody(), "destination_region", "us-east"), 403, "INT-REG-002"},
		"bare wildcard":     {mut(createBody(), "event_types", []string{"*"}), 400, "INT-SCHEMA-004"},
	} {
		r := a.do("POST", "/v1/webhook-subscriptions", whMaker, c.body)
		if r.Code != c.want || r.JSON["code"] != c.code {
			t.Errorf("%s: %d %s (want %d %s)", name, r.Code, r.Raw, c.want, c.code)
		}
		// problems name the rule; they never echo what the caller typed
		for _, echo := range []string{"api_key", "10.0.0.5", "token=abc", "surprise"} {
			if strings.Contains(r.Raw, echo) && name != "unknown field" {
				t.Errorf("%s: the problem echoes %q: %s", name, echo, r.Raw)
			}
		}
	}
	sub := a.e.draft()
	base := "/v1/webhook-subscriptions/" + sub.SubscriptionID
	if r := a.do("POST", base+"/submit", whMaker, map[string]any{}); r.Code != 400 {
		t.Errorf("a missing version: %d", r.Code)
	}
	if r := a.do("POST", base+"/submit", whMaker, map[string]any{"expected_version": 9}); r.Code != 409 || r.JSON["code"] != "INT-IDEM-005" {
		t.Errorf("a stale version: %d %s", r.Code, r.Raw)
	}
	if r := a.do("POST", base+"/submit", whMaker, "", "X-Expected-Version", "abc"); r.Code != 400 {
		t.Errorf("a malformed version header: %d", r.Code)
	}
	if r := a.do("GET", "/v1/webhook-subscriptions/00000000-0000-0000-0000-000000000000", whMaker, nil); r.Code != 404 {
		t.Errorf("unknown id: %d", r.Code)
	}
	if r := a.do("GET", "/v1/webhook-subscriptions/not-a-uuid", whMaker, nil); r.Code != 404 {
		t.Errorf("malformed id: %d", r.Code)
	}
	if r := a.do("GET", "/v1/webhook-deliveries/not-a-uuid", whMaker, nil); r.Code != 404 {
		t.Errorf("malformed delivery id: %d", r.Code)
	}
}

func mut(m map[string]any, k string, v any) map[string]any { m[k] = v; return m }

func TestWebhookAPI_OtherTenantsSeeNothing(t *testing.T) {
	a := newWhAPI(t)
	sub := a.e.active()
	a.e.emit("evt-1", "invoice.paid", a.e.invoice("1"))
	a.e.drain()
	d := a.e.delivery(sub, "evt-1")
	other := "tenant-wh-intruder"
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/webhook-subscriptions/" + sub.SubscriptionID},
		{"GET", "/v1/webhook-subscriptions/" + sub.SubscriptionID + "/deliveries"},
		{"GET", "/v1/webhook-subscriptions/" + sub.SubscriptionID + "/health"},
		{"GET", "/v1/webhook-subscriptions/" + sub.SubscriptionID + "/log"},
		{"POST", "/v1/webhook-subscriptions/" + sub.SubscriptionID + "/revoke"},
		{"GET", "/v1/webhook-deliveries/" + d.DeliveryID},
		{"POST", "/v1/webhook-deliveries/" + d.DeliveryID + "/replay"},
	} {
		body := map[string]any{"expected_version": 1, "reason": "x"}
		if strings.HasSuffix(c.path, "/replay") {
			body = map[string]any{"approver_principal_id": "approver-1", "approval_ref": "C", "reason": "x"}
		}
		r := a.do(c.method, c.path, whChecker, body, "X-Tenant-Id", other)
		if r.Code != 404 {
			t.Errorf("%s %s as another tenant: %d %s", c.method, c.path, r.Code, r.Raw)
		}
	}
	if l := a.do("GET", "/v1/webhook-subscriptions", whChecker, nil, "X-Tenant-Id", other); l.Code != 200 || len(l.JSON["subscriptions"].([]any)) != 0 {
		t.Errorf("list leaked: %s", l.Raw)
	}
	if l := a.do("GET", "/v1/webhook-dead-letters", whChecker, nil, "X-Tenant-Id", other); l.Code != 200 || len(l.JSON["dead_letters"].([]any)) != 0 {
		t.Errorf("dead letters leaked: %s", l.Raw)
	}
	if got := a.e.sub(sub.SubscriptionID); got.Status != whout.StatusActive {
		t.Fatal("another tenant revoked it")
	}
}

func TestWebhookAPI_ReplayRequiresAnEntitledDifferentApprover(t *testing.T) {
	a := newWhAPI(t)
	sub := a.e.active(func(s *whout.Spec) { s.MaxAttempts = 1 })
	a.e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 500, nil })
	a.e.emit("evt-1", "invoice.paid", a.e.invoice("1"))
	a.e.drain()
	d := a.e.delivery(sub, "evt-1")
	dead := a.do("GET", "/v1/webhook-dead-letters?subscription_id="+sub.SubscriptionID, whMaker, nil)
	if dead.Code != 200 || len(dead.JSON["dead_letters"].([]any)) != 1 {
		t.Fatalf("dead letters: %d %s", dead.Code, dead.Raw)
	}
	path := "/v1/webhook-deliveries/" + d.DeliveryID + "/replay"
	if r := a.do("POST", path, "operator-1", map[string]any{"approval_ref": "CHG-1", "reason": "x"}); r.Code != 403 {
		t.Errorf("no approver: %d %s", r.Code, r.Raw)
	}
	if r := a.do("POST", path, "operator-1", map[string]any{"approver_principal_id": "operator-1", "approval_ref": "CHG-1", "reason": "x"}); r.Code != 403 {
		t.Errorf("self approver: %d %s", r.Code, r.Raw)
	}
	// the named approver must themselves be entitled
	a.authz.deny["approver-1|"+handler.ActionWebhookReplayApprove] = true
	if r := a.do("POST", path, "operator-1", map[string]any{"approver_principal_id": "approver-1", "approval_ref": "CHG-1", "reason": "x"}); r.Code != 403 {
		t.Errorf("an approver who is not entitled: %d %s", r.Code, r.Raw)
	}
	delete(a.authz.deny, "approver-1|"+handler.ActionWebhookReplayApprove)
	// and the operator must be entitled to replay at all
	a.authz.deny["operator-1|"+handler.ActionWebhookDeliveryReplay] = true
	if r := a.do("POST", path, "operator-1", map[string]any{"approver_principal_id": "approver-1", "approval_ref": "CHG-1", "reason": "x"}); r.Code != 403 {
		t.Errorf("an operator who may not replay: %d %s", r.Code, r.Raw)
	}
	delete(a.authz.deny, "operator-1|"+handler.ActionWebhookDeliveryReplay)
	if a.e.delivery(sub, "evt-1").Status != whout.DeliveryDeadLettered {
		t.Fatal("refused replays must change nothing")
	}
	a.e.dest.setStatus(nil)
	r := a.do("POST", path, "operator-1", map[string]any{"approver_principal_id": "approver-1", "approval_ref": "CHG-9", "reason": "fixed"})
	if r.Code != 202 || r.JSON["status"] != "PENDING" || asInt(r.JSON["replay_count"]) != 1 {
		t.Fatalf("replay: %d %s", r.Code, r.Raw)
	}
	a.e.drain()
	if a.e.delivery(sub, "evt-1").Status != whout.DeliverySucceeded {
		t.Fatal("the replayed delivery should now succeed")
	}
}
