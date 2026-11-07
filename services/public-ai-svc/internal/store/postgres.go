package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres implements Store on the dedicated public_ai schema.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres wraps an open pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Migrate applies every *.up.sql file in fsys, in name order, that has not
// been applied yet. It records progress in public_ai_schema_migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public_ai_schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public_ai_schema_migrations WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai_schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// MigrateDown applies every *.down.sql file in reverse order and clears the
// migration record. Used by tests and local resets only.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	names, err := fs.Glob(fsys, "*.down.sql")
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		up := strings.TrimSuffix(name, ".down.sql") + ".up.sql"
		if _, err := pool.Exec(ctx, `DELETE FROM public_ai_schema_migrations WHERE name = $1`, up); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) CreateSession(ctx context.Context, s Session, key string, hash []byte) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.sessions
			(session_id, grant_hash, created_at, expires_at, locale, market_code, page_context, last_seen_at, origin, release_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$3,$8,$9)`,
			s.SessionID, s.GrantHash, s.CreatedAt, s.ExpiresAt, s.Locale, s.MarketCode, s.PageContext, s.Origin, s.ReleaseID); err != nil {
			return err
		}
		if key == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO public_ai.session_idempotency
			(idempotency_key, request_hash, session_id, created_at) VALUES ($1,$2,$3,$4)`,
			key, hash, s.SessionID, s.CreatedAt)
		return err
	})
}

const sessionCols = `s.session_id, s.grant_hash, s.created_at, s.expires_at, s.locale, s.market_code, s.page_context, s.deleted_at, s.pending_context,
	COALESCE(s.last_seen_at, s.created_at), s.tokens_used, s.origin, s.release_id`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.SessionID, &s.GrantHash, &s.CreatedAt, &s.ExpiresAt, &s.Locale, &s.MarketCode, &s.PageContext, &s.DeletedAt, &s.PendingContext,
		&s.LastSeenAt, &s.TokensUsed, &s.Origin, &s.ReleaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func (p *Postgres) SessionByIdempotencyKey(ctx context.Context, key string) (Session, []byte, error) {
	var hash []byte
	var s Session
	err := p.pool.QueryRow(ctx, `SELECT `+sessionCols+`, i.request_hash
		FROM public_ai.session_idempotency i JOIN public_ai.sessions s USING (session_id)
		WHERE i.idempotency_key = $1`, key).
		Scan(&s.SessionID, &s.GrantHash, &s.CreatedAt, &s.ExpiresAt, &s.Locale, &s.MarketCode, &s.PageContext, &s.DeletedAt, &s.PendingContext,
			&s.LastSeenAt, &s.TokensUsed, &s.Origin, &s.ReleaseID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, nil, ErrNotFound
	}
	return s, hash, err
}

func (p *Postgres) SessionByGrant(ctx context.Context, grantHash []byte) (Session, error) {
	return scanSession(p.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM public_ai.sessions s
		WHERE s.grant_hash = $1`, grantHash))
}

func (p *Postgres) RotateGrant(ctx context.Context, sessionID string, newHash []byte) error {
	tag, err := p.pool.Exec(ctx, `UPDATE public_ai.sessions SET grant_hash = $2 WHERE session_id = $1 AND deleted_at IS NULL`, sessionID, newHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteSession(ctx context.Context, sessionID string, at time.Time) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The grant hash is kept so a repeat DELETE by the same visitor is a
		// 204; deleted_at makes the grant useless for anything else.
		tag, err := tx.Exec(ctx, `UPDATE public_ai.sessions
			SET deleted_at = COALESCE(deleted_at, $2),
			    active_turn_id = NULL, locale = '', market_code = '', page_context = '', pending_context = NULL
			WHERE session_id = $1`, sessionID, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `DELETE FROM public_ai.turns WHERE session_id = $1`, sessionID)
		return err
	})
}

func (p *Postgres) SetSessionMarket(ctx context.Context, sessionID, market string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE public_ai.sessions SET market_code = $2 WHERE session_id = $1 AND deleted_at IS NULL`, sessionID, market)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetPendingContext(ctx context.Context, sessionID string, data []byte) error {
	tag, err := p.pool.Exec(ctx, `UPDATE public_ai.sessions SET pending_context = $2 WHERE session_id = $1 AND deleted_at IS NULL`, sessionID, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) BeginTurn(ctx context.Context, t Turn, lockUntil time.Time) (BeginResult, error) {
	var res BeginResult
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var seq int
		var active *string
		var until *time.Time
		var deleted *time.Time
		err := tx.QueryRow(ctx, `SELECT turn_seq, active_turn_id, active_until, deleted_at
			FROM public_ai.sessions WHERE session_id = $1 FOR UPDATE`, t.SessionID).Scan(&seq, &active, &until, &deleted)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && deleted != nil) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var prev Turn
		err = tx.QueryRow(ctx, `SELECT turn_id, session_id, client_turn_id, request_hash, turn_sequence, status, intent_id, response, completed, created_at
			FROM public_ai.turns WHERE session_id = $1 AND client_turn_id = $2`, t.SessionID, t.ClientTurnID).
			Scan(&prev.TurnID, &prev.SessionID, &prev.ClientTurnID, &prev.RequestHash, &prev.Sequence, &prev.Status, &prev.IntentID, &prev.Response, &prev.Completed, &prev.CreatedAt)
		switch {
		case err == nil:
			if !bytes.Equal(prev.RequestHash, t.RequestHash) {
				return ErrIdempotencyMismatch
			}
			if prev.Completed {
				res = BeginResult{Turn: prev, Replay: true}
				return nil
			}
			return ErrTurnConflict
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if active != nil && until != nil && time.Now().Before(*until) {
			return ErrTurnConflict
		}
		t.Sequence = seq + 1
		if _, err := tx.Exec(ctx, `UPDATE public_ai.sessions SET turn_seq = $2, active_turn_id = $3, active_until = $4 WHERE session_id = $1`,
			t.SessionID, t.Sequence, t.TurnID, lockUntil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.turns (turn_id, session_id, client_turn_id, request_hash, turn_sequence, created_at)
			VALUES ($1,$2,$3,$4,$5,$6)`, t.TurnID, t.SessionID, t.ClientTurnID, t.RequestHash, t.Sequence, t.CreatedAt); err != nil {
			return err
		}
		res = BeginResult{Turn: t}
		return nil
	})
	return res, err
}

func insertOutbox(ctx context.Context, tx pgx.Tx, events []OutboxEvent) error {
	for _, e := range events {
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.outbox (event_id, topic, event_type, payload, created_at)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT (event_id) DO NOTHING`, e.EventID, e.Topic, e.EventType, e.Payload, e.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) CompleteTurn(ctx context.Context, t Turn, buckets []string, tokens int64, events []OutboxEvent) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// SEC-T057: lock the session row and refuse a turn whose session
		// expired or was deleted while it ran.
		var live bool
		err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL AND expires_at > now() FROM public_ai.sessions WHERE session_id = $1 FOR UPDATE`, t.SessionID).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
			return ErrSessionGone
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE public_ai.turns SET status = $2, intent_id = $3, response = $4, completed = TRUE
			WHERE turn_id = $1 AND completed = FALSE`, t.TurnID, t.Status, t.IntentID, t.Response)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE public_ai.sessions SET active_turn_id = NULL WHERE session_id = $1 AND active_turn_id = $2`, t.SessionID, t.TurnID); err != nil {
			return err
		}
		// A completed turn keeps the session alive and counts its tokens.
		if _, err := tx.Exec(ctx, `UPDATE public_ai.sessions SET last_seen_at = now(), tokens_used = tokens_used + $2 WHERE session_id = $1`, t.SessionID, tokens); err != nil {
			return err
		}
		for _, b := range buckets {
			if _, err := tx.Exec(ctx, `INSERT INTO public_ai.usage_counters (bucket, tokens) VALUES ($1,$2)
				ON CONFLICT (bucket) DO UPDATE SET tokens = usage_counters.tokens + EXCLUDED.tokens`, b, tokens); err != nil {
				return err
			}
		}
		return insertOutbox(ctx, tx, events)
	})
}

func (p *Postgres) AbortTurn(ctx context.Context, sessionID, turnID string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM public_ai.turns WHERE turn_id = $1 AND session_id = $2 AND completed = FALSE`, turnID, sessionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE public_ai.sessions SET active_turn_id = NULL WHERE session_id = $1 AND active_turn_id = $2`, sessionID, turnID)
		return err
	})
}

func (p *Postgres) SetFeedback(ctx context.Context, sessionID, turnID, rating, reason string, at time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE public_ai.turns SET feedback_rating = $3, feedback_reason = NULLIF($4, ''), feedback_at = $5
		WHERE session_id = $1 AND turn_id = $2 AND completed = TRUE`, sessionID, turnID, rating, reason, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) PurgeSessions(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public_ai.sessions WHERE expires_at < $1 OR (deleted_at IS NOT NULL AND deleted_at < $1)`, cutoff)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		// Doc 06 §17: delivered (or dead-lettered, which the relay also marks
		// published) events, and usage counters older than the previous month.
		if _, err := tx.Exec(ctx, `DELETE FROM public_ai.outbox WHERE published_at IS NOT NULL AND published_at < $1`, cutoff); err != nil {
			return err
		}
		day, month := UsageKeepFrom(time.Now().UTC())
		_, err = tx.Exec(ctx, `DELETE FROM public_ai.usage_counters
			WHERE (bucket LIKE 'tokens:day:%' AND bucket < $1) OR (bucket LIKE 'tokens:month:%' AND bucket < $2)`, day, month)
		return err
	})
	return n, err
}

func (p *Postgres) AppendEvents(ctx context.Context, events []OutboxEvent) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return insertOutbox(ctx, tx, events) })
}

func (p *Postgres) PutRelease(ctx context.Context, r Release) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO public_ai.releases (release_id, kind, manifest, signature, created_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (release_id) DO NOTHING`, r.ReleaseID, r.Kind, r.Manifest, r.Signature, r.CreatedAt)
	return err
}

func (p *Postgres) GetRelease(ctx context.Context, id string) (Release, error) {
	var r Release
	err := p.pool.QueryRow(ctx, `SELECT release_id, kind, manifest, signature, created_at FROM public_ai.releases WHERE release_id = $1`, id).
		Scan(&r.ReleaseID, &r.Kind, &r.Manifest, &r.Signature, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	return r, err
}

func (p *Postgres) SetPointer(ctx context.Context, kind, id string, events []OutboxEvent) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var k string
		err := tx.QueryRow(ctx, `SELECT kind FROM public_ai.releases WHERE release_id = $1`, id).Scan(&k)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && k != kind) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.release_pointer (kind, release_id, updated_at) VALUES ($1,$2,now())
			ON CONFLICT (kind) DO UPDATE SET release_id = EXCLUDED.release_id, updated_at = EXCLUDED.updated_at`, kind, id); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, events)
	})
}

func (p *Postgres) GetPointer(ctx context.Context, kind string) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `SELECT release_id FROM public_ai.release_pointer WHERE kind = $1`, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func (p *Postgres) AddRevocation(ctx context.Context, r Revocation, events []OutboxEvent) (int64, error) {
	var wm int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO public_ai.revocations (target_kind, target_id, reason_code, effective_at, created_at)
			VALUES ($1,$2,$3,$4,$5) RETURNING watermark`, r.TargetKind, r.TargetID, r.ReasonCode, r.EffectiveAt, r.CreatedAt).Scan(&wm); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, stampRevocationEpoch(events, wm))
	})
	return wm, err
}

func (p *Postgres) Revocations(ctx context.Context, after int64) ([]Revocation, error) {
	rows, err := p.pool.Query(ctx, `SELECT watermark, target_kind, target_id, reason_code, effective_at, created_at
		FROM public_ai.revocations WHERE watermark > $1 ORDER BY watermark`, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Revocation, error) {
		var r Revocation
		err := row.Scan(&r.Watermark, &r.TargetKind, &r.TargetID, &r.ReasonCode, &r.EffectiveAt, &r.CreatedAt)
		return r, err
	})
}

func (p *Postgres) RecordConflict(ctx context.Context, c Conflict) error {
	state := c.ResolutionState
	if state == "" {
		state = "OPEN"
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO public_ai.conflicts (conflict_id, subject_key, claim_ids, detection_type, severity, resolution_state, detected_at, blocked_claim, assigned_owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (conflict_id) DO NOTHING`,
		c.ConflictID, c.SubjectKey, c.ClaimIDs, c.DetectionType, c.Severity, state, c.DetectedAt, c.BlockedClaim, c.AssignedOwner)
	return err
}

func (p *Postgres) ResolveConflict(ctx context.Context, id, state, evidenceRef string, audit AuditEntry) error {
	if (state != ConflictResolved && state != ConflictDismissed) || strings.TrimSpace(evidenceRef) == "" {
		return ErrConflictState
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public_ai.conflicts SET resolution_state = $2, resolution_evidence_ref = $3
			WHERE conflict_id = $1 AND resolution_state = 'OPEN'`, id, state, evidenceRef)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM public_ai.conflicts WHERE conflict_id = $1`, id).Scan(&n); err == nil && n == 0 {
				return ErrNotFound
			}
			return ErrConflictState
		}
		audit.Action, audit.Target = "conflict."+strings.ToLower(state), id
		return appendAudit(ctx, tx, audit)
	})
}

func (p *Postgres) Conflicts(ctx context.Context) ([]Conflict, error) {
	rows, err := p.pool.Query(ctx, `SELECT conflict_id, subject_key, claim_ids, detection_type, severity, resolution_state, detected_at, blocked_claim, assigned_owner,
		COALESCE(resolution_evidence_ref, '')
		FROM public_ai.conflicts ORDER BY detected_at, conflict_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Conflict, error) {
		var c Conflict
		err := row.Scan(&c.ConflictID, &c.SubjectKey, &c.ClaimIDs, &c.DetectionType, &c.Severity, &c.ResolutionState, &c.DetectedAt, &c.BlockedClaim, &c.AssignedOwner,
			&c.ResolutionEvidenceRef)
		return c, err
	})
}

func (p *Postgres) SetFlag(ctx context.Context, f Flag, events []OutboxEvent) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// The change and its audit entry commit together (Doc 09 §22).
		var before []byte
		if err := tx.QueryRow(ctx, `SELECT value FROM public_ai.flags WHERE name = $1`, f.Name).Scan(&before); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := appendAudit(ctx, tx, AuditEntry{Action: "flag.set", Target: f.Name, Actor: f.ActorRef, Approval: f.ApprovalRef,
			BeforeHash: Hash(before), AfterHash: Hash(f.Value), Environment: f.Environment, At: f.UpdatedAt}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.flags (name, value, actor_ref, approval_ref, updated_at) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, actor_ref = EXCLUDED.actor_ref,
			approval_ref = EXCLUDED.approval_ref, updated_at = EXCLUDED.updated_at`,
			f.Name, f.Value, f.ActorRef, f.ApprovalRef, f.UpdatedAt); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, events)
	})
}

func (p *Postgres) AppendAudit(ctx context.Context, e AuditEntry) error {
	// In a transaction, so the chain's advisory lock holds until commit.
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return appendAudit(ctx, tx, e) })
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// auditChainLock serialises audit appends so each row chains to the last.
const auditChainLock = 7_001_021

func appendAudit(ctx context.Context, db execer, e AuditEntry) error {
	if e.Actor == "" || e.Approval == "" {
		return ErrAuditIncomplete
	}
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, auditChainLock); err != nil {
		return err
	}
	err := db.QueryRow(ctx, `SELECT row_hash FROM public_ai.control_audit ORDER BY audit_id DESC LIMIT 1`).Scan(&e.PrevHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	e.At = e.At.UTC().Truncate(time.Microsecond) // what Postgres stores, so the hash verifies on read
	e.RowHash = AuditRowHash(e.PrevHash, e)
	_, err = db.Exec(ctx, `INSERT INTO public_ai.control_audit (action, target, actor, approval, reason, before_hash, after_hash, environment, at,
		request_id, actor_role, prev_hash, row_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, e.Action, e.Target, e.Actor, e.Approval, e.Reason, e.BeforeHash, e.AfterHash, e.Environment, e.At,
		e.RequestID, e.ActorRole, e.PrevHash, e.RowHash)
	return err
}

func (p *Postgres) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT audit_id, action, target, actor, approval, reason, before_hash, after_hash, environment, at,
		request_id, actor_role, prev_hash, row_hash
		FROM public_ai.control_audit ORDER BY audit_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		err := row.Scan(&e.AuditID, &e.Action, &e.Target, &e.Actor, &e.Approval, &e.Reason, &e.BeforeHash, &e.AfterHash, &e.Environment, &e.At,
			&e.RequestID, &e.ActorRole, &e.PrevHash, &e.RowHash)
		e.At = e.At.UTC()
		return e, err
	})
}

func (p *Postgres) Flags(ctx context.Context) ([]Flag, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, value, actor_ref, approval_ref, updated_at FROM public_ai.flags ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Flag, error) {
		var f Flag
		err := row.Scan(&f.Name, &f.Value, &f.ActorRef, &f.ApprovalRef, &f.UpdatedAt)
		return f, err
	})
}

func (p *Postgres) Usage(ctx context.Context, bucket string) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT tokens FROM public_ai.usage_counters WHERE bucket = $1`, bucket).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (p *Postgres) UnpublishedEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	rows, err := p.pool.Query(ctx, `SELECT outbox_id, event_id, topic, event_type, payload, created_at, attempts
		FROM public_ai.outbox WHERE published_at IS NULL ORDER BY outbox_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OutboxEvent, error) {
		var e OutboxEvent
		err := row.Scan(&e.OutboxID, &e.EventID, &e.Topic, &e.EventType, &e.Payload, &e.CreatedAt, &e.Attempts)
		return e, err
	})
}

func (p *Postgres) MarkPublished(ctx context.Context, ids []int64, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE public_ai.outbox SET published_at = $2 WHERE outbox_id = ANY($1)`, ids, at)
	return err
}

func (p *Postgres) MarkAttempt(ctx context.Context, ids []int64) error {
	_, err := p.pool.Exec(ctx, `UPDATE public_ai.outbox SET attempts = attempts + 1 WHERE outbox_id = ANY($1)`, ids)
	return err
}
