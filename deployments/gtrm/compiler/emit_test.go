package main

import (
	"slices"
	"strings"
	"testing"
)

// EU-only tenant → single LB to eu-pool, no failover service, header context set.
func TestEmit_NoFallback_SingleLoadBalancer(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())

	svc, ok := cfg.HTTP.Services["gtrm-svc-acme"]
	if !ok {
		t.Fatal("expected service gtrm-svc-acme")
	}
	if svc.LoadBalancer == nil || svc.LoadBalancer.Servers[0].URL != "http://eu-pool:8080" {
		t.Fatalf("expected single LB to eu-pool, got: %+v", svc.LoadBalancer)
	}

	// Router points at the service, uses strip + ctx + auth middlewares, host rule set.
	r := cfg.HTTP.Routers["gtrm-acme"]
	if r.Rule != "Host(`acme.zoikosuite.dev.internal`)" {
		t.Fatalf("unexpected router rule: %s", r.Rule)
	}
	if len(r.Middlewares) != 3 || r.Middlewares[0] != edgeStripMW || r.Middlewares[1] != "gtrm-ctx-acme" || r.Middlewares[2] != authMW {
		t.Fatalf("expected [edge-strip, ctx, gateway-auth] middlewares, got: %v", r.Middlewares)
	}
}

// Manual failover model: failover_active=false routes to the primary region.
func TestEmit_FailoverInactive_RoutesToPrimary(t *testing.T) {
	atlas := Tenant{
		TenantID: "tenant_atlas_multi", TenantSlug: "atlas",
		DataResidencyPolicyID: "residency_multi_002",
		AllowedRegions:        []string{"eu", "uk"},
		PrimaryRegion:         "eu", FallbackRegion: ptr("uk"),
		FailoverActive: false,
		QuarantineMode: QuarantineBlock, RoutingStatus: StatusActive,
	}
	cfg := Emit(validMap(atlas), testCatalog())

	if got := cfg.HTTP.Services["gtrm-svc-atlas"].LoadBalancer.Servers[0].URL; got != "http://eu-pool:8080" {
		t.Fatalf("failover inactive should route to primary eu-pool, got %s", got)
	}
	if got := cfg.HTTP.Middlewares["gtrm-ctx-atlas"].Headers.CustomRequestHeaders["X-Zoiko-Resolved-Region"]; got != "eu" {
		t.Fatalf("resolved region should be eu when failover inactive, got %s", got)
	}
}

// failover_active=true routes to the approved fallback, and the resolved-region
// header follows so backend region assertion in the fallback pool matches.
// (Sticky: nothing here auto-reverts — only a map change flips it back.)
func TestEmit_FailoverActive_RoutesToFallback(t *testing.T) {
	atlas := Tenant{
		TenantID: "tenant_atlas_multi", TenantSlug: "atlas",
		DataResidencyPolicyID: "residency_multi_002",
		AllowedRegions:        []string{"eu", "uk"},
		PrimaryRegion:         "eu", FallbackRegion: ptr("uk"),
		FailoverActive: true,
		QuarantineMode: QuarantineBlock, RoutingStatus: StatusActive,
	}
	cfg := Emit(validMap(atlas), testCatalog())

	if got := cfg.HTTP.Services["gtrm-svc-atlas"].LoadBalancer.Servers[0].URL; got != "http://uk-pool:8080" {
		t.Fatalf("failover active should route to fallback uk-pool, got %s", got)
	}
	if got := cfg.HTTP.Middlewares["gtrm-ctx-atlas"].Headers.CustomRequestHeaders["X-Zoiko-Resolved-Region"]; got != "uk" {
		t.Fatalf("resolved region should be uk when failover active, got %s", got)
	}
}

// Manual BLOCK quarantine diverts the tenant to the residency-neutral
// terminator with no resolved region and state=quarantined (§9, test F).
func TestEmit_QuarantineBlock_DivertsToTerminator(t *testing.T) {
	tn := validTenant() // QuarantineMode = BLOCK
	tn.QuarantineActive = true
	cfg := Emit(validMap(tn), testCatalog())

	if got := cfg.HTTP.Services["gtrm-svc-acme"].LoadBalancer.Servers[0].URL; got != "http://quarantine-terminator:8080" {
		t.Fatalf("BLOCK quarantine must divert to the terminator, got %s", got)
	}
	hdrs := cfg.HTTP.Middlewares["gtrm-ctx-acme"].Headers.CustomRequestHeaders
	if hdrs["X-Zoiko-GTRM-State"] != "quarantined" {
		t.Fatalf("expected state=quarantined, got %s", hdrs["X-Zoiko-GTRM-State"])
	}
	if hdrs["X-Zoiko-Resolved-Region"] != "" {
		t.Fatalf("quarantined BLOCK request must resolve no region, got %s", hdrs["X-Zoiko-Resolved-Region"])
	}
}

// Rollback: quarantine_active=false restores normal routing (§9.3, test K).
func TestEmit_QuarantineInactive_NormalRoute(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())
	if got := cfg.HTTP.Services["gtrm-svc-acme"].LoadBalancer.Servers[0].URL; got != "http://eu-pool:8080" {
		t.Fatalf("not quarantined -> normal eu-pool route, got %s", got)
	}
	if got := cfg.HTTP.Middlewares["gtrm-ctx-acme"].Headers.CustomRequestHeaders["X-Zoiko-GTRM-State"]; got != "normal" {
		t.Fatalf("expected state=normal, got %s", got)
	}
}

// Fail-closed: a SUSPENDED tenant gets NO data-bearing router (§8.1).
func TestEmit_SuspendedTenant_NoRouter(t *testing.T) {
	tn := validTenant()
	tn.RoutingStatus = StatusSuspended
	cfg := Emit(validMap(tn), testCatalog())

	if _, ok := cfg.HTTP.Routers["gtrm-acme"]; ok {
		t.Fatal("SUSPENDED tenant must not get a data-bearing router")
	}
	// but the catch-all safe route must still exist
	if _, ok := cfg.HTTP.Routers[safeRouter]; !ok {
		t.Fatal("expected the fail-closed catch-all safe router")
	}
}

// The catch-all safe route is always emitted, lowest priority, to a neutral pool.
func TestEmit_CatchAllSafeRoute_Always(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())

	r, ok := cfg.HTTP.Routers[safeRouter]
	if !ok {
		t.Fatal("expected catch-all safe router")
	}
	if r.Priority != catchAllPriority {
		t.Fatalf("catch-all must be lowest priority %d, got %d", catchAllPriority, r.Priority)
	}
	if r.Priority >= primaryPriority {
		t.Fatal("catch-all priority must be below tenant-route priority")
	}
	if cfg.HTTP.Services[safeService].LoadBalancer.Servers[0].URL != "http://quarantine-terminator:8080" {
		t.Fatal("safe route must target the residency-neutral terminator")
	}
}

// Edge strip middleware removes every untrusted inbound routing header.
func TestEmit_EdgeStrip_RemovesUntrustedHeaders(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())
	mw, ok := cfg.HTTP.Middlewares[edgeStripMW]
	if !ok || mw.Headers == nil {
		t.Fatal("expected edge-strip headers middleware")
	}
	for _, h := range untrustedInboundHeaders {
		v, present := mw.Headers.CustomRequestHeaders[h]
		if !present || v != "" {
			t.Fatalf("header %s must be stripped (set to empty), got present=%v value=%q", h, present, v)
		}
	}
}

// The edge splits the envelope: identity-class headers never survive from the
// client, while §4 caller assertions reach the owning service, which validates
// them. Named literally rather than read from the lists under test, so a
// change to either list cannot pass by changing both.
func TestEmit_EnvelopeSanitation_SplitPolicy(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())
	strip := cfg.HTTP.Middlewares[edgeStripMW].Headers.CustomRequestHeaders
	fa := cfg.HTTP.Middlewares[authMW].ForwardAuth
	overwritten := map[string]bool{}
	for _, h := range fa.AuthResponseHeaders {
		overwritten[h] = true
	}

	// Every header gateway-auth-svc sets must be overwritten by ForwardAuth,
	// or a client-supplied copy reaches the backend as if verified.
	for _, h := range []string{
		"X-Principal-Id", "X-Tenant-Id", "X-Legal-Entity-Id", "X-Correlation-Id",
		"X-Jurisdiction-Context", "X-Timezone", "X-Residency-Policy-Id", "X-Tenant-Context-Stale",
	} {
		if !overwritten[h] {
			t.Errorf("%s is set by gateway-auth but not in authResponseHeaders — client copy passes through", h)
		}
	}
	for _, h := range []string{"X-Workload-Id", "X-Support-Context-Id"} {
		if _, ok := strip[h]; !ok || !overwritten[h] {
			t.Errorf("%s must be stripped at the edge and by ForwardAuth", h)
		}
	}
	for _, h := range []string{
		"X-Purpose-Context", "X-Approval-Reference", "X-Evidence-Refs", "X-Causation-Id",
		"X-Workflow-Instance-Id", "X-Book-Id", "X-Source-Channel", "X-Expected-Version",
	} {
		if _, ok := strip[h]; ok || overwritten[h] {
			t.Errorf("%s is a §4 caller assertion and must reach the owning service", h)
		}
	}
}

// Production ingress is this compiled config (GCP runbook Step 13), so it is
// where "every route is authenticated" has to hold. Checked against the real
// routing map, not a fixture: every router that reaches a tenant pool must run
// exactly [edge-strip, its own ctx, gateway-auth]. Order is the point: ctx
// must set X-Zoiko-Resolved-Tenant-Id before ForwardAuth, or /verify never
// sees it and the token/hostname tenant check (test O) is dead. The only
// router exempt is the catch-all, which terminates at the safe backend.
func TestEmit_RealMap_EveryDataBearingRouterIsForwardAuthed(t *testing.T) {
	m, err := loadRoutingMap("../routing-map.yaml")
	if err != nil {
		t.Fatalf("load routing map: %v", err)
	}
	cat, err := loadRegions("../regions.yaml")
	if err != nil {
		t.Fatalf("load regions: %v", err)
	}
	if errs := Validate(m, cat, false); len(errs) > 0 {
		t.Fatalf("routing map invalid: %v", errs)
	}
	cfg := Emit(m, cat)
	if len(cfg.HTTP.Routers) < 2 {
		t.Fatalf("expected tenant routers plus the catch-all, got %v", cfg.routerNames())
	}
	for name, r := range cfg.HTTP.Routers {
		if name == safeRouter {
			if r.Service != safeService {
				t.Errorf("catch-all must terminate at %s, got %s", safeService, r.Service)
			}
			continue
		}
		ctx := "gtrm-ctx-" + strings.TrimPrefix(name, "gtrm-")
		want := []string{edgeStripMW, ctx, authMW}
		if !slices.Equal(r.Middlewares, want) {
			t.Errorf("router %s reaches %s with %v, want %v", name, r.Service, r.Middlewares, want)
		}
		if cfg.HTTP.Middlewares[ctx].Headers.CustomRequestHeaders["X-Zoiko-Resolved-Tenant-Id"] == "" {
			t.Errorf("%s must set X-Zoiko-Resolved-Tenant-Id for /verify to compare", ctx)
		}
	}
	// With trustForwardHeader on, a client's X-Forwarded-Method reaches
	// /verify through any trusted proxy, and a POST can present as a GET.
	if cfg.HTTP.Middlewares[authMW].ForwardAuth.TrustForwardHeader {
		t.Error("gateway-auth ForwardAuth must not trust inbound X-Forwarded-* headers")
	}
}

// Per-tenant context middleware sets trusted resolved-region/tenant headers.
func TestEmit_ContextMiddleware_SetsTrustedHeaders(t *testing.T) {
	cfg := Emit(validMap(validTenant()), testCatalog())
	mw := cfg.HTTP.Middlewares["gtrm-ctx-acme"]
	if mw.Headers == nil {
		t.Fatal("expected ctx headers middleware")
	}
	if mw.Headers.CustomRequestHeaders["X-Zoiko-Resolved-Region"] != "eu" {
		t.Fatal("ctx middleware must set resolved region = eu")
	}
	if mw.Headers.CustomRequestHeaders["X-Zoiko-Resolved-Tenant"] != "acme" {
		t.Fatal("ctx middleware must set resolved tenant = acme")
	}
	if mw.Headers.CustomRequestHeaders["X-Zoiko-Resolved-Tenant-Id"] != "tenant_acme_eu" {
		t.Fatal("ctx middleware must set resolved tenant id = tenant_acme_eu — gateway-auth-svc compares this against a session token's tenant_id claim (test O)")
	}
}
