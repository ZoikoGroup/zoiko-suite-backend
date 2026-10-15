package domain

// ZS-SVC-L-001 WFP-01 — Worker Financial Profile Interface.
//
// A versioned, effective-dated mirror of the minimum financial attributes the
// payroll interfaces need. This service is not the HR master for any of it: a
// profile is accepted from a trusted source, validated against the worker and
// entity this service does own, and otherwise quarantined — never defaulted.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ProfileActive      = "ACTIVE"
	ProfileSuperseded  = "SUPERSEDED"
	ProfileDeactivated = "DEACTIVATED"
	ProfileQuarantined = "QUARANTINED"
)

const (
	PayeeNone       = "NONE"
	PayeeUnverified = "UNVERIFIED" // stored as given; verified against payee-banking-identity-svc in Wave 7
	PayeeVerified   = "VERIFIED"
)

// Quarantine reasons. Stable codes: runbook WFP-RB-01 keys off them.
const (
	ReasonWorkerNotFound          = "WORKER_NOT_FOUND"
	ReasonCrossEntityWorker       = "CROSS_ENTITY_WORKER"
	ReasonInactiveWorker          = "INACTIVE_WORKER"
	ReasonUnsupportedJurisdiction = "UNSUPPORTED_JURISDICTION"
)

// Capability this service requires of a source's trust profile (payroll-run-svc
// owns the profiles).
const SourceCapabilityWorkerProfile = "WORKER_PROFILE"

type WorkerFinancialProfile struct {
	ProfileID                string            `json:"profile_id"`
	TenantID                 string            `json:"tenant_id"`
	WorkerID                 string            `json:"worker_id"`
	AssignmentID             string            `json:"assignment_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	PayrollJurisdictionID    string            `json:"payroll_jurisdiction_id"`
	PayCurrency              string            `json:"pay_currency"`
	EmploymentClassification string            `json:"employment_classification"`
	CostCenter               *string           `json:"cost_center,omitempty"`
	Dimensions               map[string]string `json:"dimensions"`
	// PayeeRef is the opaque payee-master reference. It is deliberately not
	// logged and not repeated in events; only payee_ref_changed travels.
	PayeeRef            *string   `json:"payee_ref,omitempty"`
	PayeeRefStatus      string    `json:"payee_ref_status"`
	PayeeRefChanged     bool      `json:"payee_ref_changed"`
	EffectiveFrom       string    `json:"effective_from"`
	EffectiveTo         *string   `json:"effective_to,omitempty"`
	SourceSystemID      string    `json:"source_system_id"`
	SourceVersion       int       `json:"source_version"`
	SourceFingerprint   string    `json:"source_fingerprint"`
	State               string    `json:"state"`
	QuarantineReason    *string   `json:"quarantine_reason,omitempty"`
	SupersedesProfileID *string   `json:"supersedes_profile_id,omitempty"`
	Version             int       `json:"version"`
	CorrelationID       string    `json:"correlation_id"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// SyncWorkerFinancialProfileRequest deliberately has no tenant_id: the tenant is
// the authenticated one, never a body field (WFP-NP-001).
type SyncWorkerFinancialProfileRequest struct {
	WorkerID                 string            `json:"worker_id"`
	AssignmentID             string            `json:"assignment_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	SourceSystemID           string            `json:"source_system_id"`
	SourceVersion            int               `json:"source_version"`
	PayrollJurisdictionID    string            `json:"payroll_jurisdiction_id"`
	PayCurrency              string            `json:"pay_currency"`
	EmploymentClassification string            `json:"employment_classification"`
	CostCenter               *string           `json:"cost_center,omitempty"`
	Dimensions               map[string]string `json:"dimensions,omitempty"`
	PayeeRef                 *string           `json:"payee_ref,omitempty"`
	EffectiveFrom            string            `json:"effective_from"`
	EffectiveTo              *string           `json:"effective_to,omitempty"`
}

type DeactivateProfileRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type RevalidateProfileRequest struct {
	ExpectedVersion int `json:"expected_version"`
}

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

const maxDimensions = 20

func (r SyncWorkerFinancialProfileRequest) Validate() error {
	if _, err := uuid.Parse(r.WorkerID); err != nil {
		return fmt.Errorf("worker_id must be a UUID")
	}
	for name, v := range map[string]string{
		"assignment_id": r.AssignmentID, "legal_entity_id": r.LegalEntityID, "source_system_id": r.SourceSystemID,
		"payroll_jurisdiction_id": r.PayrollJurisdictionID, "employment_classification": r.EmploymentClassification,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if r.SourceVersion < 1 {
		return fmt.Errorf("source_version must be >= 1")
	}
	if !currencyRE.MatchString(r.PayCurrency) {
		return fmt.Errorf("pay_currency must be an ISO 4217 code (three uppercase letters)")
	}
	if len(r.Dimensions) > maxDimensions {
		return fmt.Errorf("dimensions allows at most %d entries", maxDimensions)
	}
	for k, v := range r.Dimensions {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("dimensions keys and values must be non-empty")
		}
	}
	if r.PayeeRef != nil && strings.TrimSpace(*r.PayeeRef) == "" {
		return fmt.Errorf("payee_ref, when supplied, must not be blank")
	}
	from, err := time.Parse("2006-01-02", r.EffectiveFrom)
	if err != nil {
		return fmt.Errorf("effective_from must be YYYY-MM-DD")
	}
	if r.EffectiveTo != nil {
		to, err := time.Parse("2006-01-02", *r.EffectiveTo)
		if err != nil {
			return fmt.Errorf("effective_to must be YYYY-MM-DD")
		}
		if !to.After(from) {
			return fmt.Errorf("effective_to must be after effective_from")
		}
	}
	return nil
}

// Fingerprint identifies the MATERIAL content of a source delivery, so the same
// source_version redelivered unchanged is a replay and a changed one is a
// conflict. Length-prefixed fields keep boundaries unambiguous; dimensions are
// sorted so map order never changes the identity.
func (r SyncWorkerFinancialProfileRequest) Fingerprint(tenantID string) string {
	h := sha256.New()
	put := func(s string) { fmt.Fprintf(h, "%d:%s|", len(s), s) }
	deref := func(p *string) string {
		if p == nil {
			return "\x00nil"
		}
		return *p
	}
	for _, f := range []string{
		tenantID, r.WorkerID, r.AssignmentID, r.LegalEntityID, r.SourceSystemID, strconv.Itoa(r.SourceVersion),
		r.PayrollJurisdictionID, r.PayCurrency, r.EmploymentClassification, deref(r.CostCenter), deref(r.PayeeRef),
		r.EffectiveFrom, deref(r.EffectiveTo),
	} {
		put(f)
	}
	keys := make([]string, 0, len(r.Dimensions))
	for k := range r.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		put(k)
		put(r.Dimensions[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WorkerFacts is what this service knows about the worker, used to validate a
// profile's mapping.
type WorkerFacts struct {
	Found           bool
	LegalEntityID   string
	Status          string
	TerminationDate *string
}

// MappingReason returns why the worker facts cannot carry a profile, or "" when
// they can. A terminated worker still accepts a profile effective on or before
// the termination date (final pay and corrections), never after it.
func MappingReason(w WorkerFacts, legalEntityID, effectiveFrom string) string {
	switch {
	case !w.Found:
		return ReasonWorkerNotFound
	case w.LegalEntityID != legalEntityID:
		return ReasonCrossEntityWorker
	case w.Status == "TERMINATED" && (w.TerminationDate == nil || effectiveFrom > *w.TerminationDate):
		return ReasonInactiveWorker
	}
	return ""
}

// ── events ───────────────────────────────────────────────────────────────────

const (
	EventProfileSynced      = "worker_financial_profile_synced"
	EventProfileRejected    = "worker_financial_profile_rejected"
	EventProfileSuperseded  = "worker_financial_profile_superseded"
	EventProfileDeactivated = "worker_financial_profile_deactivated" // beyond spec 3.2
)

var EventCatalogue = map[string]bool{
	EventProfileSynced: true, EventProfileRejected: true, EventProfileSuperseded: true, EventProfileDeactivated: true,
}

// OutboxEvent is one event to be written in the caller's transaction.
type OutboxEvent struct {
	TenantID  string
	ObjectID  string
	EventType string
	Payload   []byte // the fully built envelope
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrProfileNotFound         = errorString("worker financial profile not found")
	ErrStaleSourceVersion      = errorString("source_version is older than one already received for this worker assignment")
	ErrSourceVersionConflict   = errorString("this source_version already exists with different content; a changed profile must use a new source_version")
	ErrProfileVersionConflict  = errorString("stale expected_version: the profile changed since it was read")
	ErrPartialOverlap          = errorString("effective window would split an existing profile; supply an open-ended window or align to its boundaries")
	ErrSourceNotTrusted        = errorString("source system has no ACTIVE trust profile covering WORKER_PROFILE")
	ErrSourceTrustUnavailable  = errorString("source trust profile could not be verified")
	ErrJurisdictionUnavailable = errorString("jurisdiction registry could not be reached")
	ErrProfileNotQuarantined   = errorString("only a QUARANTINED profile can be revalidated")
	ErrProfileNotActive        = errorString("only an ACTIVE profile can be deactivated")
)
