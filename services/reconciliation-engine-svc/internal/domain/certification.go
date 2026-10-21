package domain

import (
	"fmt"
	"strings"
	"time"
)

// What a certification binds (SPEC_DEVIATIONS A12; ZS-SVC-Z-001 INV-21
// "Certification binds exact observation/reconciliation/lineage evidence",
// DQC-05 "signed/immutable certification record binding exact evidence set and
// authority"; ZS-CONTROL-001 Invariant 9 "a certified control result always
// identifies the certifier, certification authority, time, run version,
// unresolved exceptions and impact assessment"). The shape mirrors
// financial-control-svc's ControlCertification (its B7).

// AuthoritySnapshot is the certification authority behind a certification:
// authorization-svc's own decision on this principal, this action and this
// legal entity, taken live at the moment of certifying. AccessDecisionID is the
// key of the decision's append-only record in authorization-svc (its
// AccessDecisionLog, which also holds the matched grants), so the snapshot can
// be traced to the full evaluation.
type AuthoritySnapshot struct {
	Source           string    `json:"source"` // "authorization-svc"
	PrincipalID      string    `json:"principal_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	ActionType       string    `json:"action_type"`
	AccessDecisionID string    `json:"access_decision_id"`
	DecisionOutcome  string    `json:"decision_outcome"`
	Decision         string    `json:"decision,omitempty"`
	DecisionBasis    string    `json:"decision_basis"`
	PolicySetVersion string    `json:"policy_set_version,omitempty"`
	ReasonCodes      []string  `json:"reason_codes,omitempty"`
	DecidedAt        time.Time `json:"decided_at"`
}

// Traceable reports whether the snapshot is a GRANTED decision of
// principalID's that names its access decision. Anything else cannot be
// recorded as a certification's authority.
func (a *AuthoritySnapshot) Traceable(principalID string) bool {
	return a != nil && a.AccessDecisionID != "" && a.DecisionOutcome == "GRANTED" && a.PrincipalID == principalID
}

// CertifiedPopulation is one frozen side the certification was sealed over:
// the run's latest snapshot generation (A8), by snapshot id and content hash,
// with the source facts the hash was frozen against.
type CertifiedPopulation struct {
	SnapshotID       string            `json:"snapshot_id"`
	Side             string            `json:"side"`
	Generation       int               `json:"generation"`
	SourceSystem     string            `json:"source_system"`
	SpecRef          string            `json:"spec_ref,omitempty"`
	SourceWatermark  string            `json:"source_watermark,omitempty"`
	ItemCount        int64             `json:"item_count"`
	DeclaredRowCount *int64            `json:"declared_row_count,omitempty"`
	DeclaredTotals   map[string]string `json:"declared_totals,omitempty"`
	ContentHash      string            `json:"content_hash"`
}

// MatchedItem is one side of a manual match, as frozen.
type MatchedItem struct {
	ItemID           string    `json:"item_id"`
	Side             string    `json:"side"`
	SourceRecordID   string    `json:"source_record_id,omitempty"`
	RefID            string    `json:"ref_id"`
	AmountMinorUnits int64     `json:"amount_minor_units"`
	Currency         string    `json:"currency"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// ManualMatchEvidence is what a manual match was checked against when it was
// made (§10 "Manual match — requires permission, reason and evidence; cannot
// hide amount difference"): both frozen items, the snapshot generation they
// belong to, and the comparison under the run's pinned tolerance that let the
// pairing through. The operator's reason stays prose; this is the evidence
// beside it. Recorded on match_results in the transaction that applies the
// match (migration 000010).
type ManualMatchEvidence struct {
	Generation int         `json:"generation"`
	ItemA      MatchedItem `json:"item_a"`
	ItemB      MatchedItem `json:"item_b"`
	// KeyOverridden: the two items' match keys differ, so the operator paired
	// what automatic matching would not have.
	KeyOverridden        bool            `json:"key_overridden"`
	Currency             string          `json:"currency"`
	DifferenceMinorUnits int64           `json:"difference_minor_units"`
	AllowedMinorUnits    int64           `json:"allowed_minor_units"`
	DateDifferenceDays   *int64          `json:"date_difference_days,omitempty"`
	AllowedDays          *int            `json:"allowed_days,omitempty"`
	Tolerance            TolerancePolicy `json:"tolerance"`
}

// NewManualMatchEvidence records the comparison of a pair that
// ValidateManualMatch has accepted: same currency, amount and date within the
// pinned tolerance. It computes the figures Compare checks, exactly as Compare
// does (percentage base side, calendar-day difference).
func NewManualMatchEvidence(a, b MatchItem, generation int, p TolerancePolicy) ManualMatchEvidence {
	base := a.Amount
	if p.PercentageBaseSide == b.Side {
		base = b.Amount
	}
	diff, _ := absDiff(a.Amount, b.Amount)
	ev := ManualMatchEvidence{
		Generation: generation, ItemA: matchedItem(a), ItemB: matchedItem(b), KeyOverridden: a.RefID != b.RefID,
		Currency: a.Currency, DifferenceMinorUnits: diff, AllowedMinorUnits: p.Allowed(a.Currency, base), Tolerance: p,
	}
	if p.DateDays != nil {
		days := dayDifference(a.OccurredAt, b.OccurredAt)
		ev.DateDifferenceDays, ev.AllowedDays = &days, p.DateDays
	}
	return ev
}

func matchedItem(m MatchItem) MatchedItem {
	return MatchedItem{ItemID: m.ItemID, Side: m.Side, SourceRecordID: m.SourceRecordID, RefID: m.RefID,
		AmountMinorUnits: m.Amount, Currency: m.Currency, OccurredAt: m.OccurredAt.UTC()}
}

// CertifiedManualMatch is one manual match of the run, as the certification
// binds it: who paired which items, why, the evidence it was checked against
// and the exceptions it resolved. Evidence is nil only for a match made before
// migration 000010 recorded it; none is reconstructed.
type CertifiedManualMatch struct {
	MatchResultID        string               `json:"match_result_id"`
	ItemAID              string               `json:"item_a_id"`
	ItemBID              string               `json:"item_b_id"`
	Reason               string               `json:"reason"`
	MatchedBy            string               `json:"matched_by"`
	MatchedAt            time.Time            `json:"matched_at"`
	Evidence             *ManualMatchEvidence `json:"evidence"`
	ResolvedExceptionIDs []string             `json:"resolved_exception_ids"`
}

// RevokeCertificationRequest withdraws a run's certification (A12/B34;
// ZS-SVC-Z-001 DQC-05 "new material evidence may revoke certification
// prospectively", RB-05; ZS-CONTROL-001 §27 "restricted command, reason,
// evidence, prior certification retained"). Both fields are mandatory: NP-47
// says a revocation with no reason or evidence must fail safe. EvidenceRefs
// name the new material evidence the revocation rests on, as evidence_ref
// names the correction on an exception's REMEDIATED transition.
type RevokeCertificationRequest struct {
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs"`
}

const (
	maxRevocationReasonLen      = 2000
	maxRevocationEvidenceRefs   = 20
	maxRevocationEvidenceRefLen = 500
)

func (r RevokeCertificationRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: a revocation needs a reason", ErrInvalidArgument)
	}
	if len(r.Reason) > maxRevocationReasonLen {
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

// CertificationRevocation is the append-only record of one revocation
// (migration 000011). It names exactly what it revokes — the sealed
// certification, the manifest hash it was sealed under, and the run's
// CERTIFICATION -> CERTIFIED transition — none of which it changes, and
// records who revoked it, why, on what evidence and by what authority.
// RevokedAt is the effective time: the revocation is prospective (RB-05).
type CertificationRevocation struct {
	RevocationID              string            `json:"revocation_id"`
	TenantID                  string            `json:"tenant_id"`
	RunID                     string            `json:"run_id"`
	CertificationID           string            `json:"certification_id"`
	RevokedManifestSHA256     string            `json:"revoked_manifest_sha256"`
	CertificationTransitionID int64             `json:"certification_transition_id"`
	CertifiedBy               string            `json:"certified_by"`
	CertifiedAt               time.Time         `json:"certified_at"`
	Reason                    string            `json:"reason"`
	EvidenceRefs              []string          `json:"evidence_refs"`
	Authority                 AuthoritySnapshot `json:"authority_snapshot"`
	RevokedBy                 string            `json:"revoked_by"`
	RevokedAt                 time.Time         `json:"revoked_at"`
}
