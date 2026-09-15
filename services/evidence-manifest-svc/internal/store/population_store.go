package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

const populationColumns = `population_id, engagement_id, tenant_id, legal_entity_id, object_class, period_start, period_end,
	source_system, source_query, source_watermark, assertion, expected_completeness_check, status, row_count, digest_sha256,
	prior_population_id, superseded_by_population_id, quarantine_reason, created_by_principal_id, created_at, frozen_at`

func scanPopulation(row pgx.Row) (*domain.AuditPopulation, error) {
	p := &domain.AuditPopulation{}
	err := row.Scan(&p.PopulationID, &p.EngagementID, &p.TenantID, &p.LegalEntityID, &p.ObjectClass, &p.PeriodStart, &p.PeriodEnd,
		&p.SourceSystem, &p.SourceQuery, &p.SourceWatermark, &p.Assertion, &p.ExpectedCompletenessCheck, &p.Status, &p.RowCount, &p.DigestSHA256,
		&p.PriorPopulationID, &p.SupersededByPopulationID, &p.QuarantineReason, &p.CreatedByPrincipalID, &p.CreatedAt, &p.FrozenAt)
	return p, err
}

// DefinePopulation is a real idempotent create. SourceSystem/SourceQuery/
// SourceWatermark are written once and never updated by any other method
// in this file — "source/filter/watermark preserved."
func (s *PgStore) DefinePopulation(ctx context.Context, p domain.DefinePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO audit_populations
			(engagement_id, tenant_id, legal_entity_id, object_class, period_start, period_end, source_system, source_query,
			 source_watermark, assertion, expected_completeness_check, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING RETURNING `+populationColumns,
			p.EngagementID, svcmiddleware.TenantFromContext(ctx), p.LegalEntityID, p.ObjectClass, p.PeriodStart, p.PeriodEnd,
			p.SourceSystem, p.SourceQuery, p.SourceWatermark, p.Assertion, p.ExpectedCompletenessCheck, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanPopulation(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE tenant_id::text=$1 AND correlation_id=$2`,
			svcmiddleware.TenantFromContext(ctx), p.CorrelationID))
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetAuditPopulation(ctx context.Context, tenantID, populationID string) (*domain.AuditPopulation, error) {
	var out *domain.AuditPopulation
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1`, populationID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPopulationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListAuditPopulationsByEngagement(ctx context.Context, tenantID, engagementID string) ([]*domain.AuditPopulation, error) {
	var out []*domain.AuditPopulation
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE engagement_id=$1 ORDER BY created_at`, engagementID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPopulation(rows)
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

// BuildPopulation inserts the caller-supplied extract as population_rows,
// each assigned a stable ordinal — the ordering AUD-04's reproducible
// sampling later walks. DEFINED -> VALIDATING in the same transaction
// (this build's extract is a synchronous local insert, so the doc's own
// transient BUILDING state is not separately persisted — the same
// doc-vs-command collapse used throughout this domain).
func (s *PgStore) BuildPopulation(ctx context.Context, p domain.BuildPopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if len(p.Rows) == 0 {
		return nil, false, fmt.Errorf("%w: at least one row is required to build a population", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.PopulationValidating {
			out = current
			return nil // idempotent replay
		}
		if current.Status != domain.PopulationDefined {
			return domain.ErrPopulationInvalidState
		}
		for i, r := range p.Rows {
			if _, err := tx.Exec(ctx, `INSERT INTO population_rows (population_id, ordinal, source_record_id, amount, row_snapshot) VALUES ($1,$2,$3,$4,$5)`,
				p.PopulationID, int64(i+1), r.SourceRecordID, r.Amount, r.RowSnapshot); err != nil {
				return err
			}
		}
		out, err = scanPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1 WHERE population_id=$2 RETURNING `+populationColumns,
			domain.PopulationValidating, p.PopulationID))
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// ValidatePopulation records one control total per requested measure. The
// computed_value is ALWAYS derived here from population_rows — a caller
// can assert what the source system reports, never what this population
// actually contains. "record_count" and "sum_amount" are the two
// supported measure names; any other key is rejected rather than silently
// ignored.
func (s *PgStore) ValidatePopulation(ctx context.Context, p domain.ValidatePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if current.Status != domain.PopulationValidating {
			return domain.ErrPopulationInvalidState
		}

		var existingCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM population_control_totals WHERE population_id=$1`, p.PopulationID).Scan(&existingCount); err != nil {
			return err
		}
		if existingCount > 0 {
			out = current
			return nil // idempotent replay — control totals are append-only, never re-validated with new inputs
		}

		for measure, sourceValue := range p.ControlTotals {
			var computed float64
			switch measure {
			case "record_count":
				var count int64
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM population_rows WHERE population_id=$1`, p.PopulationID).Scan(&count); err != nil {
					return err
				}
				computed = float64(count)
			case "sum_amount":
				if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM population_rows WHERE population_id=$1`, p.PopulationID).Scan(&computed); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: unsupported control total measure %q", domain.ErrStoreUnavailable, measure)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO population_control_totals (population_id, measure_name, source_value, computed_value) VALUES ($1,$2,$3,$4)`,
				p.PopulationID, measure, sourceValue, computed); err != nil {
				return err
			}
		}
		out = current
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// FreezePopulation is the real enforcement of AUD-CTRL-007 "population
// completeness": the CAS predicate below only succeeds if every recorded
// control total reconciles. On failure it QUARANTINES the population
// instead of leaving it stuck — a real row recording exactly which
// measure failed (AUD-NEG-008's own fail-closed path), not a silent 500.
func (s *PgStore) FreezePopulation(ctx context.Context, p domain.FreezePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.PopulationFrozen {
			out = current
			return nil // idempotent replay
		}
		if current.Status != domain.PopulationValidating {
			return domain.ErrPopulationInvalidState
		}

		var unreconciledMeasure *string
		if err := tx.QueryRow(ctx, `SELECT measure_name FROM population_control_totals WHERE population_id=$1 AND NOT reconciled LIMIT 1`, p.PopulationID).Scan(&unreconciledMeasure); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var totalCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM population_control_totals WHERE population_id=$1`, p.PopulationID).Scan(&totalCount); err != nil {
			return err
		}
		if unreconciledMeasure != nil || totalCount == 0 {
			reason := "no control totals recorded"
			if unreconciledMeasure != nil {
				reason = fmt.Sprintf("control total %q does not reconcile", *unreconciledMeasure)
			}
			out, err = scanPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, quarantine_reason=$2 WHERE population_id=$3 RETURNING `+populationColumns,
				domain.PopulationQuarantined, reason, p.PopulationID))
			if err != nil {
				return err
			}
			changed = true
			return domain.ErrPopulationControlTotalMismatch
		}

		var rowCount int64
		rows, err := tx.Query(ctx, `SELECT ordinal, source_record_id, amount FROM population_rows WHERE population_id=$1 ORDER BY ordinal`, p.PopulationID)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		for rows.Next() {
			var ordinal int64
			var srcID string
			var amount *float64
			if err := rows.Scan(&ordinal, &srcID, &amount); err != nil {
				rows.Close()
				return err
			}
			fmt.Fprintf(hasher, "%d|%s|%v\n", ordinal, srcID, amount)
			rowCount++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		digest := hex.EncodeToString(hasher.Sum(nil))

		out, err = scanPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, row_count=$2, digest_sha256=$3, frozen_at=now() WHERE population_id=$4 RETURNING `+populationColumns,
			domain.PopulationFrozen, rowCount, digest, p.PopulationID))
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationControlTotalMismatch) {
		return out, changed, err
	}
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

func (s *PgStore) SupersedePopulation(ctx context.Context, p domain.SupersedePopulationParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.PopulationSuperseded {
			out = current
			return nil
		}
		if current.Status != domain.PopulationFrozen && current.Status != domain.PopulationInUse {
			return domain.ErrPopulationInvalidState
		}
		out, err = scanPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1 WHERE population_id=$2 RETURNING `+populationColumns,
			domain.PopulationSuperseded, p.PopulationID))
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

func (s *PgStore) QuarantinePopulation(ctx context.Context, p domain.QuarantinePopulationParams) (*domain.AuditPopulation, bool, error) {
	var out *domain.AuditPopulation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.PopulationQuarantined {
			out = current
			return nil
		}
		if current.Status == domain.PopulationSuperseded {
			return domain.ErrPopulationInvalidState
		}
		out, err = scanPopulation(tx.QueryRow(ctx, `UPDATE audit_populations SET status=$1, quarantine_reason=$2 WHERE population_id=$3 RETURNING `+populationColumns,
			domain.PopulationQuarantined, p.Reason, p.PopulationID))
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// AddControlledDelta is AUD-NEG-010's own mechanism: a late item is NEVER
// written into the frozen population (blocked outright by
// trg_reject_frozen_population_mutation even if attempted). Instead this
// creates a brand-new DEFINED population carrying the old population's own
// FROZEN rows plus the new delta rows, links the two via
// prior_population_id/population_deltas, and marks the old one SUPERSEDED
// — the caller then Validates+Freezes the new version like any other.
func (s *PgStore) AddControlledDelta(ctx context.Context, p domain.AddControlledDeltaParams) (*domain.AuditPopulation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if len(p.Rows) == 0 {
		return nil, false, fmt.Errorf("%w: at least one delta row is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPopulation
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var priorNewPopulationID string
		err := tx.QueryRow(ctx, `SELECT new_population_id FROM population_deltas d JOIN audit_populations np ON np.population_id=d.new_population_id
			WHERE d.population_id=$1 AND np.tenant_id::text=$2 AND np.correlation_id=$3`, p.PopulationID, svcmiddleware.TenantFromContext(ctx), p.CorrelationID).Scan(&priorNewPopulationID)
		if err == nil {
			out, err = scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1`, priorNewPopulationID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		old, err := scanPopulation(tx.QueryRow(ctx, `SELECT `+populationColumns+` FROM audit_populations WHERE population_id=$1 FOR UPDATE`, p.PopulationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPopulationNotFound
		}
		if err != nil {
			return err
		}
		if old.Status != domain.PopulationFrozen && old.Status != domain.PopulationInUse {
			return domain.ErrPopulationInvalidState
		}

		row := tx.QueryRow(ctx, `INSERT INTO audit_populations
			(engagement_id, tenant_id, legal_entity_id, object_class, period_start, period_end, source_system, source_query,
			 source_watermark, assertion, expected_completeness_check, prior_population_id, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING `+populationColumns,
			old.EngagementID, old.TenantID, old.LegalEntityID, old.ObjectClass, old.PeriodStart, old.PeriodEnd, old.SourceSystem, old.SourceQuery,
			old.SourceWatermark, old.Assertion, old.ExpectedCompletenessCheck, old.PopulationID, p.CreatedByPrincipalID, p.CorrelationID)
		newPop, err := scanPopulation(row)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO population_rows (population_id, ordinal, source_record_id, amount, row_snapshot)
			SELECT $1, ordinal, source_record_id, amount, row_snapshot FROM population_rows WHERE population_id=$2 ORDER BY ordinal`,
			newPop.PopulationID, old.PopulationID); err != nil {
			return err
		}
		var maxOrdinal int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(ordinal),0) FROM population_rows WHERE population_id=$1`, newPop.PopulationID).Scan(&maxOrdinal); err != nil {
			return err
		}
		for i, r := range p.Rows {
			if _, err := tx.Exec(ctx, `INSERT INTO population_rows (population_id, ordinal, source_record_id, amount, row_snapshot) VALUES ($1,$2,$3,$4,$5)`,
				newPop.PopulationID, maxOrdinal+int64(i+1), r.SourceRecordID, r.Amount, r.RowSnapshot); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO population_deltas (population_id, reason, new_population_id, created_by_principal_id) VALUES ($1,$2,$3,$4)`,
			old.PopulationID, p.Reason, newPop.PopulationID, p.CreatedByPrincipalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_populations SET status=$1, superseded_by_population_id=$2 WHERE population_id=$3`,
			domain.PopulationSuperseded, newPop.PopulationID, old.PopulationID); err != nil {
			return err
		}

		out = newPop
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetControlTotals(ctx context.Context, tenantID, populationID string) ([]*domain.PopulationControlTotal, error) {
	var out []*domain.PopulationControlTotal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ct.control_total_id, ct.population_id, ct.measure_name, ct.source_value, ct.computed_value, ct.reconciled
			FROM population_control_totals ct JOIN audit_populations p ON p.population_id=ct.population_id WHERE ct.population_id=$1`, populationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ct := &domain.PopulationControlTotal{}
			if err := rows.Scan(&ct.ControlTotalID, &ct.PopulationID, &ct.MeasureName, &ct.SourceValue, &ct.ComputedValue, &ct.Reconciled); err != nil {
				return err
			}
			out = append(out, ct)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
