package kit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/accounts-receivable-svc/internal/outbox"
)

// DB is the transactional plumbing shared by every AR module. Every write goes
// through WithTx, which installs app.tenant_id so the FORCE'd RLS policies bind;
// every module query ALSO filters by tenant_id explicitly, because the service
// may connect as a role that bypasses RLS (see store/pg_store.go).
type DB struct {
	Pool *pgxpool.Pool
}

// NewDB wraps a pool.
func NewDB(pool *pgxpool.Pool) *DB { return &DB{Pool: pool} }

// WithTx runs fn in one transaction scoped to tenantID.
func (d *DB) WithTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return d.WithTxOpts(ctx, tenantID, pgx.TxOptions{}, fn)
}

// WithTxOpts is WithTx with explicit options (e.g. a REPEATABLE READ snapshot).
func (d *DB) WithTxOpts(ctx context.Context, tenantID string, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := d.Pool.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MapPgErr turns the driver errors that describe a caller's mistake into
// Problems; anything else is returned untouched and stays a loud 503.
func MapPgErr(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "22P02":
		return Invalid("an identifier is not a valid UUID")
	case "23514":
		return Unprocessable(CodeValidationFailed, "violates a data constraint: "+pgErr.ConstraintName)
	case "P0001":
		// RAISE EXCEPTION from an immutability / invariant trigger.
		return Err(http.StatusConflict, CodeImmutable, pgErr.Message)
	}
	return err
}

// IsUniqueViolation reports a 23505, optionally on a named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// ── outbox + audit ───────────────────────────────────────────────────────────

// Event is a domain event written to the outbox in the command's transaction.
type Event struct {
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       any
}

// Emit writes events to outbox_events in tx using the platform Variant-B
// envelope. Call it in the SAME transaction as the state change.
func Emit(ctx context.Context, tx pgx.Tx, c Caller, events ...Event) error {
	for _, e := range events {
		id := uuid.NewString()
		env, err := outbox.NewVariantBEnvelope(id, e.EventType, c.CorrelationID, c.TenantID, c.LegalEntityID, c.PrincipalID, e.Payload)
		if err != nil {
			return err
		}
		actor := c.PrincipalID
		if err := outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: id,
			AggregateType: e.AggregateType,
			AggregateID:   e.AggregateID,
			EventType:     e.EventType,
			TenantID:      c.TenantID,
			LegalEntityID: c.LegalEntityID,
			ActorID:       &actor,
			CorrelationID: c.CorrelationID,
			Headers:       map[string]string{"X-Event-ID": id},
			Payload:       env,
		}); err != nil {
			return fmt.Errorf("insert outbox event %s: %w", e.EventType, err)
		}
	}
	return nil
}

// Audit is one append-only evidence record (spec: persist prior/new values,
// actor/approver, reason, evidence and policy references for every command).
type Audit struct {
	AggregateType string
	AggregateID   string
	Command       string
	ReasonCode    string
	EvidenceID    string
	Before        any
	After         any
	Detail        map[string]any
}

// RecordAudit appends to ar_audit_events in tx. The table rejects UPDATE and
// DELETE, so the trail cannot be rewritten after the fact.
func RecordAudit(ctx context.Context, tx pgx.Tx, c Caller, a Audit) error {
	before, err := marshalNullable(a.Before)
	if err != nil {
		return err
	}
	after, err := marshalNullable(a.After)
	if err != nil {
		return err
	}
	detail, err := marshalNullable(a.Detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ar_audit_events (
			audit_id, tenant_id, legal_entity_id, aggregate_type, aggregate_id, command,
			actor_id, correlation_id, causation_id, reason_code, evidence_id, before_state, after_state, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13,$14)`,
		uuid.NewString(), c.TenantID, c.LegalEntityID, a.AggregateType, a.AggregateID, a.Command,
		c.PrincipalID, c.CorrelationID, c.CausationID, a.ReasonCode, a.EvidenceID, before, after, detail)
	return err
}

func marshalNullable(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// ── command idempotency (spec §14: mandatory for issue, correction, cash
// application, refund/write-off and external submission commands) ─────────────

// IdemStaleAfter is how long an unfinished claim blocks retries.
const IdemStaleAfter = 60 * time.Second

type idemRecord struct {
	operation   string
	requestHash string
	statusCode  *int
	response    []byte
	createdAt   time.Time
}

func (d *DB) idemClaim(ctx context.Context, tenantID, key, operation, hash string) (rec *idemRecord, claimed bool, err error) {
	err = d.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ar_command_idempotency (tenant_id, idempotency_key, operation, request_hash)
			VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`, tenantID, key, operation, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			claimed = true
			return nil
		}
		var r idemRecord
		var code *int32
		if err := tx.QueryRow(ctx, `
			SELECT operation, request_hash, status_code, response, created_at FROM ar_command_idempotency
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key).Scan(&r.operation, &r.requestHash, &code, &r.response, &r.createdAt); err != nil {
			return err
		}
		if code != nil {
			c := int(*code)
			r.statusCode = &c
		}
		if r.statusCode == nil && r.requestHash == hash && time.Since(r.createdAt) > IdemStaleAfter {
			t, err := tx.Exec(ctx, `UPDATE ar_command_idempotency SET created_at = now()
				WHERE tenant_id = $1 AND idempotency_key = $2 AND status_code IS NULL AND created_at = $3`, tenantID, key, r.createdAt)
			if err != nil {
				return err
			}
			if t.RowsAffected() == 1 {
				claimed = true
				return nil
			}
		}
		rec = &r
		return nil
	})
	return rec, claimed, err
}

func (d *DB) idemComplete(ctx context.Context, tenantID, key string, status int, response []byte) error {
	return d.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ar_command_idempotency SET status_code = $3, response = $4, completed_at = now()
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key, status, response)
		return err
	})
}

func (d *DB) idemRelease(ctx context.Context, tenantID, key string) error {
	return d.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM ar_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2 AND status_code IS NULL`, tenantID, key)
		return err
	})
}

type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	header http.Header
}

func (c *capture) Header() http.Header { return c.header }
func (c *capture) WriteHeader(s int)   { c.status = s }
func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}

// IdemWrap guards a command handler. A repeat with the same key and request
// replays the stored status/body with `Idempotent-Replay: true`; the same key
// with a different request is 422 IDEMPOTENCY_KEY_REUSED; a repeat while the
// first call is still running is 409. Only 2xx outcomes are stored — a failed
// command releases its claim so the caller can retry it.
func (d *DB) IdemWrap(operation string, required bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			if required {
				WriteError(w, http.StatusBadRequest, CodeIdempotencyRequired, "Idempotency-Key header is required")
				return
			}
			next(w, r)
			return
		}
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID == "" {
			next(w, r) // the handler refuses a request with no tenant scope
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			WriteError(w, http.StatusRequestEntityTooLarge, CodeValidationFailed, "request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])

		rec, claimed, err := d.idemClaim(r.Context(), tenantID, key, operation, hash)
		if err != nil {
			WriteError(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "")
			return
		}
		if !claimed {
			switch {
			case rec.operation != operation || rec.requestHash != hash:
				WriteError(w, http.StatusUnprocessableEntity, CodeIdempotencyReused, "this Idempotency-Key was used for a different request")
			case rec.statusCode != nil:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(*rec.statusCode)
				_, _ = w.Write(rec.response)
			default:
				WriteError(w, http.StatusConflict, CodeIdempotencyReused, "a request with this Idempotency-Key is still in progress")
			}
			return
		}

		c := &capture{ResponseWriter: w, header: http.Header{}}
		next(c, r)
		status := c.status
		if status == 0 {
			status = http.StatusOK
		}
		out := c.buf.Bytes()
		if status >= 200 && status < 300 && json.Valid(out) {
			if err := d.idemComplete(r.Context(), tenantID, key, status, out); err != nil {
				_ = d.idemRelease(r.Context(), tenantID, key)
			}
		} else {
			_ = d.idemRelease(r.Context(), tenantID, key)
		}
		for k, v := range c.header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
}
