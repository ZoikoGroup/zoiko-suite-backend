package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// GetRelianceStanding lists every certification of the legal entity's runs that the given object is recorded as relying
// on, each with its current state. Nothing recorded is an empty list, not an error: it says the object has recorded no
// reliance on this entity's certifications, which is not the same as resting on none.
func (s *PgStore) GetRelianceStanding(ctx context.Context, tenantID, legalEntityID string, subj domain.RelianceSubject) (*domain.RelianceStanding, error) {
	if err := subj.Validate(); err != nil {
		return nil, err
	}
	out := &domain.RelianceStanding{LegalEntityID: legalEntityID, Subject: subj, Certifications: []domain.ReliedCertification{}}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+relianceColumns+`, r.certification_state
			FROM reconciliation_certification_reliances l
			JOIN certifications c ON c.certification_id = l.certification_id
			JOIN reconciliation_runs r ON r.run_id = c.run_id
			WHERE l.tenant_id = $1 AND r.legal_entity_id = $2 AND l.relying_service = $3 AND l.relying_object_type = $4 AND l.relying_object_id = $5
			ORDER BY l.recorded_at, l.reliance_id`, tenantID, legalEntityID, subj.RelyingService, subj.RelyingObjectType, subj.RelyingObjectID)
		if err != nil {
			return fmt.Errorf("load reliance standing: %w", err)
		}
		var found []domain.ReliedCertification
		for rows.Next() {
			var rc domain.ReliedCertification
			var state string
			rel := &rc.Reliance
			if err := rows.Scan(&rel.RelianceID, &rel.TenantID, &rel.CertificationID, &rel.RunID, &rel.RelyingService, &rel.RelyingObjectType,
				&rel.RelyingObjectID, &rel.Purpose, &rel.RecordedBy, &rel.RecordedAt, &state); err != nil {
				rows.Close()
				return err
			}
			rc.CertificationState = domain.CertificationState(state)
			found = append(found, rc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, rc := range found {
			if rc.CertificationState == domain.CertRevoked {
				if rc.Revocation, err = runRevocation(ctx, tx, tenantID, rc.Reliance.RunID); err != nil {
					return err
				}
				out.Withdrawn++
			}
			if rc.CertificationState == domain.CertSuperseded {
				out.Superseded++
			}
			out.Certifications = append(out.Certifications, rc)
		}
		out.Count = len(out.Certifications)
		return nil
	})
	return out, err
}
