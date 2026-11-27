package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// GetRelianceStanding lists every certification the given object is recorded as relying on, each with its current
// status. Nothing recorded is an empty list, not an error: it says the object has recorded no reliance, which is not the
// same as resting on no certification.
func (s *PgStore) GetRelianceStanding(ctx context.Context, tenantID string, subj domain.RelianceSubject) (*domain.RelianceStanding, error) {
	if err := subj.Validate(); err != nil {
		return nil, err
	}
	out := &domain.RelianceStanding{Subject: subj, Certifications: []domain.ReliedCertification{}}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+relianceCols+`, c.valid_until FROM dq_certification_reliances l
			JOIN dq_certifications c ON c.certification_id = l.certification_id
			WHERE l.tenant_id = $1 AND l.relying_service = $2 AND l.relying_object_type = $3 AND l.relying_object_id = $4
			ORDER BY l.recorded_at, l.reliance_id`, tenantID, subj.RelyingService, subj.RelyingObjectType, subj.RelyingObjectID)
		if err != nil {
			return fmt.Errorf("load reliance standing: %w", err)
		}
		type held struct {
			rel        domain.CertificationReliance
			validUntil *time.Time
		}
		var all []held
		for rows.Next() {
			var h held
			if err := rows.Scan(&h.rel.RelianceID, &h.rel.TenantID, &h.rel.CertificationID, &h.rel.RunID, &h.rel.RelyingService,
				&h.rel.RelyingObjectType, &h.rel.RelyingObjectID, &h.rel.Purpose, &h.rel.RecordedBy, &h.rel.RecordedAt, &h.validUntil); err != nil {
				rows.Close()
				return err
			}
			all = append(all, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := s.now().UTC()
		for _, h := range all {
			rc := domain.ReliedCertification{Reliance: h.rel, Status: domain.CertificationActive}
			if h.validUntil != nil && now.After(*h.validUntil) {
				rc.Status = domain.CertificationStale
			}
			var rev domain.CertificationRevocation
			switch err := tx.QueryRow(ctx, `SELECT `+revocationCols+` FROM dq_certification_revocations WHERE certification_id = $1`, h.rel.CertificationID).Scan(revocationDest(&rev)...); {
			case err == nil:
				rc.Status, rc.Revocation = domain.CertificationRevoked, &rev
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			switch rc.Status {
			case domain.CertificationRevoked:
				out.Withdrawn++
			case domain.CertificationStale:
				out.Stale++
			}
			out.Certifications = append(out.Certifications, rc)
		}
		out.Count = len(out.Certifications)
		return nil
	})
	return out, err
}
