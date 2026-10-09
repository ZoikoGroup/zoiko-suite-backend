package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/idempotency"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncd"
	"zoiko.io/notification-svc/internal/store"
)

// HTTP-level proof of the NCD routes: the real router, tenant middleware,
// idempotency middleware and store, as the unprivileged app role the store
// suite provisions. Skips without TEST_DATABASE_URL; run the store suite first
// on a fresh database (it applies the migrations and creates the role).

type grantSet map[string]bool // principal|action

func (g grantSet) CheckAllowed(_ context.Context, principal, _, action string) error {
	if g[principal+"|"+action] || g[principal+"|*"] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type okResolver struct{}

func (okResolver) ResolveEmail(_ context.Context, _, _, recipient string) (string, error) {
	return recipient + "@example.com", nil
}

type okEmail struct{ n int }

func (e *okEmail) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	e.n++
	return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "ok; message-id=<" + n.NotificationID + ">"}
}

func ncdServer(t *testing.T) (http.Handler, *ncd.Service, string) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, _ := url.Parse(dsn)
	if !strings.Contains(u.Path, "test") {
		t.Fatalf("refusing to run against %s", u.Path)
	}
	// The store suite's older tests recreate some tables after the NCD
	// suite runs, which drops the app role's grants; restore them.
	if admin, err := pgxpool.New(context.Background(), dsn); err == nil {
		_, _ = admin.Exec(context.Background(), `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_ncd_app;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_ncd_app`)
		admin.Close()
	}
	u.User = url.UserPassword("zoiko_ncd_app", "ncd")
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("app role not provisioned (run the store suite first): %v", err)
	}
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('ncd_communications') IS NOT NULL`).Scan(&ok); err != nil || !ok {
		t.Skip("NCD schema not present (run the store suite first)")
	}
	t.Cleanup(pool.Close)
	pg := store.New(pool)
	st := store.NewNCD(pg)
	svc := ncd.NewService(st, okResolver{}, ncd.RouterTransport{Email: &okEmail{}, Inbox: st}, ncd.DefaultLimits(), zap.NewNop())
	grants := grantSet{"alice|*": true, "bob|*": true}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(idempotency.Middleware(pg, zap.NewNop()))
	handler.RegisterNCDRoutes(r, handler.NewNCDHandler(svc, grants, zap.NewNop()))
	return r, svc, "tenant-" + uuid.NewString()[:8]
}

func call(t *testing.T, h http.Handler, method, path, tenant, principal string, body any, key string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestNCDHTTP_EndToEnd(t *testing.T) {
	h, svc, tenant := ncdServer(t)
	intent := map[string]any{"legal_entity_id": "le-1", "intent_code": "invoice", "display_name": "Invoice",
		"purpose_class": "TRANSACTIONAL_RELATIONSHIP", "domain_owner": "billing", "sensitivity": "S1", "urgency": "U1",
		"evidence_class": "E1", "allowed_channels": []string{"IN_APP"},
		"variable_contract": []map[string]any{{"name": "invoice_no", "type": "string", "required": true, "sensitivity": "S1"}}}

	// Authorization is enforced: a principal with no grant is refused.
	if code, _ := call(t, h, "POST", "/v1/communication-intents", tenant, "mallory", intent, "k0"); code != http.StatusForbidden {
		t.Fatalf("ungranted create should be 403, got %d", code)
	}
	code, body := call(t, h, "POST", "/v1/communication-intents", tenant, "alice", intent, "k1")
	if code != http.StatusCreated {
		t.Fatalf("create intent: %d %v", code, body)
	}
	id := body["intent_id"].(string)
	// Idempotency-Key replays the first answer rather than creating twice.
	code, again := call(t, h, "POST", "/v1/communication-intents", tenant, "alice", intent, "k1")
	if code != http.StatusCreated || again["intent_id"] != id {
		t.Fatalf("replay must return the same intent: %d %v", code, again)
	}
	// SoD over HTTP.
	if code, b := call(t, h, "POST", "/v1/communication-intents/"+id+"/activate", tenant, "alice", map[string]any{"version": 1}, "k2"); code != http.StatusForbidden {
		t.Fatalf("self-activation should be 403: %d %v", code, b)
	}
	if code, b := call(t, h, "POST", "/v1/communication-intents/"+id+"/activate", tenant, "bob", map[string]any{"version": 1}, "k3"); code != http.StatusOK {
		t.Fatalf("activate: %d %v", code, b)
	}
	code, tv := call(t, h, "POST", "/v1/templates", tenant, "alice", map[string]any{"intent_id": id, "channel": "IN_APP", "locale": "en-GB",
		"subject": "Invoice {{invoice_no}}", "body": "<p>{{invoice_no}}</p>"}, "k4")
	if code != http.StatusCreated {
		t.Fatalf("create template: %d %v", code, tv)
	}
	tid := tv["template_version_id"].(string)
	for i, step := range []struct{ path, who string }{{"validate", "alice"}, {"approve", "bob"}, {"publish", "bob"}} {
		if code, b := call(t, h, "POST", "/v1/templates/"+tid+"/"+step.path, tenant, step.who, nil, "t"+strconv.Itoa(i)); code != http.StatusOK {
			t.Fatalf("%s: %d %v", step.path, code, b)
		}
	}
	code, eff := call(t, h, "GET", "/v1/intents/"+id+"/effective", tenant, "alice", nil, "")
	if code != http.StatusOK || len(eff["templates"].([]any)) != 1 {
		t.Fatalf("effective: %d %v", code, eff)
	}

	comm := map[string]any{"intent_id": id, "legal_entity_id": "le-1", "recipient_principal_id": "pat", "locale": "en-GB",
		"variables": map[string]string{"invoice_no": "INV-7"}, "source_event_id": "evt-" + uuid.NewString()}
	code, cb := call(t, h, "POST", "/v1/communications", tenant, "alice", comm, "c1")
	if code != http.StatusCreated {
		t.Fatalf("create communication: %d %v", code, cb)
	}
	cid := cb["communication"].(map[string]any)["communication_id"].(string)
	// NP-21 over HTTP with a DIFFERENT Idempotency-Key: the purpose-scoped
	// key still finds the same communication, reported as NCD-019.
	code, dup := call(t, h, "POST", "/v1/communications", tenant, "alice", comm, "c2")
	if code != http.StatusOK || dup["reason_code"] != "NCD-019" {
		t.Fatalf("duplicate source event should replay with NCD-019: %d %v", code, dup)
	}
	if code, b := call(t, h, "POST", "/v1/communications/"+cid+"/prepare", tenant, "alice", nil, "c3"); code != http.StatusOK || b["refusal"] != nil {
		t.Fatalf("prepare: %d %v", code, b)
	}
	if code, b := call(t, h, "POST", "/v1/communications/"+cid+"/dispatch", tenant, "alice", nil, "c4"); code != http.StatusAccepted {
		t.Fatalf("dispatch: %d %v", code, b)
	}
	svc.RunOnce(context.Background())
	code, v := call(t, h, "GET", "/v1/communications/"+cid, tenant, "alice", nil, "")
	claims := v["claims"].(map[string]any)
	if code != http.StatusOK || claims["delivery_state"] != "DELIVERED_TO_INBOX" || claims["legally_served"] != "NOT_DETERMINED_BY_NCD" {
		t.Fatalf("§3.3 state over HTTP: %d %v", code, claims)
	}
	if _, has := v["communication"].(map[string]any)["sent"]; has {
		t.Fatal("§3.3: no generic sent field")
	}
	// A coded refusal is 422 with its stable code.
	code, ref := call(t, h, "POST", "/v1/communications/"+cid+"/resend", tenant, "alice", map[string]any{"reason": ""}, "c5")
	if code != http.StatusBadRequest {
		t.Fatalf("resend without reason should be 400: %d %v", code, ref)
	}
	code, ev := call(t, h, "GET", "/v1/evidence/"+cid, tenant, "alice", nil, "")
	if code != http.StatusOK || len(ev["attempts"].([]any)) != 1 || ev["package_content_hash"] == "" {
		t.Fatalf("evidence bundle: %d %v", code, ev)
	}
	// Another tenant sees nothing.
	if code, _ := call(t, h, "GET", "/v1/communications/"+cid, "tenant-other", "alice", nil, ""); code != http.StatusNotFound {
		t.Fatalf("cross-tenant read should be 404, got %d", code)
	}
}

func TestNCDHTTP_ProviderEventsNeedSignature(t *testing.T) {
	h, _, _ := ncdServer(t)
	body := []byte(`{"events":[{"event_id":"e1","event_type":"delivered","attempt_token":"x"}]}`)
	req := httptest.NewRequest("POST", "/v1/provider-events/smtp-primary", bytes.NewReader(body))
	req.Header.Set("X-NCD-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-NCD-Signature", "sha256=forged")
	rr := httptest.NewRecorder()
	prev := ncd.SecretLookup
	ncd.SecretLookup = func(string) string { return "s" }
	defer func() { ncd.SecretLookup = prev }()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("NP-26: forged callback must be 401, got %d %s", rr.Code, rr.Body.String())
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req = httptest.NewRequest("POST", "/v1/provider-events/smtp-primary", bytes.NewReader(body))
	req.Header.Set("X-NCD-Timestamp", ts)
	req.Header.Set("X-NCD-Signature", ncd.SignCallback("s", ts, body))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "UNMATCHED") {
		t.Fatalf("a signed callback for no attempt is accepted and left UNMATCHED: %d %s", rr.Code, rr.Body.String())
	}
}

// NP-60 / INV-25: unsubscribe and complaint handling cannot be switched off
// from below. No request body that reaches the plane has such a switch, and
// an attempt to supply one is refused rather than silently ignored.
func TestNCDHTTP_UnsubscribeHandlingCannotBeDisabled(t *testing.T) {
	h, _, tenant := ncdServer(t)
	for path, body := range map[string]map[string]any{
		"/v1/preferences": {"muted_channels": []string{}, "disable_unsubscribe": true},
		"/v1/communication-intents": {"legal_entity_id": "le-1", "intent_code": "promo", "display_name": "Promo",
			"purpose_class": "MARKETING", "domain_owner": "growth", "sensitivity": "S1", "urgency": "U1",
			"evidence_class": "E1", "allowed_channels": []string{"EMAIL"}, "complaint_handling": "off"},
	} {
		if code, out := call(t, h, "POST", path, tenant, "alice", body, ""); code != http.StatusBadRequest {
			t.Errorf("%s with an unsubscribe/complaint switch must be 400, got %d %v", path, code, out)
		}
	}
}
