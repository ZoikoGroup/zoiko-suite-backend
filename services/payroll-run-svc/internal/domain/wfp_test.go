package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func baseRunRef() domain.RegisterPayrollRunRefRequest {
	return domain.RegisterPayrollRunRefRequest{
		LegalEntityID: "le-1", SourceSystemID: "zoiko-payroll", SourceRunID: "RUN-7", SourceRunVersion: 1,
		RunKind: domain.RunKindRegular, PayrollPeriodID: "2026-09", PayDate: "2026-09-30",
		RuleManifestID: "11111111-1111-1111-1111-111111111111",
	}
}

func TestRunIdentityFingerprint_DeterministicAndSensitiveToEveryField(t *testing.T) {
	base := baseRunRef()
	fp := domain.RunIdentityFingerprint("tenant-a", base)
	assert.Equal(t, fp, domain.RunIdentityFingerprint("tenant-a", base), "same tuple, same identity")
	assert.Len(t, fp, 64)

	mutations := map[string]func(*domain.RegisterPayrollRunRefRequest){
		"legal entity":   func(r *domain.RegisterPayrollRunRefRequest) { r.LegalEntityID = "le-2" },
		"source system":  func(r *domain.RegisterPayrollRunRefRequest) { r.SourceSystemID = "other" },
		"source run":     func(r *domain.RegisterPayrollRunRefRequest) { r.SourceRunID = "RUN-8" },
		"source version": func(r *domain.RegisterPayrollRunRefRequest) { r.SourceRunVersion = 2 },
		"period":         func(r *domain.RegisterPayrollRunRefRequest) { r.PayrollPeriodID = "2026-10" },
		"pay date":       func(r *domain.RegisterPayrollRunRefRequest) { r.PayDate = "2026-10-01" },
		"rule manifest": func(r *domain.RegisterPayrollRunRefRequest) {
			r.RuleManifestID = "22222222-2222-2222-2222-222222222222"
		},
	}
	for name, mutate := range mutations {
		r := base
		mutate(&r)
		assert.NotEqual(t, fp, domain.RunIdentityFingerprint("tenant-a", r), "%s must change identity", name)
	}
	assert.NotEqual(t, fp, domain.RunIdentityFingerprint("tenant-b", base), "tenant must change identity")
}

func TestRunIdentityFingerprint_FieldBoundariesCannotCollide(t *testing.T) {
	a, b := baseRunRef(), baseRunRef()
	a.SourceSystemID, a.SourceRunID = "ab", "c"
	b.SourceSystemID, b.SourceRunID = "a", "bc"
	assert.NotEqual(t, domain.RunIdentityFingerprint("t", a), domain.RunIdentityFingerprint("t", b))
}

func TestRunKindValidation(t *testing.T) {
	r := baseRunRef()
	r.RunKind = "WEEKLY"
	require.Error(t, r.Validate())
	for _, k := range []string{"REGULAR", "OFF_CYCLE", "RETRO", "CORRECTION"} {
		r.RunKind = k
		require.NoError(t, r.Validate(), k)
	}
}

func TestRegisterRunRefValidation(t *testing.T) {
	cases := map[string]func(*domain.RegisterPayrollRunRefRequest){
		"legal_entity_id":    func(r *domain.RegisterPayrollRunRefRequest) { r.LegalEntityID = " " },
		"source_system_id":   func(r *domain.RegisterPayrollRunRefRequest) { r.SourceSystemID = "" },
		"source_run_id":      func(r *domain.RegisterPayrollRunRefRequest) { r.SourceRunID = "" },
		"rule_manifest_id":   func(r *domain.RegisterPayrollRunRefRequest) { r.RuleManifestID = "" },
		"source_run_version": func(r *domain.RegisterPayrollRunRefRequest) { r.SourceRunVersion = 0 },
		"pay_date":           func(r *domain.RegisterPayrollRunRefRequest) { r.PayDate = "30/09/2026" },
	}
	for field, mutate := range cases {
		r := baseRunRef()
		mutate(&r)
		err := r.Validate()
		require.Error(t, err, field)
		assert.Contains(t, err.Error(), field)
	}
}

func TestAcceptanceTransitions(t *testing.T) {
	allowed := [][2]string{
		{domain.AcceptanceReceived, domain.AcceptanceValidating},
		{domain.AcceptanceReceived, domain.AcceptanceQuarantined},
		{domain.AcceptanceValidating, domain.AcceptanceAccepted},
		{domain.AcceptanceValidating, domain.AcceptanceRejected},
		{domain.AcceptanceQuarantined, domain.AcceptanceValidating},
		{domain.AcceptanceAccepted, domain.AcceptanceSuperseded},
	}
	for _, e := range allowed {
		assert.True(t, domain.AcceptanceTransitionAllowed(e[0], e[1]), "%s -> %s", e[0], e[1])
	}
	forbidden := [][2]string{
		{domain.AcceptanceReceived, domain.AcceptanceAccepted},   // acceptance requires validation
		{domain.AcceptanceAccepted, domain.AcceptanceRejected},   // an accepted result is immutable
		{domain.AcceptanceAccepted, domain.AcceptanceValidating}, // ditto
		{domain.AcceptanceRejected, domain.AcceptanceValidating}, // terminal
		{domain.AcceptanceSuperseded, domain.AcceptanceAccepted}, // terminal
		{domain.AcceptanceValidating, domain.AcceptanceSuperseded},
	}
	for _, e := range forbidden {
		assert.False(t, domain.AcceptanceTransitionAllowed(e[0], e[1]), "%s -> %s", e[0], e[1])
	}
}

func TestTrustTransitionsAndAllows(t *testing.T) {
	assert.True(t, domain.TrustTransitionAllowed(domain.TrustPendingCertification, domain.TrustActive))
	assert.True(t, domain.TrustTransitionAllowed(domain.TrustActive, domain.TrustSuspended))
	assert.True(t, domain.TrustTransitionAllowed(domain.TrustSuspended, domain.TrustActive))
	assert.False(t, domain.TrustTransitionAllowed(domain.TrustPendingCertification, domain.TrustSuspended))
	assert.False(t, domain.TrustTransitionAllowed(domain.TrustRetired, domain.TrustActive), "RETIRED is terminal")

	p := &domain.SourceTrustProfile{Status: domain.TrustActive, AllowedCapabilities: []string{domain.CapPayrollResult}}
	assert.True(t, p.Allows(domain.CapPayrollResult))
	assert.False(t, p.Allows(domain.CapTimeLeave), "trust is per capability")
	p.Status = domain.TrustSuspended
	assert.False(t, p.Allows(domain.CapPayrollResult), "only ACTIVE authorizes")
	var nilProfile *domain.SourceTrustProfile
	assert.False(t, nilProfile.Allows(domain.CapPayrollResult))
}

func TestCreateTrustProfileValidation(t *testing.T) {
	ok := domain.CreateSourceTrustProfileRequest{SourceSystemID: "zp", SourceKind: domain.SourceKindZoikoPayroll, AllowedCapabilities: []string{domain.CapPayrollResult}}
	require.NoError(t, ok.Validate())

	for name, mutate := range map[string]func(*domain.CreateSourceTrustProfileRequest){
		"empty source":    func(r *domain.CreateSourceTrustProfileRequest) { r.SourceSystemID = "" },
		"bad kind":        func(r *domain.CreateSourceTrustProfileRequest) { r.SourceKind = "SPREADSHEET" },
		"no capabilities": func(r *domain.CreateSourceTrustProfileRequest) { r.AllowedCapabilities = nil },
		"unknown cap":     func(r *domain.CreateSourceTrustProfileRequest) { r.AllowedCapabilities = []string{"PAYMENT"} },
		"duplicate cap": func(r *domain.CreateSourceTrustProfileRequest) {
			r.AllowedCapabilities = []string{"TIME_LEAVE", "TIME_LEAVE"}
		},
	} {
		r := ok
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestManifestEffectiveOnIsExclusiveAtEndAndRefusesFuture(t *testing.T) {
	to := "2026-10-01"
	m := &domain.PayrollRuleManifest{EffectiveFrom: "2026-04-06", EffectiveTo: &to}
	assert.False(t, m.EffectiveOn("2026-04-05"), "future-effective rules must not apply to an earlier pay date")
	assert.True(t, m.EffectiveOn("2026-04-06"))
	assert.True(t, m.EffectiveOn("2026-09-30"))
	assert.False(t, m.EffectiveOn("2026-10-01"), "effective_to is exclusive")

	open := &domain.PayrollRuleManifest{EffectiveFrom: "2026-04-06"}
	assert.True(t, open.EffectiveOn("2030-01-01"))
}

func TestRegisterManifestValidation(t *testing.T) {
	good := domain.RegisterRuleManifestRequest{
		SourceSystemID: "zp", JurisdictionID: "GB", ManifestVersion: "2026-27.1",
		ManifestDigest: strings.Repeat("a", 64), EffectiveFrom: "2026-04-06", EvidenceRef: "doc://rules/gb-2026",
	}
	require.NoError(t, good.Validate())

	bad := good
	bad.ManifestDigest = strings.Repeat("A", 64) // uppercase is not the canonical form
	assert.Error(t, bad.Validate())
	bad = good
	bad.ManifestDigest = "abc"
	assert.Error(t, bad.Validate())
	bad = good
	before := "2026-01-01"
	bad.EffectiveTo = &before
	assert.Error(t, bad.Validate(), "effective_to must follow effective_from")
	bad = good
	bad.EvidenceRef = ""
	assert.Error(t, bad.Validate(), "a manifest without evidence is not provenance")
}

func TestEventCatalogueCoversSpecEvents(t *testing.T) {
	for _, e := range []string{
		"worker_financial_profile_synced", "time_leave_financial_batch_accepted", "payroll_input_batch_frozen",
		"payroll_result_accepted", "payroll_result_rejected", "payroll_result_superseded",
		"payroll_accounting_event_created", "payroll_accounting_rejected", "payroll_liability_recognized",
		"payroll_liability_settled", "payroll_payment_instruction_approved", "payroll_payment_instruction_invalidated",
		"payroll_payment_submitted_to_banking", "payroll_reconciliation_exception_created", "payroll_evidence_package_certified",
	} {
		assert.True(t, domain.EventCatalogue[e], e)
	}
	assert.False(t, domain.EventCatalogue["payroll_paid"])
}

func TestStateDimensions(t *testing.T) {
	// acceptance has its own edge table; the other six dimensions of spec 2.3 live here.
	assert.Len(t, domain.StateDimensions, 6)
	assert.Contains(t, domain.StateDimensions["bank_settlement"], "PENDING_UNKNOWN", "Pending/Unknown is a first-class state")
}
