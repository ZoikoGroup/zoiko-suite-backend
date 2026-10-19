package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/egress"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
)

const credOK = "0a000000-0000-4000-8000-000000000001"

// fakeAuthz stands in for authorization-svc. It grants unless the
// (principal, action) pair was denied, and can be made unavailable.
type fakeAuthz struct {
	mu      sync.Mutex
	denied  map[string]bool
	down    bool
	actions []string
}

func (f *fakeAuthz) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PrincipalID string `json:"principal_id"`
			ActionType  string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		down, deny := f.down, f.denied[req.PrincipalID+"|"+req.ActionType]
		f.actions = append(f.actions, req.ActionType)
		f.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out := "GRANTED"
		if deny {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}
}

func (f *fakeAuthz) deny(principal, action string) {
	f.mu.Lock()
	f.denied[principal+"|"+action] = true
	f.mu.Unlock()
}

type fakeVault struct {
	mu   sync.Mutex
	refs map[string]error
}

func (f *fakeVault) Verify(_ context.Context, ref string, _ vault.Caller) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.refs[strings.ToLower(ref)]; ok {
		return err
	}
	return vault.ErrNotFound
}

type fakeDNS map[string][]string

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

type harness struct {
	r     chi.Router
	mem   *store.MemoryStore
	svc   *connector.Service
	authz *fakeAuthz
	vault *fakeVault
	now   *time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mem := store.NewMemoryStore()
	az := &fakeAuthz{denied: map[string]bool{}}
	azSrv := httptest.NewServer(az.handler())
	t.Cleanup(azSrv.Close)
	v := &fakeVault{refs: map[string]error{credOK: nil}}
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	h := &harness{mem: mem, authz: az, vault: v, now: &now}
	h.svc = &connector.Service{
		Store: mem, Vault: v,
		Egress:               egress.Policy{Resolver: fakeDNS{"api.bank-x.example": {"93.184.216.34"}}},
		EgressAllowedRegions: []string{"uk"},
		Now:                  func() time.Time { return *h.now },
	}
	hd := New(h.svc, mem, authz.NewClient(azSrv.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, hd)
	h.r = r

	pctx := middleware.WithTenant(context.Background(), store.PlatformScopeID)
	admin := connector.Actor{PrincipalID: "platform-admin"}
	if err := h.svc.PublishProfile(pctx, store.PlatformScopeID, admin, &domain.Profile{ProviderKey: "bank-x", ProviderType: "BANK", Version: 2,
		AllowedRegions: []string{"uk"}, AllowedHostSuffixes: []string{"bank-x.example"},
		SupportedScopes: []string{"ingest", "statements.read", "payments.write"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileCertified); err != nil {
		t.Fatal(err)
	}
	mem.ResetEvents()
	return h
}

// call performs one request as (tenant, principal).
func (h *harness) call(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

func decodeBridge(t *testing.T, w *httptest.ResponseRecorder) domain.ApiBridge {
	t.Helper()
	var b domain.ApiBridge
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return b
}

func fullBody() map[string]any {
	return map[string]any{
		"legal_entity_id": "le-1", "bridge_name": "Bank X statements", "protocol": "REST",
		"endpoint_url": "https://api.bank-x.example/v2/feed", "auth_type": "OAUTH2",
		"purpose": "bank-statement-import", "provider_key": "bank-x", "provider_profile_version": 2,
		"credential_ref": credOK, "destination_region": "uk", "sync_mode": "PULL",
		"scopes": []string{"ingest", "statements.read"},
	}
}

// activeBridge drives a connector through the whole approval path over HTTP.
func (h *harness) activeBridge(t *testing.T, tenant string) domain.ApiBridge {
	t.Helper()
	w := h.call("POST", "/v1/bridges", tenant, "maker", fullBody())
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	b := decodeBridge(t, w)
	for _, step := range []struct{ path, principal string }{
		{"submit", "maker"}, {"approve", "checker"}, {"activate", "checker"},
	} {
		w = h.call("POST", "/v1/bridges/"+b.BridgeID+"/"+step.path, tenant, step.principal, map[string]any{"expected_version": b.Version})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
		b = decodeBridge(t, w)
	}
	if b.Status != domain.StatusActive {
		t.Fatalf("status %s", b.Status)
	}
	return b
}

// setupTestRouter keeps the signature the pre-governance tests used.
func setupTestRouter(t *testing.T) (chi.Router, *harness) {
	h := newHarness(t)
	return h.r, h
}

// The pre-governance flow, updated for governance: a connector is created, then
// approved by someone else and activated, then it can ingest.
func TestCreateAndIngestBridge(t *testing.T) {
	_, h := setupTestRouter(t)

	w := h.call("POST", "/v1/bridges", "tenant-test", "principal-01", fullBody())
	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	created := decodeBridge(t, w)
	if created.BridgeID == "" || created.BridgeName != "Bank X statements" || created.ConnectionID != created.BridgeID {
		t.Fatalf("invalid bridge response: %+v", created)
	}
	if created.Status != domain.StatusDraft {
		t.Fatalf("a new connector must start as DRAFT, got %s", created.Status)
	}

	ingest := map[string]any{"payload_summary": "Batch Invoice Ingestion #994", "raw_data": `{"invoices": [101, 102]}`}
	w = h.call("POST", "/v1/bridges/"+created.BridgeID+"/ingest", "tenant-test", "principal-01", ingest)
	if w.Code != http.StatusConflict {
		t.Fatalf("an unapproved connector must not ingest: got %d %s", w.Code, w.Body.String())
	}

	for _, step := range []struct{ path, who string }{{"submit", "principal-01"}, {"approve", "principal-02"}, {"activate", "principal-02"}} {
		cur := decodeBridge(t, h.call("GET", "/v1/bridges/"+created.BridgeID, "tenant-test", "principal-01", nil))
		w = h.call("POST", "/v1/bridges/"+created.BridgeID+"/"+step.path, "tenant-test", step.who, map[string]any{"expected_version": cur.Version})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
	}
	w = h.call("POST", "/v1/bridges/"+created.BridgeID+"/ingest", "tenant-test", "principal-01", ingest)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on ingest, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListBridges(t *testing.T) {
	_, h := setupTestRouter(t)
	w := h.call("GET", "/v1/bridges?legal_entity_id=le-101", "tenant-test", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
}
