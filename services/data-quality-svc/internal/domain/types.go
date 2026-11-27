// Package domain defines the authoritative domain types for
// data-quality-svc (DATA-02, ZS-SVC-N-001 §4).
//
// Design decision (the scoping call this service's whole shape depends
// on): this service has no access to the actual source rows it evaluates
// quality over — it is not the owning business domain's database. So
// "evaluating a rule" here does NOT mean executing rule expressions
// against live data. It means: the CALLER (the owning domain, e.g.
// data-ingestion-svc, or an operator tool) computes its own rule outcomes
// against its own data and submits them as evidence to RunDQ. This
// service's actual job is everything AROUND that evidence — binding it to
// an immutable rule-set version, freezing the population it was measured
// against, running aggregate materiality over the submitted failure
// magnitudes, managing the issue/reperform/certification workflow, and
// sealing a certified outcome. It never repairs source data and it never
// computes rule pass/fail itself.
package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	PrefixRuleSet        = "dqs_"
	PrefixRuleSetVersion = "dqv_"
	PrefixRun            = "dqn_"
	PrefixResult         = "dqr_"
	PrefixIssue          = "dqi_"
	PrefixCertification  = "dqc_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── DQRuleSet / DQRuleSetVersion ────────────────────────────────────────────

// DQRuleSet is the named, stable identity of a rule collection.
// DQRuleSetVersion is the actual versioned content — PublishRuleSetVersion
// always creates a NEW version row; an existing published version is
// never edited in place (doc's own named acceptance test).
type DQRuleSet struct {
	RuleSetID string    `json:"ruleset_id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// RuleDefinition is one rule's identity within a version — this service
// stores WHAT the rule is called and its declared materiality thresholds;
// it never stores or executes the rule's actual matching logic.
type RuleDefinition struct {
	RuleKey                       string  `json:"rule_key"`
	Description                   string  `json:"description"`
	PerRecordMaterialityThreshold *string `json:"per_record_materiality_threshold,omitempty"`
	AggregateMaterialityThreshold *string `json:"aggregate_materiality_threshold,omitempty"`
}

type DQRuleSetVersion struct {
	VersionID     string           `json:"version_id"`
	TenantID      string           `json:"tenant_id"`
	RuleSetID     string           `json:"ruleset_id"`
	VersionNumber int              `json:"version_number"`
	Rules         []RuleDefinition `json:"rules"`
	PublishedAt   time.Time        `json:"published_at"`
	PublishedBy   string           `json:"published_by"`
}

// ── DQRun ────────────────────────────────────────────────────────────────────

type DQRunStatus string

const (
	RunPlanned        DQRunStatus = "Planned"
	RunRunning        DQRunStatus = "Running"
	RunExceptionsOpen DQRunStatus = "ExceptionsOpen"
	RunReperformed    DQRunStatus = "Reperformed"
	RunCertified      DQRunStatus = "Certified"
	RunFailed         DQRunStatus = "Failed"
)

// DQRun is one evaluation attempt against a FROZEN population — a
// caller-supplied, opaque reference plus a caller-supplied row count and
// content hash (the caller computes these from whatever it's evaluating
// and submits them as evidence; this service trusts and records them, it
// never recomputes them — same "frozen populations" doctrine as DATA-07).
type DQRun struct {
	RunID            string `json:"run_id"`
	TenantID         string `json:"tenant_id"`
	RuleSetVersionID string `json:"ruleset_version_id,omitempty"`
	// ContractID is the exact DQC-01 contract version the run was judged against, instead of a rule-set
	// version. Exactly one of the two is set (DQC-02: an exact contract, never "latest").
	ContractID *string `json:"contract_id,omitempty"`
	// ObservationID is the observation the run's population was taken from, when it names one. The run's
	// population is then exactly what was observed (same row count, the observation's evidence hash).
	ObservationID         *string     `json:"observation_id,omitempty"`
	PopulationRef         string      `json:"population_ref"`
	PopulationRowCount    int64       `json:"population_row_count"`
	PopulationContentHash string      `json:"population_content_hash"`
	Status                DQRunStatus `json:"status"`
	SupersedesRunID       *string     `json:"supersedes_run_id,omitempty"`
	// EvidenceAsOf is when the population was observed, as asserted by the
	// caller (like the population hash, this service records it and does not
	// recompute it). Freshness is measured from it; when absent, from
	// StartedAt (ZS-SVC-Z-001 DQC-05).
	EvidenceAsOf *time.Time `json:"evidence_as_of,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	CreatedBy    string     `json:"created_by"`
	// Deduplicated is set on the response when this exact evidence had already been recorded against this
	// contract, and the existing run is returned instead of a second one (ZS-SVC-Z-001 section 11).
	Deduplicated bool `json:"deduplicated,omitempty"`
}

// RuleOutcome is the caller-submitted evidence for one rule's evaluation
// against its own data — pass/fail counts and the SUM of failure
// magnitudes (not just a count), so aggregate materiality can be checked
// against a real total rather than an occurrence count alone.
type RuleOutcome struct {
	RuleKey             string   `json:"rule_key"`
	PassCount           int64    `json:"pass_count"`
	FailCount           int64    `json:"fail_count"`
	FailureMagnitudeSum string   `json:"failure_magnitude_sum"` // decimal string, e.g. "100.00"
	FailingRecordRefs   []string `json:"failing_record_refs,omitempty"`
}

type RunDQRequest struct {
	// Exactly one rule source: a rule-set version, or an exact published contract version.
	RuleSetVersionID string `json:"ruleset_version_id,omitempty"`
	ContractAssetID  string `json:"contract_asset_id,omitempty"`
	ContractVersion  int    `json:"contract_version,omitempty"`
	// ObservationID optionally names the DQC-02 observation this population was taken from (contract runs only).
	ObservationID         string        `json:"observation_id,omitempty"`
	PopulationRef         string        `json:"population_ref"`
	PopulationRowCount    int64         `json:"population_row_count"`
	PopulationContentHash string        `json:"population_content_hash"`
	RuleOutcomes          []RuleOutcome `json:"rule_outcomes"`
	EvidenceAsOf          *time.Time    `json:"evidence_as_of,omitempty"`
}

func (r RunDQRequest) Validate() error {
	byContract := r.ContractAssetID != "" || r.ContractVersion != 0
	switch {
	case byContract && r.RuleSetVersionID != "":
		return fmt.Errorf("give either ruleset_version_id or contract_asset_id and contract_version, not both")
	case byContract && (r.ContractAssetID == "" || r.ContractVersion < 1):
		return fmt.Errorf("contract_asset_id and a contract_version of at least 1 are both required; a run never resolves the latest contract")
	case !byContract && r.RuleSetVersionID == "":
		return fmt.Errorf("ruleset_version_id (or contract_asset_id and contract_version) is required")
	}
	if r.PopulationRef == "" {
		return fmt.Errorf("population_ref is required")
	}
	if r.PopulationRowCount <= 0 {
		return fmt.Errorf("population_row_count must be positive")
	}
	if r.PopulationContentHash == "" {
		return fmt.Errorf("population_content_hash is required")
	}
	if len(r.RuleOutcomes) == 0 {
		return fmt.Errorf("at least one rule_outcome is required")
	}
	if r.EvidenceAsOf != nil && r.EvidenceAsOf.After(time.Now().Add(MaxEvidenceClockSkew)) {
		return fmt.Errorf("evidence_as_of is in the future")
	}
	for i, ro := range r.RuleOutcomes {
		if ro.RuleKey == "" {
			return fmt.Errorf("rule_outcomes[%d].rule_key is required", i)
		}
		if ro.FailureMagnitudeSum == "" {
			return fmt.Errorf("rule_outcomes[%d].failure_magnitude_sum is required", i)
		}
	}
	return nil
}

// ── DQResult ─────────────────────────────────────────────────────────────────

type DQResultStatus string

const (
	ResultPass DQResultStatus = "Pass"
	ResultFail DQResultStatus = "Fail"
	// ResultIndeterminate means the evidence needed to decide the rule is
	// absent: no outcome was submitted for it, or the outcome evaluated zero
	// records. ZS-SVC-Z-001 INV-04/INV-05: missing evidence is never a PASS,
	// and an Indeterminate result can never be coerced into one. It blocks
	// certification exactly as a Fail does, and is cleared the same way — by a
	// Reperform that supplies the evidence.
	ResultIndeterminate DQResultStatus = "Indeterminate"
)

// DQResult is one rule's outcome for one run. Materiality is evaluated
// server-side from the submitted magnitudes/counts against the rule's
// declared thresholds — a rule whose aggregate failure magnitude exceeds
// AggregateMaterialityThreshold fails EVEN IF every individual failure
// would pass a per-record threshold (the doc's own named acceptance test:
// "aggregate materiality catches many individually small failures").
type DQResult struct {
	ResultID            string         `json:"result_id"`
	TenantID            string         `json:"tenant_id"`
	RunID               string         `json:"run_id"`
	RuleKey             string         `json:"rule_key"`
	Status              DQResultStatus `json:"status"`
	PassCount           int64          `json:"pass_count"`
	FailCount           int64          `json:"fail_count"`
	FailureMagnitudeSum string         `json:"failure_magnitude_sum"`
	FailingRecordRefs   []string       `json:"failing_record_refs,omitempty"`
	// Severity, Dimension and Mandatory are the rule's values in the contract version the run was judged
	// against, frozen here. Results of rule-set runs carry none.
	Severity    *Severity `json:"severity,omitempty"`
	Dimension   *string   `json:"dimension,omitempty"`
	Mandatory   *bool     `json:"mandatory,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// ── DQIssue ──────────────────────────────────────────────────────────────────

type DQIssueStatus string

const (
	IssueOpen     DQIssueStatus = "Open"
	IssueAssigned DQIssueStatus = "Assigned"
)

// DQIssue tracks a failing result requiring disposition. There is
// deliberately NO command that marks an issue resolved directly — the
// only path to clearing one is a Reperform whose new run passes (the
// doc's own named acceptance test: "operator cannot mark a source defect
// fixed without source-domain evidence" — the source-domain evidence IS
// the clean re-run, submitted the same way the original evidence was).
type DQIssue struct {
	IssueID     string        `json:"issue_id"`
	TenantID    string        `json:"tenant_id"`
	RunID       string        `json:"run_id"`
	ResultID    string        `json:"result_id"`
	Description string        `json:"description"`
	Status      DQIssueStatus `json:"status"`
	Assignee    *string       `json:"assignee,omitempty"`
	RaisedAt    time.Time     `json:"raised_at"`
	RaisedBy    string        `json:"raised_by"`
	// Deduplicated is set on the response when an exception for this result already existed and is returned
	// instead of a second one.
	Deduplicated bool `json:"deduplicated,omitempty"`
}

type RaiseIssueRequest struct {
	RunID       string `json:"run_id"`
	ResultID    string `json:"result_id"`
	Description string `json:"description"`
}

func (r RaiseIssueRequest) Validate() error {
	if r.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if r.ResultID == "" {
		return fmt.Errorf("result_id is required")
	}
	if r.Description == "" {
		return fmt.Errorf("description is required")
	}
	return nil
}

type AssignIssueRequest struct {
	IssueID  string `json:"issue_id"`
	Assignee string `json:"assignee"`
}

func (r AssignIssueRequest) Validate() error {
	if r.IssueID == "" {
		return fmt.Errorf("issue_id is required")
	}
	if r.Assignee == "" {
		return fmt.Errorf("assignee is required")
	}
	return nil
}

// ── DQCertification ──────────────────────────────────────────────────────────

// DQCertification is the sealed, immutable outcome of a run once it has
// no open issues — same "sealed evidence, sha256, immutable at the
// database" doctrine as data-lineage-svc's ProvenanceManifest.
type DQCertification struct {
	CertificationID string                 `json:"certification_id"`
	TenantID        string                 `json:"tenant_id"`
	RunID           string                 `json:"run_id"`
	Summary         DQCertificationSummary `json:"summary"`
	SummarySHA256   string                 `json:"summary_sha256"`
	// BasisSHA256 identifies WHAT was certified, independent of which run or when:
	// the same evidence always yields the same basis, so it is unique per tenant.
	BasisSHA256           string    `json:"basis_sha256,omitempty"`
	LineageManifestID     *string   `json:"lineage_manifest_id,omitempty"`
	LineageManifestSHA256 *string   `json:"lineage_manifest_sha256,omitempty"`
	CertifiedAt           time.Time `json:"certified_at"`
	CertifiedBy           string    `json:"certified_by"`

	// Status is ACTIVE until the certification is revoked. The sealed certification itself never changes;
	// revocation is a separate append-only record (ZS-SVC-Z-001 RB-05, DQC-017).
	Status     string                   `json:"status"`
	Revocation *CertificationRevocation `json:"revocation,omitempty"`
	// ValidityWindowSeconds and ValidUntil are set when the certification was issued against a contract that
	// states a validity window; past ValidUntil the certification reads as STALE (DQC-016). Frozen at issue.
	ValidityWindowSeconds *int64     `json:"validity_window_seconds,omitempty"`
	ValidUntil            *time.Time `json:"valid_until,omitempty"`
}

const (
	CertificationStale   = "STALE"
	CertificationActive  = "ACTIVE"
	CertificationRevoked = "REVOKED"
)

const PrefixRevocation = "dqx_"

type RevocationReason string

const (
	RevokeFalsePass       RevocationReason = "FALSE_PASS"
	RevokeSourceDefect    RevocationReason = "SOURCE_DEFECT"
	RevokeControlDefect   RevocationReason = "CONTROL_DEFECT"
	RevokeEvidenceInvalid RevocationReason = "EVIDENCE_INVALID"
	RevokeOther           RevocationReason = "OTHER"
)

var validRevocationReason = map[RevocationReason]bool{
	RevokeFalsePass: true, RevokeSourceDefect: true, RevokeControlDefect: true, RevokeEvidenceInvalid: true, RevokeOther: true,
}

// CertificationRevocation withdraws reliance on a certification from RevokedAt onward. It does not
// alter, and cannot alter, the sealed certification or its evidence.
type CertificationRevocation struct {
	RevocationID    string           `json:"revocation_id"`
	CertificationID string           `json:"certification_id"`
	ReasonCode      RevocationReason `json:"reason_code"`
	Reason          string           `json:"reason"`
	EvidenceRefs    []string         `json:"evidence_refs"`
	CertifiedBy     string           `json:"certified_by"`
	CertifiedAt     time.Time        `json:"certified_at"`
	RevokedBy       string           `json:"revoked_by"`
	RevokedAt       time.Time        `json:"revoked_at"`
}

type RevokeCertificationRequest struct {
	ReasonCode RevocationReason `json:"reason_code"`
	Reason     string           `json:"reason"`
	// EvidenceRefs name the new material evidence the revocation rests on. At least one is required.
	EvidenceRefs []string `json:"evidence_refs"`
}

const (
	maxRevocationReasonLen      = 2000
	maxRevocationEvidenceRefs   = 20
	maxRevocationEvidenceRefLen = 500
)

func (r RevokeCertificationRequest) Validate() error {
	if !validRevocationReason[r.ReasonCode] {
		return fmt.Errorf("%w: reason_code must be one of FALSE_PASS, SOURCE_DEFECT, CONTROL_DEFECT, EVIDENCE_INVALID, OTHER", ErrRevocationInvalid)
	}
	if len(strings.TrimSpace(r.Reason)) == 0 {
		return fmt.Errorf("%w: a reason is required", ErrRevocationInvalid)
	}
	if len(r.Reason) > maxRevocationReasonLen {
		return fmt.Errorf("%w: reason is too long", ErrRevocationInvalid)
	}
	if len(r.EvidenceRefs) == 0 {
		return fmt.Errorf("%w: a revocation needs at least one evidence reference", ErrRevocationInvalid)
	}
	if len(r.EvidenceRefs) > maxRevocationEvidenceRefs {
		return fmt.Errorf("%w: at most %d evidence references", ErrRevocationInvalid, maxRevocationEvidenceRefs)
	}
	for _, e := range r.EvidenceRefs {
		if strings.TrimSpace(e) == "" || len(e) > maxRevocationEvidenceRefLen {
			return fmt.Errorf("%w: each evidence reference must be 1-%d characters", ErrRevocationInvalid, maxRevocationEvidenceRefLen)
		}
	}
	return nil
}

type DQCertificationSummary struct {
	// AcceptedRisks are the governed risk acceptances the certification was issued over, when the run had a failed
	// mandatory rule. Sealed with the rest, so the certification says on its face what it accepted and who approved.
	AcceptedRisks         []AcceptedRisk `json:"accepted_risks,omitempty"`
	RunID                 string         `json:"run_id"`
	RuleSetVersionID      string         `json:"ruleset_version_id,omitempty"`
	ContractID            string         `json:"contract_id,omitempty"`
	ContractSHA256        string         `json:"contract_sha256,omitempty"`
	PopulationRef         string         `json:"population_ref"`
	PopulationRowCount    int64          `json:"population_row_count"`
	PopulationContentHash string         `json:"population_content_hash"`
	Results               []DQResult     `json:"results"`

	// Freshness state at the moment of certification (TC-13), sealed with the rest.
	BasisSHA256           string     `json:"basis_sha256,omitempty"`
	EvidenceAsOf          *time.Time `json:"evidence_as_of,omitempty"`
	EvidenceMaxAgeSeconds int64      `json:"evidence_max_age_seconds,omitempty"`
	CertifiedAgainstAge   int64      `json:"evidence_age_seconds_at_certification,omitempty"`
	LineageManifestID     *string    `json:"lineage_manifest_id,omitempty"`
	LineageManifestSHA256 *string    `json:"lineage_manifest_sha256,omitempty"`
}

// BasisExistsError is ErrCertificationBasisExists carrying the certification that already holds the basis,
// so a caller can be given it ("conflict returns existing certification or 409", ZS-SVC-Z-001 section 11).
type BasisExistsError struct{ ExistingID string }

func (e *BasisExistsError) Error() string {
	return fmt.Sprintf("%s: %s", ErrCertificationBasisExists, e.ExistingID)
}
func (e *BasisExistsError) Unwrap() error { return ErrCertificationBasisExists }

var (
	ErrObservationMismatch         = errorString("the runs population is not what the observation it names recorded")
	ErrObservationNotFound         = errorString("DQC-001 observation not found")
	ErrEvidenceAlreadyRecorded     = errorString("a run for this exact evidence already exists, so it cannot supersede another run")
	ErrObservationConflict         = errorString("an observation for this asset, window and source watermark already exists with different evidence; late-arriving evidence is a new observation at a later watermark")
	ErrCertificationAlreadyRevoked = errorString("DQC-017 the certification is already revoked")
	ErrRevocationInvalid           = errorString("invalid revocation")
	ErrRevokerIsCertifier          = errorString("the revoker must be someone other than the certifier")
	ErrContractNotPublished        = errorString("a run can only be judged against a PUBLISHED contract version")
	ErrRuleSetNotFound             = errorString("rule set not found")
	ErrRuleSetVersionNotFound      = errorString("rule set version not found")
	ErrRunNotFound                 = errorString("dq run not found")
	ErrResultNotFound              = errorString("dq result not found")
	ErrIssueNotFound               = errorString("dq issue not found")
	ErrCertificationNotFound       = errorString("dq certification not found")
	ErrRunHasOpenIssues            = errorString("dq run has open issues and cannot be certified")
	ErrRunAlreadyCertified         = errorString("dq run is already certified")
	ErrRunNotInExceptionsOpen      = errorString("dq run must be in ExceptionsOpen state to raise an issue")
	ErrIssueAlreadyAssigned        = errorString("dq issue is already assigned")
	ErrIdempotencyKeyReused        = errorString("idempotency key was already used for a different request")
	ErrImmutableViolation          = errorString("this record cannot be mutated in that way")

	// ZS-SVC-Z-001 INV-04 / INV-15.
	ErrUnknownRule              = errorString("rule_outcomes names a rule that is not in the pinned rule-set version")
	ErrDuplicateRuleOutcome     = errorString("rule_outcomes carries more than one outcome for the same rule")
	ErrEvidenceIncomplete       = errorString("certification requires a Pass result for every rule in the pinned rule-set version")
	ErrEvidenceStale            = errorString("the run's evidence is older than the certification freshness window; reperform it with current evidence")
	ErrCertificationBasisExists = errorString("a certification with the same basis already exists")
)

// ── Certification evidence (ZS-SVC-Z-001 DQC-05) ─────────────────────────────

// CertifyDQRequest carries the optional references a certification binds in
// addition to the run itself. The lineage manifest is a reference into the
// lineage authority (data-lineage-svc): this service records it in the basis
// and does not fetch or re-verify it — the same posture as the population
// hash, which is caller-asserted evidence.
type CertifyDQRequest struct {
	LineageManifestID     *string `json:"lineage_manifest_id,omitempty"`
	LineageManifestSHA256 *string `json:"lineage_manifest_sha256,omitempty"`
}

func (r CertifyDQRequest) Validate() error {
	if (r.LineageManifestID == nil) != (r.LineageManifestSHA256 == nil) {
		return fmt.Errorf("lineage_manifest_id and lineage_manifest_sha256 must be given together")
	}
	if r.LineageManifestID != nil && !lineageManifestIDRe.MatchString(*r.LineageManifestID) {
		return fmt.Errorf("lineage_manifest_id must look like dpm_<uuid>")
	}
	if r.LineageManifestSHA256 != nil && !sha256HexRe.MatchString(*r.LineageManifestSHA256) {
		return fmt.Errorf("lineage_manifest_sha256 must be a lowercase sha256 hex digest")
	}
	return nil
}

var (
	lineageManifestIDRe = regexp.MustCompile(`^dpm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256HexRe         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// MaxEvidenceClockSkew is how far in the future a caller-asserted evidence time
// may be before it is refused: evidence cannot have been observed tomorrow.
const MaxEvidenceClockSkew = 5 * time.Minute
