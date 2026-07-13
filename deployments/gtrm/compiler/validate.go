package main

import (
	"fmt"
	"regexp"
	"strings"
)

// slugPattern — canonical tenant slug: lowercase alphanumerics and single
// hyphens, 1–63 chars, no leading/trailing hyphen. Used as a DNS label.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedSlugs must never be used as tenant slugs (they collide with platform
// hostnames or the safe-resolution path).
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "admin": true, "gateway": true,
	"internal": true, "safe": true, "quarantine": true,
}

// Validate runs every §4.2 gate over the routing map against the region
// catalog. It returns ALL violations found (not just the first) so a bad map
// surfaces every problem at once. A non-empty result means the compiler must
// reject the map and emit no config (§4.2, acceptance test H).
//
// requireProdSafe mirrors the compiler's --require-prod-safe flag: when the
// target deployment is production-equivalent, a map marked env:dev (or a
// dev-only domain) is rejected (§4.2 "environment validation").
func Validate(m RoutingMap, cat RegionCatalog, requireProdSafe bool) []string {
	var errs []string

	// ── Top-level schema ──────────────────────────────────────────────────
	if m.SchemaVersion == 0 {
		errs = append(errs, "schema_version is required")
	}
	if m.MapVersion == 0 {
		errs = append(errs, "map_version is required and must be > 0 (it is stamped into logs)")
	}
	if m.EnvDomain == "" {
		errs = append(errs, "env_domain is required (it forms the {tenant_slug}.{env_domain} routing key)")
	}
	switch m.Env {
	case "dev", "staging", "prod":
	default:
		errs = append(errs, fmt.Sprintf("env %q invalid (want dev|staging|prod)", m.Env))
	}
	if len(m.Tenants) == 0 {
		errs = append(errs, "routing map has no tenants — nothing to compile")
	}

	// ── Environment validation (§4.2) ─────────────────────────────────────
	// Do not generate production-equivalent routing from a dev-only map.
	if requireProdSafe && m.Env != "prod" {
		errs = append(errs, fmt.Sprintf("refusing to generate prod-equivalent config from env=%q map", m.Env))
	}

	seenSlug := map[string]string{} // slug -> tenant_id, for duplicate detection

	for _, t := range m.Tenants {
		id := t.TenantID
		if id == "" {
			errs = append(errs, "a tenant is missing tenant_id")
			id = "<unknown>"
		}

		// Tenant slug: canonical, unique, non-reserved (§4.2, §6.1).
		switch {
		case t.TenantSlug == "":
			errs = append(errs, fmt.Sprintf("%s: tenant_slug is required", id))
		case !slugPattern.MatchString(t.TenantSlug):
			errs = append(errs, fmt.Sprintf("%s: tenant_slug %q is malformed (must be a canonical DNS label)", id, t.TenantSlug))
		case reservedSlugs[t.TenantSlug]:
			errs = append(errs, fmt.Sprintf("%s: tenant_slug %q is reserved", id, t.TenantSlug))
		default:
			if prev, dup := seenSlug[t.TenantSlug]; dup {
				errs = append(errs, fmt.Sprintf("%s: tenant_slug %q duplicates %s", id, t.TenantSlug, prev))
			} else {
				seenSlug[t.TenantSlug] = id
			}
		}

		// Routing status enum (always validated).
		switch t.RoutingStatus {
		case StatusActive, StatusSuspended, StatusPending:
		default:
			errs = append(errs, fmt.Sprintf("%s: routing_status %q invalid (want ACTIVE|SUSPENDED|PENDING_RESOLUTION)", id, t.RoutingStatus))
		}

		// Fail-closed modelling: only ACTIVE tenants get a data-bearing route
		// emitted, so only ACTIVE tenants must have fully-resolved residency.
		// A SUSPENDED or PENDING_RESOLUTION tenant may legitimately have no
		// resolved policy/region yet — it routes to the safe catch-all (§8.1),
		// so requiring those fields would wrongly reject a valid map.
		if t.RoutingStatus != StatusActive {
			continue
		}

		if t.DataResidencyPolicyID == "" {
			errs = append(errs, fmt.Sprintf("%s: data_residency_policy_id is required for ACTIVE tenants", id))
		}

		// allowed_regions: non-empty, all known, no duplicates.
		if len(t.AllowedRegions) == 0 {
			errs = append(errs, fmt.Sprintf("%s: allowed_regions must not be empty", id))
		}
		seenRegion := map[string]bool{}
		for _, r := range t.AllowedRegions {
			if !cat.has(r) {
				errs = append(errs, fmt.Sprintf("%s: allowed_regions contains unknown region %q", id, r))
			}
			if seenRegion[r] {
				errs = append(errs, fmt.Sprintf("%s: allowed_regions lists %q more than once", id, r))
			}
			seenRegion[r] = true
		}

		// Allowed-region validation (§4.2): primary must be known AND allowed.
		if t.PrimaryRegion == "" {
			errs = append(errs, fmt.Sprintf("%s: primary_region is required", id))
		} else {
			if !cat.has(t.PrimaryRegion) {
				errs = append(errs, fmt.Sprintf("%s: primary_region %q is not a known region", id, t.PrimaryRegion))
			}
			if !t.allows(t.PrimaryRegion) {
				errs = append(errs, fmt.Sprintf("%s: primary_region %q is outside allowed_regions %v", id, t.PrimaryRegion, t.AllowedRegions))
			}
		}

		// Fallback validation (§4.2): if set, must be known, allowed, and not
		// equal to primary. If null, no failover is emitted (handled in emit).
		if t.FallbackRegion != nil {
			fb := *t.FallbackRegion
			switch {
			case !cat.has(fb):
				errs = append(errs, fmt.Sprintf("%s: fallback_region %q is not a known region", id, fb))
			case !t.allows(fb):
				errs = append(errs, fmt.Sprintf("%s: fallback_region %q is outside allowed_regions %v", id, fb, t.AllowedRegions))
			case fb == t.PrimaryRegion:
				errs = append(errs, fmt.Sprintf("%s: fallback_region %q must differ from primary_region", id, fb))
			}
		}

		// Manual failover switch (§8.3/§8.4): can only be activated when an
		// approved in-boundary fallback exists. This is what makes a
		// non-compliant fallback impossible (acceptance test D): a tenant with
		// no fallback_region simply cannot be failed over anywhere.
		if t.FailoverActive && t.FallbackRegion == nil {
			errs = append(errs, fmt.Sprintf("%s: failover_active is true but no approved fallback_region exists", id))
		}

		// Quarantine validation (§4.2).
		switch t.QuarantineMode {
		case QuarantineNone, QuarantineBlock:
			if t.QuarantinePool != nil && *t.QuarantinePool != "" {
				errs = append(errs, fmt.Sprintf("%s: quarantine_pool must be empty for %s mode", id, t.QuarantineMode))
			}
		case QuarantineIsolated:
			if t.QuarantinePool == nil || *t.QuarantinePool == "" {
				errs = append(errs, fmt.Sprintf("%s: ISOLATED_SERVE requires a quarantine_pool", id))
			} else if !regionCompatiblePool(*t.QuarantinePool, t.AllowedRegions) {
				errs = append(errs, fmt.Sprintf("%s: quarantine_pool %q is not region-compatible with allowed_regions %v", id, *t.QuarantinePool, t.AllowedRegions))
			}
		default:
			errs = append(errs, fmt.Sprintf("%s: quarantine_mode %q invalid (want NONE|BLOCK|ISOLATED_SERVE)", id, t.QuarantineMode))
		}
	}

	return errs
}

// regionCompatiblePool checks an ISOLATED_SERVE quarantine pool names a region
// within the tenant's allowed set — e.g. "eu-quarantine-pool" is compatible
// only if "eu" ∈ allowed_regions. This keeps incident-time traffic inside the
// residency boundary (§9.1).
func regionCompatiblePool(pool string, allowed []string) bool {
	for _, r := range allowed {
		if strings.HasPrefix(pool, r+"-") {
			return true
		}
	}
	return false
}
