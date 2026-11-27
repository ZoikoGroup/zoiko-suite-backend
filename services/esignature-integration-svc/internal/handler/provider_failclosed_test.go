package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/provider"
	"zoiko.io/esignature-integration-svc/internal/store"
)

// testProvider is a TEST-ONLY adapter double. It is never compiled into the
// binary; production builds have no adapter (internal/provider).
type testProvider struct {
	name  string
	calls int
	fail  bool
}

func (p *testProvider) Name() string { return p.name }
func (p *testProvider) Send(_ context.Context, req provider.SendRequest) (provider.SendResult, error) {
	p.calls++
	if p.fail {
		return provider.SendResult{}, errors.New("provider unreachable")
	}
	return provider.SendResult{ExternalRef: "test-ref-" + req.EnvelopeID}, nil
}

// newRouterWith builds the handlers with the given registry. The
// orchestration store is nil: the participant tests below must be refused
// before any store access.
func newRouterWith(t *testing.T, reg *provider.Registry) (chi.Router, *store.MemoryStore, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	az := authz.NewClient(newGrantingAuthzServer(t).URL)
	h := New(st, pub, az, zap.NewNop()).WithProviders(reg)
	oh := NewOrchestrationHandler(nil, st, pub, az, zap.NewNop()).WithProviders(reg)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	RegisterOrchestrationRoutes(r, oh)
	return r, st, pub
}

func doJSON(r http.Handler, method, path, tenant string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var envelopeReq = domain.CreateEnvelopeRequest{
	LegalEntityID: "le-1", Provider: domain.ProviderDocuSign,
	DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S",
}

// The production default (no ESIGN_PROVIDERS) must refuse to create an
// envelope instead of recording it as SENT, and must persist and publish
// nothing.
func TestCreateEnvelope_NoProvider_FailsClosed(t *testing.T) {
	reg, err := provider.Build("") // exactly what cmd/server does by default
	if err != nil {
		t.Fatal(err)
	}
	r, st, pub := newRouterWith(t, reg)

	w := doJSON(r, "POST", "/v1/esignature/envelopes", "tenant-a", envelopeReq)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("esignature_provider_not_configured")) {
		t.Fatalf("want provider_not_configured error, got %s", w.Body.String())
	}
	list, _ := st.ListEnvelopes(middleware.WithTenant(context.Background(), "tenant-a"), "")
	if len(list) != 0 {
		t.Fatalf("nothing may be persisted without a provider, found %d envelopes", len(list))
	}
	if len(pub.Events) != 0 {
		t.Fatalf("no event may be published without a provider, got %d", len(pub.Events))
	}
}

// Provider-asserted statuses cannot be recorded when the envelope's provider
// has no adapter; VOIDED (a local decision) still can.
func TestUpdateStatus_NoProvider_RefusesProviderAssertedStatuses(t *testing.T) {
	r, st, _ := newRouterWith(t, provider.NewRegistry())
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := &domain.SignatureEnvelope{LegalEntityID: "le-1", Provider: domain.ProviderDocuSign,
		DocumentTitle: "MSA", SignerEmail: "s@example.com", SignerName: "S", Status: domain.EnvelopeSent}
	if err := st.CreateEnvelope(ctx, env); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{domain.EnvelopeSent, domain.EnvelopeDelivered, domain.EnvelopeSigned} {
		w := doJSON(r, "POST", "/v1/esignature/envelopes/"+env.EnvelopeID+"/status", "tenant-a",
			domain.UpdateStatusRequest{Status: status})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without provider: want 503, got %d: %s", status, w.Code, w.Body.String())
		}
	}
	got, _ := st.GetEnvelopeByID(ctx, env.EnvelopeID)
	if got.Status != domain.EnvelopeSent {
		t.Fatalf("status must be unchanged, got %s", got.Status)
	}
	w := doJSON(r, "POST", "/v1/esignature/envelopes/"+env.EnvelopeID+"/status", "tenant-a",
		domain.UpdateStatusRequest{Status: domain.EnvelopeVoided})
	if w.Code != http.StatusOK {
		t.Fatalf("VOIDED is local: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateStatus_UnknownStatus_400(t *testing.T) {
	r, _ := setupTestRouter(t)
	id := createEnvelopeAs(t, r, "tenant-a", "le-a")
	w := doJSON(r, "POST", "/v1/esignature/envelopes/"+id+"/status", "tenant-a", domain.UpdateStatusRequest{Status: "COMPLETED"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestParticipantProgress_NoProvider_503(t *testing.T) {
	r, _, _ := newRouterWith(t, provider.NewRegistry())
	for _, p := range []string{"mark-viewed", "mark-signed", "decline"} {
		w := doJSON(r, "POST", "/v1/esignature/participants/p-1/"+p, "tenant-a", map[string]string{"reason": "x"})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without provider: want 503, got %d: %s", p, w.Code, w.Body.String())
		}
	}
}

// With an adapter configured, creation calls the provider exactly once and
// stores SENT together with the provider's reference.
func TestCreateEnvelope_WithProvider_RecordsProviderRef(t *testing.T) {
	tp := &testProvider{name: domain.ProviderDocuSign}
	r, _, _ := newRouterWith(t, provider.NewRegistry(tp))
	w := doJSON(r, "POST", "/v1/esignature/envelopes", "tenant-a", envelopeReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var env domain.SignatureEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if tp.calls != 1 || env.Status != domain.EnvelopeSent || env.ExternalRef != "test-ref-"+env.EnvelopeID {
		t.Fatalf("calls=%d status=%s ref=%s", tp.calls, env.Status, env.ExternalRef)
	}
}

// A failed provider call leaves the envelope PENDING and says so (502).
func TestCreateEnvelope_ProviderFailure_StaysPending(t *testing.T) {
	tp := &testProvider{name: domain.ProviderDocuSign, fail: true}
	r, st, _ := newRouterWith(t, provider.NewRegistry(tp))
	w := doJSON(r, "POST", "/v1/esignature/envelopes", "tenant-a", envelopeReq)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
	list, _ := st.ListEnvelopes(middleware.WithTenant(context.Background(), "tenant-a"), "")
	if len(list) != 1 || list[0].Status != domain.EnvelopePending {
		t.Fatalf("want one PENDING envelope, got %+v", list)
	}
}
