package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// Reliance on a certification and the blast radius of revoking it (ZS-SVC-Z-001 RB-05, DQC-017). See
// domain/reliance.go for the rules and migration 000017 for the database's own copy of them.

const relianceColumns = `l.reliance_id, l.tenant_id, l.certification_id, c.run_id, l.relying_service, l.relying_object_type,
	l.relying_object_id, l.purpose, l.recorded_by, l.recorded_at`

func scanReliance(row pgx.Row) (*domain.CertificationReliance, error) {
	var r domain.CertificationReliance
	if err := row.Scan(&r.RelianceID, &r.TenantID, &r.CertificationID, &r.RunID, &r.RelyingService, &r.RelyingObjectType,
		&r.RelyingObjectID, &r.Purpose, &r.RecordedBy, &r.RecordedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func loadReliances(ctx context.Context, tx pgx.Tx, tenantID, certificationID string) ([]domain.CertificationReliance, error) {
	rows, err := tx.Query(ctx, `SELECT `+relianceColumns+` FROM reconciliation_certification_reliances l
		JOIN certifications c ON c.certification_id = l.certification_id
		WHERE l.tenant_id = $1 AND l.certification_id = $2 ORDER BY l.recorded_at, l.reliance_id`, tenantID, certificationID)
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

// RecordReliance records that something relied on the run's certification. Idempotent by its natural key: the same
// service, object type and object id again returns the first record (created=false) and changes nothing. Refused
// (ErrRelianceInactive) unless the run's certification is CERTIFIED, so never once it is revoked or superseded.
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
		// A share lock on the run: concurrent reliances do not queue behind each other, but a revocation (which locks the
		// run exclusively) cannot slip in between the checks below and the insert.
		var entity, status, certState string
		err := tx.QueryRow(ctx, `SELECT legal_entity_id, status, certification_state FROM reconciliation_runs
			WHERE tenant_id = $1 AND run_id = $2 FOR SHARE`, tenantID, runID).Scan(&entity, &status, &certState)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		if status != string(domain.RunCertified) || certState != string(domain.CertCertified) {
			return domain.ErrRelianceInactive
		}
		var certID string
		if err := tx.QueryRow(ctx, `SELECT certification_id FROM certifications WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID).Scan(&certID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRelianceInactive
			}
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO reconciliation_certification_reliances (reliance_id, tenant_id, certification_id, relying_service, relying_object_type,
				relying_object_id, purpose, recorded_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (certification_id, relying_service, relying_object_type, relying_object_id) DO NOTHING`,
			newID(domain.PrefixReliance), tenantID, certID, req.RelyingService, req.RelyingObjectType, req.RelyingObjectID,
			strings.TrimSpace(req.Purpose), actor)
		if err != nil {
			return fmt.Errorf("insert reliance: %w", err)
		}
		created = tag.RowsAffected() == 1
		got, err := scanReliance(tx.QueryRow(ctx, `SELECT `+relianceColumns+` FROM reconciliation_certification_reliances l
			JOIN certifications c ON c.certification_id = l.certification_id
			WHERE l.tenant_id = $1 AND l.certification_id = $2 AND l.relying_service = $3 AND l.relying_object_type = $4 AND l.relying_object_id = $5`,
			tenantID, certID, req.RelyingService, req.RelyingObjectType, req.RelyingObjectID))
		if err != nil {
			return fmt.Errorf("load reliance: %w", err)
		}
		out = got
		if !created {
			return nil
		}
		return s.enqueueEvent(ctx, tx, evCertRelianceRecorded, tenantID, entity, certID, actor, domain.IdempotencyClaim{}, got)
	})
	return out, created, err
}

// GetBlastRadius is everything recorded as relying on the run's certification, with the certification's state.
func (s *PgStore) GetBlastRadius(ctx context.Context, tenantID, runID string) (*domain.BlastRadius, error) {
	var out *domain.BlastRadius
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var certID, state string
		err := tx.QueryRow(ctx, `SELECT c.certification_id, r.certification_state FROM certifications c
			JOIN reconciliation_runs r ON r.run_id = c.run_id WHERE c.tenant_id = $1 AND c.run_id = $2`, tenantID, runID).Scan(&certID, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		rels, err := loadReliances(ctx, tx, tenantID, certID)
		if err != nil {
			return err
		}
		rev, err := runRevocation(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		by := map[string]int{}
		for _, r := range rels {
			by[r.RelyingService]++
		}
		out = &domain.BlastRadius{CertificationID: certID, RunID: runID, CertificationState: domain.CertificationState(state),
			Revocation: rev, Count: len(rels), ByService: by, Reliances: rels}
		return nil
	})
	return out, err
}
