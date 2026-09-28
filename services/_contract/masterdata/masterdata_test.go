package masterdata

import (
	"testing"
	"time"

	"zoiko.io/contract/types"
)

// TestScenarioA1_CustomerAndSupplierDualRole verifies that a single Party identity can hold
// both Customer and Supplier roles simultaneously without duplicate base records (Scenario A1 & Anti-Pattern B2).
func TestScenarioA1_CustomerAndSupplierDualRole(t *testing.T) {
	tenantID := types.MustNewV7()
	partyID := types.MustNewV7()

	party := Party{
		PartyID:          partyID,
		TenantID:         tenantID,
		PartyKind:        PartyKindOrganization,
		DisplayName:      "Acme Global Logistics Corp",
		LegalName:        "Acme Global Logistics Corporation",
		MergeStatus:      MergeStatusActive,
		SensitivityClass: SensitivityClassConfidential,
		CreatedAt:        time.Now().UTC(),
		CreatedBy:        "admin-user",
	}

	if err := party.Validate(); err != nil {
		t.Fatalf("party validation failed: %v", err)
	}

	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Role 1: Customer
	customerRole := PartyRole{
		PartyRoleID: types.MustNewV7(),
		TenantID:    tenantID,
		PartyID:     partyID,
		RoleType:    RoleTypeCustomer,
		Status:      RoleStatusActive,
		Temporal:    types.NewBitemporalRecord(validFrom, time.Now().UTC()),
	}

	// Role 2: Supplier
	supplierRole := PartyRole{
		PartyRoleID: types.MustNewV7(),
		TenantID:    tenantID,
		PartyID:     partyID,
		RoleType:    RoleTypeSupplier,
		Status:      RoleStatusActive,
		Temporal:    types.NewBitemporalRecord(validFrom, time.Now().UTC()),
	}

	if err := customerRole.Validate(); err != nil {
		t.Fatalf("customer role invalid: %v", err)
	}
	if err := supplierRole.Validate(); err != nil {
		t.Fatalf("supplier role invalid: %v", err)
	}

	// Verify both roles point to the SAME party identity
	if customerRole.PartyID != supplierRole.PartyID {
		t.Fatalf("dual roles must point to the identical party ID")
	}
	if customerRole.PartyRoleID == supplierRole.PartyRoleID {
		t.Fatalf("roles must have distinct role IDs")
	}
}

// TestScenarioA8_BitemporalCorporateOwnership verifies that retrospective changes to corporate ownership
// can be tracked and evaluated using As-Of bitemporal queries (Scenario A8).
func TestScenarioA8_BitemporalCorporateOwnership(t *testing.T) {
	tenantID := types.MustNewV7()
	parentID := types.MustNewV7()
	subsidiaryID := types.MustNewV7()

	// Initial belief on Jan 1: parent owns 80% effective from Jan 1
	initialRecord := CorporateRelationship{
		RelationshipID:        types.MustNewV7(),
		TenantID:              tenantID,
		ParentEntityOrPartyID: parentID,
		ChildLegalEntityID:    subsidiaryID,
		RelationshipType:      CorporateRelationshipTypeParentSubsidiary,
		OwnershipPercentage:   types.MustParseRate("80.000000000000000000"),
		VotingPercentage:      types.MustParseRate("80.000000000000000000"),
		ConsolidationMethod:   ConsolidationMethodFull,
		Temporal: types.BitemporalRecord{
			ValidFrom:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			RecordedAt: time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
		},
	}

	if err := initialRecord.Validate(); err != nil {
		t.Fatalf("initial corporate relationship invalid: %v", err)
	}

	// On March 1, discovered an error: retrospective correction that actual ownership was 75% from Jan 1
	supersededTime := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	initialRecord.Temporal.SupersededAt = &supersededTime

	correctedRecord := CorporateRelationship{
		RelationshipID:        types.MustNewV7(),
		TenantID:              tenantID,
		ParentEntityOrPartyID: parentID,
		ChildLegalEntityID:    subsidiaryID,
		RelationshipType:      CorporateRelationshipTypeParentSubsidiary,
		OwnershipPercentage:   types.MustParseRate("75.000000000000000000"),
		VotingPercentage:      types.MustParseRate("75.000000000000000000"),
		ConsolidationMethod:   ConsolidationMethodFull,
		Temporal: types.BitemporalRecord{
			ValidFrom:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			RecordedAt: supersededTime,
		},
	}

	if err := correctedRecord.Validate(); err != nil {
		t.Fatalf("corrected corporate relationship invalid: %v", err)
	}

	businessDate := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// Query 1: What did we think on Feb 10 for Jan 15? -> 80% (initialRecord is active)
	asOfFeb10 := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	if !initialRecord.Temporal.AsOf(businessDate, asOfFeb10) {
		t.Fatalf("expected initial record active as of Feb 10")
	}
	if correctedRecord.Temporal.AsOf(businessDate, asOfFeb10) {
		t.Fatalf("corrected record should NOT be known as of Feb 10")
	}

	// Query 2: What do we think on March 15 for Jan 15? -> 75% (correctedRecord is active)
	asOfMarch15 := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if initialRecord.Temporal.AsOf(businessDate, asOfMarch15) {
		t.Fatalf("initial record must be superseded as of March 15")
	}
	if !correctedRecord.Temporal.AsOf(businessDate, asOfMarch15) {
		t.Fatalf("corrected record must be active as of March 15")
	}
}

// TestMultiBookSetup verifies that a LegalEntity can configure Statutory and Management books.
func TestMultiBookSetup(t *testing.T) {
	tenantID := types.MustNewV7()
	entityID := types.MustNewV7()
	calendarID := types.MustNewV7()

	statutoryBook := AccountingBook{
		AccountingBookID:    types.MustNewV7(),
		TenantID:            tenantID,
		LegalEntityID:       entityID,
		BookCode:            "STAT_IFRS",
		BookType:            BookTypeStatutory,
		AccountingFramework: "IFRS",
		FunctionalCurrency:  "EUR",
		FiscalCalendarID:    calendarID,
		ValidFrom:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	managementBook := AccountingBook{
		AccountingBookID:    types.MustNewV7(),
		TenantID:            tenantID,
		LegalEntityID:       entityID,
		BookCode:            "MGMT_USD",
		BookType:            BookTypeManagement,
		AccountingFramework: "INTERNAL_MGMT",
		FunctionalCurrency:  "EUR",
		ReportingCurrency:   "USD",
		FiscalCalendarID:    calendarID,
		ValidFrom:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := statutoryBook.Validate(); err != nil {
		t.Fatalf("statutory book invalid: %v", err)
	}
	if err := managementBook.Validate(); err != nil {
		t.Fatalf("management book invalid: %v", err)
	}
}
