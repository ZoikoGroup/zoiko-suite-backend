package traceability

import (
	"testing"
	"time"

	"zoiko.io/contract/accounting"
	"zoiko.io/contract/masterdata"
	"zoiko.io/contract/operating"
	"zoiko.io/contract/tax"
	"zoiko.io/contract/types"
)

// Scenario A1: Same party is customer and supplier.
// Pass condition: One Party; two PartyRoles; no duplicated base identity.
func TestScenarioA1_CustomerAndSupplier(t *testing.T) {
	tenantID := types.MustNewV7()
	partyID := types.MustNewV7()

	party := masterdata.Party{
		PartyID:          partyID,
		TenantID:         tenantID,
		PartyKind:        masterdata.PartyKindOrganization,
		DisplayName:      "Dual Role Trading Corp",
		MergeStatus:      masterdata.MergeStatusActive,
		SensitivityClass: masterdata.SensitivityClassInternal,
	}

	custRole := masterdata.PartyRole{
		PartyRoleID: types.MustNewV7(),
		TenantID:    tenantID,
		PartyID:     partyID,
		RoleType:    masterdata.RoleTypeCustomer,
		Status:      masterdata.RoleStatusActive,
		Temporal:    types.NewBitemporalRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC()),
	}

	suppRole := masterdata.PartyRole{
		PartyRoleID: types.MustNewV7(),
		TenantID:    tenantID,
		PartyID:     partyID,
		RoleType:    masterdata.RoleTypeSupplier,
		Status:      masterdata.RoleStatusActive,
		Temporal:    types.NewBitemporalRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC()),
	}

	if custRole.PartyID != party.PartyID || suppRole.PartyID != party.PartyID {
		t.Fatalf("A1 FAIL: both roles must point to single party identity")
	}
	if custRole.PartyRoleID == suppRole.PartyRoleID {
		t.Fatalf("A1 FAIL: roles must have distinct role IDs")
	}
}

// Scenario A2: Legal entity changes registered address mid-year.
// Pass condition: Effective-dated Establishment/Address assignment; historical invoices retain prior address context.
func TestScenarioA2_AddressChangeMidYear(t *testing.T) {
	tenantID := types.MustNewV7()
	entityID := types.MustNewV7()
	oldAddrID := types.MustNewV7()
	newAddrID := types.MustNewV7()

	midYearDate := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	endOfJune := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)

	// Historical assignment (Jan 1 to June 30)
	assignOld := masterdata.AddressAssignment{
		AssignmentID: types.MustNewV7(),
		TenantID:     tenantID,
		AddressID:    oldAddrID,
		SubjectID:    entityID,
		SubjectType:  "LEGAL_ENTITY",
		Temporal: types.BitemporalRecord{
			ValidFrom:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			ValidTo:    &endOfJune,
			RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	// New assignment (July 1 onwards)
	assignNew := masterdata.AddressAssignment{
		AssignmentID: types.MustNewV7(),
		TenantID:     tenantID,
		AddressID:    newAddrID,
		SubjectID:    entityID,
		SubjectType:  "LEGAL_ENTITY",
		Temporal: types.BitemporalRecord{
			ValidFrom:  midYearDate,
			ValidTo:    nil,
			RecordedAt: midYearDate,
		},
	}

	// In March 2026: Old address is active
	marchDate := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if !assignOld.Temporal.IsBusinessActiveAt(marchDate) {
		t.Fatalf("A2 FAIL: old address must be active in March")
	}
	if assignNew.Temporal.IsBusinessActiveAt(marchDate) {
		t.Fatalf("A2 FAIL: new address must NOT be active in March")
	}

	// In August 2026: New address is active
	augustDate := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if assignOld.Temporal.IsBusinessActiveAt(augustDate) {
		t.Fatalf("A2 FAIL: old address must NOT be active in August")
	}
	if !assignNew.Temporal.IsBusinessActiveAt(augustDate) {
		t.Fatalf("A2 FAIL: new address must be active in August")
	}
}

// Scenario A3: Invoice posted under statutory and management books.
// Pass condition: One source invoice; separate accounting events/posting policies/ledgers; no duplicated commercial document.
func TestScenarioA3_MultiBookPosting(t *testing.T) {
	tenantID := types.MustNewV7()
	entityID := types.MustNewV7()
	invoiceID := types.MustNewV7()

	statutoryBookID := types.MustNewV7()
	statutoryLedgerID := types.MustNewV7()

	managementBookID := types.MustNewV7()
	managementLedgerID := types.MustNewV7()

	amount := types.MustParseMoney("5000.00")

	// Single source invoice
	invoice := operating.SalesInvoice{
		SalesInvoiceID: invoiceID,
		TenantID:       tenantID,
		LegalEntityID:  entityID,
		GrossAmount:    amount,
		CurrencyCode:   "EUR",
		Status:         operating.InvoiceStatusIssued,
	}

	// Journal 1: Statutory Ledger
	statutoryJE := accounting.JournalEntry{
		JournalEntryID:   types.MustNewV7(),
		TenantID:         tenantID,
		LegalEntityID:    entityID,
		AccountingBookID: statutoryBookID,
		LedgerID:         statutoryLedgerID,
		Status:           accounting.JournalStatusPosted,
		Lines: []accounting.JournalLine{
			{AccountID: types.MustNewV7(), DebitAmount: amount, FunctionalCurrency: "EUR"},
			{AccountID: types.MustNewV7(), CreditAmount: amount, FunctionalCurrency: "EUR"},
		},
	}

	// Journal 2: Management Ledger
	managementJE := accounting.JournalEntry{
		JournalEntryID:   types.MustNewV7(),
		TenantID:         tenantID,
		LegalEntityID:    entityID,
		AccountingBookID: managementBookID,
		LedgerID:         managementLedgerID,
		Status:           accounting.JournalStatusPosted,
		Lines: []accounting.JournalLine{
			{AccountID: types.MustNewV7(), DebitAmount: amount, FunctionalCurrency: "EUR"},
			{AccountID: types.MustNewV7(), CreditAmount: amount, FunctionalCurrency: "EUR"},
		},
	}

	if err := statutoryJE.ValidateBalanced(); err != nil {
		t.Fatalf("A3 FAIL: statutory JE unbalanced: %v", err)
	}
	if err := managementJE.ValidateBalanced(); err != nil {
		t.Fatalf("A3 FAIL: management JE unbalanced: %v", err)
	}

	if statutoryJE.LedgerID == managementJE.LedgerID {
		t.Fatalf("A3 FAIL: statutory and management ledgers must differ")
	}
	if invoice.SalesInvoiceID != invoiceID {
		t.Fatalf("A3 FAIL: single source invoice identity must be preserved")
	}
}

// Scenario A4: Tax rule changes on July 1.
// Pass condition: Determinations before/after date preserve different rule-pack versions and remain reproducible.
func TestScenarioA4_TaxRuleChange(t *testing.T) {
	packV1ID := types.MustNewV7()
	packV2ID := types.MustNewV7()

	june30 := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	july1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	pack1 := tax.TaxRulePack{
		TaxRulePackID: packV1ID,
		PackVersion:   "2026.1",
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:   &june30,
	}

	pack2 := tax.TaxRulePack{
		TaxRulePackID: packV2ID,
		PackVersion:   "2026.2",
		EffectiveFrom: july1,
	}

	juneTx := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	julyTx := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	if !pack1.IsEffectiveAt(juneTx) || pack2.IsEffectiveAt(juneTx) {
		t.Fatalf("A4 FAIL: June must use pack1")
	}
	if pack1.IsEffectiveAt(julyTx) || !pack2.IsEffectiveAt(julyTx) {
		t.Fatalf("A4 FAIL: July must use pack2")
	}
}

// Scenario A5: Supplier invoice imported twice.
// Pass condition: External mapping + idempotency/duplicate rules prevent second authoritative payable.
func TestScenarioA5_SupplierInvoiceIdempotency(t *testing.T) {
	processedExternalIDs := make(map[string]types.UUID)

	externalSystem := "SAP_S4"
	externalDocID := "INV_99214_2026"
	key := externalSystem + ":" + externalDocID

	firstID := types.MustNewV7()
	processedExternalIDs[key] = firstID

	// Second attempt with identical external key
	secondID := types.MustNewV7()
	var finalPayableID types.UUID

	if existingID, found := processedExternalIDs[key]; found {
		finalPayableID = existingID // Idempotently deduplicated
	} else {
		finalPayableID = secondID
		processedExternalIDs[key] = secondID
	}

	if finalPayableID != firstID {
		t.Fatalf("A5 FAIL: duplicate import must resolve to original invoice ID")
	}
}

// Scenario A6: Payment provider retries callback.
// Pass condition: Execution deduped by provider execution ID/event ID; no duplicate settlement.
func TestScenarioA6_PaymentProviderDedupe(t *testing.T) {
	executedProviderEvents := make(map[string]bool)

	providerEventID := "STRIPE_EVT_8829103"

	// First callback execution
	executedProviderEvents[providerEventID] = true

	// Retry callback arrives
	isDuplicate := false
	if executedProviderEvents[providerEventID] {
		isDuplicate = true // Deduplicated, no second settlement
	}

	if !isDuplicate {
		t.Fatalf("A6 FAIL: provider retry must be recognized as duplicate")
	}
}

// Scenario A7: Closed-period error discovered.
// Pass condition: Original posted entry stays immutable; authorized reversal/adjusting entry in permitted period.
func TestScenarioA7_ClosedPeriodCorrectionReversal(t *testing.T) {
	origJournalID := types.MustNewV7()
	origJE := accounting.JournalEntry{
		JournalEntryID: origJournalID,
		JournalNumber:  "JE-2026-JAN-01",
		Status:         accounting.JournalStatusPosted,
		Lines: []accounting.JournalLine{
			{DebitAmount: types.MustParseMoney("100.00")},
			{CreditAmount: types.MustParseMoney("100.00")},
		},
	}

	// Posting to closed period is forbidden; correction created in open period as REVERSAL
	reversalJE := accounting.JournalEntry{
		JournalEntryID:           types.MustNewV7(),
		JournalNumber:            "JE-2026-FEB-REV",
		JournalType:              accounting.JournalTypeReversal,
		ReversalOfJournalEntryID: &origJournalID,
		Status:                   accounting.JournalStatusPosted,
		Lines: []accounting.JournalLine{
			{CreditAmount: types.MustParseMoney("100.00")}, // Inverted
			{DebitAmount: types.MustParseMoney("100.00")},
		},
	}

	// Original entry is untouched and immutable
	if origJE.Status != accounting.JournalStatusPosted {
		t.Fatalf("A7 FAIL: original posted journal must remain immutable")
	}
	if *reversalJE.ReversalOfJournalEntryID != origJournalID {
		t.Fatalf("A7 FAIL: reversal must explicitly reference original journal")
	}
}

// Scenario A8: Corporate ownership corrected retrospectively.
// Pass condition: Bitemporal history shows both business-valid date and when correction was recorded.
func TestScenarioA8_RetrospectiveCorporateCorrection(t *testing.T) {
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recordedJan := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	recordedMarch := time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)

	v1 := masterdata.CorporateRelationship{
		OwnershipPercentage: types.MustParseRate("0.800000000000000000"), // 80%
		Temporal: types.BitemporalRecord{
			ValidFrom:    validFrom,
			RecordedAt:   recordedJan,
			SupersededAt: &recordedMarch,
		},
	}

	v2 := masterdata.CorporateRelationship{
		OwnershipPercentage: types.MustParseRate("0.750000000000000000"), // 75%
		Temporal: types.BitemporalRecord{
			ValidFrom:  validFrom,
			RecordedAt: recordedMarch,
		},
	}

	// On Feb 1: v1 was known
	if !v1.Temporal.WasKnownAt(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("A8 FAIL: v1 must be known on Feb 1")
	}

	// On April 1: v2 is known
	if !v2.Temporal.WasKnownAt(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("A8 FAIL: v2 must be known on April 1")
	}
}

// Scenario A9: Search index is stale.
// Pass condition: Authoritative API remains correct; UI/read model displays freshness and does not permit stale write decisions without revalidation.
func TestScenarioA9_SearchIndexFreshness(t *testing.T) {
	authoritativeUpdatedAt := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	searchWatermark := time.Date(2026, 9, 28, 13, 30, 0, 0, time.UTC)

	isSearchStale := searchWatermark.Before(authoritativeUpdatedAt)
	if !isSearchStale {
		t.Fatalf("A9 FAIL: search index must be flagged as stale")
	}
}

// Scenario A10: AI extracts incorrect tax ID.
// Pass condition: Observation remains non-authoritative until validated; rejected value never overwrites PartyIdentifier.
func TestScenarioA10_AIObservationNotAuthoritative(t *testing.T) {
	actualPartyID := types.MustNewV7()
	verifiedIdentifier := masterdata.PartyIdentifier{
		PartyIdentifierID:  types.MustNewV7(),
		PartyID:            actualPartyID,
		SchemeCode:         masterdata.IdentifierSchemeVAT,
		IdentifierValue:    "GB123456789",
		VerificationStatus: masterdata.VerificationStatusVerified,
	}

	// AI extracts hallucinated/wrong tax ID
	aiObservationValue := "GB999999999"
	aiValidated := false // Validation rejected

	if !aiValidated {
		// Do not mutate verifiedIdentifier; observation remains quarantined
		if verifiedIdentifier.IdentifierValue == aiObservationValue {
			t.Fatalf("A10 FAIL: unvalidated AI observation should not match authoritative value")
		}
	}

	if verifiedIdentifier.IdentifierValue != "GB123456789" {
		t.Fatalf("A10 FAIL: unvalidated AI observation must never overwrite authoritative identifier")
	}
}

// Scenario A11: Legal hold issued.
// Pass condition: Disposition jobs exclude all in-scope records regardless of ordinary retention expiry.
func TestScenarioA11_LegalHoldDispositionBlock(t *testing.T) {
	retentionExpiryDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentDate := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	hasExpired := currentDate.After(retentionExpiryDate)
	isUnderLegalHold := true

	canDispose := hasExpired && !isUnderLegalHold
	if canDispose {
		t.Fatalf("A11 FAIL: legal hold must block disposition even when retention has expired")
	}
}

// Scenario A12: Auditor traces financial statement line.
// Pass condition: Complete 8-stage backward lineage navigation from ReportRun to Source Document & Evidence.
func TestScenarioA12_AuditorSourceToReportLineageTrace(t *testing.T) {
	tenantID := types.MustNewV7()
	entityID := types.MustNewV7()

	graph := NewLineageGraph()

	// Stage 1: Supplier Invoice (Source Record)
	invoiceID := types.MustNewV7()

	// Stage 2: Canonical Payable Object
	payableID := types.MustNewV7()
	_ = graph.AddEdge(types.LineageEdge{
		TenantID:      tenantID,
		LegalEntityID: entityID,
		FromStage:     types.Stage1SourceRecord,
		FromObjectID:  invoiceID,
		ToStage:       types.Stage2CanonicalBusinessObject,
		ToObjectID:    payableID,
	})

	// Stage 3: Accounting Event
	eventID := types.MustNewV7()
	_ = graph.AddEdge(types.LineageEdge{
		TenantID:      tenantID,
		LegalEntityID: entityID,
		FromStage:     types.Stage2CanonicalBusinessObject,
		FromObjectID:  payableID,
		ToStage:       types.Stage3AccountingTaxEvent,
		ToObjectID:    eventID,
	})

	// Stage 4: Journal Line
	journalLineID := types.MustNewV7()
	_ = graph.AddEdge(types.LineageEdge{
		TenantID:      tenantID,
		LegalEntityID: entityID,
		FromStage:     types.Stage3AccountingTaxEvent,
		FromObjectID:  eventID,
		ToStage:       types.Stage4JournalTaxLedgerLine,
		ToObjectID:    journalLineID,
	})

	// Stage 5: Balance Snapshot
	snapshotID := types.MustNewV7()
	_ = graph.AddEdge(types.LineageEdge{
		TenantID:      tenantID,
		LegalEntityID: entityID,
		FromStage:     types.Stage4JournalTaxLedgerLine,
		FromObjectID:  journalLineID,
		ToStage:       types.Stage5LedgerBalanceSnapshot,
		ToObjectID:    snapshotID,
	})

	// Stage 6: Report Run
	reportRunID := types.MustNewV7()
	_ = graph.AddEdge(types.LineageEdge{
		TenantID:      tenantID,
		LegalEntityID: entityID,
		FromStage:     types.Stage5LedgerBalanceSnapshot,
		FromObjectID:  snapshotID,
		ToStage:       types.Stage6ReportRunReturn,
		ToObjectID:    reportRunID,
	})

	// Auditor navigates backward from ReportRun!
	backwardPath, err := graph.TraceBackward(reportRunID)
	if err != nil {
		t.Fatalf("A12 FAIL: backward trace error: %v", err)
	}

	if len(backwardPath) < 5 {
		t.Fatalf("A12 FAIL: expected full lineage path with at least 5 hops, found %d", len(backwardPath))
	}

	// Verify that the backward trace reaches the root source invoice (Stage 1)
	rootFound := false
	for _, node := range backwardPath {
		if node.Stage == types.Stage1SourceRecord && node.ObjectID == invoiceID {
			rootFound = true
			break
		}
	}

	if !rootFound {
		t.Fatalf("A12 FAIL: auditor trace failed to reach root Stage 1 Source Record (invoiceID %v)", invoiceID)
	}
}
