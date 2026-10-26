package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

const adjCols = `adjustment_id::text, group_id::text, fiscal_period, description, policy_ref, evidence_ref, lines, status, created_by, created_at, COALESCE(approved_by, ''), approved_at,
	COALESCE(withdrawn_by, ''), withdrawn_at, COALESCE(withdrawal_reason, ''), COALESCE(source_ref, '')`

func scanAdjustment(row pgx.Row, a *kernel.ConsolAdjustment) error {
	var lines []byte
	if err := row.Scan(&a.AdjustmentID, &a.GroupID, &a.FiscalPeriod, &a.Description, &a.PolicyRef, &a.EvidenceRef, &lines, &a.Status, &a.CreatedBy, &a.CreatedAt, &a.ApprovedBy, &a.ApprovedAt,
		&a.WithdrawnBy, &a.WithdrawnAt, &a.WithdrawalReason, &a.SourceRef); err != nil {
		return err
	}
	return json.Unmarshal(lines, &a.Lines)
}

// InsertAdjustment implements kernel.ConsolRepo.
func (r *KernelRepo) InsertAdjustment(ctx context.Context, tenant string, a *kernel.ConsolAdjustment, out kernel.OutEvent) error {
	return kernelErr(r.tx(ctx, tenant, func(t pgx.Tx) error {
		lines, _ := json.Marshal(a.Lines)
		if _, e := t.Exec(ctx, `INSERT INTO consolidation_adjustments (adjustment_id, tenant_id, group_id, fiscal_period, description, policy_ref, evidence_ref, lines, status, created_by, created_at, source_ref)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8::jsonb, 'PROPOSED', $9, $10, NULLIF($11, ''))`, a.AdjustmentID, tenant, a.GroupID, a.FiscalPeriod, a.Description, a.PolicyRef, a.EvidenceRef,
			string(lines), a.CreatedBy, a.CreatedAt, a.SourceRef); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	}))
}

// GetAdjustment implements kernel.ConsolRepo.
func (r *KernelRepo) GetAdjustment(ctx context.Context, tenant, id string) (*kernel.ConsolAdjustment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var a kernel.ConsolAdjustment
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanAdjustment(t.QueryRow(ctx, `SELECT `+adjCols+` FROM consolidation_adjustments WHERE tenant_id = $1::uuid AND adjustment_id = $2::uuid`, tenant, id), &a)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &a, nil
}

func (r *KernelRepo) adjustments(ctx context.Context, tenant, groupID, period string, approvedUpTo bool) ([]kernel.ConsolAdjustment, error) {
	out := []kernel.ConsolAdjustment{}
	if _, err := uuid.Parse(groupID); err != nil {
		return out, nil
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		q := `SELECT ` + adjCols + ` FROM consolidation_adjustments WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND `
		if approvedUpTo {
			q += `status = 'APPROVED' AND fiscal_period <= $3 ORDER BY fiscal_period, created_at, adjustment_id`
		} else {
			q += `($3 = '' OR fiscal_period = $3) ORDER BY fiscal_period DESC, created_at DESC LIMIT 200`
		}
		rows, e := t.Query(ctx, q, tenant, groupID, period)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a kernel.ConsolAdjustment
			if e := scanAdjustment(rows, &a); e != nil {
				return e
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ListAdjustments implements kernel.ConsolRepo.
func (r *KernelRepo) ListAdjustments(ctx context.Context, tenant, groupID, period string) ([]kernel.ConsolAdjustment, error) {
	return r.adjustments(ctx, tenant, groupID, period, false)
}

// ApprovedAdjustments implements kernel.ConsolRepo.
func (r *KernelRepo) ApprovedAdjustments(ctx context.Context, tenant, groupID, upToPeriod string) ([]kernel.ConsolAdjustment, error) {
	return r.adjustments(ctx, tenant, groupID, upToPeriod, true)
}

// ApproveAdjustment implements kernel.ConsolRepo.
func (r *KernelRepo) ApproveAdjustment(ctx context.Context, tenant, id, actor string, at time.Time, out kernel.OutEvent) error {
	return kernelErr(r.tx(ctx, tenant, func(t pgx.Tx) error {
		tag, e := t.Exec(ctx, `UPDATE consolidation_adjustments SET status = 'APPROVED', approved_by = $3, approved_at = $4 WHERE tenant_id = $1::uuid AND adjustment_id = $2::uuid AND status = 'PROPOSED'`,
			tenant, id, actor, at)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return kernel.ErrState
		}
		return r.enqueue(ctx, t, tenant, out)
	}))
}

// WithdrawAdjustment implements kernel.ConsolRepo.
func (r *KernelRepo) WithdrawAdjustment(ctx context.Context, tenant, id, actor, reason string, at time.Time, out kernel.OutEvent) error {
	return kernelErr(r.tx(ctx, tenant, func(t pgx.Tx) error {
		tag, e := t.Exec(ctx, `UPDATE consolidation_adjustments SET status = 'WITHDRAWN', withdrawn_by = $3, withdrawn_at = $4, withdrawal_reason = $5
			WHERE tenant_id = $1::uuid AND adjustment_id = $2::uuid AND status IN ('PROPOSED', 'APPROVED')`, tenant, id, actor, at, reason)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return kernel.ErrState
		}
		return r.enqueue(ctx, t, tenant, out)
	}))
}

// GetAdjustmentBySourceRef implements kernel.ConsolRepo.
func (r *KernelRepo) GetAdjustmentBySourceRef(ctx context.Context, tenant, ref string) (*kernel.ConsolAdjustment, error) {
	var a kernel.ConsolAdjustment
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanAdjustment(t.QueryRow(ctx, `SELECT `+adjCols+` FROM consolidation_adjustments WHERE tenant_id = $1::uuid AND source_ref = $2`, tenant, ref), &a)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &a, nil
}
