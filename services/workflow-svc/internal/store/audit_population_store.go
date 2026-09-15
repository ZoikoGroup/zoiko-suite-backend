package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const auditPopulationColumns = `population_id, engagement_id, tenant_id, source_system, source_object, filter_spec,
	period_start, period_end, watermark, status, item_count, control_total_amount, digest,
	prior_population_id, superseded_by_population_id, supersede_reason, quarantine_reason,
	created_by_principal_id, created_at, frozen_at`

func scanAuditPopulation(row pgx.Row) (*domain.AuditPopulation, error) {
	p := &domain.AuditPopulation{}
	err := row.Scan(&p.PopulationID, &p.EngagementID, &p.TenantID, &p.SourceSystem, &p.SourceObject, &p.FilterSpec,
		&p.PeriodStart, &p.PeriodEnd, &p.Watermark, &p.Status, &p.ItemCount, &p.ControlTotalAmount, &p.Digest,
		&p.PriorPopulationID, &p.SupersededByPopulationID, &p.SupersedeReason, &p.QuarantineReason,
		&p.CreatedByPrincipalID, &p.CreatedAt, &p.FrozenAt)
	return p, err
}

func computePopulationDigest(sourceSystem, sourceObject, filterSpec, watermark string, itemCount int64, controlTotal float64, at time.Time) string {
	h := sha256.New()
	h.Write([]byte(sourceSystem))
	h.Write([]byte(sourceObject))
	h.Write([]byte(filterSpec))
	h.Write([]byte(watermark))
	fmt.Fprintf(h, "|%d|%.2f|%d", itemCount, controlTotal, at.UnixNano())
	return hex.EncodeToString(h.Sum(nil))
}

func (s *PgStore) DefinePopulation(ctx context.Context, p domain.DefinePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO audit_populations (
			population_id, engagement_id, tenant_id, source_system, source_object, filter_spec,
			period_start, period_end, watermark, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT DO NOTHING RETURNING `+auditPopulationColumns,
			uuid.NewString(), p.EngagementID, p.TenantID, p.SourceSystem, p.SourceObject, p.FilterSpec,
			p.PeriodStart, p.PeriodEnd, p.Watermark, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanAuditPopulation(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations
			WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetAuditPopulation(ctx context.Context, tenantID, populationID string) (*domain.AuditPopulation, error) {
	var out *domain.AuditPopulation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE population_id=$1`, populationID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditPopulationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListAuditPopulationsByEngagement(ctx context.Context, tenantID, engagementID string) ([]*domain.AuditPopulation, error) {
	var out []*domain.AuditPopulation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE engagement_id=$1 ORDER BY created_at`, engagementID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanAuditPopulation(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// transitionPopulation is the shared CAS + append-only-history helper behind
// every AUD-03 lifecycle command below — same idempotency-by-correlation-id
// shape as TransitionAuditEngagement.
func (s *PgStore) transitionPopulation(ctx context.Context, tenantID, populationID, actorID, correlationID, reason string, expectedStatuses []string, apply func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error)) (*domain.AuditPopulation, bool, error) {
	if correlationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	changed := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var priorPopulationID string
		err := tx.QueryRow(ctx, `SELECT population_id FROM audit_population_transitions WHERE tenant_id=$1 AND correlation_id=$2`, tenantID, correlationID).Scan(&priorPopulationID)
		if err == nil {
			if priorPopulationID != populationID {
				return domain.ErrAuditPopulationInvalidState
			}
			out, err = scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE population_id=$1`, populationID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		current, err := scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, populationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditPopulationNotFound
		}
		if err != nil {
			return err
		}
		allowed := len(expectedStatuses) == 0
		for _, st := range expectedStatuses {
			if current.Status == st {
				allowed = true
				break
			}
		}
		if !allowed {
			return domain.ErrAuditPopulationInvalidState
		}

		next, err := apply(tx, current)
		if err != nil {
			return err
		}
		out = next
		var reasonArg any
		if reason != "" {
			reasonArg = reason
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_population_transitions
			(population_id, tenant_id, from_status, to_status, actor_principal_id, reason, correlation_id, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, populationID, tenantID, current.Status, next.Status, actorID, reasonArg, correlationID, time.Now().UTC())
		if err == nil {
			changed = true
		}
		return err
	})
	if errors.Is(err, domain.ErrAuditPopulationNotFound) || errors.Is(err, domain.ErrAuditPopulationInvalidState) || errors.Is(err, domain.ErrAuditPopulationValidationFailed) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// BuildPopulation records the caller-computed extract (item_count/control
// total) for a DEFINED population and moves it to BUILDING — the extract
// itself is produced outside this service (§233 "this service does not own
// a source-system extraction engine").
func (s *PgStore) BuildPopulation(ctx context.Context, p domain.BuildPopulationParams) (*domain.AuditPopulation, bool, error) {
	return s.transitionPopulation(ctx, p.TenantID, p.PopulationID, p.ActorPrincipalID, p.CorrelationID, "",
		[]string{domain.AuditPopulationDefined},
		func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error) {
			return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, item_count=$2, control_total_amount=$3
				WHERE population_id=$4 RETURNING `+auditPopulationColumns,
				domain.AuditPopulationBuilding, p.ItemCount, p.ControlTotalAmount, p.PopulationID))
		})
}

// ValidatePopulation reconciles the built extract against an independently
// supplied expectation. A mismatch quarantines rather than allowing a silent
// freeze (AUD-NEG-008, AUD-CTRL-007) — the quarantine is itself a
// successful, committed state transition, not a store error: the caller
// distinguishes the two outcomes by the returned population's Status.
func (s *PgStore) ValidatePopulation(ctx context.Context, p domain.ValidatePopulationParams) (*domain.AuditPopulation, bool, error) {
	return s.transitionPopulation(ctx, p.TenantID, p.PopulationID, p.ActorPrincipalID, p.CorrelationID, "",
		[]string{domain.AuditPopulationBuilding},
		func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error) {
			if current.ItemCount == nil || current.ControlTotalAmount == nil {
				return nil, domain.ErrAuditPopulationInvalidState
			}
			mismatch := *current.ItemCount != p.ExpectedItemCount || *current.ControlTotalAmount != p.ExpectedControlTotalAmount
			if mismatch {
				reason := fmt.Sprintf("built extract (count=%d, control_total=%.2f) does not reconcile to expected (count=%d, control_total=%.2f)",
					*current.ItemCount, *current.ControlTotalAmount, p.ExpectedItemCount, p.ExpectedControlTotalAmount)
				return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, quarantine_reason=$2
					WHERE population_id=$3 RETURNING `+auditPopulationColumns, domain.AuditPopulationQuarantined, reason, p.PopulationID))
			}
			return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1 WHERE population_id=$2 RETURNING `+auditPopulationColumns,
				domain.AuditPopulationValidating, p.PopulationID))
		})
}

// FreezePopulation seals a validated extract with a SHA-256 digest over its
// identity fields. AUD-CTRL-008 immutability from here on is enforced by the
// migration's reject_frozen_population_manifest_mutation trigger, not just
// by this code path.
func (s *PgStore) FreezePopulation(ctx context.Context, p domain.FreezePopulationParams) (*domain.AuditPopulation, bool, error) {
	return s.transitionPopulation(ctx, p.TenantID, p.PopulationID, p.ActorPrincipalID, p.CorrelationID, "",
		[]string{domain.AuditPopulationValidating},
		func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error) {
			if current.ItemCount == nil || current.ControlTotalAmount == nil {
				return nil, domain.ErrAuditPopulationInvalidState
			}
			now := time.Now().UTC()
			digest := computePopulationDigest(current.SourceSystem, current.SourceObject, current.FilterSpec, current.Watermark, *current.ItemCount, *current.ControlTotalAmount, now)
			return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, digest=$2, frozen_at=$3
				WHERE population_id=$4 RETURNING `+auditPopulationColumns, domain.AuditPopulationFrozen, digest, now, p.PopulationID))
		})
}

// SupersedePopulation retires a frozen population explicitly (e.g. a
// framework/scope change) without creating a replacement — the caller
// defines a new one separately via DefinePopulation.
func (s *PgStore) SupersedePopulation(ctx context.Context, p domain.SupersedePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.Reason == "" {
		return nil, false, domain.ErrAuditPopulationInvalidState
	}
	return s.transitionPopulation(ctx, p.TenantID, p.PopulationID, p.ActorPrincipalID, p.CorrelationID, p.Reason,
		[]string{domain.AuditPopulationFrozen, domain.AuditPopulationInUse},
		func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error) {
			return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, supersede_reason=$2
				WHERE population_id=$3 RETURNING `+auditPopulationColumns, domain.AuditPopulationSuperseded, p.Reason, p.PopulationID))
		})
}

// QuarantinePopulation is the manual quarantine path (e.g. cross-tenant
// contamination detected post-freeze, AUD-NEG-009) — distinct from
// ValidatePopulation's automatic reconciliation-failure quarantine.
func (s *PgStore) QuarantinePopulation(ctx context.Context, p domain.QuarantinePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.Reason == "" {
		return nil, false, domain.ErrAuditPopulationInvalidState
	}
	return s.transitionPopulation(ctx, p.TenantID, p.PopulationID, p.ActorPrincipalID, p.CorrelationID, p.Reason,
		nil, // any non-terminal status may be quarantined
		func(tx pgx.Tx, current *domain.AuditPopulation) (*domain.AuditPopulation, error) {
			if current.Status == domain.AuditPopulationSuperseded || current.Status == domain.AuditPopulationQuarantined {
				return nil, domain.ErrAuditPopulationInvalidState
			}
			return scanAuditPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, quarantine_reason=$2
				WHERE population_id=$3 RETURNING `+auditPopulationColumns, domain.AuditPopulationQuarantined, p.Reason, p.PopulationID))
		})
}

// AddControlledDelta implements AUD-NEG-010: a late item never mutates the
// original frozen population. It creates a brand-new successor row carrying
// the original's totals plus the delta, freezes it immediately (the delta
// is itself a reconciled correction, not a fresh unvalidated extract), and
// marks the original SUPERSEDED with the linkage recorded both ways.
func (s *PgStore) AddControlledDelta(ctx context.Context, p domain.AddControlledDeltaParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if p.Reason == "" {
		return nil, false, domain.ErrAuditPopulationInvalidState
	}
	var out *domain.AuditPopulation
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorSuccessorID string
		err := tx.QueryRow(ctx, `SELECT population_id FROM audit_populations WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorSuccessorID)
		if err == nil {
			out, err = scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE population_id=$1`, priorSuccessorID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		original, err := scanAuditPopulation(tx.QueryRow(ctx, `SELECT `+auditPopulationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditPopulationNotFound
		}
		if err != nil {
			return err
		}
		if original.Status != domain.AuditPopulationFrozen && original.Status != domain.AuditPopulationInUse {
			return domain.ErrAuditPopulationInvalidState
		}
		if original.ItemCount == nil || original.ControlTotalAmount == nil {
			return domain.ErrAuditPopulationInvalidState
		}

		newCount := *original.ItemCount + p.DeltaItemCount
		newTotal := *original.ControlTotalAmount + p.DeltaControlTotalAmount
		now := time.Now().UTC()
		digest := computePopulationDigest(original.SourceSystem, original.SourceObject, original.FilterSpec, original.Watermark, newCount, newTotal, now)
		successorID := uuid.NewString()
		out, err = scanAuditPopulation(tx.QueryRow(ctx, `INSERT INTO audit_populations (
			population_id, engagement_id, tenant_id, source_system, source_object, filter_spec,
			period_start, period_end, watermark, status, item_count, control_total_amount, digest,
			prior_population_id, created_by_principal_id, correlation_id, frozen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			RETURNING `+auditPopulationColumns,
			successorID, original.EngagementID, p.TenantID, original.SourceSystem, original.SourceObject, original.FilterSpec,
			original.PeriodStart, original.PeriodEnd, original.Watermark, domain.AuditPopulationFrozen, newCount, newTotal, digest,
			p.PopulationID, p.ActorPrincipalID, p.CorrelationID, now))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE audit_populations SET status=$1, supersede_reason=$2, superseded_by_population_id=$3 WHERE population_id=$4`,
			domain.AuditPopulationSuperseded, p.Reason, successorID, p.PopulationID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_population_transitions
			(population_id, tenant_id, from_status, to_status, actor_principal_id, reason, correlation_id, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, p.PopulationID, p.TenantID, original.Status, domain.AuditPopulationSuperseded, p.ActorPrincipalID, p.Reason, p.CorrelationID, now)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrAuditPopulationNotFound) || errors.Is(err, domain.ErrAuditPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}
