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
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// InboundRepo is the Postgres implementation of inbound.Repo (migration 006).
type InboundRepo struct{ p *PgStore }

// NewInboundRepo wraps a PgStore.
func NewInboundRepo(p *PgStore) *InboundRepo { return &InboundRepo{p: p} }

var _ inbound.Repo = (*InboundRepo)(nil)

func (r *InboundRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e inbound.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

func inboundErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23505":
		switch {
		case strings.Contains(msg, "xic_ix_dedupe"):
			return inbound.ErrDedupe
		case strings.Contains(msg, "xic_target_one_live"):
			return inbound.ErrTargetExists
		case strings.Contains(msg, "xic_qr_one_open"):
			return inbound.ErrRequestState
		}
	case "23000":
		if strings.Contains(msg, "cannot go") || strings.Contains(msg, "final") {
			return inbound.ErrTransition
		}
	case "42501":
		return inbound.ErrNotPlatform
	}
	return err
}

const inboundCols = `exchange_id::text, tenant_id, connection_id::text, connector_id, operation_id, contract_version_id::text, state, domain_outcome,
	COALESCE(quarantine_class, ''), reason_code, xic_code, COALESCE(original_exchange_id::text, ''), COALESCE(dedupe_key, ''), source_ref_hash, body_hash,
	body_size, content_type, source_schema, COALESCE(mapping_version_id::text, ''), canonical_contract, canonical_hash, lineage, raw_expires_at,
	COALESCE(target_id::text, ''), handoff_attempts, next_attempt_at, last_error_code, domain_ref, domain_status, auth_mechanism, source_net_hash,
	correlation_id, attempt_no, version, received_at, updated_at, (raw IS NOT NULL)`

func scanInbound(row pgx.Row, e *inbound.Exchange) error {
	var lin []byte
	if err := row.Scan(&e.ExchangeID, &e.TenantID, &e.ConnectionID, &e.ConnectorID, &e.OperationID, &e.ContractVersionID, &e.State, &e.DomainOutcome,
		&e.QuarantineClass, &e.ReasonCode, &e.XICCode, &e.OriginalExchangeID, &e.DedupeKey, &e.SourceRefHash, &e.BodyHash, &e.BodySize, &e.ContentType,
		&e.SourceSchema, &e.MappingVersionID, &e.CanonicalContract, &e.CanonicalHash, &lin, &e.RawExpiresAt, &e.TargetID, &e.HandoffAttempts,
		&e.NextAttemptAt, &e.LastErrorCode, &e.DomainRef, &e.DomainStatus, &e.AuthMechanism, &e.SourceNetHash, &e.CorrelationID, &e.AttemptNo,
		&e.Version, &e.ReceivedAt, &e.UpdatedAt, &e.RawRetained); err != nil {
		return err
	}
	if len(lin) > 0 && string(lin) != "null" {
		e.Lineage = json.RawMessage(lin)
	}
	return nil
}

func loadInbound(ctx context.Context, tx pgx.Tx, id string, bodies, forUpdate bool) (*inbound.Exchange, error) {
	var e inbound.Exchange
	q := `SELECT ` + inboundCols + ` FROM xic_inbound_exchanges WHERE exchange_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanInbound(tx.QueryRow(ctx, q, id), &e); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, inbound.ErrNotFound
		}
		return nil, err
	}
	if bodies {
		if err := tx.QueryRow(ctx, `SELECT raw, canonical FROM xic_inbound_exchanges WHERE exchange_id=$1::uuid`, id).Scan(&e.Raw, &e.Canonical); err != nil {
			return nil, err
		}
	}
	return &e, nil
}

func insertInbound(ctx context.Context, tx pgx.Tx, e *inbound.Exchange) error {
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var lin any
	if len(e.Lineage) > 0 {
		lin = []byte(e.Lineage)
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		state, domain_outcome, quarantine_class, reason_code, xic_code, original_exchange_id, dedupe_key, source_ref_hash, body_hash, body_size, content_type,
		source_schema, mapping_version_id, canonical_contract, canonical_hash, lineage, raw, canonical, raw_expires_at, target_id, handoff_attempts,
		next_attempt_at, last_error_code, domain_ref, domain_status, auth_mechanism, source_net_hash, correlation_id, attempt_no, version, received_at, updated_at)
		VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6::uuid,$7,$8,$9,$10,$11,$12::uuid,$13,$14,$15,$16,$17,$18,$19::uuid,$20,$21,$22,$23,$24,$25,$26::uuid,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38)`,
		e.ExchangeID, e.TenantID, e.ConnectionID, e.ConnectorID, e.OperationID, e.ContractVersionID, e.State, e.DomainOutcome, nul(e.QuarantineClass),
		e.ReasonCode, e.XICCode, nul(e.OriginalExchangeID), nul(e.DedupeKey), e.SourceRefHash, e.BodyHash, e.BodySize, e.ContentType, e.SourceSchema,
		nul(e.MappingVersionID), e.CanonicalContract, e.CanonicalHash, lin, nilBytes(e.Raw), nilBytes(e.Canonical), e.RawExpiresAt, nul(e.TargetID),
		e.HandoffAttempts, e.NextAttemptAt, e.LastErrorCode, e.DomainRef, e.DomainStatus, e.AuthMechanism, e.SourceNetHash, e.CorrelationID,
		e.AttemptNo, e.Version, e.ReceivedAt, e.UpdatedAt)
	return err
}

func nilBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func insertInboundEvent(ctx context.Context, tx pgx.Tx, id, tenant string, ev inbound.Event) error {
	var detail any
	if len(ev.Detail) > 0 {
		detail = []byte(ev.Detail)
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_inbound_events (exchange_id, tenant_id, kind, from_state, to_state, detail, actor, at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`, id, tenant, ev.Kind, ev.From, ev.To, detail, ev.Actor, at)
	return err
}

func (r *InboundRepo) Insert(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) (*inbound.Exchange, error) {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := insertInbound(ctx, tx, e); err != nil {
			return err
		}
		if err := insertInboundEvent(ctx, tx, e.ExchangeID, e.TenantID, ev); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, e.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil, nil
	}
	if errors.Is(inboundErr(err), inbound.ErrDedupe) {
		holder, gerr := r.GetByDedupe(ctx, e.TenantID, e.DedupeKey)
		if gerr != nil {
			return nil, gerr
		}
		return holder, inbound.ErrDedupe
	}
	return nil, inboundErr(err)
}

func (r *InboundRepo) InsertDuplicate(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) error {
	_, err := r.Insert(ctx, e, ev, out)
	return err
}

func (r *InboundRepo) Get(ctx context.Context, id string) (*inbound.Exchange, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrNotFound
	}
	var e *inbound.Exchange
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		e, err = loadInbound(ctx, tx, id, true, false)
		return err
	})
	return e, err
}

func (r *InboundRepo) GetByDedupe(ctx context.Context, tenant, key string) (*inbound.Exchange, error) {
	var e inbound.Exchange
	err := r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return scanInbound(tx.QueryRow(ctx, `SELECT `+inboundCols+` FROM xic_inbound_exchanges WHERE tenant_id=$1 AND dedupe_key=$2`, tenant, key), &e)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, inbound.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *InboundRepo) List(ctx context.Context, f inbound.Filter) ([]inbound.Exchange, error) {
	if f.Limit < 1 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []inbound.Exchange{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var connID any
		if f.ConnectionID != "" {
			if _, err := uuid.Parse(f.ConnectionID); err != nil {
				return nil
			}
			connID = f.ConnectionID
		}
		rows, err := tx.Query(ctx, `SELECT `+inboundCols+` FROM xic_inbound_exchanges
			WHERE ($1='' OR state=$1) AND ($2::uuid IS NULL OR connection_id=$2::uuid) AND ($3='' OR quarantine_class=$3)
			ORDER BY received_at DESC, exchange_id LIMIT $4`, f.State, connID, f.Class, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e inbound.Exchange
			if err := scanInbound(rows, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (r *InboundRepo) Events(ctx context.Context, id string) ([]inbound.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrNotFound
	}
	out := []inbound.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_inbound_exchanges WHERE exchange_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return inbound.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, from_state, to_state, detail, actor, at FROM xic_inbound_events WHERE exchange_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev inbound.Event
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

func loadQRequests(ctx context.Context, tx pgx.Tx, id string) ([]inbound.QRequest, error) {
	rows, err := tx.Query(ctx, `SELECT request_id::text, exchange_id::text, action, reason, status, requested_by, COALESCE(decided_by, ''), decided_at, created_at
		FROM xic_quarantine_requests WHERE exchange_id=$1::uuid ORDER BY created_at, request_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []inbound.QRequest{}
	for rows.Next() {
		var q inbound.QRequest
		if err := rows.Scan(&q.RequestID, &q.ExchangeID, &q.Action, &q.Reason, &q.Status, &q.RequestedBy, &q.DecidedBy, &q.DecidedAt, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (r *InboundRepo) Requests(ctx context.Context, id string) ([]inbound.QRequest, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrNotFound
	}
	var out []inbound.QRequest
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_inbound_exchanges WHERE exchange_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return inbound.ErrNotFound
			}
			return err
		}
		var err error
		out, err = loadQRequests(ctx, tx, id)
		return err
	})
	return out, err
}

func (r *InboundRepo) Mutate(ctx context.Context, id string, fn func(tx *inbound.Tx) error) (*inbound.Exchange, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrNotFound
	}
	var out *inbound.Exchange
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadInbound(ctx, pg, id, true, true)
		if err != nil {
			return err
		}
		reqs, err := loadQRequests(ctx, pg, id)
		if err != nil {
			return err
		}
		w := *cur
		tx := &inbound.Tx{E: &w, Requests: reqs}
		if err := fn(tx); err != nil {
			return err
		}
		nul := func(s string) any {
			if s == "" {
				return nil
			}
			return s
		}
		var lin any
		if len(w.Lineage) > 0 {
			lin = []byte(w.Lineage)
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_inbound_exchanges SET state=$2, domain_outcome=$3, quarantine_class=$4, reason_code=$5, xic_code=$6, source_schema=$7,
			mapping_version_id=$8::uuid, canonical_contract=$9, canonical_hash=$10, lineage=$11, canonical=$12, raw_expires_at=$13, target_id=$14::uuid,
			handoff_attempts=$15, next_attempt_at=$16, last_error_code=$17, domain_ref=$18, domain_status=$19, attempt_no=$20, version=version+1, updated_at=now()
			WHERE exchange_id=$1::uuid`, id, w.State, w.DomainOutcome, nul(w.QuarantineClass), w.ReasonCode, w.XICCode, w.SourceSchema, nul(w.MappingVersionID),
			w.CanonicalContract, w.CanonicalHash, lin, nilBytes(w.Canonical), w.RawExpiresAt, nul(w.TargetID), w.HandoffAttempts, w.NextAttemptAt,
			w.LastErrorCode, w.DomainRef, w.DomainStatus, w.AttemptNo); err != nil {
			return err
		}
		if tx.NewEvent != nil {
			if err := insertInboundEvent(ctx, pg, id, cur.TenantID, *tx.NewEvent); err != nil {
				return err
			}
		}
		if q := tx.NewRequest; q != nil {
			if _, err := pg.Exec(ctx, `INSERT INTO xic_quarantine_requests (request_id, exchange_id, tenant_id, action, reason, status, requested_by, created_at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,'PENDING',$6,$7)`, q.RequestID, id, cur.TenantID, q.Action, q.Reason, q.RequestedBy, q.CreatedAt); err != nil {
				return err
			}
		}
		if q := tx.UpdateRequest; q != nil {
			if _, err := pg.Exec(ctx, `UPDATE xic_quarantine_requests SET status=$2, decided_by=$3, decided_at=$4 WHERE request_id=$1::uuid`,
				q.RequestID, q.Status, nul(q.DecidedBy), q.DecidedAt); err != nil {
				return err
			}
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		out, err = loadInbound(ctx, pg, id, true, false)
		return err
	})
	if err != nil {
		return nil, inboundErr(err)
	}
	return out, nil
}

func (r *InboundRepo) workerTx(ctx context.Context, purge bool, fn func(pgx.Tx) error) error {
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

func (r *InboundRepo) Due(ctx context.Context, now time.Time, limit int) ([]inbound.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []inbound.DueRef
	err := r.workerTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, exchange_id::text FROM xic_inbound_exchanges WHERE state='VALIDATED' AND next_attempt_at <= $1
			ORDER BY next_attempt_at, exchange_id LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d inbound.DueRef
			if err := rows.Scan(&d.TenantID, &d.ExchangeID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (r *InboundRepo) PurgeExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 5000 {
		limit = 500
	}
	n := 0
	err := r.workerTx(ctx, true, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE xic_inbound_exchanges SET raw=NULL, canonical=NULL WHERE exchange_id IN (
			SELECT exchange_id FROM xic_inbound_exchanges WHERE raw_expires_at <= LEAST($1::timestamptz, now()) AND (raw IS NOT NULL OR canonical IS NOT NULL)
			ORDER BY raw_expires_at LIMIT $2)`, now, limit)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// ── targets ──────────────────────────────────────────────────────────────────

const targetCols = `target_id::text, canonical_contract, base_url, path, region, legal_entity_pointer, status, fingerprint, created_by, approved_by, approved_at,
	version, created_at, updated_at`

func scanTarget(row pgx.Row, t *inbound.Target) error {
	return row.Scan(&t.TargetID, &t.CanonicalContract, &t.BaseURL, &t.Path, &t.Region, &t.LegalEntityPointer, &t.Status, &t.Fingerprint, &t.CreatedBy,
		&t.ApprovedBy, &t.ApprovedAt, &t.Version, &t.CreatedAt, &t.UpdatedAt)
}

func (r *InboundRepo) CreateTarget(ctx context.Context, t *inbound.Target, ev inbound.OutEvent) error {
	if middleware.GetTenantID(ctx) != "00000000-0000-0000-0000-00000000f001" {
		return inbound.ErrNotPlatform
	}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, legal_entity_pointer, status, fingerprint, created_by, version, created_at, updated_at)
			VALUES ($1::uuid,$2,$3,$4,$5,$6,'DRAFT',$7,$8,1,$9,$10)`, t.TargetID, t.CanonicalContract, t.BaseURL, t.Path, t.Region, t.LegalEntityPointer,
			t.Fingerprint, t.CreatedBy, t.CreatedAt, t.UpdatedAt); err != nil {
			return err
		}
		return r.enqueue(ctx, tx, middleware.GetTenantID(ctx), ev)
	})
	return inboundErr(err)
}

func (r *InboundRepo) GetTarget(ctx context.Context, id string) (*inbound.Target, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrTargetNotFound
	}
	var t inbound.Target
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanTarget(tx.QueryRow(ctx, `SELECT `+targetCols+` FROM xic_handoff_targets WHERE target_id=$1::uuid`, id), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, inbound.ErrTargetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *InboundRepo) ActiveTarget(ctx context.Context, canonicalContract string) (*inbound.Target, error) {
	var t inbound.Target
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanTarget(tx.QueryRow(ctx, `SELECT `+targetCols+` FROM xic_handoff_targets WHERE canonical_contract=$1 AND status='ACTIVE'`, canonicalContract), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *InboundRepo) ListTargets(ctx context.Context) ([]inbound.Target, error) {
	out := []inbound.Target{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+targetCols+` FROM xic_handoff_targets ORDER BY canonical_contract, created_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t inbound.Target
			if err := scanTarget(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (r *InboundRepo) MutateTarget(ctx context.Context, id string, fn func(t *inbound.Target) (*inbound.OutEvent, error)) (*inbound.Target, error) {
	if middleware.GetTenantID(ctx) != "00000000-0000-0000-0000-00000000f001" {
		return nil, inbound.ErrNotPlatform
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, inbound.ErrTargetNotFound
	}
	var t inbound.Target
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := scanTarget(tx.QueryRow(ctx, `SELECT `+targetCols+` FROM xic_handoff_targets WHERE target_id=$1::uuid FOR UPDATE`, id), &t); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return inbound.ErrTargetNotFound
			}
			return err
		}
		ev, err := fn(&t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE xic_handoff_targets SET status=$2, approved_by=$3, approved_at=$4, version=version+1, updated_at=now() WHERE target_id=$1::uuid`,
			id, t.Status, t.ApprovedBy, t.ApprovedAt); err != nil {
			return err
		}
		if ev != nil {
			if err := r.enqueue(ctx, tx, middleware.GetTenantID(ctx), *ev); err != nil {
				return err
			}
		}
		return scanTarget(tx.QueryRow(ctx, `SELECT `+targetCols+` FROM xic_handoff_targets WHERE target_id=$1::uuid`, id), &t)
	})
	if err != nil {
		return nil, inboundErr(err)
	}
	return &t, nil
}

func (r *InboundRepo) RecordSignal(ctx context.Context, tenant string, ev inbound.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}
