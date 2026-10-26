package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.FXRepo = (*KernelRepo)(nil)

const fxCols = `fx_rate_id::text, tenant_id::text, base_currency, quote_currency, rate::text, rate_type, effective_at, rate_date::text, source, source_rate_id, ingested_at, ingested_by,
	override, COALESCE(override_reason, ''), COALESCE(override_approved_by, '')`

func scanRate(row pgx.Row, r *kernel.FXRate) error {
	return row.Scan(&r.FXRateID, &r.TenantID, &r.Base, &r.Quote, &r.Rate, &r.RateType, &r.EffectiveAt, &r.RateDate, &r.Source, &r.SourceRateID, &r.IngestedAt, &r.IngestedBy,
		&r.Override, &r.OverrideReason, &r.OverrideApprovedBy)
}

func trimRate(s string) string {
	// NUMERIC(28,12) comes back with trailing zeros; the rate is shown as it was entered
	if i := indexByte(s, '.'); i >= 0 {
		end := len(s)
		for end > i+1 && s[end-1] == '0' {
			end--
		}
		if end == i+1 {
			end = i
		}
		return s[:end]
	}
	return s
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// InsertRate implements kernel.FXRepo.
func (r *KernelRepo) InsertRate(ctx context.Context, rate *kernel.FXRate, out kernel.OutEvent) (*kernel.FXRate, bool, error) {
	var stored kernel.FXRate
	existing := false
	err := r.tx(ctx, rate.TenantID, func(t pgx.Tx) error {
		tag, e := t.Exec(ctx, `INSERT INTO fx_rates (fx_rate_id, tenant_id, base_currency, quote_currency, rate, rate_type, effective_at, rate_date, source, source_rate_id, ingested_at, ingested_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::numeric, $6, $7, $8::date, $9, $10, $11, $12) ON CONFLICT (tenant_id, source, source_rate_id) DO NOTHING`,
			rate.FXRateID, rate.TenantID, rate.Base, rate.Quote, rate.Rate, rate.RateType, rate.EffectiveAt, rate.RateDate, rate.Source, rate.SourceRateID, rate.IngestedAt, rate.IngestedBy)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 1 {
			stored = *rate
			return r.enqueue(ctx, t, rate.TenantID, out)
		}
		if e := scanRate(t.QueryRow(ctx, `SELECT `+fxCols+` FROM fx_rates WHERE tenant_id = $1::uuid AND source = $2 AND source_rate_id = $3`, rate.TenantID, rate.Source, rate.SourceRateID), &stored); e != nil {
			return e
		}
		existing = true
		stored.Rate = trimRate(stored.Rate)
		if stored.Base != rate.Base || stored.Quote != rate.Quote || stored.Rate != rate.Rate || stored.RateType != rate.RateType || !stored.EffectiveAt.Equal(rate.EffectiveAt.Truncate(time.Microsecond)) {
			return &kernel.Exception{Family: kernel.FamData, Code: "SOURCE_RATE_CONFLICT", Detail: "this source reference was already ingested with a different rate",
				Facts: map[string]any{"fx_rate_id": stored.FXRateID}}
		}
		return nil
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, existing, err
		}
		return nil, false, kernelErr(err)
	}
	stored.Rate = trimRate(stored.Rate)
	return &stored, existing, nil
}

// ListRates implements kernel.FXRepo.
func (r *KernelRepo) ListRates(ctx context.Context, tenant string, f kernel.FXFilter) ([]kernel.FXRate, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := []kernel.FXRate{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+fxCols+` FROM fx_rates WHERE tenant_id = $1::uuid AND ($2 = '' OR base_currency = $2) AND ($3 = '' OR quote_currency = $3) AND ($4 = '' OR rate_type = $4)
			AND ($5::timestamptz IS NULL OR effective_at >= $5) AND ($6::timestamptz IS NULL OR effective_at <= $6) ORDER BY effective_at DESC, ingested_at DESC LIMIT $7`,
			tenant, f.Base, f.Quote, f.RateType, f.From, f.To, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var x kernel.FXRate
			if e := scanRate(rows, &x); e != nil {
				return e
			}
			x.Rate = trimRate(x.Rate)
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// BestRate implements kernel.FXRepo.
func (r *KernelRepo) BestRate(ctx context.Context, tenant, base, quote, rateType string, at time.Time) (*kernel.FXRate, error) {
	var x kernel.FXRate
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanRate(t.QueryRow(ctx, `SELECT `+fxCols+` FROM fx_rates WHERE tenant_id = $1::uuid AND base_currency = $2 AND quote_currency = $3 AND rate_type = $4 AND effective_at <= $5
			ORDER BY effective_at DESC, ingested_at DESC LIMIT 1`, tenant, base, quote, rateType, at), &x)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	x.Rate = trimRate(x.Rate)
	return &x, nil
}

const ovCols = `request_id::text, tenant_id::text, base_currency, quote_currency, rate::text, rate_type, effective_at, rate_date::text, source, source_rate_id, reason, requested_by, requested_at,
	status, COALESCE(decided_by, ''), decided_at, COALESCE(decision_note, ''), COALESCE(fx_rate_id::text, '')`

func scanOverride(row pgx.Row, o *kernel.FXOverride) error {
	if err := row.Scan(&o.RequestID, &o.TenantID, &o.Base, &o.Quote, &o.Rate, &o.RateType, &o.EffectiveAt, &o.RateDate, &o.Source, &o.SourceRate, &o.Reason, &o.RequestedBy, &o.RequestedAt,
		&o.Status, &o.DecidedBy, &o.DecidedAt, &o.Note, &o.FXRateID); err != nil {
		return err
	}
	o.Rate = trimRate(o.Rate)
	return nil
}

// InsertOverride implements kernel.FXRepo.
func (r *KernelRepo) InsertOverride(ctx context.Context, o *kernel.FXOverride, out kernel.OutEvent) error {
	return r.tx(ctx, o.TenantID, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `INSERT INTO fx_rate_override_requests (request_id, tenant_id, base_currency, quote_currency, rate, rate_type, effective_at, rate_date, source, source_rate_id, reason, requested_by, requested_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::numeric, $6, $7, $8::date, $9, $10, $11, $12, $13)`,
			o.RequestID, o.TenantID, o.Base, o.Quote, o.Rate, o.RateType, o.EffectiveAt, o.RateDate, o.Source, o.SourceRate, o.Reason, o.RequestedBy, o.RequestedAt); e != nil {
			return e
		}
		return r.enqueue(ctx, t, o.TenantID, out)
	})
}

// GetOverride implements kernel.FXRepo.
func (r *KernelRepo) GetOverride(ctx context.Context, tenant, id string) (*kernel.FXOverride, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var o kernel.FXOverride
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanOverride(t.QueryRow(ctx, `SELECT `+ovCols+` FROM fx_rate_override_requests WHERE tenant_id = $1::uuid AND request_id = $2::uuid`, tenant, id), &o)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &o, nil
}

// ListOverrides implements kernel.FXRepo.
func (r *KernelRepo) ListOverrides(ctx context.Context, tenant, status string) ([]kernel.FXOverride, error) {
	out := []kernel.FXOverride{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+ovCols+` FROM fx_rate_override_requests WHERE tenant_id = $1::uuid AND ($2 = '' OR status = $2) ORDER BY requested_at DESC LIMIT 500`, tenant, status)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var o kernel.FXOverride
			if e := scanOverride(rows, &o); e != nil {
				return e
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// DecideOverride implements kernel.FXRepo: approval creates the override rate in the same transaction as the decision.
func (r *KernelRepo) DecideOverride(ctx context.Context, tenant, id, decider string, approve bool, note string, at time.Time, out kernel.OutEvent) (*kernel.FXOverride, *kernel.FXRate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil, kernel.ErrNotFound
	}
	var o kernel.FXOverride
	var rate *kernel.FXRate
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := scanOverride(t.QueryRow(ctx, `SELECT `+ovCols+` FROM fx_rate_override_requests WHERE tenant_id = $1::uuid AND request_id = $2::uuid FOR UPDATE`, tenant, id), &o); e != nil {
			return e
		}
		if o.Status != "PENDING" {
			return kernel.ErrState
		}
		if approve && o.RequestedBy == decider {
			return kernel.ErrSelfApproval
		}
		var rateID any
		if approve {
			x := &kernel.FXRate{FXRateID: uuid.NewString(), TenantID: tenant, Base: o.Base, Quote: o.Quote, Rate: o.Rate, RateType: o.RateType, EffectiveAt: o.EffectiveAt, RateDate: o.RateDate,
				Source: o.Source, SourceRateID: o.SourceRate, IngestedAt: at, IngestedBy: o.RequestedBy, Override: true, OverrideReason: o.Reason, OverrideApprovedBy: decider}
			if _, e := t.Exec(ctx, `INSERT INTO fx_rates (fx_rate_id, tenant_id, base_currency, quote_currency, rate, rate_type, effective_at, rate_date, source, source_rate_id, ingested_at, ingested_by,
					override, override_reason, override_approved_by)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5::numeric, $6, $7, $8::date, $9, $10, $11, $12, true, $13, $14)`,
				x.FXRateID, tenant, x.Base, x.Quote, x.Rate, x.RateType, x.EffectiveAt, x.RateDate, x.Source, x.SourceRateID, x.IngestedAt, x.IngestedBy, x.OverrideReason, x.OverrideApprovedBy); e != nil {
				return e
			}
			rate, rateID = x, x.FXRateID
		}
		status := "REJECTED"
		if approve {
			status = "APPROVED"
		}
		if _, e := t.Exec(ctx, `UPDATE fx_rate_override_requests SET status = $3, decided_by = $4, decided_at = $5, decision_note = NULLIF($6, ''), fx_rate_id = $7::uuid
			WHERE tenant_id = $1::uuid AND request_id = $2::uuid`, tenant, id, status, decider, at, note, rateID); e != nil {
			return e
		}
		o.Status, o.DecidedBy, o.DecidedAt, o.Note = status, decider, &at, note
		if rate != nil {
			o.FXRateID = rate.FXRateID
			out.Data["fx_rate_id"] = rate.FXRateID
		}
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, nil, kernelErr(err)
	}
	if errors.Is(err, nil) && rate != nil {
		rate.Rate = trimRate(rate.Rate)
	}
	return &o, rate, nil
}
