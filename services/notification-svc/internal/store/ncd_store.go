package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/ncd"
)

// NCDStore implements ncd.Store over the service's Postgres pool — the
// persistence of the ZS-SVC-Y-001 control plane (migrations 000015–000021).
type NCDStore struct {
	s *PgStore
}

// NewNCD wraps a PgStore.
func NewNCD(s *PgStore) *NCDStore { return &NCDStore{s: s} }

var _ ncd.Store = (*NCDStore)(nil)

// InTx runs fn in one tenant-scoped transaction.
func (n *NCDStore) InTx(ctx context.Context, tenantID string, fn func(ncd.Tx) error) error {
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return n.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return fn(&ncdTx{ctx: ctx, tx: tx, tenant: tenantID})
	})
}

// withPlatformScope runs a cross-tenant SELECT under the SELECT-only hatch of
// migration 000004 (extended to the NCD tables by 000018–000020).
func (n *NCDStore) withPlatformScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin platform scope: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return fmt.Errorf("set platform scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withBindingAdmin runs a write to platform binding configuration under its
// own flag — never the SELECT-only platform scope, never a tenant.
func (n *NCDStore) withBindingAdmin(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin binding admin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.binding_admin', 'true', true)"); err != nil {
		return fmt.Errorf("set binding admin: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanRefs(rows pgx.Rows) ([]ncd.WorkRef, error) {
	defer rows.Close()
	var out []ncd.WorkRef
	for rows.Next() {
		var r ncd.WorkRef
		if err := rows.Scan(&r.ID, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (n *NCDStore) refs(ctx context.Context, q string, args ...any) ([]ncd.WorkRef, error) {
	var out []ncd.WorkRef
	err := n.withPlatformScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		out, err = scanRefs(rows)
		return err
	})
	return out, err
}

// DueJobs finds runnable jobs, highest priority first (NP-53).
func (n *NCDStore) DueJobs(ctx context.Context, now time.Time, limit int) ([]ncd.WorkRef, error) {
	return n.refs(ctx, `
		SELECT job_id::text, tenant_id FROM ncd_delivery_jobs
		WHERE state IN ('QUEUED','AWAITING_EVIDENCE') AND next_run_at <= $1
		  AND (leased_until IS NULL OR leased_until < $1)
		ORDER BY priority DESC, next_run_at
		LIMIT $2`, now, limit)
}

// Backlog aggregates the §13.1 backlog across tenants under the SELECT-only
// platform scope. It returns counts and ages only — no id, tenant or address
// leaves the database, so nothing personal can reach a metric (§13.3).
func (n *NCDStore) Backlog(ctx context.Context, now time.Time) (ncd.Backlog, error) {
	var b ncd.Backlog
	var unknownAge, queuedAge, recordAge float64
	err := n.withPlatformScope(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM ncd_attempts WHERE state = 'UNKNOWN'),
				COALESCE((SELECT EXTRACT(EPOCH FROM $1 - min(state_changed_at))::float8 FROM ncd_attempts WHERE state = 'UNKNOWN'), 0),
				(SELECT count(*) FROM ncd_delivery_jobs WHERE state = 'QUEUED'),
				COALESCE((SELECT EXTRACT(EPOCH FROM $1 - min(next_run_at))::float8 FROM ncd_delivery_jobs
				          WHERE state = 'QUEUED' AND next_run_at <= $1), 0),
				(SELECT count(*) FROM ncd_regulated_notices WHERE deadline_at <= $1
				    AND state IN ('READY','DELIVERY_IN_PROGRESS','DELIVERY_EVIDENCED','ACK_PENDING','EXCEPTION')),
				(SELECT count(*) FROM ncd_regulated_notices WHERE record_status = 'PENDING'),
				COALESCE((SELECT EXTRACT(EPOCH FROM $1 - min(created_at))::float8 FROM ncd_regulated_notices
				          WHERE record_status = 'PENDING'), 0)`, now).
			Scan(&b.UnknownAttempts, &unknownAge, &b.QueuedJobs, &queuedAge, &b.NoticesPastDeadline,
				&b.RecordDeclarationsPending, &recordAge)
	})
	if err != nil {
		return b, err
	}
	var exceptionAge float64
	b.OpenExceptions = map[string]int{}
	err = n.withPlatformScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT kind, count(*), EXTRACT(EPOCH FROM $1 - min(created_at))::float8
			FROM ncd_exceptions WHERE resolved_at IS NULL GROUP BY kind`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var count int
			var age float64
			if err := rows.Scan(&kind, &count, &age); err != nil {
				return err
			}
			b.OpenExceptions[kind] = count
			exceptionAge = max(exceptionAge, age)
		}
		return rows.Err()
	})
	b.OldestOpenException = time.Duration(exceptionAge * float64(time.Second))
	b.OldestUnknown = time.Duration(unknownAge * float64(time.Second))
	b.OldestQueued = time.Duration(queuedAge * float64(time.Second))
	b.OldestRecordPending = time.Duration(recordAge * float64(time.Second))
	return b, err
}

// UnknownPastDue finds UNKNOWN attempts past their resolution deadline.
func (n *NCDStore) UnknownPastDue(ctx context.Context, now time.Time, limit int) ([]ncd.WorkRef, error) {
	return n.refs(ctx, `
		SELECT attempt_id::text, tenant_id FROM ncd_attempts
		WHERE state = 'UNKNOWN' AND resolution_due_at <= $1
		ORDER BY resolution_due_at LIMIT $2`, now, limit)
}

// StaleSubmitting finds attempts a lost process left SUBMITTING.
func (n *NCDStore) StaleSubmitting(ctx context.Context, before time.Time, limit int) ([]ncd.WorkRef, error) {
	return n.refs(ctx, `
		SELECT attempt_id::text, tenant_id FROM ncd_attempts
		WHERE state = 'SUBMITTING' AND submitted_at < $1
		ORDER BY submitted_at LIMIT $2`, before, limit)
}

// NoticesDue finds notices whose clock or record handoff needs attention.
func (n *NCDStore) NoticesDue(ctx context.Context, horizon time.Time, limit int) ([]ncd.WorkRef, error) {
	return n.refs(ctx, `
		SELECT DISTINCT notice_id::text, tenant_id FROM ncd_regulated_notices
		WHERE (deadline_at <= $1 AND state IN ('READY','DELIVERY_IN_PROGRESS','DELIVERY_EVIDENCED','ACK_PENDING','EXCEPTION'))
		   OR (record_requirement AND record_status = 'PENDING')
		LIMIT $2`, horizon, limit)
}

// FindAttempt maps a callback to the attempt it concerns, scoped to the
// binding that sent it (§7.5 "within tenant/provider scope").
func (n *NCDStore) FindAttempt(ctx context.Context, bindingID, token, providerMessageID string) (*ncd.WorkRef, error) {
	refs, err := n.refs(ctx, `
		SELECT attempt_id::text, tenant_id FROM ncd_attempts
		WHERE binding_id = $1
		  AND (($2 <> '' AND idempotency_token = $2) OR ($3 <> '' AND provider_message_id = $3))
		LIMIT 1`, bindingID, token, providerMessageID)
	if err != nil || len(refs) == 0 {
		return nil, err
	}
	return &refs[0], nil
}

// Reputation aggregates outcomes per tenant, stream and binding (§7.4).
func (n *NCDStore) Reputation(ctx context.Context, since time.Time) ([]ncd.ReputationRow, error) {
	var out []ncd.ReputationRow
	err := n.withPlatformScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH a AS (
				SELECT tenant_id, binding_id, attempt_id, state,
				       CASE purpose_class
				         WHEN 'SECURITY_CRITICAL' THEN 'CRITICAL'
				         WHEN 'REGULATED_RIGHTS_AFFECTING' THEN 'TRANSACTIONAL'
				         WHEN 'TRANSACTIONAL_RELATIONSHIP' THEN 'TRANSACTIONAL'
				         WHEN 'MARKETING_PROMOTIONAL' THEN 'MARKETING'
				         ELSE 'OPERATIONAL' END AS stream
				FROM ncd_attempts WHERE created_at >= $1
			), c AS (
				SELECT attempt_id, count(*) AS n FROM ncd_delivery_evidence
				WHERE evidence_type = 'COMPLAINT' AND received_at >= $1 AND attempt_id IS NOT NULL
				GROUP BY attempt_id
			)
			SELECT a.tenant_id, a.stream, a.binding_id, count(*)::int,
			       (count(*) FILTER (WHERE a.state = 'BOUNCED'))::int,
			       (count(*) FILTER (WHERE c.n > 0))::int,
			       (count(*) FILTER (WHERE a.state = 'FAILED'))::int
			FROM a LEFT JOIN c ON c.attempt_id = a.attempt_id
			GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ncd.ReputationRow
			if err := rows.Scan(&r.TenantID, &r.Stream, &r.BindingID, &r.Attempts, &r.HardBounces, &r.Complaints, &r.Failures); err != nil {
				return err
			}
			if r.Attempts > 0 {
				r.BounceRate = float64(r.HardBounces) / float64(r.Attempts)
				r.ComplaintRate = float64(r.Complaints) / float64(r.Attempts)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// circuitCooldown is how long an automatically opened circuit stays open
// before one trial submission is let through (half-open).
const circuitCooldown = 5 * time.Minute

// Bindings returns every provider binding with its health.
func (n *NCDStore) Bindings(ctx context.Context, now time.Time) ([]ncd.Binding, error) {
	rows, err := n.s.pool.Query(ctx, `
		SELECT b.binding_id, b.channel, b.provider_name, b.regions, b.evidence_capability, b.supports_receipts,
		       b.sender_identity, b.certified, b.failover_group, b.priority, b.cost_rank, COALESCE(b.callback_secret_env, ''),
		       b.status, COALESCE(h.state, 'HEALTHY'), COALESCE(h.reason, ''), COALESCE(h.changed_by, 'system'),
		       COALESCE(h.changed_at, now())
		FROM ncd_provider_bindings b LEFT JOIN ncd_binding_health h ON h.binding_id = b.binding_id
		ORDER BY b.channel, b.priority, b.binding_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ncd.Binding
	for rows.Next() {
		var b ncd.Binding
		var ev string
		var changedBy string
		var changedAt time.Time
		if err := rows.Scan(&b.BindingID, &b.Channel, &b.ProviderName, &b.Regions, &ev, &b.SupportsReceipts,
			&b.SenderIdentity, &b.Certified, &b.FailoverGroup, &b.Priority, &b.CostRank, &b.CallbackSecretEnv,
			&b.Status, &b.Health, &b.HealthReason, &changedBy, &changedAt); err != nil {
			return nil, err
		}
		b.EvidenceCapability = ncd.Level(ev)
		if b.Health == "CIRCUIT_OPEN" && changedBy == "system" && now.Sub(changedAt) >= circuitCooldown {
			b.Health = "HALF_OPEN"
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBindingHealth is the operator's circuit control.
func (n *NCDStore) SetBindingHealth(ctx context.Context, bindingID, state, reason, actor string) error {
	return n.withBindingAdmin(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE ncd_binding_health SET state = $2, reason = $3, changed_by = $4, changed_at = now(), consecutive_failures = 0
			WHERE binding_id = $1`, bindingID, state, reason, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ncd.ErrNotFound
		}
		return nil
	})
}

// RecordBindingResult feeds the automatic circuit breaker. An operator's
// open circuit is never closed by traffic; an automatic one closes on the
// first success after its cooldown.
func (n *NCDStore) RecordBindingResult(ctx context.Context, bindingID string, success bool, threshold int) error {
	return n.withBindingAdmin(ctx, func(tx pgx.Tx) error {
		if success {
			_, err := tx.Exec(ctx, `
				UPDATE ncd_binding_health SET consecutive_failures = 0,
				       reason = CASE WHEN changed_by = 'system' AND state = 'CIRCUIT_OPEN' THEN 'recovered' ELSE reason END,
				       changed_at = CASE WHEN changed_by = 'system' AND state = 'CIRCUIT_OPEN' THEN now() ELSE changed_at END,
				       state = CASE WHEN changed_by = 'system' THEN 'HEALTHY' ELSE state END
				WHERE binding_id = $1`, bindingID)
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE ncd_binding_health SET
			       reason = CASE WHEN consecutive_failures + 1 >= $2 AND (state = 'HEALTHY' OR changed_by = 'system')
			                     THEN (consecutive_failures + 1)::text || ' consecutive transport failures' ELSE reason END,
			       changed_at = CASE WHEN consecutive_failures + 1 >= $2 AND (state = 'HEALTHY' OR changed_by = 'system') THEN now() ELSE changed_at END,
			       changed_by = CASE WHEN consecutive_failures + 1 >= $2 AND (state = 'HEALTHY' OR changed_by = 'system') THEN 'system' ELSE changed_by END,
			       state = CASE WHEN consecutive_failures + 1 >= $2 THEN 'CIRCUIT_OPEN' ELSE state END,
			       consecutive_failures = consecutive_failures + 1
			WHERE binding_id = $1`, bindingID, threshold)
		return err
	})
}

// DeliverInApp places an NCD in-app notice in the recipient's register,
// idempotent on the attempt token (the row's correlation id).
func (n *NCDStore) DeliverInApp(ctx context.Context, tenantID string, nt domain.Notification) error {
	return n.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO notifications (
				notification_id, tenant_id, legal_entity_id, recipient_principal_id, channel, subject, body,
				status, source_event_type, source_reference, correlation_id, created_by_principal_id,
				created_at, sent_at, provider_response, delivery_attempts, last_attempt_at
			) VALUES ($1,$2,$3,$4,'IN_APP',$5,$6,'SENT',$7,$8,$9,$10,$11,$11,$12,1,$11)
			ON CONFLICT (tenant_id, correlation_id) WHERE idempotency_key IS NULL DO NOTHING`,
			nt.NotificationID, tenantID, nt.LegalEntityID, nt.RecipientPrincipalID, truncateRunes(nt.Subject, 255), nt.Body,
			nt.SourceEventType, nt.SourceReference, nt.CorrelationID, nt.CreatedByPrincipalID, nt.CreatedAt, nt.ProviderResponse)
		return err
	})
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ── error helpers ───────────────────────────────────────────────────────────

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// nf maps "no row" and a malformed uuid (22P02) to ncd.ErrNotFound: an id that
// cannot be a UUID names nothing.
func nf(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || pgCode(err) == "22P02" {
		return ncd.ErrNotFound
	}
	return err
}

// savepoint runs fn in a nested transaction so an expected constraint
// violation does not abort the caller's transaction.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// ncdTx implements ncd.Tx.
type ncdTx struct {
	ctx    context.Context
	tx     pgx.Tx
	tenant string
}

func (t *ncdTx) TenantID() string { return t.tenant }

func (t *ncdTx) Enqueue(out events.Outbound) error { return enqueue(t.ctx, t.tx, t.tenant, out) }
