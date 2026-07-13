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

// ── Environment validation ────────────────────────────────────────────────────

func TestValidate_ProdSafeFromDevMap_Rejected(t *testing.T) {
	errs := Validate(validMap(validTenant()), testCatalog(), true) // requireProdSafe, env=dev
	if !hasErrContaining(errs, "refusing to generate prod-equivalent") {
		t.Fatalf("expected prod-safe violation, got: %v", errs)
	}
}
