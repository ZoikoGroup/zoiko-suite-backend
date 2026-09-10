package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

const profitabilityProjectionColumns = `
	projection_id, legal_entity_id, project_id, status,
	revenue, cost, margin, billed_amount, unbilled_amount,
	cost_watermark_at, revenue_run_id, revenue_watermark_at,
	refreshed_at, refreshed_by_principal_id, created_at`

func scanProjection(row pgx.Row) (*domain.ProfitabilityProjection, error) {
	var p domain.ProfitabilityProjection
	if err := row.Scan(
		&p.ProjectionID, &p.LegalEntityID, &p.ProjectID, &p.Status,
		&p.Revenue, &p.Cost, &p.Margin, &p.BilledAmount, &p.UnbilledAmount,
		&p.CostWatermarkAt, &p.RevenueRunID, &p.RevenueWatermarkAt,
		&p.RefreshedAt, &p.RefreshedByPrincipalID, &p.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// computeLiveProfitability re-derives revenue/cost/margin/billed/unbilled
// and their own watermarks directly from PRJ-02's real project_cost_entries
// and PRJ-03's real project_recognition_runs (in-process, same database —
// the same cross-capability read pattern used throughout this sub-domain).
// Never writes to either table. Revenue is read from the project's own
// most recently calculated, non-draft/frozen/superseded recognition run —
// the same "usable run" filter PRJ-03's own prior-cumulative query uses.
func computeLiveProfitability(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (revenue, cost, margin, billed, unbilled float64, costWatermark time.Time, runID *string, revenueWatermark *time.Time, err error) {
	if err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0), COALESCE(MAX(created_at), TIMESTAMP WITH TIME ZONE 'epoch')
		FROM project_cost_entries WHERE tenant_id = $1 AND project_id = $2 AND status != 'REVERSED'
	`, tenantID, projectID).Scan(&cost, &costWatermark); err != nil {
		return
	}

	var cumulativeRevenue, billedToDate, balanceAmount *float64
	var latestRunID string
	var latestCalculatedAt time.Time
	rowErr := tx.QueryRow(ctx, `
		SELECT run_id, cumulative_recognized_revenue, billed_to_date, balance_amount, calculated_at
		FROM project_recognition_runs
		WHERE tenant_id = $1 AND project_id = $2 AND status NOT IN ('DRAFT', 'POPULATION_FROZEN', 'SUPERSEDED')
		ORDER BY calculated_at DESC LIMIT 1
	`, tenantID, projectID).Scan(&latestRunID, &cumulativeRevenue, &billedToDate, &balanceAmount, &latestCalculatedAt)
	if rowErr != nil && !errors.Is(rowErr, pgx.ErrNoRows) {
		err = rowErr
		return
	}
	if rowErr == nil {
		runID = &latestRunID
		revenueWatermark = &latestCalculatedAt
		if cumulativeRevenue != nil {
			revenue = *cumulativeRevenue
		}
		if billedToDate != nil {
			billed = *billedToDate
		}
		if balanceAmount != nil && *balanceAmount > 0 {
			unbilled = *balanceAmount
		}
	}

	margin = roundCents(revenue - cost)
	return revenue, cost, margin, billed, unbilled, costWatermark, runID, revenueWatermark, nil
}

// RefreshProfitabilityProjection is RefreshProfitabilityProjection +
// RebuildProjection, collapsed into one real recompute — see migration
// 000004's doc comment. A real upsert: exactly one live projection per
// project.
func (s *PgStore) RefreshProfitabilityProjection(ctx context.Context, projectID, principalID string, at time.Time) (*domain.ProfitabilityProjection, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ProfitabilityProjection
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var legalEntityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM projects WHERE project_id = $1 AND tenant_id = $2`, projectID, tenantID).Scan(&legalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProjectNotFound
			}
			return err
		}

		revenue, cost, margin, billed, unbilled, costWatermark, runID, revenueWatermark, err := computeLiveProfitability(ctx, tx, tenantID, projectID)
		if err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO project_profitability_projections (
				projection_id, tenant_id, legal_entity_id, project_id, status,
				revenue, cost, margin, billed_amount, unbilled_amount,
				cost_watermark_at, revenue_run_id, revenue_watermark_at,
				refreshed_at, refreshed_by_principal_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $14)
			ON CONFLICT (tenant_id, project_id) DO UPDATE SET
				status = $5, revenue = $6, cost = $7, margin = $8, billed_amount = $9, unbilled_amount = $10,
				cost_watermark_at = $11, revenue_run_id = $12, revenue_watermark_at = $13,
				refreshed_at = $14, refreshed_by_principal_id = $15
			RETURNING `+profitabilityProjectionColumns,
			uuidNewString(), tenantID, legalEntityID, projectID, domain.ProfitabilityProjectionStatusCurrent,
			revenue, cost, margin, billed, unbilled, costWatermark, runID, revenueWatermark, at, principalID)
		p, err := scanProjection(row)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkFreshness re-derives the live cost/revenue watermarks and, if
// either has moved past what the stored projection knows about, marks
// the projection STALE in place — the real enforcement of negative path
// #1, "Stale project margin shown as certified," run both on every read
// and immediately before a snapshot is built or certified.
func checkFreshness(ctx context.Context, tx pgx.Tx, tenantID, projectID string, p *domain.ProfitabilityProjection) error {
	_, _, _, _, _, liveCostWatermark, liveRunID, liveRevenueWatermark, err := computeLiveProfitability(ctx, tx, tenantID, projectID)
	if err != nil {
		return err
	}
	stale := liveCostWatermark.After(p.CostWatermarkAt)
	if !stale {
		liveHasRun := liveRunID != nil
		storedHasRun := p.RevenueRunID != nil
		switch {
		case liveHasRun != storedHasRun:
			stale = true
		case liveHasRun && storedHasRun && *liveRunID != *p.RevenueRunID:
			stale = true
		case liveHasRun && storedHasRun && liveRevenueWatermark != nil && p.RevenueWatermarkAt != nil && liveRevenueWatermark.After(*p.RevenueWatermarkAt):
			stale = true
		}
	}
	if !stale || p.Status == domain.ProfitabilityProjectionStatusStale {
		if stale {
			p.Status = domain.ProfitabilityProjectionStatusStale
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE project_profitability_projections SET status = $1 WHERE tenant_id = $2 AND project_id = $3`,
		domain.ProfitabilityProjectionStatusStale, tenantID, projectID); err != nil {
		return err
	}
	p.Status = domain.ProfitabilityProjectionStatusStale
	return nil
}

// GetProjectProfitability is GetProjectProfitability + GetFreshness,
// served by the same read — the response's own status field IS the
// freshness label.
func (s *PgStore) GetProjectProfitability(ctx context.Context, projectID string) (*domain.ProfitabilityProjection, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ProfitabilityProjection
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+profitabilityProjectionColumns+` FROM project_profitability_projections WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID)
		p, err := scanProjection(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrProjectionNotBuilt
		}
		if err != nil {
			return err
		}
		if err := checkFreshness(ctx, tx, tenantID, projectID, p); err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BuildProfitabilitySnapshot is BuildProfitabilitySnapshot, landing
// directly in RECONCILED — see migration 000004's doc comment on why
// DRAFT is unreachable in this v1. Refuses if the live projection is (or
// has just become) STALE.
func (s *PgStore) BuildProfitabilitySnapshot(ctx context.Context, projectID, principalID string, at time.Time) (*domain.ProfitabilitySnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ProfitabilitySnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var legalEntityID string
		row := tx.QueryRow(ctx, `SELECT `+profitabilityProjectionColumns+` FROM project_profitability_projections WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID)
		p, err := scanProjection(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrProjectionNotBuilt
		}
		if err != nil {
			return err
		}
		if err := checkFreshness(ctx, tx, tenantID, projectID, p); err != nil {
			return err
		}
		if p.Status != domain.ProfitabilityProjectionStatusCurrent {
			return domain.ErrProjectionStale
		}
		legalEntityID = p.LegalEntityID

		snapshotRow := tx.QueryRow(ctx, `
			INSERT INTO project_profitability_snapshots (
				snapshot_id, tenant_id, legal_entity_id, project_id, status,
				revenue, cost, margin, billed_amount, unbilled_amount,
				cost_watermark_at, revenue_run_id, revenue_watermark_at,
				built_at, built_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			RETURNING `+profitabilitySnapshotColumns,
			uuidNewString(), tenantID, legalEntityID, projectID, domain.ProfitabilitySnapshotStatusReconciled,
			p.Revenue, p.Cost, p.Margin, p.BilledAmount, p.UnbilledAmount,
			p.CostWatermarkAt, p.RevenueRunID, p.RevenueWatermarkAt, at, principalID)
		s, err := scanSnapshot(snapshotRow)
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const profitabilitySnapshotColumns = `
	snapshot_id, legal_entity_id, project_id, status,
	revenue, cost, margin, billed_amount, unbilled_amount,
	cost_watermark_at, revenue_run_id, revenue_watermark_at,
	built_at, built_by_principal_id, certified_at, certified_by_principal_id`

func scanSnapshot(row pgx.Row) (*domain.ProfitabilitySnapshot, error) {
	var s domain.ProfitabilitySnapshot
	if err := row.Scan(
		&s.SnapshotID, &s.LegalEntityID, &s.ProjectID, &s.Status,
		&s.Revenue, &s.Cost, &s.Margin, &s.BilledAmount, &s.UnbilledAmount,
		&s.CostWatermarkAt, &s.RevenueRunID, &s.RevenueWatermarkAt,
		&s.BuiltAt, &s.BuiltByPrincipalID, &s.CertifiedAt, &s.CertifiedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *PgStore) GetProfitabilitySnapshot(ctx context.Context, snapshotID string) (*domain.ProfitabilitySnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ProfitabilitySnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+profitabilitySnapshotColumns+` FROM project_profitability_snapshots WHERE snapshot_id = $1 AND tenant_id = $2`, snapshotID, tenantID)
		snap, err := scanSnapshot(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSnapshotNotFound
		}
		if err != nil {
			return err
		}
		out = snap
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CertifyProfitabilitySnapshot re-verifies the snapshot's own frozen
// watermarks against LIVE source data before certifying — the real
// enforcement of negative path #1 at certify time, not just at build
// time. No self-certification refusal: see migration 000004's doc
// comment on why that SoD clause is not enforceable in this v1.
func (s *PgStore) CertifyProfitabilitySnapshot(ctx context.Context, snapshotID, principalID string, at time.Time) (*domain.ProfitabilitySnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ProfitabilitySnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+profitabilitySnapshotColumns+` FROM project_profitability_snapshots WHERE snapshot_id = $1 AND tenant_id = $2`, snapshotID, tenantID)
		snap, err := scanSnapshot(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSnapshotNotFound
		}
		if err != nil {
			return err
		}
		if snap.Status != domain.ProfitabilitySnapshotStatusReconciled {
			return domain.ErrInvalidSnapshotTransition
		}

		_, _, _, _, _, liveCostWatermark, liveRunID, liveRevenueWatermark, err := computeLiveProfitability(ctx, tx, tenantID, snap.ProjectID)
		if err != nil {
			return err
		}
		if liveCostWatermark.After(snap.CostWatermarkAt) {
			return domain.ErrSnapshotStaleAtCertification
		}
		liveHasRun := liveRunID != nil
		snapHasRun := snap.RevenueRunID != nil
		if liveHasRun != snapHasRun {
			return domain.ErrSnapshotStaleAtCertification
		}
		if liveHasRun && snapHasRun {
			if *liveRunID != *snap.RevenueRunID {
				return domain.ErrSnapshotStaleAtCertification
			}
			if liveRevenueWatermark != nil && snap.RevenueWatermarkAt != nil && liveRevenueWatermark.After(*snap.RevenueWatermarkAt) {
				return domain.ErrSnapshotStaleAtCertification
			}
		}

		tag, err := tx.Exec(ctx, `
			UPDATE project_profitability_snapshots SET status = $1, certified_at = $2, certified_by_principal_id = $3
			WHERE snapshot_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.ProfitabilitySnapshotStatusCertified, at, principalID, snapshotID, domain.ProfitabilitySnapshotStatusReconciled, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidSnapshotTransition
		}
		snap.Status, snap.CertifiedAt, snap.CertifiedByPrincipalID = domain.ProfitabilitySnapshotStatusCertified, &at, &principalID
		out = snap
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
