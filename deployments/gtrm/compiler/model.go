// Package main implements the GTRM configuration compiler.
//
// It is the integrity gate of the Global Traffic & Residency Manager
// (docs/architecture/global-traffic-residency-manager-decision.md §2, §4): it
// reads the authored routing map, validates it against the region catalog and
// the Phase 1 rules, and emits Traefik dynamic configuration. Traefik config is
// never hand-authored — this tool is the only thing that produces it.
//
// This is a build/deploy-time tool, NOT a runtime routing microservice (a
// dedicated routing service is explicitly prohibited for Phase 1, §15).
package main

// ── Region catalog (regions.yaml) ────────────────────────────────────────────

// RegionCatalog is the closed set of region codes the compiler accepts. Any
// routing-map region code not present here is rejected (§4.2).
type RegionCatalog struct {
	Regions []Region `yaml:"regions"`
}

type Region struct {
	Code string `yaml:"code"`
	Name string `yaml:"name"`
	Pool string `yaml:"pool"` // logical backend pool name (e.g. eu-pool)
}

// has reports whether code is a known region.
func (c RegionCatalog) has(code string) bool {
	for _, r := range c.Regions {
		if r.Code == code {
			return true
		}
	}
	return false
}

// pool returns the backend pool name for a region code (empty if unknown).
func (c RegionCatalog) pool(code string) string {
	for _, r := range c.Regions {
		if r.Code == code {
			return r.Pool
		}
	}
	return ""
}

// ── Routing map (routing-map.yaml) ───────────────────────────────────────────

// Quarantine modes.
const (
	QuarantineNone     = "NONE"
	QuarantineBlock    = "BLOCK"
	QuarantineIsolated = "ISOLATED_SERVE"
)

// Routing statuses.
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusPending   = "PENDING_RESOLUTION"
)

// RoutingMap is the single authored representation of routing policy (§4.1).
type RoutingMap struct {
	SchemaVersion int      `yaml:"schema_version"`
	MapVersion    int      `yaml:"map_version"`
	Env           string   `yaml:"env"`        // dev | staging | prod
	EnvDomain     string   `yaml:"env_domain"` // {tenant_slug}.{env_domain}
	Tenants       []Tenant `yaml:"tenants"`
}

type Tenant struct {
	TenantID              string   `yaml:"tenant_id"`
	TenantSlug            string   `yaml:"tenant_slug"`
	WorkspaceID           string   `yaml:"workspace_id"`
	DataResidencyPolicyID string   `yaml:"data_residency_policy_id"`
	PolicyVersion         int      `yaml:"policy_version"`
	AllowedRegions        []string `yaml:"allowed_regions"`
	PrimaryRegion         string   `yaml:"primary_region"`
	FallbackRegion        *string  `yaml:"fallback_region"`
	// FailoverActive is the manual failover switch (§8.3/§8.4). Normally
	// false → route to primary_region. An operator sets it true during a
	// primary-region outage (a reviewed routing-map change) → route to
	// fallback_region. It is STICKY: when the primary recovers, traffic stays
	// on the fallback until an operator sets this back to false — there is no
	// automatic flap-back. Requires an approved fallback_region to be true.
	FailoverActive bool `yaml:"failover_active"`
	QuarantineMode        string   `yaml:"quarantine_mode"`
	QuarantinePool        *string  `yaml:"quarantine_pool"`
	// QuarantineActive is the manual incident-diversion switch (§9). Normally
	// false. An operator sets it true via a reviewed, two-person routing-map
	// change during an incident → traffic is diverted per QuarantineMode
	// (BLOCK => residency-neutral terminator, no tenant data; ISOLATED_SERVE =>
	// the region-scoped quarantine_pool). Rollback is setting it back to false
	// (§9.3 states). Requires QuarantineMode != NONE.
	QuarantineActive bool `yaml:"quarantine_active"`
	RoutingStatus         string   `yaml:"routing_status"`
	LastUpdatedAt         string   `yaml:"last_updated_at"`
	UpdatedBy             string   `yaml:"updated_by"`
	ChangeReference       string   `yaml:"change_reference"`
}

// allows reports whether region is within the tenant's allowed_regions.
func (t Tenant) allows(region string) bool {
	for _, r := range t.AllowedRegions {
		if r == region {
			return true
		}
	}
	return false
}

// activeRegion is the region traffic is currently routed to: the fallback when
// an operator has manually activated failover, otherwise the primary.
func (t Tenant) activeRegion() string {
	if t.FailoverActive && t.FallbackRegion != nil {
		return *t.FallbackRegion
	}
	return t.PrimaryRegion
}
