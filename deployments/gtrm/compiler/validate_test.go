package main

import (
	"strings"
	"testing"
)

func testCatalog() RegionCatalog {
	return RegionCatalog{Regions: []Region{
		{Code: "eu", Name: "European Union", Pool: "eu-pool"},
		{Code: "us", Name: "United States", Pool: "us-pool"},
		{Code: "uk", Name: "United Kingdom", Pool: "uk-pool"},
	}}
}

func ptr(s string) *string { return &s }

// validTenant returns a minimal ACTIVE, EU-only, no-fallback tenant.
func validTenant() Tenant {
	return Tenant{
		TenantID:              "tenant_acme_eu",
		TenantSlug:            "acme",
		DataResidencyPolicyID: "residency_eu_001",
		AllowedRegions:        []string{"eu"},
		PrimaryRegion:         "eu",
		FallbackRegion:        nil,
		QuarantineMode:        QuarantineBlock,
		RoutingStatus:         StatusActive,
	}
}

func validMap(tenants ...Tenant) RoutingMap {
	return RoutingMap{
		SchemaVersion: 1,
		MapVersion:    47,
		Env:           "dev",
		EnvDomain:     "zoikosuite.dev.internal",
		Tenants:       tenants,
	}
}

func hasErrContaining(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// ── Happy path ───────────────────────────────────────────────────────────────

func TestValidate_ValidMap_NoErrors(t *testing.T) {
	fb := ptr("uk")
	atlas := Tenant{
		TenantID: "tenant_atlas_multi", TenantSlug: "atlas",
		DataResidencyPolicyID: "residency_multi_002",
		AllowedRegions:        []string{"eu", "uk"},
		PrimaryRegion:         "eu", FallbackRegion: fb,
		QuarantineMode: QuarantineBlock, RoutingStatus: StatusActive,
	}
	errs := Validate(validMap(validTenant(), atlas), testCatalog(), false)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

// ── Acceptance test H — configuration validation rejects bad maps ─────────────

func TestValidate_RouteOutsideAllowedRegions_Rejected(t *testing.T) {
	tn := validTenant()
	tn.PrimaryRegion = "us" // not in allowed_regions [eu]
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "outside allowed_regions") {
		t.Fatalf("expected allowed-region violation, got: %v", errs)
	}
}

func TestValidate_UnknownRegionCode_Rejected(t *testing.T) {
	tn := validTenant()
	tn.AllowedRegions = []string{"eu", "mars"}
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "unknown region \"mars\"") {
		t.Fatalf("expected unknown-region violation, got: %v", errs)
	}
}

func TestValidate_InvalidFallback_OutsideAllowed_Rejected(t *testing.T) {
	tn := validTenant()
	tn.FallbackRegion = ptr("us") // known region, but not in allowed [eu]
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "fallback_region") || !hasErrContaining(errs, "outside allowed_regions") {
		t.Fatalf("expected fallback-outside-allowed violation, got: %v", errs)
	}
}

func TestValidate_FallbackEqualsPrimary_Rejected(t *testing.T) {
	tn := validTenant()
	tn.AllowedRegions = []string{"eu", "uk"}
	tn.FallbackRegion = ptr("eu") // same as primary
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "must differ from primary_region") {
		t.Fatalf("expected fallback==primary violation, got: %v", errs)
	}
}

func TestValidate_IsolatedServeWithoutPool_Rejected(t *testing.T) {
	tn := validTenant()
	tn.QuarantineMode = QuarantineIsolated
	tn.QuarantinePool = nil
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "ISOLATED_SERVE requires a quarantine_pool") {
		t.Fatalf("expected isolated-serve-pool violation, got: %v", errs)
	}
}

func TestValidate_IsolatedServePoolWrongRegion_Rejected(t *testing.T) {
	tn := validTenant()
	tn.QuarantineMode = QuarantineIsolated
	tn.QuarantinePool = ptr("us-quarantine-pool") // us not in allowed [eu]
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "not region-compatible") {
		t.Fatalf("expected region-incompatible quarantine pool violation, got: %v", errs)
	}
}

func TestValidate_BlockModeWithPool_Rejected(t *testing.T) {
	tn := validTenant()
	tn.QuarantineMode = QuarantineBlock
	tn.QuarantinePool = ptr("eu-quarantine-pool")
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "quarantine_pool must be empty") {
		t.Fatalf("expected block-mode-pool violation, got: %v", errs)
	}
}

func TestValidate_DuplicateSlug_Rejected(t *testing.T) {
	a := validTenant()
	b := validTenant()
	b.TenantID = "tenant_other"
	// same slug "acme"
	errs := Validate(validMap(a, b), testCatalog(), false)
	if !hasErrContaining(errs, "duplicates") {
		t.Fatalf("expected duplicate-slug violation, got: %v", errs)
	}
}

func TestValidate_MalformedSlug_Rejected(t *testing.T) {
	tn := validTenant()
	tn.TenantSlug = "Acme_Corp!" // uppercase + underscore + bang
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "malformed") {
		t.Fatalf("expected malformed-slug violation, got: %v", errs)
	}
}

func TestValidate_ReservedSlug_Rejected(t *testing.T) {
	tn := validTenant()
	tn.TenantSlug = "admin"
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "reserved") {
		t.Fatalf("expected reserved-slug violation, got: %v", errs)
	}
}

func TestValidate_InvalidStatus_Rejected(t *testing.T) {
	tn := validTenant()
	tn.RoutingStatus = "PARKED"
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "routing_status") {
		t.Fatalf("expected invalid-status violation, got: %v", errs)
	}
}

// ── Fail-closed modelling: non-ACTIVE tenants need no resolved residency ──────

func TestValidate_PendingTenant_NoResidency_Valid(t *testing.T) {
	// A PENDING_RESOLUTION tenant whose policy/region isn't resolved yet must
	// be a VALID map entry (it fails closed to the safe path, not the compile).
	pending := Tenant{
		TenantID:       "tenant_newco_pending",
		TenantSlug:     "newco",
		AllowedRegions: nil,
		PrimaryRegion:  "",
		QuarantineMode: QuarantineNone,
		RoutingStatus:  StatusPending,
	}
	errs := Validate(validMap(validTenant(), pending), testCatalog(), false)
	if len(errs) != 0 {
		t.Fatalf("pending tenant with unresolved residency should be valid, got: %v", errs)
	}
}

func TestValidate_ActiveTenant_MissingResidency_Rejected(t *testing.T) {
	// But an ACTIVE tenant still must have a resolved residency policy.
	tn := validTenant()
	tn.DataResidencyPolicyID = ""
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "data_residency_policy_id is required") {
		t.Fatalf("expected missing-policy violation for ACTIVE tenant, got: %v", errs)
	}
}

// ── Manual quarantine switch (§9) ─────────────────────────────────────────────

func TestValidate_QuarantineActiveWithNoneMode_Rejected(t *testing.T) {
	tn := validTenant()
	tn.QuarantineMode = QuarantineNone
	tn.QuarantineActive = true
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "quarantine_active is true but quarantine_mode is NONE") {
		t.Fatalf("expected quarantine-active-with-none violation, got: %v", errs)
	}
}

func TestValidate_QuarantineActiveBlock_Valid(t *testing.T) {
	tn := validTenant() // QuarantineMode = BLOCK
	tn.QuarantineActive = true
	errs := Validate(validMap(tn), testCatalog(), false)
	if len(errs) != 0 {
		t.Fatalf("quarantine active in BLOCK mode should be valid, got: %v", errs)
	}
}

// ── Manual failover switch (test D enforcement) ───────────────────────────────

func TestValidate_FailoverActiveWithoutFallback_Rejected(t *testing.T) {
	tn := validTenant() // no fallback_region
	tn.FailoverActive = true
	errs := Validate(validMap(tn), testCatalog(), false)
	if !hasErrContaining(errs, "failover_active is true but no approved fallback_region") {
		t.Fatalf("expected failover-without-fallback violation, got: %v", errs)
	}
}

func TestValidate_FailoverActiveWithFallback_Valid(t *testing.T) {
	tn := validTenant()
	tn.AllowedRegions = []string{"eu", "uk"}
	tn.FallbackRegion = ptr("uk")
	tn.FailoverActive = true
	errs := Validate(validMap(tn), testCatalog(), false)
	if len(errs) != 0 {
		t.Fatalf("failover active with an approved fallback should be valid, got: %v", errs)
	}
}

// ── Environment validation ────────────────────────────────────────────────────

func TestValidate_ProdSafeFromDevMap_Rejected(t *testing.T) {
	errs := Validate(validMap(validTenant()), testCatalog(), true) // requireProdSafe, env=dev
	if !hasErrContaining(errs, "refusing to generate prod-equivalent") {
		t.Fatalf("expected prod-safe violation, got: %v", errs)
	}
}
