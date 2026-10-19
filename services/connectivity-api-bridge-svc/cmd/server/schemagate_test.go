package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/config"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/integration"
)

// registryStandIn serves one contract for connector.approved and nothing else.
func registryStandIn(t *testing.T, down *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/v1/schemas/connector.approved/versions/latest" || r.Header.Get("X-Principal-Id") == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"event_name": "connector.approved", "version": 3, "owning_service": "connectivity-api-bridge-svc",
			"json_schema": json.RawMessage(`{"type":"object","required":["connector_id","approved_by"],"properties":{"connector_id":{"type":"string"},"approved_by":{"type":"string"}}}`),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func connectorEvent(t *testing.T, eventType string, data map[string]any) (string, []byte) {
	t.Helper()
	env, err := envelope.New(envelope.Spec{Type: eventType, Service: "connectivity-api-bridge-svc", SchemaVersion: "1.0.0", OccurredAt: time.Now(),
		TenantID: "tenant-1", AggregateType: "connector", AggregateID: "c-1", ResidencyRegion: "uk", Classification: envelope.Confidential, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return eventType, raw
}

// INT-CTRL-005 / INT-NP-006 at the service: with a registry configured, the connectivity relay is gated. The event a
// producer would send is quarantined when it breaks its contract, passes when it keeps it, and is HELD (never published
// unchecked) when the registry cannot answer.
func TestTheConnectivityRelayIsGatedByTheRegisteredContract(t *testing.T) {
	var down atomic.Bool
	srv := registryStandIn(t, &down)
	g, err := schemaGate(&config.Config{SchemaRegistryURL: srv.URL, SchemaGatePrincipal: "connectivity-api-bridge-svc"})
	if err != nil || g == nil {
		t.Fatalf("a configured registry builds a gate: %v %v", g, err)
	}
	ctx := context.Background()
	const typ = "com.zoikosuite.connectivity.connector.approved"

	et, ok := connectorEvent(t, typ, map[string]any{"connector_id": "c-1", "approved_by": "alice"})
	if err := g.Check(ctx, et, ok); err != nil {
		t.Fatalf("an event that keeps its contract passes: %v", err)
	}
	et, bad := connectorEvent(t, typ, map[string]any{"connector_id": "c-1"})
	err = g.Check(ctx, et, bad)
	e, isProblem := integration.As(err)
	if !isProblem || e.Code != integration.CodeSchemaMismatch {
		t.Fatalf("an event that breaks its contract is refused for the relay to quarantine: %v", err)
	}
	if errors.Is(err, integration.ErrRegistryUnavailable) {
		t.Fatal("a contract violation is not an outage")
	}

	down.Store(true)
	// a fresh event name so the gate's cache cannot answer for the registry
	et, fresh := connectorEvent(t, "com.zoikosuite.connectivity.connector.suspended", map[string]any{"connector_id": "c-1"})
	if err := g.Check(ctx, et, fresh); !errors.Is(err, integration.ErrRegistryUnavailable) {
		t.Fatalf("a registry that cannot answer must hold the event, not publish it unchecked: %v", err)
	}
}

func TestNoRegistryMeansNoGateAndABadRegistryIsRefused(t *testing.T) {
	if g, err := schemaGate(&config.Config{}); g != nil || err != nil {
		t.Fatalf("an unconfigured registry leaves the gate off: %v %v", g, err)
	}
	if _, err := schemaGate(&config.Config{SchemaRegistryURL: "not a url", SchemaGatePrincipal: "x"}); err == nil {
		t.Fatal("a malformed registry URL is a startup error, not a silently ungated relay")
	}
	if _, err := schemaGate(&config.Config{SchemaRegistryURL: "http://registry:8080"}); err == nil {
		t.Fatal("the registry needs a workload identity to read contracts")
	}
	if c := config.Load(); !c.SchemaGateAllowUnregistered || c.SchemaRegistryURL != "" {
		t.Fatalf("defaults: off, and lenient about unregistered events while a rollout happens: %+v", c)
	}
}
