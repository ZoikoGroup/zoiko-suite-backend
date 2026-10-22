package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
)

// OutboundRepo is the Postgres implementation of outbound.Repo (migration 007).
type OutboundRepo struct{ p *PgStore }

// NewOutboundRepo wraps a PgStore.
func NewOutboundRepo(p *PgStore) *OutboundRepo { return &OutboundRepo{p: p} }

var _ outbound.Repo = (*OutboundRepo)(nil)

func (r *OutboundRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e outbound.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

func outboundErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23505":
		if strings.Contains(msg, "xic_oa_logical") || strings.Contains(msg, "xic_cr_dedupe") {
			return outbound.ErrDuplicate
		}
	case "23000", "23514":
		if strings.Contains(msg, "cannot go") || strings.Contains(msg, "final") || strings.Contains(msg, "may not be submitted again") ||
			strings.Contains(msg, "submit_count") || strings.Contains(msg, "xic_oa_") {
			return outbound.ErrTransition
		}
	}
	return err
}

const outboundCols = `attempt_id::text, tenant_id, connection_id::text, connector_id, operation_id, contract_version_id::text, domain_request_ref,
	payload_profile, payload_hash, payload_size, provider_idem_ref_hash, resubmit_allowed, state, ambiguity_reason, provider_reference, provider_status,
	submit_count, last_submit_at, resolve_count, next_resolution_at, escalated_at, resolution_method, resolved_by, evidence_ref, final_outcome,
	body_expires_at, correlation_id, requested_by, version, created_at, updated_at, (body IS NOT NULL)`

func scanOutbound(row pgx.Row, a *outbound.Attempt) error {
	var fin []byte
	if err := row.Scan(&a.AttemptID, &a.TenantID, &a.ConnectionID, &a.ConnectorID, &a.OperationID, &a.ContractVersionID, &a.DomainRequestRef,
		&a.PayloadProfile, &a.PayloadHash, &a.PayloadSize, &a.ProviderIdemRefHash, &a.ResubmitAllowed, &a.State, &a.AmbiguityReason, &a.ProviderReference,
		&a.ProviderStatus, &a.SubmitCount, &a.LastSubmitAt, &a.ResolveCount, &a.NextResolutionAt, &a.EscalatedAt, &a.ResolutionMethod, &a.ResolvedBy,
		&a.EvidenceRef, &fin, &a.BodyExpiresAt, &a.CorrelationID, &a.RequestedBy, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.BodyRetained); err != nil {
		return err
	}
	if len(fin) > 0 && string(fin) != "null" {
		a.FinalOutcomeRef = json.RawMessage(fin)
	}
	return nil
}

func loadOutbound(ctx context.Context, tx pgx.Tx, id string, bodies, forUpdate bool) (*outbound.Attempt, error) {
	var a outbound.Attempt
	q := `SELECT ` + outboundCols + ` FROM xic_outbound_attempts WHERE attempt_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanOutbound(tx.QueryRow(ctx, q, id), &a); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, outbound.ErrNotFound
		}
		return nil, err
	}
	if bodies {
		if err := tx.QueryRow(ctx, `SELECT body FROM xic_outbound_attempts WHERE attempt_id=$1::uuid`, id).Scan(&a.Body); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

func insertOutboundEvent(ctx context.Context, tx pgx.Tx, id, tenant string, ev outbound.Event) error {
	var detail any
	if len(ev.Detail) > 0 {
		detail = []byte(ev.Detail)
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_outbound_events (attempt_id, tenant_id, kind, from_state, to_state, detail, actor, at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`, id, tenant, ev.Kind, ev.From, ev.To, detail, ev.Actor, at)
	return err
}

// Reserve implements outbound.Repo.
func (r *OutboundRepo) Reserve(ctx context.Context, a *outbound.Attempt, ev outbound.Event, out []outbound.OutEvent) (*outbound.Attempt, error) {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var fin any
		if len(a.FinalOutcomeRef) > 0 {
			fin = []byte(a.FinalOutcomeRef)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_outbound_attempts (attempt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
			domain_request_ref, payload_profile, payload_hash, payload_size, provider_idem_ref_hash, resubmit_allowed, state, final_outcome, body, body_expires_at,
			correlation_id, requested_by, version, created_at, updated_at)
			VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6::uuid,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			a.AttemptID, a.TenantID, a.ConnectionID, a.ConnectorID, a.OperationID, a.ContractVersionID, a.DomainRequestRef, a.PayloadProfile, a.PayloadHash,
			a.PayloadSize, a.ProviderIdemRefHash, a.ResubmitAllowed, a.State, fin, nilBytes(a.Body), a.BodyExpiresAt, a.CorrelationID, a.RequestedBy, a.Version,
			a.CreatedAt, a.UpdatedAt); err != nil {
			return err
		}
		if err := insertOutboundEvent(ctx, tx, a.AttemptID, a.TenantID, ev); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, a.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil, nil
	}
	if errors.Is(outboundErr(err), outbound.ErrDuplicate) {
		var ex outbound.Attempt
		gerr := r.p.withTenant(ctx, func(tx pgx.Tx) error {
			return scanOutbound(tx.QueryRow(ctx, `SELECT `+outboundCols+` FROM xic_outbound_attempts
				WHERE tenant_id=$1 AND connection_id=$2::uuid AND operation_id=$3 AND domain_request_ref=$4`,
				a.TenantID, a.ConnectionID, a.OperationID, a.DomainRequestRef), &ex)
		})
		if gerr != nil {
			return nil, gerr
		}
		return &ex, outbound.ErrDuplicate
	}
	return nil, outboundErr(err)
}

// Get implements outbound.Repo. The returned attempt carries its stored body for the engine; the API never returns it.
func (r *OutboundRepo) Get(ctx context.Context, id string) (*outbound.Attempt, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, outbound.ErrNotFound
	}
	var a *outbound.Attempt
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = loadOutbound(ctx, tx, id, true, false)
		return err
	})
	return a, err
}

// List implements outbound.Repo.
func (r *OutboundRepo) List(ctx context.Context, f outbound.Filter) ([]outbound.Attempt, error) {
	if f.Limit < 1 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []outbound.Attempt{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var connID any
		if f.ConnectionID != "" {
			if _, err := uuid.Parse(f.ConnectionID); err != nil {
				return nil
			}
			connID = f.ConnectionID
		}
		rows, err := tx.Query(ctx, `SELECT `+outboundCols+` FROM xic_outbound_attempts
			WHERE ($1='' OR state=$1) AND ($2::uuid IS NULL OR connection_id=$2::uuid) AND ($3='' OR domain_request_ref=$3)
			ORDER BY created_at DESC, attempt_id LIMIT $4`, f.State, connID, f.DomainRequestRef, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a outbound.Attempt
			if err := scanOutbound(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// Events implements outbound.Repo.
func (r *OutboundRepo) Events(ctx context.Context, id string) ([]outbound.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, outbound.ErrNotFound
	}
	out := []outbound.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_outbound_attempts WHERE attempt_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return outbound.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, from_state, to_state, detail, actor, at FROM xic_outbound_events WHERE attempt_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev outbound.Event
			var d []byte
			if err := rows.Scan(&ev.Kind, &ev.From, &ev.To, &d, &ev.Actor, &ev.At); err != nil {
				return err
			}
			if len(d) > 0 {
				ev.Detail = json.RawMessage(d)
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// Mutate implements outbound.Repo.
func (r *OutboundRepo) Mutate(ctx context.Context, id string, fn func(tx *outbound.Tx) error) (*outbound.Attempt, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, outbound.ErrNotFound
	}
	var out *outbound.Attempt
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadOutbound(ctx, pg, id, true, true)
		if err != nil {
			return err
		}
		w := *cur
		tx := &outbound.Tx{A: &w}
		if err := fn(tx); err != nil {
			return err
		}
		var fin any
		if len(w.FinalOutcomeRef) > 0 {
			fin = []byte(w.FinalOutcomeRef)
		}
		dropBody := len(cur.Body) > 0 && len(w.Body) == 0
		if tx.NoUpdate {
			// the attempt is left exactly as it is (a final attempt cannot be updated at all); evidence is still written
		} else if _, err := pg.Exec(ctx, `UPDATE xic_outbound_attempts SET state=$2, ambiguity_reason=$3, provider_reference=$4, provider_status=$5, submit_count=$6,
			last_submit_at=$7, resolve_count=$8, next_resolution_at=$9, escalated_at=$10, resolution_method=$11, resolved_by=$12, evidence_ref=$13, final_outcome=$14,
			body = CASE WHEN $15 THEN NULL ELSE body END, version=version+1, updated_at=now()
			WHERE attempt_id=$1::uuid`, id, w.State, w.AmbiguityReason, w.ProviderReference, w.ProviderStatus, w.SubmitCount, w.LastSubmitAt, w.ResolveCount,
			w.NextResolutionAt, w.EscalatedAt, w.ResolutionMethod, w.ResolvedBy, w.EvidenceRef, fin, dropBody); err != nil {
			return err
		}
		if tx.Event != nil {
			if err := insertOutboundEvent(ctx, pg, id, cur.TenantID, *tx.Event); err != nil {
				return err
			}
		}
		if tx.Receipt != nil {
			if err := insertReceipt(ctx, pg, tx.Receipt); err != nil {
				return err
			}
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		out, err = loadOutbound(ctx, pg, id, true, false)
		return err
	})
	if err != nil {
		return nil, outboundErr(err)
	}
	return out, nil
}

func (r *OutboundRepo) workerTx(ctx context.Context, purge bool, fn func(pgx.Tx) error) error {
	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.xic_worker', 'true', true)`); err != nil {
		return err
	}
	if purge {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.xic_purge', 'true', true)`); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DueForResolution implements outbound.Repo.
func (r *OutboundRepo) DueForResolution(ctx context.Context, now, escalateBefore time.Time, limit int) ([]outbound.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []outbound.DueRef
	err := r.workerTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, attempt_id::text FROM xic_outbound_attempts
			WHERE state IN ('SUBMITTING', 'PENDING', 'UNKNOWN')
			  AND (next_resolution_at <= $1 OR (escalated_at IS NULL AND state IN ('PENDING', 'UNKNOWN') AND last_submit_at <= $2))
			ORDER BY COALESCE(next_resolution_at, last_submit_at), attempt_id LIMIT $3`, now, escalateBefore, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d outbound.DueRef
			if err := rows.Scan(&d.TenantID, &d.AttemptID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// PurgeBodies implements outbound.Repo.
func (r *OutboundRepo) PurgeBodies(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 5000 {
		limit = 500
	}
	n := 0
	err := r.workerTx(ctx, true, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE xic_outbound_attempts SET body=NULL WHERE attempt_id IN (
			SELECT attempt_id FROM xic_outbound_attempts WHERE body IS NOT NULL
			  AND (state IN ('SETTLED', 'FAILED', 'CANCELLED') OR body_expires_at <= LEAST($1::timestamptz, now()))
			ORDER BY body_expires_at LIMIT $2)`, now, limit)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// RecordSignal implements outbound.Repo.
func (r *OutboundRepo) RecordSignal(ctx context.Context, tenant string, ev outbound.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}
