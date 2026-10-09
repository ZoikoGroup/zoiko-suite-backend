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
	EntryPoints []string `yaml:"entryPoints,omitempty"`
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
	Headers      *traefikHeaders      `yaml:"headers,omitempty"`
	ForwardAuth  *traefikForwardAuth  `yaml:"forwardAuth,omitempty"`
}

type traefikHeaders struct {
	CustomRequestHeaders map[string]string `yaml:"customRequestHeaders,omitempty"`
}

type traefikForwardAuth struct {
	Address        string `yaml:"address"`
	TrustForwardHeader bool   `yaml:"trustForwardHeader,omitempty"`
	AuthResponseHeaders []string `yaml:"authResponseHeaders,omitempty"`
}

// untrustedInboundHeaders are the internal routing-context headers AND
// governance envelope headers that must be stripped from any client request
// at the edge before trusted values are set (§6.2 trusted header injection,
// §10.2 proof headers, §5 provenance classes). In Traefik, setting a
// customRequestHeader to "" removes it.
var untrustedInboundHeaders = []string{
	// GTRM routing headers (existing)
	"X-Zoiko-Tenant",
	"X-Zoiko-Resolved-Tenant",
	"X-Zoiko-Resolved-Tenant-Id",
	"X-Zoiko-Resolved-Region",
	"X-Zoiko-Residency-Policy",
	"X-Zoiko-Route-Decision",
	"X-Zoiko-GTRM-State",
	"X-Zoiko-GTRM-Map-Version",

	// Identity-class envelope headers. Nothing a client writes can establish a
	// workload identity or a support session, so they are stripped here and
	// again by ForwardAuth (see gatewayAuthResponseHeaders).
	"X-Workload-Id",
	"X-Support-Context-Id",

	// NOT stripped, deliberately: X-Purpose-Context, X-Approval-Reference,
	// X-Evidence-Refs, X-Causation-Id, X-Workflow-Instance-Id, X-Book-Id,
	// X-Source-Channel, X-Expected-Version. The §4 envelope contract makes these
	// caller-written assertions that the owning service validates. Stripping
	// them failed every service that requires purpose or book with 400
	// envelope_incomplete, and silently disabled optimistic concurrency.
}

// gatewayAuthResponseHeaders is the ForwardAuth authResponseHeaders list.
// Traefik deletes each name from the forwarded request and then copies the
// gateway's value, if it set one, so the list does two jobs: it makes every
// header gateway-auth-svc's Verify sets authoritative, and it strips the ones
// the gateway never sets. A header the handler sets and this list omits
// passes the client's copy through untouched. It must match the compose
// "gateway-auth" middleware; gateway-auth-svc's edge-contract test checks both.
var gatewayAuthResponseHeaders = []string{
	"X-Principal-Id",
	"X-Tenant-Id",
	"X-Legal-Entity-Id",
	"X-Correlation-Id",
	"X-Jurisdiction-Context",
	"X-Timezone",
	"X-Residency-Policy-Id",
	"X-Tenant-Context-Stale",
	"X-Workload-Id",
	"X-Support-Context-Id",
}

const (
	backendPort       = "8080" // pools run as non-root (distroless); can't bind <1024
	edgeStripMW       = "gtrm-edge-strip"
	authMW            = "gateway-auth"
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

	// gateway-auth ForwardAuth middleware: calls gateway-auth-svc /verify
	// to validate every request's identity envelope. Configured once and
	// shared by all tenant routers.
	//
	// trustForwardHeader stays off. With it on, Traefik passes the incoming
	// X-Forwarded-Method/-Uri/-For to /verify instead of setting them from
	// the real request, so behind any proxy the entrypoint trusts (the GCP
	// L7 load balancer in production), a client's own "X-Forwarded-Method:
	// GET" on a POST made the gateway score a write as a read.
	cfg.HTTP.Middlewares[authMW] = traefikMiddleware{
		ForwardAuth: &traefikForwardAuth{
			Address:             "http://gateway-auth-svc:8092/verify",
			AuthResponseHeaders: gatewayAuthResponseHeaders,
		},
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

		// Decide the target pool + trusted routing context for this tenant.
		//
		// Precedence: an active quarantine (§9) overrides normal routing. Then
		// the active region — primary normally, or the approved fallback when an
		// operator has manually activated failover (§8.3/§8.4). Failover,
		// failback and quarantine are all compiled routing-map changes, so all
		// are inherently STICKY — no Traefik auto flap-back.
		var targetPool, resolvedRegion, gtrmState string
		switch {
		case t.QuarantineActive && t.QuarantineMode == QuarantineBlock:
			// BLOCK: divert to the residency-neutral terminator. No region is
			// resolved because no tenant data is processed (§9.1).
			targetPool, resolvedRegion, gtrmState = safeBackend, "", "quarantined"
		case t.QuarantineActive && t.QuarantineMode == QuarantineIsolated:
			// ISOLATED_SERVE: region-scoped quarantine pool (validated in-boundary).
			targetPool, resolvedRegion, gtrmState = *t.QuarantinePool, t.activeRegion(), "quarantined"
		default:
			resolvedRegion = t.activeRegion()
			targetPool, gtrmState = cat.pool(resolvedRegion), "normal"
		}

		// Per-tenant context middleware: set trusted internal headers AFTER the
		// strip middleware removed any client-supplied copies. The resolved
		// region is the ACTIVE region, so backend region assertion matches.
		//
		// X-Zoiko-Resolved-Tenant-Id carries the canonical tenant_id (not the
		// human-readable slug) alongside the existing slug header — added so
		// gateway-auth-svc can compare a session token's tenant_id claim
		// directly against the hostname-resolved tenant (acceptance test O,
		// decision doc §6.2/§11) without needing a live slug-to-id lookup.
		cfg.HTTP.Middlewares[ctxMW] = traefikMiddleware{Headers: &traefikHeaders{
			CustomRequestHeaders: map[string]string{
				"X-Zoiko-Resolved-Tenant":    slug,
				"X-Zoiko-Resolved-Tenant-Id": t.TenantID,
				"X-Zoiko-Resolved-Region":    resolvedRegion,
				"X-Zoiko-GTRM-Map-Version":   strconv.Itoa(m.MapVersion),
				"X-Zoiko-GTRM-State":         gtrmState,
			},
		}}

		cfg.HTTP.Routers[routerName] = traefikRouter{
			Rule:        fmt.Sprintf("Host(`%s.%s`)", slug, m.EnvDomain),
			Service:     svcName,
			// ctx BEFORE auth. Traefik runs these in order, and ForwardAuth
			// sends /verify the request as it stands at that point. With auth
			// second, the edge had just deleted X-Zoiko-Resolved-Tenant-Id and
			// ctx had not yet set it, so the token/hostname tenant check
			// (acceptance test O) saw no header and never compared anything.
			Middlewares: []string{edgeStripMW, ctxMW, authMW},
			Priority:    primaryPriority,
			EntryPoints: []string{"web", "websecure"},
		}

		// Single load balancer to the target pool. A tenant with no approved
		// fallback can never be failed over (validation rejects failover_active
		// without a fallback), so when its single pool is down it simply fails —
		// it never spills to a non-compliant region (test D).
		cfg.HTTP.Services[svcName] = traefikService{
			LoadBalancer: &traefikLB{Servers: []traefikServer{{URL: poolURL(targetPool)}}},
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
