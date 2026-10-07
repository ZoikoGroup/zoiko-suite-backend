// Package store is the Postgres implementation of service.Store, plus the
// outbox claim side the relay uses.
//
// Every request-path transaction installs app.tenant_id (the ACTOR's tenant) as
// a transaction-local setting. Every table is tenant-scoped with FORCE row-level
// security, so a connection that forgets the setting sees no rows at all.
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

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/service"
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

// SQLSTATEs the store translates into typed domain errors.
const (
	sqlUniqueViolation    = "23505"
	sqlExclusionViolation = "23P01"
	sqlCheckViolation     = "23514"
	sqlRestrictViolation  = "23001"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
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

func dateOrNil(d *domain.Date) *time.Time {
	if d == nil {
		return nil
	}
	t := d.Time
	return &t
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
		FROM fiscal_calendar_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, t.tenant, key).
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
		INSERT INTO fiscal_calendar_idempotency (tenant_id, idempotency_key, operation, request_hash, response)
		VALUES ($1, $2, $3, $4, $5)`, t.tenant, rec.Key, rec.Operation, rec.RequestHash, []byte(rec.Response))
	return err
}

// ── calendars ────────────────────────────────────────────────────────────────

const calendarColumns = `calendar_id::text, tenant_id, legal_entity_id, code, scope, status, version, created_by, created_at, recorded_at`

func scanCalendar(row pgx.Row) (*domain.FiscalCalendar, error) {
	var c domain.FiscalCalendar
	var status string
	if err := row.Scan(&c.CalendarID, &c.TenantID, &c.LegalEntityID, &c.Code, &c.Scope, &status, &c.Version, &c.CreatedBy, &c.CreatedAt, &c.RecordedAt); err != nil {
		return nil, err
	}
	c.Status = domain.CalendarStatus(status)
	return &c, nil
}

func (t *pgTx) GetCalendarByID(ctx context.Context, id string, forUpdate bool) (*domain.FiscalCalendar, error) {
	if !validUUID(id) {
		return nil, nil // a malformed id cannot name a row
	}
	q := "SELECT " + calendarColumns + " FROM fiscal_calendars WHERE tenant_id = $1 AND calendar_id = $2::uuid"
	if forUpdate {
		q += " FOR UPDATE"
	}
	c, err := scanCalendar(t.tx.QueryRow(ctx, q, t.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (t *pgTx) FindCalendarByCode(ctx context.Context, legalEntityID, code string) (*domain.FiscalCalendar, error) {
	c, err := scanCalendar(t.tx.QueryRow(ctx,
		"SELECT "+calendarColumns+" FROM fiscal_calendars WHERE tenant_id = $1 AND legal_entity_id = $2 AND code = $3",
		t.tenant, legalEntityID, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (t *pgTx) InsertCalendar(ctx context.Context, c *domain.FiscalCalendar) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO fiscal_calendars (calendar_id, tenant_id, legal_entity_id, code, scope, status, version, created_by, created_at, recorded_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		c.CalendarID, c.TenantID, c.LegalEntityID, c.Code, c.Scope, string(c.Status), c.Version, c.CreatedBy, c.CreatedAt, c.RecordedAt)
	if pgCode(err) == sqlUniqueViolation {
		return domain.Errf(domain.CodeDuplicateCandidate, "calendar %q already exists for this legal entity", c.Code)
	}
	return err
}

func (t *pgTx) UpdateCalendar(ctx context.Context, c *domain.FiscalCalendar, expectedVersion int64) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE fiscal_calendars SET status = $3, version = $4, recorded_at = $5
		WHERE tenant_id = $1 AND calendar_id = $2::uuid AND version = $6`,
		t.tenant, c.CalendarID, string(c.Status), c.Version, c.RecordedAt, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

// ── versions ─────────────────────────────────────────────────────────────────

const versionColumns = `version_id::text, calendar_id::text, tenant_id, legal_entity_id, scope, version_no, pattern,
	fiscal_year_start_month, fiscal_year_start_day, effective_from, effective_to, status, proposed_by, proposal_reason,
	COALESCE(approved_by, ''), COALESCE(approval_reason, ''), approved_at, COALESCE(activated_by, ''), activated_at,
	COALESCE(superseded_by_version_id::text, ''), recorded_at, version`

func scanVersion(row pgx.Row) (*domain.FiscalCalendarVersion, error) {
	var v domain.FiscalCalendarVersion
	var status string
	var patternJSON []byte
	var from time.Time
	var to *time.Time
	if err := row.Scan(&v.VersionID, &v.CalendarID, &v.TenantID, &v.LegalEntityID, &v.Scope, &v.VersionNo, &patternJSON,
		&v.FiscalYearStartMonth, &v.FiscalYearStartDay, &from, &to, &status, &v.ProposedBy, &v.ProposalReason,
		&v.ApprovedBy, &v.ApprovalReason, &v.ApprovedAt, &v.ActivatedBy, &v.ActivatedAt,
		&v.SupersededByVersion, &v.RecordedAt, &v.Version); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(patternJSON, &v.Pattern); err != nil {
		return nil, fmt.Errorf("decode stored pattern: %w", err)
	}
	v.EffectiveFrom = domain.FromTime(from)
	if to != nil {
		d := domain.FromTime(*to)
		v.EffectiveTo = &d
	}
	v.Status = domain.VersionStatus(status)
	return &v, nil
}

func (t *pgTx) queryVersions(ctx context.Context, sql string, args ...any) ([]domain.FiscalCalendarVersion, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.FiscalCalendarVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (t *pgTx) GetVersionByID(ctx context.Context, id string, forUpdate bool) (*domain.FiscalCalendarVersion, error) {
	if !validUUID(id) {
		return nil, nil
	}
	q := "SELECT " + versionColumns + " FROM fiscal_calendar_versions WHERE tenant_id = $1 AND version_id = $2::uuid"
	if forUpdate {
		q += " FOR UPDATE"
	}
	v, err := scanVersion(t.tx.QueryRow(ctx, q, t.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func (t *pgTx) ListVersions(ctx context.Context, calendarID string) ([]domain.FiscalCalendarVersion, error) {
	if !validUUID(calendarID) {
		return nil, nil
	}
	return t.queryVersions(ctx, "SELECT "+versionColumns+` FROM fiscal_calendar_versions
		WHERE tenant_id = $1 AND calendar_id = $2::uuid ORDER BY version_no`, t.tenant, calendarID)
}

func (t *pgTx) ListInForceVersions(ctx context.Context, legalEntityID, scope string) ([]domain.FiscalCalendarVersion, error) {
	return t.queryVersions(ctx, "SELECT "+versionColumns+` FROM fiscal_calendar_versions
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND scope = $3 AND status IN ('ACTIVE', 'SUPERSEDED')
		ORDER BY effective_from, version_id`, t.tenant, legalEntityID, scope)
}

func (t *pgTx) InsertVersion(ctx context.Context, v *domain.FiscalCalendarVersion) error {
	pattern, err := v.Pattern.CanonicalJSON()
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO fiscal_calendar_versions (version_id, calendar_id, tenant_id, legal_entity_id, scope, version_no, pattern,
			fiscal_year_start_month, fiscal_year_start_day, effective_from, effective_to, status, proposed_by, proposal_reason,
			recorded_at, version)
		VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		v.VersionID, v.CalendarID, v.TenantID, v.LegalEntityID, v.Scope, v.VersionNo, pattern,
		v.FiscalYearStartMonth, v.FiscalYearStartDay, v.EffectiveFrom.Time, dateOrNil(v.EffectiveTo), string(v.Status),
		v.ProposedBy, v.ProposalReason, v.RecordedAt, v.Version)
	if pgCode(err) == sqlUniqueViolation {
		return domain.Errf(domain.CodeVersionConflict, "another version was proposed for this calendar concurrently; re-read and retry")
	}
	return err
}

// UpdateVersion writes only the lifecycle columns. The pattern, start anchor
// and effective_from are not in the statement, and the database trigger would
// refuse a change to them anyway.
func (t *pgTx) UpdateVersion(ctx context.Context, v *domain.FiscalCalendarVersion, expectedVersion int64) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE fiscal_calendar_versions SET status = $3, effective_to = $4, approved_by = $5, approval_reason = $6, approved_at = $7,
			activated_by = $8, activated_at = $9, superseded_by_version_id = $10::uuid, recorded_at = $11, version = $12
		WHERE tenant_id = $1 AND version_id = $2::uuid AND version = $13`,
		t.tenant, v.VersionID, string(v.Status), dateOrNil(v.EffectiveTo), nullable(v.ApprovedBy), nullable(v.ApprovalReason), v.ApprovedAt,
		nullable(v.ActivatedBy), v.ActivatedAt, nullable(v.SupersededByVersion), v.RecordedAt, v.Version, expectedVersion)
	switch pgCode(err) {
	case "":
	case sqlExclusionViolation:
		return domain.Errf(domain.CodeInvalidTransition,
			"effective interval overlaps another in-force calendar version for this legal entity and scope (database exclusion constraint)")
	default:
		return err
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

func (t *pgTx) InsertStatusHistory(ctx context.Context, e *domain.StatusHistoryEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO fiscal_calendar_status_history (history_id, tenant_id, version_id, from_status, to_status, actor, reason,
			recorded_at, resulting_version, correlation_id)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10)`,
		e.HistoryID, t.tenant, e.VersionID, nullable(string(e.FromStatus)), string(e.ToStatus), e.Actor, e.Reason,
		e.RecordedAt, e.ResultingVersion, nullable(e.CorrelationID))
	return err
}

// ── transition plans ─────────────────────────────────────────────────────────

const planColumns = `plan_id::text, tenant_id, legal_entity_id, calendar_id::text, from_version_id::text, to_version_id::text,
	impact_assessment, affects_posted_periods, mapping, status, proposed_by, reason, COALESCE(decided_by, ''),
	COALESCE(decision_reason, ''), decided_at, version, created_at`

func scanPlan(row pgx.Row) (*domain.CalendarTransitionPlan, error) {
	var p domain.CalendarTransitionPlan
	var status string
	var impact, mapping []byte
	if err := row.Scan(&p.PlanID, &p.TenantID, &p.LegalEntityID, &p.CalendarID, &p.FromVersionID, &p.ToVersionID,
		&impact, &p.AffectsPostedPeriods, &mapping, &status, &p.ProposedBy, &p.Reason, &p.DecidedBy,
		&p.DecisionReason, &p.DecidedAt, &p.Version, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.ImpactAssessment, p.Mapping = json.RawMessage(impact), json.RawMessage(mapping)
	p.Status = domain.PlanStatus(status)
	return &p, nil
}

func (t *pgTx) GetPlanByID(ctx context.Context, id string, forUpdate bool) (*domain.CalendarTransitionPlan, error) {
	if !validUUID(id) {
		return nil, nil
	}
	q := "SELECT " + planColumns + " FROM calendar_transition_plans WHERE tenant_id = $1 AND plan_id = $2::uuid"
	if forUpdate {
		q += " FOR UPDATE"
	}
	p, err := scanPlan(t.tx.QueryRow(ctx, q, t.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (t *pgTx) ListPlansForVersion(ctx context.Context, toVersionID string) ([]domain.CalendarTransitionPlan, error) {
	if !validUUID(toVersionID) {
		return nil, nil
	}
	rows, err := t.tx.Query(ctx, "SELECT "+planColumns+` FROM calendar_transition_plans
		WHERE tenant_id = $1 AND to_version_id = $2::uuid ORDER BY created_at, plan_id`, t.tenant, toVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CalendarTransitionPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (t *pgTx) InsertPlan(ctx context.Context, p *domain.CalendarTransitionPlan) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO calendar_transition_plans (plan_id, tenant_id, legal_entity_id, calendar_id, from_version_id, to_version_id,
			impact_assessment, affects_posted_periods, mapping, status, proposed_by, reason, version, created_at)
		VALUES ($1::uuid,$2,$3,$4::uuid,$5::uuid,$6::uuid,$7,$8,$9,$10,$11,$12,$13,$14)`,
		p.PlanID, p.TenantID, p.LegalEntityID, p.CalendarID, p.FromVersionID, p.ToVersionID,
		[]byte(p.ImpactAssessment), p.AffectsPostedPeriods, []byte(p.Mapping), string(p.Status), p.ProposedBy, p.Reason, p.Version, p.CreatedAt)
	if pgCode(err) == sqlUniqueViolation {
		return domain.Errf(domain.CodeDuplicateCandidate, "version %s already has a live transition plan", p.ToVersionID)
	}
	return err
}

func (t *pgTx) UpdatePlan(ctx context.Context, p *domain.CalendarTransitionPlan, expectedVersion int64) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE calendar_transition_plans SET status = $3, decided_by = $4, decision_reason = $5, decided_at = $6, version = $7
		WHERE tenant_id = $1 AND plan_id = $2::uuid AND version = $8`,
		t.tenant, p.PlanID, string(p.Status), nullable(p.DecidedBy), nullable(p.DecisionReason), p.DecidedAt, p.Version, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

// ── outbox ───────────────────────────────────────────────────────────────────

// Enqueue writes the event in the CALLER's transaction: a change that commits
// always has its event, and a change that rolls back never leaks one.
func (t *pgTx) Enqueue(ctx context.Context, e service.OutboxEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO fiscal_calendar_outbox (tenant_id, object_id, event_type, payload)
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
			FROM fiscal_calendar_outbox
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
				UPDATE fiscal_calendar_outbox SET attempts = attempts + 1, last_error = $2 WHERE outbox_id = ANY($1)`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE fiscal_calendar_outbox SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM fiscal_calendar_outbox WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
