package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.SealRepo = (*KernelRepo)(nil)

func sealLockKey(tenant, entity, period string) string {
	return "period:" + tenant + ":" + entity + ":" + period
}

// assertPeriodOpen is called inside every posting transaction. It takes the period's lock in shared mode, so any number of
// postings proceed together, and refuses when the period is sealed. Sealing takes the same lock exclusively and therefore waits for
// the postings already in flight: a posting either commits before the seal's watermark is taken or sees the seal and is refused.
func assertPeriodOpen(ctx context.Context, tx pgx.Tx, tenant, entity, period string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, sealLockKey(tenant, entity, period)); err != nil {
		return err
	}
	var status string
	var wm int64
	err := tx.QueryRow(ctx, `SELECT status, watermark FROM period_seals WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3`, tenant, entity, period).Scan(&status, &wm)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapPgError(err)
	}
	if status == "SEALED" {
		return fmt.Errorf("%w: period %s is sealed at ledger watermark %d", domain.ErrPeriodLocked, period, wm)
	}
	return nil
}

const sealCols = `legal_entity_id::text, fiscal_period, status, watermark, sealed_by, sealed_at, COALESCE(reopened_by, ''), reopened_at, COALESCE(reopen_authority, ''), COALESCE(reopen_reason, ''), seal_count`

func scanSeal(row pgx.Row, s *kernel.PeriodSeal) error {
	return row.Scan(&s.LegalEntityID, &s.FiscalPeriod, &s.Status, &s.Watermark, &s.SealedBy, &s.SealedAt, &s.ReopenedBy, &s.ReopenedAt, &s.ReopenAuthority, &s.ReopenReason, &s.SealCount)
}

// ApplySeal implements kernel.SealRepo: seal a period, reopen it under named authority, or seal it again after the reopen.
func (r *KernelRepo) ApplySeal(ctx context.Context, tenant, entity, period, action, actor, authorityRef, reason string, at time.Time) (*kernel.PeriodSeal, error) {
	if _, err := uuid.Parse(entity); err != nil {
		return nil, kernel.ErrNotFound
	}
	var out kernel.PeriodSeal
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, sealLockKey(tenant, entity, period)); e != nil {
			return e
		}
		var cur kernel.PeriodSeal
		found := true
		if e := scanSeal(t.QueryRow(ctx, `SELECT `+sealCols+` FROM period_seals WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3 FOR UPDATE`, tenant, entity, period), &cur); e != nil {
			if !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
			found = false
		}
		var wm int64
		if e := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, tenant, entity).Scan(&wm); e != nil {
			return e
		}
		event := func(kind string, w int64, since *int64) error {
			_, e := t.Exec(ctx, `INSERT INTO period_seal_events (event_id, tenant_id, legal_entity_id, fiscal_period, kind, watermark, actor, authority_ref, reason, entries_since_reopen, at)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11)`, uuid.NewString(), tenant, entity, period, kind, w, actor, authorityRef, reason, since, at)
			return e
		}
		raise := func(fact string, s *kernel.PeriodSeal) error {
			return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: fact, AggregateType: "period-seal", AggregateID: entity + ":" + period, Actor: actor, LegalEntityID: entity,
				Data: map[string]any{"legal_entity_id": entity, "fiscal_period": period, "status": s.Status, "ledger_watermark": s.Watermark, "authority_ref": authorityRef}})
		}
		switch action {
		case kernel.SealActionSeal:
			if found {
				if cur.Status == "SEALED" {
					out = cur
					out.Existing = true
					return nil
				}
				return kernel.ErrState // reopened: it is re-sealed, not sealed
			}
			if _, e := t.Exec(ctx, `INSERT INTO period_seals (tenant_id, legal_entity_id, fiscal_period, status, watermark, sealed_by, sealed_at) VALUES ($1::uuid, $2::uuid, $3, 'SEALED', $4, $5, $6)`,
				tenant, entity, period, wm, actor, at); e != nil {
				return e
			}
			if e := event("SEAL", wm, nil); e != nil {
				return e
			}
			out = kernel.PeriodSeal{LegalEntityID: entity, FiscalPeriod: period, Status: "SEALED", Watermark: wm, SealedBy: actor, SealedAt: at, SealCount: 1}
			return raise("period.sealed", &out)
		case kernel.SealActionReopen:
			if !found || cur.Status != "SEALED" {
				return kernel.ErrState
			}
			if _, e := t.Exec(ctx, `UPDATE period_seals SET status = 'REOPENED', reopened_by = $4, reopened_at = $5, reopen_authority = $6, reopen_reason = $7
				WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3`, tenant, entity, period, actor, at, authorityRef, reason); e != nil {
				return e
			}
			if e := event("REOPEN", cur.Watermark, nil); e != nil {
				return e
			}
			out = cur
			out.Status, out.ReopenedBy, out.ReopenedAt, out.ReopenAuthority, out.ReopenReason = "REOPENED", actor, &at, authorityRef, reason
			return raise("period.reopened", &out)
		case kernel.SealActionReseal:
			if !found || cur.Status != "REOPENED" {
				return kernel.ErrState
			}
			var since int64
			if e := t.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND entry_seq > $3`, tenant, entity, cur.Watermark).Scan(&since); e != nil {
				return e
			}
			if _, e := t.Exec(ctx, `UPDATE period_seals SET status = 'SEALED', watermark = $4, sealed_by = $5, sealed_at = $6, seal_count = seal_count + 1
				WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3`, tenant, entity, period, wm, actor, at); e != nil {
				return e
			}
			if e := event("RESEAL", wm, &since); e != nil {
				return e
			}
			out = cur
			out.Status, out.Watermark, out.SealedBy, out.SealedAt, out.SealCount, out.EntriesSinceReopen = "SEALED", wm, actor, at, cur.SealCount+1, since
			return raise("period.resealed", &out)
		}
		return kernel.ErrBadRequest
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &out, nil
}

// GetSeal implements kernel.SealRepo: the current seal of a period and its history.
func (r *KernelRepo) GetSeal(ctx context.Context, tenant, entity, period string) (*kernel.PeriodSeal, []kernel.SealEvent, error) {
	if _, err := uuid.Parse(entity); err != nil {
		return nil, nil, kernel.ErrNotFound
	}
	var s kernel.PeriodSeal
	events := []kernel.SealEvent{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := scanSeal(t.QueryRow(ctx, `SELECT `+sealCols+` FROM period_seals WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3`, tenant, entity, period), &s); e != nil {
			return e
		}
		rows, e := t.Query(ctx, `SELECT kind, watermark, actor, COALESCE(authority_ref, ''), COALESCE(reason, ''), entries_since_reopen, at FROM period_seal_events
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3 ORDER BY at, event_id`, tenant, entity, period)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var ev kernel.SealEvent
			if e := rows.Scan(&ev.Kind, &ev.Watermark, &ev.Actor, &ev.AuthorityRef, &ev.Reason, &ev.EntriesSinceReopen, &ev.At); e != nil {
				return e
			}
			events = append(events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, kernelErr(err)
	}
	return &s, events, nil
}

// ListSeals implements kernel.SealRepo.
func (r *KernelRepo) ListSeals(ctx context.Context, tenant, entity string) ([]kernel.PeriodSeal, error) {
	out := []kernel.PeriodSeal{}
	if entity != "" {
		if _, err := uuid.Parse(entity); err != nil {
			return out, nil
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+sealCols+` FROM period_seals WHERE tenant_id = $1::uuid AND ($2 = '' OR legal_entity_id::text = $2) ORDER BY legal_entity_id, fiscal_period LIMIT 1000`, tenant, entity)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var s kernel.PeriodSeal
			if e := scanSeal(rows, &s); e != nil {
				return e
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}
