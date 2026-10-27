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
			(session_id, grant_hash, created_at, expires_at, locale, market_code, page_context)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			s.SessionID, s.GrantHash, s.CreatedAt, s.ExpiresAt, s.Locale, s.MarketCode, s.PageContext); err != nil {
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

const sessionCols = `s.session_id, s.grant_hash, s.created_at, s.expires_at, s.locale, s.market_code, s.page_context, s.deleted_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.SessionID, &s.GrantHash, &s.CreatedAt, &s.ExpiresAt, &s.Locale, &s.MarketCode, &s.PageContext, &s.DeletedAt)
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
		Scan(&s.SessionID, &s.GrantHash, &s.CreatedAt, &s.ExpiresAt, &s.Locale, &s.MarketCode, &s.PageContext, &s.DeletedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, nil, ErrNotFound
	}
	return s, hash, err
}

func (p *Postgres) SessionByGrant(ctx context.Context, grantHash []byte) (Session, error) {
	return scanSession(p.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM public_ai.sessions s
		WHERE s.grant_hash = $1 AND s.deleted_at IS NULL`, grantHash))
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
		// The grant hash is replaced so the old grant can never authenticate again.
		tag, err := tx.Exec(ctx, `UPDATE public_ai.sessions
			SET deleted_at = COALESCE(deleted_at, $2), grant_hash = sha256(session_id::bytea || 'deleted'::bytea),
			    active_turn_id = NULL, locale = '', market_code = '', page_context = ''
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
		return insertOutbox(ctx, tx, events)
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
	_, err := p.pool.Exec(ctx, `INSERT INTO public_ai.conflicts (conflict_id, subject_key, claim_ids, detection_type, severity, resolution_state, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (conflict_id) DO NOTHING`,
		c.ConflictID, c.SubjectKey, c.ClaimIDs, c.DetectionType, c.Severity, state, c.DetectedAt)
	return err
}

func (p *Postgres) SetFlag(ctx context.Context, f Flag, events []OutboxEvent) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public_ai.flags (name, value, actor_ref, approval_ref, updated_at) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, actor_ref = EXCLUDED.actor_ref,
			approval_ref = EXCLUDED.approval_ref, updated_at = EXCLUDED.updated_at`,
			f.Name, f.Value, f.ActorRef, f.ApprovalRef, f.UpdatedAt); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, events)
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
