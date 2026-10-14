package domain_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/employee-master-svc/internal/domain"
)

func goodRequest() domain.SyncWorkerFinancialProfileRequest {
	return domain.SyncWorkerFinancialProfileRequest{
		WorkerID: uuid.NewString(), AssignmentID: "A1", LegalEntityID: "le-1", SourceSystemID: "hris",
		SourceVersion: 1, PayrollJurisdictionID: "GB", PayCurrency: "GBP", EmploymentClassification: "EMPLOYEE",
		EffectiveFrom: "2026-01-01",
	}
}

func TestSyncRequestValidation(t *testing.T) {
	require.NoError(t, goodRequest().Validate())

	blank := " "
	to := "2025-12-31"
	for name, mutate := range map[string]func(*domain.SyncWorkerFinancialProfileRequest){
		"worker_id":                 func(r *domain.SyncWorkerFinancialProfileRequest) { r.WorkerID = "not-a-uuid" },
		"assignment_id":             func(r *domain.SyncWorkerFinancialProfileRequest) { r.AssignmentID = "" },
		"legal_entity_id":           func(r *domain.SyncWorkerFinancialProfileRequest) { r.LegalEntityID = " " },
		"source_system_id":          func(r *domain.SyncWorkerFinancialProfileRequest) { r.SourceSystemID = "" },
		"payroll_jurisdiction_id":   func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayrollJurisdictionID = "" },
		"employment_classification": func(r *domain.SyncWorkerFinancialProfileRequest) { r.EmploymentClassification = "" },
		"source_version":            func(r *domain.SyncWorkerFinancialProfileRequest) { r.SourceVersion = 0 },
		"pay_currency":              func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayCurrency = "gbp" },
		"pay_currency (length)":     func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayCurrency = "GBPX" },
		"payee_ref":                 func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayeeRef = &blank },
		"effective_from":            func(r *domain.SyncWorkerFinancialProfileRequest) { r.EffectiveFrom = "01/01/2026" },
		"effective_to":              func(r *domain.SyncWorkerFinancialProfileRequest) { r.EffectiveTo = &to },
		"dimensions":                func(r *domain.SyncWorkerFinancialProfileRequest) { r.Dimensions = map[string]string{"cc": ""} },
	} {
		r := goodRequest()
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}

	r := goodRequest()
	r.Dimensions = map[string]string{}
	for i := 0; i < 21; i++ {
		r.Dimensions[uuid.NewString()] = "v"
	}
	assert.Error(t, r.Validate(), "too many dimensions")
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	base := goodRequest()
	base.Dimensions = map[string]string{"a": "1", "b": "2"}
	fp := base.Fingerprint("tenant-a")
	assert.Len(t, fp, 64)

	reordered := base
	reordered.Dimensions = map[string]string{"b": "2", "a": "1"}
	assert.Equal(t, fp, reordered.Fingerprint("tenant-a"), "map order never changes identity")
	assert.NotEqual(t, fp, base.Fingerprint("tenant-b"))

	payee, empty := "p-1", ""
	for name, mutate := range map[string]func(*domain.SyncWorkerFinancialProfileRequest){
		"currency":     func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayCurrency = "EUR" },
		"jurisdiction": func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayrollJurisdictionID = "US" },
		"payee":        func(r *domain.SyncWorkerFinancialProfileRequest) { r.PayeeRef = &payee },
		"cost center":  func(r *domain.SyncWorkerFinancialProfileRequest) { r.CostCenter = &payee },
		"from":         func(r *domain.SyncWorkerFinancialProfileRequest) { r.EffectiveFrom = "2026-02-01" },
		"dimension": func(r *domain.SyncWorkerFinancialProfileRequest) {
			r.Dimensions = map[string]string{"a": "1", "b": "3"}
		},
		"version":        func(r *domain.SyncWorkerFinancialProfileRequest) { r.SourceVersion = 2 },
		"entity":         func(r *domain.SyncWorkerFinancialProfileRequest) { r.LegalEntityID = "le-2" },
		"classification": func(r *domain.SyncWorkerFinancialProfileRequest) { r.EmploymentClassification = "CONTRACTOR" },
	} {
		r := base
		mutate(&r)
		assert.NotEqual(t, fp, r.Fingerprint("tenant-a"), name)
	}

	// "absent" and "empty string" are different facts.
	a, b := goodRequest(), goodRequest()
	b.WorkerID, b.CostCenter = a.WorkerID, &empty
	assert.NotEqual(t, a.Fingerprint("t"), b.Fingerprint("t"))
}

func TestMappingReason(t *testing.T) {
	term := "2026-06-30"
	active := domain.WorkerFacts{Found: true, LegalEntityID: "le-1", Status: "ACTIVE"}

	assert.Equal(t, "", domain.MappingReason(active, "le-1", "2026-01-01"))
	assert.Equal(t, domain.ReasonWorkerNotFound, domain.MappingReason(domain.WorkerFacts{}, "le-1", "2026-01-01"))
	assert.Equal(t, domain.ReasonCrossEntityWorker, domain.MappingReason(active, "le-2", "2026-01-01"))

	terminated := domain.WorkerFacts{Found: true, LegalEntityID: "le-1", Status: "TERMINATED", TerminationDate: &term}
	assert.Equal(t, "", domain.MappingReason(terminated, "le-1", "2026-06-30"), "final pay on the termination date is allowed")
	assert.Equal(t, domain.ReasonInactiveWorker, domain.MappingReason(terminated, "le-1", "2026-07-01"))
	noDate := domain.WorkerFacts{Found: true, LegalEntityID: "le-1", Status: "TERMINATED"}
	assert.Equal(t, domain.ReasonInactiveWorker, domain.MappingReason(noDate, "le-1", "2026-01-01"), "terminated with no date is not guessed at")
}

func TestEventCatalogue(t *testing.T) {
	for _, e := range []string{"worker_financial_profile_synced", "worker_financial_profile_rejected", "worker_financial_profile_superseded"} {
		assert.True(t, domain.EventCatalogue[e], e)
	}
	assert.False(t, domain.EventCatalogue["worker_paid"])
}
