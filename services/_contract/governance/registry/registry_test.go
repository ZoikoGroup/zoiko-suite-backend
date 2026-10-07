package registry_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/registry"
	"zoiko.io/contract/types"
)

func TestAssetRegistration_Validation(t *testing.T) {
	tenantID := types.MustNewV7()
	domainID := types.MustNewV7()
	assetID := types.MustNewV7()
	now := time.Now().UTC()

	validator := registry.NewInvariantValidator()

	classification := &registry.DataClassificationBinding{
		BindingID:        types.MustNewV7(),
		TenantID:         tenantID,
		AssetID:          assetID,
		SensitivityLevel: registry.SensitivityRestricted,
		RegulatoryRegime: "SOX",
		EffectiveFrom:    now.Add(-24 * time.Hour),
	}

	criticalAsset := &registry.DataAsset{
		AssetID:              assetID,
		TenantID:             tenantID,
		DomainID:             domainID,
		AssetCode:            "GENERAL_LEDGER_POSTINGS",
		AssetName:            "General Ledger Postings",
		AssetType:            registry.AssetTypeTable,
		CriticalityTier:      registry.CriticalityTier1Critical,
		IsAuthoritative:      true,
		AuthoritativeService: "general-ledger-svc",
		StorageLocation:      "postgresql://general_ledger.gl_postings",
		Scope: registry.GovernanceScope{
			BusinessProcess: "FINANCIAL_RECORD_TO_REPORT",
		},
		Status:    registry.AssetStatusRegistered,
		CreatedAt: now,
		UpdatedAt: now,
	}

	owner := registry.DataOwnerAssignment{
		AssignmentID:         types.MustNewV7(),
		TenantID:             tenantID,
		AssetID:              assetID,
		OwnerPrincipalID:     "usr_controller_01",
		OwnerRole:            "FINANCIAL_CONTROLLER",
		EffectiveFrom:        now.Add(-24 * time.Hour),
		ApprovedBy:           "usr_cfo_01",
		AssignmentEvidenceID: types.MustNewV7(),
		CreatedAt:            now,
	}

	steward := registry.StewardshipAssignment{
		AssignmentID:       types.MustNewV7(),
		TenantID:           tenantID,
		AssetID:            assetID,
		StewardPrincipalID: "usr_lead_accountant_01",
		OperationalUnit:    "GLOBAL_FINANCE_OPS",
		EffectiveFrom:      now.Add(-24 * time.Hour),
		CreatedAt:          now,
	}

	t.Run("NP-01: Critical asset has no owner", func(t *testing.T) {
		err := validator.ValidateAssetRegistration(criticalAsset, nil, []registry.StewardshipAssignment{steward}, classification, now)
		if !errors.Is(err, registry.ErrUnownedCriticalAsset) {
			t.Fatalf("expected ErrUnownedCriticalAsset, got: %v", err)
		}
	})

	t.Run("DG-002: Critical asset has no steward", func(t *testing.T) {
		err := validator.ValidateAssetRegistration(criticalAsset, []registry.DataOwnerAssignment{owner}, nil, classification, now)
		if !errors.Is(err, registry.ErrUnstewardedCriticalAsset) {
			t.Fatalf("expected ErrUnstewardedCriticalAsset, got: %v", err)
		}
	})

	t.Run("DG-007: Missing classification", func(t *testing.T) {
		err := validator.ValidateAssetRegistration(criticalAsset, []registry.DataOwnerAssignment{owner}, []registry.StewardshipAssignment{steward}, nil, now)
		if !errors.Is(err, registry.ErrMissingClassification) {
			t.Fatalf("expected ErrMissingClassification, got: %v", err)
		}
	})

	t.Run("DG-009: Missing governance scope", func(t *testing.T) {
		unscopedAsset := *criticalAsset
		unscopedAsset.Scope.BusinessProcess = ""
		err := validator.ValidateAssetRegistration(&unscopedAsset, []registry.DataOwnerAssignment{owner}, []registry.StewardshipAssignment{steward}, classification, now)
		if !errors.Is(err, registry.ErrInvalidGovernanceScope) {
			t.Fatalf("expected ErrInvalidGovernanceScope, got: %v", err)
		}
	})

	t.Run("Valid registration succeeds", func(t *testing.T) {
		err := validator.ValidateAssetRegistration(criticalAsset, []registry.DataOwnerAssignment{owner}, []registry.StewardshipAssignment{steward}, classification, now)
		if err != nil {
			t.Fatalf("expected valid registration to succeed, got: %v", err)
		}
	})
}

func TestNP02_ConflictingAuthoritativeWriter(t *testing.T) {
	validator := registry.NewInvariantValidator()

	err := validator.AssertSingleAuthoritativeWriter("SALES_INVOICE", "accounts-receivable-svc", "billing-shadow-svc")
	if !errors.Is(err, registry.ErrConflictingAuthoritativeOwnership) {
		t.Fatalf("expected ErrConflictingAuthoritativeOwnership, got: %v", err)
	}

	err = validator.AssertSingleAuthoritativeWriter("SALES_INVOICE", "accounts-receivable-svc", "accounts-receivable-svc")
	if err != nil {
		t.Fatalf("expected same writer to succeed, got: %v", err)
	}
}

func TestEffectiveDatedResolvers(t *testing.T) {
	tenantID := types.MustNewV7()
	assetID := types.MustNewV7()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	owners := []registry.DataOwnerAssignment{
		{
			AssignmentID:     types.MustNewV7(),
			TenantID:         tenantID,
			AssetID:          assetID,
			OwnerPrincipalID: "usr_alice",
			EffectiveFrom:    t0,
			EffectiveTo:      &t1,
		},
		{
			AssignmentID:     types.MustNewV7(),
			TenantID:         tenantID,
			AssetID:          assetID,
			OwnerPrincipalID: "usr_bob",
			EffectiveFrom:    t1,
			EffectiveTo:      nil,
		},
	}

	// As of March 2026 -> Alice
	active, err := registry.ResolveActiveOwner(owners, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || active.OwnerPrincipalID != "usr_alice" {
		t.Fatalf("expected alice as of March, got: %v, err: %v", active, err)
	}

	// As of August 2026 -> Bob
	active, err = registry.ResolveActiveOwner(owners, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || active.OwnerPrincipalID != "usr_bob" {
		t.Fatalf("expected bob as of August, got: %v, err: %v", active, err)
	}

	// Before t0 -> NoActiveAssignment
	_, err = registry.ResolveActiveOwner(owners, time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, registry.ErrNoActiveAssignment) {
		t.Fatalf("expected ErrNoActiveAssignment before t0, got: %v", err)
	}

	// Overlapping assignments -> conflict
	overlapping := append(owners, registry.DataOwnerAssignment{
		AssignmentID:     types.MustNewV7(),
		TenantID:         tenantID,
		AssetID:          assetID,
		OwnerPrincipalID: "usr_charlie",
		EffectiveFrom:    t0,
		EffectiveTo:      &t2,
	})
	_, err = registry.ResolveActiveOwner(overlapping, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, registry.ErrConflictingActiveAssignments) {
		t.Fatalf("expected ErrConflictingActiveAssignments for overlapping, got: %v", err)
	}
}
