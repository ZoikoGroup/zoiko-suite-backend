package main

import (
	"fmt"
	"sort"
	"strconv"
)

// ── Traefik dynamic-configuration model (file provider) ──────────────────────

type traefikConfig struct {
	HTTP traefikHTTP `yaml:"http"`
}

type traefikHTTP struct {
	Routers     map[string]traefikRouter     `yaml:"routers"`
	Services    map[string]traefikService    `yaml:"services"`
	Middlewares map[string]traefikMiddleware `yaml:"middlewares"`
}

type traefikRouter struct {
	Rule        string   `yaml:"rule"`
	Service     string   `yaml:"service"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	Priority    int      `yaml:"priority,omitempty"`
}

type traefikService struct {
	LoadBalancer *traefikLB `yaml:"loadBalancer,omitempty"`
}

type traefikLB struct {
	Servers []traefikServer `yaml:"servers"`
}

type traefikServer struct {
	URL string `yaml:"url"`
}

type traefikMiddleware struct {
	Headers *traefikHeaders `yaml:"headers,omitempty"`
}

type traefikHeaders struct {
	CustomRequestHeaders map[string]string `yaml:"customRequestHeaders,omitempty"`
}

// untrustedInboundHeaders are the internal routing-context headers that must be
// stripped from any client request at the edge before trusted values are set
// (§6.2 trusted header injection, §10.2 proof headers). In Traefik, setting a
// customRequestHeader to "" removes it.
var untrustedInboundHeaders = []string{
	"X-Zoiko-Tenant",
	"X-Zoiko-Resolved-Tenant",
	"X-Zoiko-Resolved-Region",
	"X-Zoiko-Residency-Policy",
	"X-Zoiko-Route-Decision",
	"X-Zoiko-GTRM-State",
	"X-Zoiko-GTRM-Map-Version",
}

const (
	backendPort       = "8080" // pools run as non-root (distroless); can't bind <1024
	edgeStripMW       = "gtrm-edge-strip"
	safeRouter        = "gtrm-catchall-safe"
	safeService       = "gtrm-safe-endpoint"
	safeBackend       = "quarantine-terminator" // residency-neutral, no tenant data (§8.1)
	primaryPriority  = 100
	catchAllPriority = 1 // lowest, so explicit tenant routes always win (§Appendix D)
)

// Emit compiles a validated routing map + region catalog into Traefik dynamic
// configuration. It MUST only be called after Validate returns no errors.
//
// Fail-closed (§8.1): tenants whose routing_status is not ACTIVE get NO
// data-bearing router emitted, so their traffic falls through to the
// lowest-priority catch-all safe route rather than any regional pool.
func Emit(m RoutingMap, cat RegionCatalog) traefikConfig {
	cfg := traefikConfig{HTTP: traefikHTTP{
		Routers:     map[string]traefikRouter{},
		Services:    map[string]traefikService{},
		Middlewares: map[string]traefikMiddleware{},
	}}

	// Shared edge middleware: strip all untrusted inbound routing-context
	// headers. Runs before any per-tenant context middleware.
	strip := map[string]string{}
	for _, h := range untrustedInboundHeaders {
		strip[h] = ""
	}
	cfg.HTTP.Middlewares[edgeStripMW] = traefikMiddleware{
		Headers: &traefikHeaders{CustomRequestHeaders: strip},
	}

	for _, t := range m.Tenants {
		if t.RoutingStatus != StatusActive {
			// Fail-closed: no route emitted → falls through to catch-all.
			continue
		}

		slug := t.TenantSlug
		routerName := "gtrm-" + slug
		svcName := "gtrm-svc-" + slug
		ctxMW := "gtrm-ctx-" + slug

		// The region traffic is currently routed to: primary normally, or the
		// approved fallback when an operator has manually activated failover
		// (§8.3/§8.4). Failover and failback are compiled routing-map changes,
		// which makes failback inherently STICKY — there is no Traefik auto
		// flap-back when the primary recovers.
		active := t.activeRegion()

		// Per-tenant context middleware: set trusted internal headers AFTER
		// the strip middleware has removed any client-supplied copies. The
		// resolved region is the ACTIVE region, so backend region assertion in
		// the target pool matches.
		cfg.HTTP.Middlewares[ctxMW] = traefikMiddleware{Headers: &traefikHeaders{
			CustomRequestHeaders: map[string]string{
				"X-Zoiko-Resolved-Tenant":  slug,
				"X-Zoiko-Resolved-Region":  active,
				"X-Zoiko-GTRM-Map-Version": strconv.Itoa(m.MapVersion),
			},
		}}

		cfg.HTTP.Routers[routerName] = traefikRouter{
			Rule:        fmt.Sprintf("Host(`%s.%s`)", slug, m.EnvDomain),
			Service:     svcName,
			Middlewares: []string{edgeStripMW, ctxMW},
			Priority:    primaryPriority,
		}

		// Single load balancer to the active region's pool. A tenant with no
		// approved fallback can never be failed over (validation rejects
		// failover_active without a fallback), so when its single pool is down
		// it simply fails — it never spills to a non-compliant region (test D).
		cfg.HTTP.Services[svcName] = traefikService{
			LoadBalancer: &traefikLB{Servers: []traefikServer{{URL: poolURL(cat.pool(active))}}},
		}
	}

	// Lowest-priority catch-all → safe, residency-neutral endpoint. Terminates
	// unresolved / suspended / unknown-host traffic without touching a regional
	// pool (§8.1 fail-closed).
	cfg.HTTP.Routers[safeRouter] = traefikRouter{
		Rule:     "HostRegexp(`^.+$`)",
		Service:  safeService,
		Priority: catchAllPriority,
	}
	cfg.HTTP.Services[safeService] = traefikService{
		LoadBalancer: &traefikLB{Servers: []traefikServer{{URL: poolURL(safeBackend)}}},
	}

	return cfg
}

func poolURL(pool string) string {
	return fmt.Sprintf("http://%s:%s", pool, backendPort)
}

// routerNames returns the emitted router names sorted — used by tests and for
// deterministic diffing.
func (c traefikConfig) routerNames() []string {
	names := make([]string, 0, len(c.HTTP.Routers))
	for n := range c.HTTP.Routers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
