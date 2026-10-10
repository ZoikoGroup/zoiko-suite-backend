// Package store is the Postgres implementation of service.Store, plus the
// outbox claim side the relay uses.
//
// Every request-path transaction installs app.tenant_id (the ACTOR's tenant) as
// a transaction-local setting. Currency data is GLOBAL reference data and its
// tables carry no tenant_id and no RLS (see migration 000001); the tenant
// setting matters for the tables that ARE tenant-scoped: tenant_currency_support,
// currency_idempotency and currency_outbox (FORCE row-level security).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/service"
)

// PgStore is the Postgres-backed store.
type PgStore struct {
	pool *pgxpool.Pool
}

// New builds the store over a pool.
func New(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

var _ service.Store = (*PgStore)(nil)

// InTx runs fn in one transaction with app.tenant_id installed.
func (s *PgStore) InTx(ctx context.Context, tenantID string, fn func(service.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(&pgTx{tx: tx, tenant: tenantID}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

type pgTx struct {
	tx     pgx.Tx
	tenant string
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (t *pgTx) LockKey(ctx context.Context, key string) error {
	_, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key)
	return err
}

// ── idempotency ──────────────────────────────────────────────────────────────

func (t *pgTx) GetIdempotency(ctx context.Context, key string) (*service.IdempotencyRecord, error) {
	var r service.IdempotencyRecord
	err := t.tx.QueryRow(ctx, `
		SELECT idempotency_key, operation, request_hash, response
		FROM currency_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, t.tenant, key).
		Scan(&r.Key, &r.Operation, &r.RequestHash, &r.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (t *pgTx) PutIdempotency(ctx context.Context, rec service.IdempotencyRecord) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO currency_idempotency (tenant_id, idempotency_key, operation, request_hash, response)
		VALUES ($1, $2, $3, $4, $5)`, t.tenant, rec.Key, rec.Operation, rec.RequestHash, []byte(rec.Response))
	return err
}

// ── currencies ───────────────────────────────────────────────────────────────

const currencyColumns = `currency_id::text, alpha_code, numeric_code, name, fund_or_metal_flag, status,
	valid_from, valid_to, version, COALESCE(last_import_id::text, ''), COALESCE(last_import_actor, ''),
	created_at, recorded_at`

func scanCurrency(row pgx.Row) (*domain.Currency, error) {
	var c domain.Currency
	var status string
	if err := row.Scan(&c.CurrencyID, &c.AlphaCode, &c.NumericCode, &c.Name, &c.FundOrMetalFlag, &status,
		&c.ValidFrom, &c.ValidTo, &c.Version, &c.LastImportID, &c.LastImportActor, &c.CreatedAt, &c.RecordedAt); err != nil {
		return nil, err
	}
	c.Status = domain.Status(status)
	return &c, nil
}

func (t *pgTx) queryCurrencies(ctx context.Context, sql string, args ...any) ([]domain.Currency, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Currency
	for rows.Next() {
		c, err := scanCurrency(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (t *pgTx) GetCurrencyByID(ctx context.Context, id string, forUpdate bool) (*domain.Currency, error) {
	if !validUUID(id) {
		return nil, nil // a malformed id cannot name a row
	}
	q := "SELECT " + currencyColumns + " FROM currencies WHERE currency_id = $1"
	if forUpdate {
		q += " FOR UPDATE"
	}
	c, err := scanCurrency(t.tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (t *pgTx) FindCurrenciesByAlpha(ctx context.Context, alpha string) ([]domain.Currency, error) {
	return t.queryCurrencies(ctx, "SELECT "+currencyColumns+" FROM currencies WHERE alpha_code = $1 ORDER BY valid_from, currency_id", alpha)
}

func (t *pgTx) FindCurrenciesByNumeric(ctx context.Context, numeric string) ([]domain.Currency, error) {
	return t.queryCurrencies(ctx, "SELECT "+currencyColumns+" FROM currencies WHERE numeric_code = $1 ORDER BY valid_from, currency_id", numeric)
}

func (t *pgTx) ListCurrencies(ctx context.Context, status string, limit, offset int) ([]domain.Currency, error) {
	return t.queryCurrencies(ctx, "SELECT "+currencyColumns+` FROM currencies
		WHERE ($1 = '' OR status = $1) ORDER BY alpha_code, valid_from, currency_id LIMIT $2 OFFSET $3`, status, limit, offset)
}

func (t *pgTx) InsertCurrency(ctx context.Context, c *domain.Currency) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO currencies (currency_id, alpha_code, numeric_code, name, fund_or_metal_flag, status,
			valid_from, valid_to, version, last_import_id, last_import_actor, created_at, recorded_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10::uuid,$11,$12,$13)`,
		c.CurrencyID, c.AlphaCode, c.NumericCode, c.Name, c.FundOrMetalFlag, string(c.Status),
		c.ValidFrom, c.ValidTo, c.Version, nullable(c.LastImportID), nullable(c.LastImportActor), c.CreatedAt, c.RecordedAt)
	if isUniqueViolation(err) {
		return domain.Errf(domain.CodeDuplicateCandidate, "a non-retired currency with alpha_code %q or numeric_code %q already exists", c.AlphaCode, c.NumericCode)
	}
	return err
}

func (t *pgTx) UpdateCurrency(ctx context.Context, c *domain.Currency, expectedVersion int64) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE currencies SET name = $3, fund_or_metal_flag = $4, status = $5, valid_to = $6, version = $7,
			last_import_id = $8::uuid, last_import_actor = $9, recorded_at = $10
		WHERE currency_id = $1::uuid AND version = $2`,
		c.CurrencyID, expectedVersion, c.Name, c.FundOrMetalFlag, string(c.Status), c.ValidTo, c.Version,
		nullable(c.LastImportID), nullable(c.LastImportActor), c.RecordedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

// ── minor-unit versions, status history ──────────────────────────────────────

func (t *pgTx) InsertMinorUnitVersion(ctx context.Context, mv *domain.MinorUnitVersion) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO currency_minor_unit_versions (currency_id, minor_unit, valid_from, source_version, import_id, evidence_ref, recorded_at)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7)`,
		mv.CurrencyID, int16(mv.MinorUnit), mv.ValidFrom, mv.SourceVersion, nullable(mv.ImportID), mv.EvidenceRef, mv.RecordedAt)
	return err
}

func (t *pgTx) ListMinorUnitVersions(ctx context.Context, currencyID string) ([]domain.MinorUnitVersion, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT currency_id::text, minor_unit, valid_from, source_version, COALESCE(import_id::text, ''), evidence_ref, recorded_at
		FROM currency_minor_unit_versions WHERE currency_id = $1::uuid ORDER BY valid_from`, currencyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MinorUnitVersion
	for rows.Next() {
		var v domain.MinorUnitVersion
		var mu int16
		if err := rows.Scan(&v.CurrencyID, &mu, &v.ValidFrom, &v.SourceVersion, &v.ImportID, &v.EvidenceRef, &v.RecordedAt); err != nil {
			return nil, err
		}
		v.MinorUnit = int(mu)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (t *pgTx) InsertStatusHistory(ctx context.Context, e *domain.StatusHistoryEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO currency_status_history (history_id, currency_id, from_status, to_status, actor, approver, reason,
			effective_at, recorded_at, resulting_version, correlation_id)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.HistoryID, e.CurrencyID, string(e.FromStatus), string(e.ToStatus), e.Actor, nullable(e.Approver), e.Reason,
		e.EffectiveAt, e.RecordedAt, e.ResultingVersion, nullable(e.CorrelationID))
	return err
}

// ── imports ──────────────────────────────────────────────────────────────────

func (t *pgTx) GetImportBySourceVersion(ctx context.Context, source, version string) (*domain.Import, error) {
	var imp domain.Import
	var status string
	var errsJSON, sumJSON []byte
	err := t.tx.QueryRow(ctx, `
		SELECT import_id::text, source_name, source_version, manifest_hash, status, row_count, COALESCE(quarantine_reason, ''),
			row_errors, summary, effective_at, actor, actor_tenant_id, reason, COALESCE(correlation_id, ''), created_at
		FROM currency_imports WHERE source_name = $1 AND source_version = $2
		ORDER BY created_at LIMIT 1`, source, version).
		Scan(&imp.ImportID, &imp.SourceName, &imp.SourceVersion, &imp.ManifestHash, &status, &imp.RowCount, &imp.QuarantineReason,
			&errsJSON, &sumJSON, &imp.EffectiveAt, &imp.Actor, &imp.ActorTenantID, &imp.Reason, &imp.CorrelationID, &imp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	imp.Status = domain.ImportStatus(status)
	if len(errsJSON) > 0 {
		if err := json.Unmarshal(errsJSON, &imp.RowErrors); err != nil {
			return nil, fmt.Errorf("decode row_errors: %w", err)
		}
	}
	if len(sumJSON) > 0 && string(sumJSON) != "null" {
		imp.Summary = &domain.ImportSummary{}
		if err := json.Unmarshal(sumJSON, imp.Summary); err != nil {
			return nil, fmt.Errorf("decode summary: %w", err)
		}
	}
	return &imp, nil
}

func (t *pgTx) InsertImport(ctx context.Context, imp *domain.Import) error {
	rows, err := json.Marshal(imp.Rows)
	if err != nil {
		return err
	}
	rowErrs, err := json.Marshal(imp.RowErrors)
	if err != nil {
		return err
	}
	summary, err := json.Marshal(imp.Summary)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO currency_imports (import_id, source_name, source_version, manifest_hash, status, row_count, quarantine_reason,
			rows, row_errors, summary, effective_at, actor, actor_tenant_id, reason, correlation_id, created_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		imp.ImportID, imp.SourceName, imp.SourceVersion, imp.ManifestHash, string(imp.Status), imp.RowCount, nullable(imp.QuarantineReason),
		rows, rowErrs, summary, imp.EffectiveAt, imp.Actor, imp.ActorTenantID, imp.Reason, nullable(imp.CorrelationID), imp.CreatedAt)
	if isUniqueViolation(err) {
		return domain.Errf(domain.CodeDuplicateCandidate, "this source/version/manifest was imported concurrently")
	}
	return err
}

// ── tenant overlay ───────────────────────────────────────────────────────────

const tenantSupportColumns = `s.tenant_id, s.currency_id::text, c.alpha_code, s.enabled, s.version, s.reason, s.actor, s.created_at, s.updated_at`

func scanTenantSupport(row pgx.Row) (*domain.TenantSupport, error) {
	var ts domain.TenantSupport
	if err := row.Scan(&ts.TenantID, &ts.CurrencyID, &ts.AlphaCode, &ts.Enabled, &ts.Version, &ts.Reason, &ts.Actor, &ts.CreatedAt, &ts.UpdatedAt); err != nil {
		return nil, err
	}
	return &ts, nil
}

func (t *pgTx) GetTenantSupport(ctx context.Context, tenantID, currencyID string) (*domain.TenantSupport, error) {
	ts, err := scanTenantSupport(t.tx.QueryRow(ctx, `
		SELECT `+tenantSupportColumns+` FROM tenant_currency_support s JOIN currencies c USING (currency_id)
		WHERE s.tenant_id = $1 AND s.currency_id = $2::uuid`, tenantID, currencyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ts, err
}

func (t *pgTx) UpsertTenantSupport(ctx context.Context, ts *domain.TenantSupport, expectedVersion int64) error {
	if expectedVersion == 0 {
		tag, err := t.tx.Exec(ctx, `
			INSERT INTO tenant_currency_support (tenant_id, currency_id, enabled, version, reason, actor, created_at, updated_at)
			VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
			ts.TenantID, ts.CurrencyID, ts.Enabled, ts.Version, ts.Reason, ts.Actor, ts.CreatedAt, ts.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrVersionConflictStore
		}
		return nil
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE tenant_currency_support SET enabled = $4, version = $5, reason = $6, actor = $7, updated_at = $8
		WHERE tenant_id = $1 AND currency_id = $2::uuid AND version = $3`,
		ts.TenantID, ts.CurrencyID, expectedVersion, ts.Enabled, ts.Version, ts.Reason, ts.Actor, ts.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

func (t *pgTx) ListTenantSupport(ctx context.Context, tenantID string) ([]domain.TenantSupport, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT `+tenantSupportColumns+` FROM tenant_currency_support s JOIN currencies c USING (currency_id)
		WHERE s.tenant_id = $1 ORDER BY c.alpha_code`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TenantSupport
	for rows.Next() {
		ts, err := scanTenantSupport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ts)
	}
	return out, rows.Err()
}

// ── outbox ───────────────────────────────────────────────────────────────────

// Enqueue writes the event in the CALLER's transaction: a change that commits
// always has its event, and a change that rolls back never leaks one.
func (t *pgTx) Enqueue(ctx context.Context, e service.OutboxEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO currency_outbox (tenant_id, object_id, event_type, payload)
		VALUES ($1, $2::uuid, $3, $4)`, e.TenantID, e.ObjectID, e.EventType, e.Payload)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", e.EventType, err)
	}
	return nil
}

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant: the
// relay drains every tenant's backlog and is admitted by the outbox policy's
// explicit second disjunct.
func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set relay context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit relay transaction: %w", err)
	}
	return nil
}

// ClaimOutbox takes up to limit unpublished events, oldest first, locking them
// (FOR UPDATE SKIP LOCKED, so replicas never double-publish) while fn publishes.
// At-least-once: a crash mid-publish rolls the marking back and re-delivers.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, object_id::text, payload
			FROM currency_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		var claimed []OutboxRecord
		for rows.Next() {
			var r OutboxRecord
			if err := rows.Scan(&r.OutboxID, &r.EventType, &r.Key, &r.Body); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbox rows: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if err := fn(claimed); err != nil {
			if _, uerr := tx.Exec(ctx, `
				UPDATE currency_outbox SET attempts = attempts + 1, last_error = $2 WHERE outbox_id = ANY($1)`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE currency_outbox SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM currency_outbox WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
