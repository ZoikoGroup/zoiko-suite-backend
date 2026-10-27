package domain

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// CertifyRequest is a certifier's decision on a run that is READY_TO_CERTIFY.
type CertifyRequest struct {
	Decision string `json:"decision"` // CERTIFY | REJECT
	Reason   string `json:"reason"`
}

const (
	DecisionCertify = "CERTIFY"
	DecisionReject  = "REJECT"
)

func (r CertifyRequest) Validate() error {
	switch r.Decision {
	case DecisionCertify:
	case DecisionReject:
		if strings.TrimSpace(r.Reason) == "" {
			return fmt.Errorf("%w: a rejection needs a reason", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: decision must be CERTIFY or REJECT", ErrInvalidArgument)
	}
	if len(r.Reason) > 1000 {
		return fmt.Errorf("%w: reason is too long", ErrInvalidArgument)
	}
	return nil
}

// ErrSegregation: the actor took part in producing the run they are asked to certify.
const ErrSegregation = errorString("the certifier must be independent of the run's creation and execution")

// RevokeCertificationRequest withdraws a run's certification (ZS-CONTROL-001
// §27; ZS-SVC-Z-001 RB-05). Both fields are mandatory: §25 requires a reason,
// and NP-47 says a revocation with no reason or evidence must fail safe.
type RevokeCertificationRequest struct {
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs"`
}

const (
	maxRevocationEvidenceRefs   = 20
	maxRevocationEvidenceRefLen = 512
)

func (r RevokeCertificationRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: a revocation needs a reason", ErrInvalidArgument)
	}
	if len(r.Reason) > 1000 {
		return fmt.Errorf("%w: reason is too long", ErrInvalidArgument)
	}
	if len(r.EvidenceRefs) == 0 {
		return fmt.Errorf("%w: a revocation needs at least one evidence reference", ErrInvalidArgument)
	}
	if len(r.EvidenceRefs) > maxRevocationEvidenceRefs {
		return fmt.Errorf("%w: at most %d evidence references", ErrInvalidArgument, maxRevocationEvidenceRefs)
	}
	for _, e := range r.EvidenceRefs {
		if strings.TrimSpace(e) == "" || len(e) > maxRevocationEvidenceRefLen {
			return fmt.Errorf("%w: each evidence reference must be 1-%d characters", ErrInvalidArgument, maxRevocationEvidenceRefLen)
		}
	}
	return nil
}

// ErrRevokerIsCertifier: the actor certified the run whose certification they
// are asked to revoke.
const ErrRevokerIsCertifier = errorString("a certification cannot be revoked by the principal who certified it")

// CheckRevokerIndependence: the principal who certified a run cannot revoke
// that certification, the same independence a certifier has from the run's
// preparers (Invariant 12; reconciliation-engine-svc's
// CheckCertifierIndependence). certifiers is everyone recorded as moving the
// run's certification to CERTIFIED.
func CheckRevokerIndependence(actor string, certifiers []string) error {
	for _, c := range certifiers {
		if actor == c {
			return ErrRevokerIsCertifier
		}
	}
	return nil
}

// Close-gate statuses.
const (
	GatePassed   = "PASSED"
	GateBlocking = "BLOCKING"
)

// CloseGateItem is one mandatory (close_gating) control for an entity and period.
type CloseGateItem struct {
	ControlCode  string `json:"control_code"`
	ControlName  string `json:"control_name"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	Lifecycle    string `json:"lifecycle_state,omitempty"`
	Result       string `json:"result_state,omitempty"`
	Certificatio string `json:"certification_state,omitempty"`

	// Set when the latest run's certification was revoked: when, and why.
	// The revocation is prospective (RB-05): it tells whoever reads the gate
	// that the certification is withdrawn from that instant.
	CertificationRevokedAt string `json:"certification_revoked_at,omitempty"`
	RevocationReason       string `json:"revocation_reason,omitempty"`

	// Set when the latest run is certified and an unresolved late-data
	// exception says its source changed since (Invariant 14). The
	// certification itself is untouched; the gate reads the exception.
	LateDataExceptionID string `json:"late_data_exception_id,omitempty"`
	LateDataDetectedAt  string `json:"late_data_detected_at,omitempty"`
}

// GateReasonCertificationRevoked: the latest run was certified, and that
// certification has since been revoked.
const GateReasonCertificationRevoked = "CERTIFICATION_REVOKED"

// GateReasonLateDataAfterCertification: the latest run is certified, but its
// source has changed since and the late-data exception that records it is
// unresolved. A new run naming it as prior, or resolving the exception under
// authority (waiver, carry-forward, closure), clears it.
const GateReasonLateDataAfterCertification = "LATE_DATA_AFTER_CERTIFICATION"

// CloseGate is the entity-level decision (FIN-CTRL-041). Open means every
// mandatory control's LATEST run for the period is CERTIFIED. It is fail-closed:
// an entity with no mandatory controls configured is NOT open, because "nothing
// is required" and "nothing was configured" are indistinguishable from here.
type CloseGate struct {
	LegalEntityID string          `json:"legal_entity_id"`
	PeriodID      string          `json:"period_id"`
	Open          bool            `json:"open"`
	Configured    bool            `json:"configured"`
	Blocking      int             `json:"blocking_count"`
	Items         []CloseGateItem `json:"items"`
}

// EvaluateGate derives the gate from each mandatory control's latest run.
// A nil run means the control has not been run for the period.
func EvaluateGate(entityID, periodID string, controls []GateInput) CloseGate {
	g := CloseGate{LegalEntityID: entityID, PeriodID: periodID, Configured: len(controls) > 0, Items: []CloseGateItem{}}
	sort.SliceStable(controls, func(i, j int) bool { return controls[i].ControlCode < controls[j].ControlCode })
	for _, c := range controls {
		it := CloseGateItem{ControlCode: c.ControlCode, ControlName: c.ControlName}
		switch {
		case c.RunID == "":
			it.Status, it.Reason = GateBlocking, "NO_RUN_FOR_PERIOD"
		case c.Lifecycle == string(LifecycleCertified) && c.Certification == string(CertRevoked):
			it.Status, it.Reason = GateBlocking, GateReasonCertificationRevoked
			it.RunID, it.Lifecycle, it.Result, it.Certificatio = c.RunID, c.Lifecycle, c.Result, c.Certification
			it.CertificationRevokedAt, it.RevocationReason = c.RevokedAt, c.RevocationReason
		case c.Lifecycle == string(LifecycleCertified) && c.Certification == string(CertCertified) && c.LateDataExceptionID != "":
			it.Status, it.Reason = GateBlocking, GateReasonLateDataAfterCertification
			it.RunID, it.Lifecycle, it.Result, it.Certificatio = c.RunID, c.Lifecycle, c.Result, c.Certification
			it.LateDataExceptionID, it.LateDataDetectedAt = c.LateDataExceptionID, c.LateDataDetectedAt
		case c.Lifecycle == string(LifecycleCertified) && c.Certification == string(CertCertified):
			it.Status = GatePassed
			it.RunID, it.Lifecycle, it.Result, it.Certificatio = c.RunID, c.Lifecycle, c.Result, c.Certification
		default:
			it.Status = GateBlocking
			it.Reason = "LATEST_RUN_" + c.Lifecycle + "_" + c.Result
			it.RunID, it.Lifecycle, it.Result, it.Certificatio = c.RunID, c.Lifecycle, c.Result, c.Certification
		}
		if it.Status == GateBlocking {
			g.Blocking++
		}
		g.Items = append(g.Items, it)
	}
	g.Open = g.Configured && g.Blocking == 0
	return g
}

// GateInput is a mandatory control with its latest run for the period (RunID empty if none).
type GateInput struct {
	ControlCode, ControlName                string
	RunID, Lifecycle, Result, Certification string
	// RevokedAt (RFC 3339) and RevocationReason are set when the latest run's
	// certification was revoked.
	RevokedAt, RevocationReason string
	// LateDataExceptionID and LateDataDetectedAt (RFC 3339) are set when the
	// latest run has an unresolved late-data exception.
	LateDataExceptionID, LateDataDetectedAt string
}

// ExposureRow is the unresolved exception exposure of one currency and severity.
type ExposureRow struct {
	Currency string
	Severity string
	Count    int
	Exposure string // exact decimal
}

// CurrencyExposure aggregates one currency.
type CurrencyExposure struct {
	Currency string `json:"currency"`
	Count    int    `json:"unresolved_count"`
	High     int    `json:"high"`
	Medium   int    `json:"medium"`
	Low      int    `json:"low"`
	Exposure string `json:"total_exposure"`
}

// ExceptionSummary is the entity-period aggregation (FIN-CTRL-042). Exposure
// from different controls can describe the SAME underlying issue, so the total is
// an upper bound for materiality, never a misstatement estimate.
type ExceptionSummary struct {
	LegalEntityID string             `json:"legal_entity_id"`
	PeriodID      string             `json:"period_id"`
	Currencies    []CurrencyExposure `json:"currencies"`
	Materiality   *MaterialityView2  `json:"materiality,omitempty"`
	// AggregateMaterial is nil when it cannot be assessed: no materiality policy, or
	// no exposure in the policy currency to compare (no FX basis is applied).
	AggregateMaterial    *bool    `json:"aggregate_material"`
	UnassessedCurrencies []string `json:"unassessed_currencies,omitempty"`
	HighSeverity         int      `json:"individually_material_count"`
}

// MaterialityView2 is the policy the aggregation was assessed against.
type MaterialityView2 struct {
	MaterialityID      string `json:"materiality_id"`
	AggregateThreshold string `json:"aggregate_threshold"`
	Currency           string `json:"currency"`
}

// Summarise folds exposure rows into the summary and assesses aggregate materiality.
func Summarise(entityID, periodID string, rows []ExposureRow, mat *MaterialityView2) (ExceptionSummary, error) {
	s := ExceptionSummary{LegalEntityID: entityID, PeriodID: periodID, Currencies: []CurrencyExposure{}, Materiality: mat}
	sums := map[string]*big.Rat{}
	by := map[string]*CurrencyExposure{}
	for _, r := range rows {
		x, ok := new(big.Rat).SetString(r.Exposure)
		if !ok {
			return s, fmt.Errorf("exposure %q is not a decimal", r.Exposure)
		}
		c := by[r.Currency]
		if c == nil {
			c = &CurrencyExposure{Currency: r.Currency}
			by[r.Currency] = c
			sums[r.Currency] = new(big.Rat)
		}
		sums[r.Currency].Add(sums[r.Currency], x)
		c.Count += r.Count
		switch r.Severity {
		case string(SeverityHigh):
			c.High += r.Count
			s.HighSeverity += r.Count
		case string(SeverityMedium):
			c.Medium += r.Count
		default:
			c.Low += r.Count
		}
	}
	for ccy, c := range by {
		c.Exposure = CanonicalAmount(sums[ccy])
		s.Currencies = append(s.Currencies, *c)
	}
	sort.Slice(s.Currencies, func(i, j int) bool { return s.Currencies[i].Currency < s.Currencies[j].Currency })
	if mat == nil {
		return s, nil
	}
	thr, ok := new(big.Rat).SetString(mat.AggregateThreshold)
	if !ok {
		return s, fmt.Errorf("aggregate threshold %q is not a decimal", mat.AggregateThreshold)
	}
	for _, c := range s.Currencies {
		if c.Currency != mat.Currency {
			s.UnassessedCurrencies = append(s.UnassessedCurrencies, c.Currency)
		}
	}
	if sum, ok := sums[mat.Currency]; ok {
		m := sum.Cmp(thr) >= 0
		s.AggregateMaterial = &m
	}
	return s, nil
}
