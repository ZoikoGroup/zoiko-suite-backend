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
	LoadBalancer *traefikLB       `yaml:"loadBalancer,omitempty"`
	Failover     *traefikFailover `yaml:"failover,omitempty"`
}

type traefikLB struct {
	Servers     []traefikServer `yaml:"servers"`
	HealthCheck *traefikHealth  `yaml:"healthCheck,omitempty"`
}

type traefikServer struct {
	URL string `yaml:"url"`
}

type traefikHealth struct {
	Path     string `yaml:"path"`
	Interval string `yaml:"interval"`
}

// traefikFailover models Traefik v3's failover service: route to Service while
// healthy, else to Fallback. Emitted ONLY when an approved fallback exists.
//
// Traefik's failover.healthCheck is an EMPTY object — it only toggles health
// checking on the failover service; the actual probe (path/interval) lives on
// the primary load-balancer service's healthCheck, not here.
type traefikFailover struct {
	Service     string       `yaml:"service"`
	Fallback    string       `yaml:"fallback"`
	HealthCheck *emptyStruct `yaml:"healthCheck,omitempty"`
}

// emptyStruct marshals to `{}` — used for Traefik's toggle-only healthCheck.
type emptyStruct struct{}

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
	primaryPriority   = 100
	catchAllPriority  = 1 // lowest, so explicit tenant routes always win (§Appendix D)
	healthCheckPath   = "/healthz"
	healthCheckPeriod = "10s"
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

		// Per-tenant context middleware: set trusted internal headers AFTER
		// the strip middleware has removed any client-supplied copies.
		cfg.HTTP.Middlewares[ctxMW] = traefikMiddleware{Headers: &traefikHeaders{
			CustomRequestHeaders: map[string]string{
				"X-Zoiko-Resolved-Tenant":  slug,
				"X-Zoiko-Resolved-Region":  t.PrimaryRegion,
				"X-Zoiko-GTRM-Map-Version": strconv.Itoa(m.MapVersion),
			},
		}}

		cfg.HTTP.Routers[routerName] = traefikRouter{
			Rule:        fmt.Sprintf("Host(`%s.%s`)", slug, m.EnvDomain),
			Service:     svcName,
			Middlewares: []string{edgeStripMW, ctxMW},
			Priority:    primaryPriority,
		}

		primaryURL := poolURL(cat.pool(t.PrimaryRegion))

		if t.FallbackRegion == nil {
			// No approved fallback → single load balancer, no failover service
			// (§4.2 fallback validation: restraint enforced by absence).
			cfg.HTTP.Services[svcName] = traefikService{
				LoadBalancer: &traefikLB{Servers: []traefikServer{{URL: primaryURL}}},
			}
		} else {
			// Approved fallback → Traefik failover service with a health check
			// on the primary; fallback stays within allowed_regions (validated).
			primarySvc := "gtrm-primary-" + slug
			fallbackSvc := "gtrm-fallback-" + slug
			cfg.HTTP.Services[svcName] = traefikService{Failover: &traefikFailover{
				Service:     primarySvc,
				Fallback:    fallbackSvc,
				HealthCheck: &emptyStruct{},
			}}
			cfg.HTTP.Services[primarySvc] = traefikService{LoadBalancer: &traefikLB{
				Servers:     []traefikServer{{URL: primaryURL}},
				HealthCheck: &traefikHealth{Path: healthCheckPath, Interval: healthCheckPeriod},
			}}
			// The fallback also needs a health check: Traefik requires health
			// checks on BOTH members of a failover service to register the
			// fallback as an updater.
			cfg.HTTP.Services[fallbackSvc] = traefikService{LoadBalancer: &traefikLB{
				Servers:     []traefikServer{{URL: poolURL(cat.pool(*t.FallbackRegion))}},
				HealthCheck: &traefikHealth{Path: healthCheckPath, Interval: healthCheckPeriod},
			}}
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
