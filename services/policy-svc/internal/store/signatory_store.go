package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/signatory"
)

// EventSignatoryMatrixPublished announces that a signatory matrix version now
// governs (ZS-GOV-INT-002 §10.2 policy.signatory_matrix.published.v1; the
// event version travels on the envelope, as for every policy-svc event).
// Workflow, document and payment owners re-check open signature requirements
// against it.
const EventSignatoryMatrixPublished = "policy.signatory_matrix.published"

// enqueueSignatoryPublished adds the signatory event when v's family is a
// SIGNATORY_MATRIX, in the activation's own transaction.
func (s *PgStore) enqueueSignatoryPublished(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, actor string, meta domain.EventMeta, superseded []string) error {
	var policyType, policyCode string
	if err := tx.QueryRow(ctx, `SELECT policy_type, policy_code FROM policies WHERE policy_id = $1;`, v.PolicyID).Scan(&policyType, &policyCode); err != nil {
		return err
	}
	if policyType != signatory.PolicyType {
		return nil
	}
	extra := map[string]any{
		"policy_type":            policyType,
		"policy_code":            policyCode,
		"superseded_version_ids": superseded,
	}
	// The matrix's scope facts, never its full content (Doc 02 §10.1: only
	// the necessary facts; readers fetch detail through the API).
	if m, err := signatory.ParseMatrix(v.RulePayload); err == nil {
		extra["instrument_types"] = m.InstrumentTypes
		extra["currency"] = m.Currency
		extra["valuation_basis"] = m.ValuationBasis
		extra["high_risk"] = m.HighRisk
	}
	return s.enqueue(ctx, tx, versionEvent(EventSignatoryMatrixPublished, v, actor, meta, extra))
}

// guardSignatoryOverlap refuses to activate a matrix while another family's
// ACTIVE or SCHEDULED matrix at the same scope and overlapping period would
// govern one of the same instruments in the same risk regime.
func (s *PgStore) guardSignatoryOverlap(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion) error {
	mine, err := signatory.ParseMatrix(v.RulePayload)
	if err != nil {
		return fmt.Errorf("%w: the version's matrix does not parse: %v", domain.ErrInvalidTransition, err)
	}
	rows, err := tx.Query(ctx, `
		SELECT pv.policy_version_id, pv.rule_payload
		FROM policy_versions pv
		JOIN policies p ON p.policy_id = pv.policy_id
		WHERE p.policy_type = $1
		  AND pv.policy_id <> $2
		  AND pv.tenant_id IS NOT DISTINCT FROM $3::uuid
		  AND pv.legal_entity_id IS NOT DISTINCT FROM $4::uuid
		  AND pv.version_status IN ('ACTIVE', 'SCHEDULED')
		  AND pv.withdrawn_at IS NULL
		  AND pv.effective_from < COALESCE($6::timestamptz, 'infinity'::timestamptz)
		  AND COALESCE(pv.effective_to, 'infinity'::timestamptz) > $5;`,
		signatory.PolicyType, v.PolicyID, v.TenantID, v.LegalEntityID, v.EffectiveFrom, v.EffectiveTo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		other, err := signatory.ParseMatrix(payload)
		if err != nil {
			// An unreadable governing matrix is treated as overlapping: never
			// activate beside something whose reach cannot be known.
			return fmt.Errorf("%w: version %s's matrix does not parse", domain.ErrPolicyConflicted, id)
		}
		if instrument, overlap := mine.Overlaps(other); overlap {
			return fmt.Errorf("%w: version %s of another signatory matrix already governs %s at this scope and period", domain.ErrPolicyConflicted, id, instrument)
		}
	}
	return rows.Err()
}
