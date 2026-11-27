package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/outbox"
)

// Reliance on a certification and the blast radius of revoking it (ZS-SVC-Z-001 RB-05, DQC-017). See
// domain/reliance.go for the rules and migration 000011 for the database's own copy of them.

const relianceCols = `l.reliance_id, l.tenant_id, l.certification_id, c.run_id, l.relying_service, l.relying_object_type,
	l.relying_object_id, l.purpose, l.recorded_by, l.recorded_at`

func scanReliance(row pgx.Row) (*domain.CertificationReliance, error) {
	var r domain.CertificationReliance
	if err := row.Scan(&r.RelianceID, &r.TenantID, &r.CertificationID, &r.RunID, &r.RelyingService, &r.RelyingObjectType,
		&r.RelyingObjectID, &r.Purpose, &r.RecordedBy, &r.RecordedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func loadReliances(ctx context.Context, tx pgx.Tx, certificationID string) ([]domain.CertificationReliance, error) {
	rows, err := tx.Query(ctx, `SELECT `+relianceCols+` FROM dq_certification_reliances l
		JOIN dq_certifications c ON c.certification_id = l.certification_id
		WHERE l.certification_id = $1 ORDER BY l.recorded_at, l.reliance_id`, certificationID)
	if err != nil {
		return nil, fmt.Errorf("load reliances: %w", err)
	}
	defer rows.Close()
	out := []domain.CertificationReliance{}
	for rows.Next() {
		r, err := scanReliance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RecordReliance records that something relied on the run's certification. It is idempotent by its natural key:
// the same service, object type and object id relying on the same certification again returns the first record
// (created=false) and changes nothing. It is refused (ErrRelianceInactive) once the certification is revoked or
// past its validity window: reliance on withdrawn or stale evidence is the thing a revocation exists to stop.
func (s *PgStore) RecordReliance(ctx context.Context, tenantID, runID, actor string, req domain.RecordRelianceRequest) (*domain.CertificationReliance, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, false, fmt.Errorf("%w: the principal recording the reliance is required", domain.ErrRelianceInvalid)
	}
	var out *domain.CertificationReliance
	created := false
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var certID string
		var validUntil *time.Time
		// A share lock: concurrent reliances do not queue behind each other, but a revocation (which locks the
		// certification exclusively) cannot slip in between the checks below and the insert.
		err := tx.QueryRow(ctx, `SELECT certification_id, valid_until FROM dq_certifications WHERE run_id = $1 FOR SHARE`, runID).Scan(&certID, &validUntil)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCertificationNotFound
		}
		if err != nil {
			return err
		}
		var revoked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dq_certification_revocations WHERE certification_id = $1)`, certID).Scan(&revoked); err != nil {
			return err
		}
		now := s.now().UTC()
		if revoked || (validUntil != nil && now.After(*validUntil)) {
			return domain.ErrRelianceInactive
		}
		id := newID(domain.PrefixReliance)
		tag, err := tx.Exec(ctx, `
			INSERT INTO dq_certification_reliances (reliance_id, tenant_id, certification_id, relying_service, relying_object_type,
				relying_object_id, purpose, recorded_by, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (certification_id, relying_service, relying_object_type, relying_object_id) DO NOTHING`,
			id, tenantID, certID, req.RelyingService, req.RelyingObjectType, req.RelyingObjectID, strings.TrimSpace(req.Purpose), actor, now)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
				return domain.ErrRelianceInactive
			}
			return fmt.Errorf("insert reliance: %w", err)
		}
		created = tag.RowsAffected() == 1
		got, err := scanReliance(tx.QueryRow(ctx, `SELECT `+relianceCols+` FROM dq_certification_reliances l
			JOIN dq_certifications c ON c.certification_id = l.certification_id
			WHERE l.certification_id = $1 AND l.relying_service = $2 AND l.relying_object_type = $3 AND l.relying_object_id = $4`,
			certID, req.RelyingService, req.RelyingObjectType, req.RelyingObjectID))
		if err != nil {
			return fmt.Errorf("load reliance: %w", err)
		}
		out = got
		if !created {
			return nil
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_certification", AggregateID: certID,
			EventType: "dqc.certification.relied-on", TenantID: &tenantID, Payload: map[string]any{
				"reliance_id": got.RelianceID, "certification_id": certID, "run_id": runID, "relying_service": got.RelyingService,
				"relying_object_type": got.RelyingObjectType, "relying_object_id": got.RelyingObjectID, "recorded_by": actor, "recorded_at": got.RecordedAt,
			}})
	})
	return out, created, err
}

// GetBlastRadius is everything recorded as relying on the run's certification, with the certification's current
// status (ACTIVE, STALE or REVOKED). For a revoked certification it is the set of things that now rest on
// withdrawn evidence.
func (s *PgStore) GetBlastRadius(ctx context.Context, tenantID, runID string) (*domain.BlastRadius, error) {
	cert, err := s.GetCertification(ctx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	var out *domain.BlastRadius
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rels, err := loadReliances(ctx, tx, cert.CertificationID)
		if err != nil {
			return err
		}
		by := map[string]int{}
		for _, r := range rels {
			by[r.RelyingService]++
		}
		out = &domain.BlastRadius{CertificationID: cert.CertificationID, RunID: runID, Status: cert.Status, Revocation: cert.Revocation,
			Count: len(rels), ByService: by, Reliances: rels}
		return nil
	})
	return out, err
}
