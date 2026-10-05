package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/events"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// mapPgError turns a malformed identifier into "not found".
//
// delegation_id is a uuid column, so a caller passing a non-UUID string reaches
// Postgres and comes back as 22P02 invalid_text_representation. That was
// surfacing as 503 store_unavailable -- a request the caller got wrong,
// reported as an outage, sending whoever is on call to look at a healthy
// database. A malformed id cannot name a row, which is exactly what 404 means.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrDelegationNotFound
	}
	// 23P01: the exclusion constraint refused an overlapping ACTIVE grant —
	// the race the application pre-check cannot close on its own.
	if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
		return domain.ErrOverlapConflict
	}
	return err
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const delegationColumns = `
	delegation_id, tenant_id, legal_entity_id, delegator_principal_id, delegate_principal_id,
	action_type, effective_from, effective_to, status, created_by_principal_id, correlation_id,
	created_at, updated_at, revoked_by_principal_id, revoked_at, expired_at,
	authority_limit_cents, authority_limit_currency, authority_limit_quantity,
	version, reason, approved_by_principal_id, approved_at, approval_method,
	suspended_by_principal_id, suspended_at, suspension_reason, revocation_reason
`

// prefixedDelegationColumns qualifies every column with a table alias.
//
// Needed by the cross-tenant sweep, whose UPDATE ... FROM puts two relations in
// scope: the target table and the CTE naming the batch. delegation_id exists in
// both, so the bare list is ambiguous and Postgres refuses the statement. Built
// from the same constant rather than written out a second time, so a column
// added to one list cannot go missing from the other -- scanDelegation reads
// positionally and a divergence would be a silent field-shift, not an error.
func prefixedDelegationColumns(alias string) string {
	cols := strings.FieldsFunc(delegationColumns, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	})
	for i, c := range cols {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ", ")
}

func scanDelegation(row pgx.Row, d *domain.DelegationGrant) error {
	return scanDelegationPlus(row, d)
}

// scanDelegationPlus reads delegationColumns, then any extra columns. One
// scanner for both, so the positional list cannot drift between them.
func scanDelegationPlus(row pgx.Row, d *domain.DelegationGrant, extra ...any) error {
	var status string
	var reason *string
	dest := []any{
		&d.DelegationID, &d.TenantID, &d.LegalEntityID, &d.DelegatorPrincipalID, &d.DelegatePrincipalID,
		&d.ActionType, &d.EffectiveFrom, &d.EffectiveTo, &status, &d.CreatedByPrincipalID, &d.CorrelationID,
		&d.CreatedAt, &d.UpdatedAt, &d.RevokedByPrincipalID, &d.RevokedAt, &d.ExpiredAt,
		&d.AuthorityLimitCents, &d.AuthorityLimitCurrency, &d.AuthorityLimitQuantity,
		&d.Version, &reason, &d.ApprovedByPrincipalID, &d.ApprovedAt, &d.ApprovalMethod,
		&d.SuspendedByPrincipalID, &d.SuspendedAt, &d.SuspensionReason, &d.RevocationReason,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	d.Status = domain.DelegationStatus(status)
	d.Reason = ""
	if reason != nil {
		d.Reason = *reason
	}
	return nil
}

// CreateDelegation inserts a new delegation grant, idempotent on
// (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id, effective_from, effective_to).
// Per the audit gap: "Idempotency scoped by principal+role+scope+validity".
func (s *PgStore) CreateDelegation(ctx context.Context, d *domain.DelegationGrant) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrTenantMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO delegation_grants (
				delegation_id, tenant_id, legal_entity_id, delegator_principal_id, delegate_principal_id,
				action_type, effective_from, effective_to, status, created_by_principal_id, correlation_id,
				created_at, updated_at, revoked_by_principal_id, revoked_at, expired_at,
				authority_limit_cents, authority_limit_currency, authority_limit_quantity,
				version, reason, approved_by_principal_id, approved_at, approval_method
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
			ON CONFLICT (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id, effective_from, effective_to) DO NOTHING
		`, d.DelegationID, tenantID, d.LegalEntityID, d.DelegatorPrincipalID, d.DelegatePrincipalID,
			d.ActionType, d.EffectiveFrom, d.EffectiveTo, string(d.Status), d.CreatedByPrincipalID, d.CorrelationID,
			d.CreatedAt, d.UpdatedAt, d.RevokedByPrincipalID, d.RevokedAt, d.ExpiredAt,
			d.AuthorityLimitCents, d.AuthorityLimitCurrency, d.AuthorityLimitQuantity, 1,
			d.Reason, d.ApprovedByPrincipalID, d.ApprovedAt, d.ApprovalMethod)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			// Same transaction as the INSERT. A grant that exists always has
			// its event; ON CONFLICT DO NOTHING means a replay reaches the
			// branch below instead and enqueues nothing, so an idempotent
			// retry cannot emit a second authority.delegated.
			d.TenantID = tenantID
			d.Version = 1
			transition := domain.TransitionActivated
			if d.Status == domain.DelegationStatusProposed {
				transition = domain.TransitionProposed
			}
			if err := recordHistory(ctx, tx, d, transition, d.CreatedByPrincipalID, d.Reason); err != nil {
				return err
			}
			// A PROPOSED grant confers nothing, so nothing downstream is told
			// until it is activated (DelegationActivated = authority.delegated).
			if d.Status != domain.DelegationStatusActive {
				return nil
			}
			return enqueue(ctx, tx, events.EventDelegated, *d)
		}
		// Replay: find the existing grant using the composite idempotency key
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND correlation_id = $2 AND created_by_principal_id = $3 AND action_type = $4 AND legal_entity_id = $5 AND effective_from = $6 AND effective_to = $7", tenantID, d.CorrelationID, d.CreatedByPrincipalID, d.ActionType, d.LegalEntityID, d.EffectiveFrom, d.EffectiveTo)
		return scanDelegation(row, d)
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return created, nil
}

// ExpireDue lazily flips any ACTIVE delegation whose EffectiveTo has passed
// to EXPIRED, in a single statement, and returns the rows that actually
// flipped — so the caller can publish authority.expired for each one
// exactly once, at the moment the flip is observed, rather than running a
// separate scheduler process.
func (s *PgStore) ExpireDue(ctx context.Context) ([]domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var out []domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		// expired_at = effective_to, not now().
		//
		// They are different facts. effective_to is when the authority ended,
		// which is a property of the grant and was knowable the moment it was
		// written. now() is when this service got around to looking. Writing
		// the second into a column named for the first misdates the end of an
		// authority by however long the gap was -- a weekend, for a grant that
		// lapsed on Friday and was next read on Monday -- and Doc 04 §6.3
		// requires these records to stand as evidence. updated_at keeps now(),
		// so "when did it end" and "when did we notice" are both recorded and
		// are no longer the same column.
		rows, err := tx.Query(ctx, `
			UPDATE delegation_grants
			SET status = 'EXPIRED', expired_at = effective_to, updated_at = $1, version = version + 1
			WHERE tenant_id = $2 AND status IN ('PROPOSED', 'ACTIVE', 'SUSPENDED') AND effective_to <= $1
			RETURNING `+delegationColumns, now, tenantID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d domain.DelegationGrant
			if err := scanDelegation(rows, &d); err != nil {
				rows.Close()
				return err
			}
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Enqueued inside the sweep's own transaction, after the cursor is
		// closed — pgx allows one statement at a time per connection, so
		// enqueueing inside the loop would abort the UPDATE mid-read.
		//
		// Atomicity matters more here than anywhere else in this file: the
		// flip to EXPIRED is the only record that the lapse was observed, and
		// it happens exactly once. Published separately, a broker failure at
		// this moment would lose authority.expired permanently, because the
		// next sweep finds no ACTIVE row left to flip.
		for _, d := range out {
			if err := recordHistory(ctx, tx, &d, domain.TransitionExpired, "system:expiry", "window ended"); err != nil {
				return err
			}
			if err := enqueue(ctx, tx, events.EventExpired, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetDelegation(ctx context.Context, delegationID string) (*domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var d domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2", tenantID, delegationID)
		return scanDelegation(row, &d)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDelegationNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &d, nil
}

func (s *PgStore) ListDelegations(ctx context.Context, f domain.ListDelegationsFilter) ([]domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var out []domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + delegationColumns + " FROM delegation_grants WHERE tenant_id = $1"
		args := []any{tenantID}
		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if f.DelegatorPrincipalID != "" {
			args = append(args, f.DelegatorPrincipalID)
			query += fmt.Sprintf(" AND delegator_principal_id = $%d", len(args))
		}
		if f.DelegatePrincipalID != "" {
			args = append(args, f.DelegatePrincipalID)
			query += fmt.Sprintf(" AND delegate_principal_id = $%d", len(args))
		}
		if f.Status != "" {
			args = append(args, f.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		// The self scope. Applied when the read was NOT authorized against a
		// legal entity, so an unscoped read answers with the delegations the
		// caller is party to rather than the tenant's whole register.
		if f.SelfPrincipalID != "" {
			args = append(args, f.SelfPrincipalID)
			query += fmt.Sprintf(" AND (delegator_principal_id = $%d OR delegate_principal_id = $%d)", len(args), len(args))
		}
		// delegation_id breaks ties. created_at alone is not a total order --
		// two grants written in the same transaction share it -- so without
		// the tiebreaker a paged read can return one row twice and skip
		// another entirely.
		query += " ORDER BY created_at DESC, delegation_id DESC"
		if f.Limit > 0 {
			args = append(args, f.Limit)
			query += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if f.Offset > 0 {
			args = append(args, f.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.DelegationGrant
			if err := scanDelegation(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) RecordRefusedEscalation(ctx context.Context, r *domain.RefusedEscalation) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	if r.RefusedID == "" {
		r.RefusedID = uuid.NewString()
	}
	if r.RefusedAt.IsZero() {
		r.RefusedAt = time.Now().UTC()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO refused_escalations (refused_id, tenant_id, legal_entity_id, caller_principal_id, delegator_principal_id, delegate_principal_id, action_type, effective_from, effective_to, refusal_reason, correlation_id, idempotency_key, request_id, source_channel, refused_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		`, r.RefusedID, tenantID, r.LegalEntityID, r.CallerPrincipalID, r.DelegatorPrincipalID, r.DelegatePrincipalID, r.ActionType, r.EffectiveFrom, r.EffectiveTo, r.RefusalReason, r.CorrelationID, r.IdempotencyKey, r.RequestID, r.SourceChannel, r.RefusedAt)
		return err
	})
}

func enqueue(ctx context.Context, tx pgx.Tx, eventType string, d domain.DelegationGrant) error {
	return enqueueBy(ctx, tx, eventType, d, "", "")
}

// enqueueBy enqueues an event naming the principal who performed the change.
func enqueueBy(ctx context.Context, tx pgx.Tx, eventType string, d domain.DelegationGrant, actor, reason string) error {
	key, body, err := events.BuildFor(eventType, d, actor, reason)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO delegation_outbox (tenant_id, delegation_id, event_type, payload)
		VALUES ($1, $2, $3, $4)
	`, d.TenantID, key, eventType, body)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", eventType, err)
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

// withRelay runs fn with app.outbox_relay installed instead of a tenant.
//
// The relay is the one code path in this service that legitimately crosses
// tenants: it drains every tenant's backlog from a single loop. Rather than
// letting it run unscoped — which under FORCE ROW LEVEL SECURITY would simply
// see nothing — it names itself, so the policy admits it by an explicit,
// auditable disjunct rather than by the absence of a control.
// withSweeper runs fn with the cross-tenant expiry exemption installed.
//
// The same shape as withRelay and for the same reason: expiry is not a request
// and belongs to no one tenant. A grant lapses because its window closed, which
// happens whether or not anybody is looking, so the loop that records it cannot
// be scoped to the tenant that happens to be making a request. Admitted by
// migration 000004's named capability rather than by connecting as a role that
// bypasses RLS -- one documented exemption instead of unlimited reach.
func (s *PgStore) withSweeper(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin sweeper transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.expiry_sweeper', 'true', true)"); err != nil {
		return fmt.Errorf("set sweeper context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit sweeper transaction: %w", err)
	}
	return nil
}

// ExpireDueAllTenants flips every due delegation in every tenant and enqueues
// authority.expired for each, in one transaction.
//
// This is the authoritative expiry path. ExpireDue still runs on the read paths
// so a register read never shows a grant as ACTIVE past its window, but it can
// only see the tenant making the request -- a tenant whose register nobody
// opens would otherwise never expire anything, and its delegates would keep
// authority indefinitely with no authority.expired ever published.
//
// limit bounds one pass. A register that has been unswept for a long time, or
// one restored from backup, can have a large due backlog, and taking it in one
// statement would hold a write lock across the whole table. The caller loops
// while the count comes back full.
func (s *PgStore) ExpireDueAllTenants(ctx context.Context, limit int) ([]domain.DelegationGrant, error) {
	if limit <= 0 {
		limit = 500
	}
	var out []domain.DelegationGrant
	err := s.withSweeper(ctx, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		// The CTE picks the batch with FOR UPDATE SKIP LOCKED so a second
		// replica sweeping concurrently steps over these rows rather than
		// blocking behind them. Without it, two instances would serialise on
		// the same batch and one would do no work while holding a transaction
		// open for the other's duration.
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT delegation_id
				  FROM delegation_grants
				 WHERE status IN ('PROPOSED', 'ACTIVE', 'SUSPENDED') AND effective_to <= $1
				 ORDER BY effective_to
				 LIMIT $2
				 FOR UPDATE SKIP LOCKED
			)
			UPDATE delegation_grants g
			   SET status = 'EXPIRED', expired_at = g.effective_to, updated_at = $1, version = g.version + 1
			  FROM due
			 WHERE g.delegation_id = due.delegation_id
			RETURNING `+prefixedDelegationColumns("g"), now, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d domain.DelegationGrant
			if err := scanDelegation(rows, &d); err != nil {
				rows.Close()
				return err
			}
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Same transaction as the flip, same reasoning as ExpireDue: the flip
		// happens exactly once, so an event published separately and lost could
		// never be regenerated -- the next pass finds no ACTIVE row left.
		for _, d := range out {
			if err := recordHistory(ctx, tx, &d, domain.TransitionExpired, "system:expiry", "window ended"); err != nil {
				return err
			}
			if err := enqueue(ctx, tx, events.EventExpired, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DueCount reports how many ACTIVE grants are past their window across every
// tenant, and how overdue the oldest of them is.
//
// Separate from the sweep so the numbers are reported even on a pass where the
// sweep itself failed -- which is exactly the pass where a growing backlog is
// the thing worth seeing. Same split, and the same reason, as the relay's
// observeDepth.
func (s *PgStore) DueCount(ctx context.Context) (due int64, oldestOverdue time.Duration, err error) {
	err = s.withSweeper(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*), min(effective_to)
			  FROM delegation_grants
			 WHERE status IN ('PROPOSED', 'ACTIVE', 'SUSPENDED') AND effective_to <= now()
		`).Scan(&due, &oldest); err != nil {
			return err
		}
		if oldest != nil {
			oldestOverdue = time.Since(*oldest)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return due, oldestOverdue, nil
}

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
// for the duration of the caller's drain.
//
// FOR UPDATE SKIP LOCKED is what makes more than one replica safe: a second
// instance draining concurrently steps over the rows this one holds instead of
// blocking behind them or, worse, publishing them a second time.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, delegation_id::text, payload
			FROM delegation_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, limit)
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

		// fn publishes. It runs while the rows are still locked and BEFORE the
		// marking commits, so a crash mid-publish rolls the marking back and the
		// events are re-delivered rather than lost. At-least-once, chosen
		// deliberately: a duplicate authority.revoked ends a session that is
		// already ending, a lost one leaves it open.
		if err := fn(claimed); err != nil {
			ids := make([]int64, 0, len(claimed))
			for _, r := range claimed {
				ids = append(ids, r.OutboxID)
			}
			// Recorded on the rows themselves, so a stuck event can be diagnosed
			// from the table without correlating against logs.
			if _, uerr := tx.Exec(ctx, `
				UPDATE delegation_outbox
				SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)
			`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			// Committed: the attempt count and the error are worth keeping even
			// though the publish failed. published_at is untouched, so the rows
			// are claimed again on the next tick.
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}

		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE delegation_outbox SET published_at = now() WHERE outbox_id = ANY($1)
		`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
//
// Both, not just the depth. A backlog of ten that is three seconds old is a
// service under load; a backlog of ten that is an hour old is a relay that has
// stopped, and on this service that means a revocation has not reached the
// consumer that ends the delegate's session. Depth alone cannot tell them
// apart, which is why the alert rule uses the age.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		row := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM delegation_outbox WHERE published_at IS NULL
		`)
		if err := row.Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
