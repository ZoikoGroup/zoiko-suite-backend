// Package store provides the PostgreSQL implementation of
// kill-switch-registry-svc's persistence layer.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/kill-switch-registry-svc/internal/domain"
	svcmiddleware "zoiko.io/kill-switch-registry-svc/internal/middleware"
)

type Store interface {
	// AppendEvent inserts a new ENGAGE/DISENGAGE record. Never an UPDATE —
	// the append-only doctrine is enforced here, not just documented.
	AppendEvent(ctx context.Context, e *domain.KillSwitchEvent) error

	// LatestEventForScope returns the most recent event for the EXACT
	// scope tuple given (nil means that dimension is nil in the row too —
	// no fallback matching). Returns nil, nil if no event exists yet for
	// this exact tuple.
	LatestEventForScope(ctx context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchEvent, error)

	// ResolveKillSwitch checks every candidate scope tuple that could apply
	// to the given request (any combination of the four dimensions being
	// nil in a stored row means "not scoped along that dimension", so it
	// always matches), and returns the most specific one whose latest
	// event is ENGAGE, if any.
	ResolveKillSwitch(ctx context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchResolution, error)

	// ListCurrentStates returns the latest event for every distinct scope
	// tuple that has ever had an event — the "visible in operations" list.
	ListCurrentStates(ctx context.Context) ([]domain.KillSwitchState, error)

	// ListHistoryForScope returns every event ever recorded for the EXACT
	// scope tuple given, newest first — the audit trail for one switch.
	ListHistoryForScope(ctx context.Context, plane, domainName, providerCode, tenantID *string) ([]domain.KillSwitchEvent, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so migration 000002's policy has a value to enforce
// against.
//
// A transaction is required rather than incidental: set_config's third
// argument is is_local, and only a transaction-local setting is safe on a
// pooled connection. Setting it session-wide would leak one request's
// tenant into whichever request acquires that connection next.
//
// The tenant comes from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a query parameter, which is
// where ResolveKillSwitch and ListHistoryForScope used to take it. It
// returns "" when absent, and "" is meaningful HERE rather than merely
// fail-closed: under the policy in migration 000002, "" matches no
// tenant-specific row but still matches every tenant_id IS NULL row. That
// is exactly right for a platform-level resolution — see the policy's own
// header for why hiding those rows would be a silent safety bypass.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const eventColumns = `
	kill_switch_event_id, plane, domain, provider_code, tenant_id,
	action, reason, reconciliation_procedure_ref,
	approved_by_principal_id, created_at, created_by_principal_id`

func scanEvent(row pgx.Row) (*domain.KillSwitchEvent, error) {
	e := &domain.KillSwitchEvent{}
	var action string
	err := row.Scan(
		&e.KillSwitchEventID, &e.Plane, &e.Domain, &e.ProviderCode, &e.TenantID,
		&action, &e.Reason, &e.ReconciliationProcedureRef,
		&e.ApprovedByPrincipalID, &e.CreatedAt, &e.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	e.Action = domain.KillSwitchAction(action)
	return e, nil
}

// AppendEvent records an ENGAGE or DISENGAGE. Never an UPDATE — the
// append-only doctrine is structural.
//
// Note what RLS does and does not guard here. A tenant-scoped event must
// belong to the caller's tenant (WITH CHECK enforces that). A PLATFORM-WIDE
// event — tenant_id NULL — passes WITH CHECK for any caller, because the
// policy must keep the IS NULL branch for reads. The control on that path
// is the handler's authorization at platform scope, not this. See migration
// 000002's header, which says so at length rather than leaving it implied.
func (s *PgStore) AppendEvent(ctx context.Context, e *domain.KillSwitchEvent) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kill_switch_events (
				kill_switch_event_id, plane, domain, provider_code, tenant_id,
				action, reason, reconciliation_procedure_ref,
				approved_by_principal_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, e.KillSwitchEventID, e.Plane, e.Domain, e.ProviderCode, e.TenantID,
			string(e.Action), e.Reason, e.ReconciliationProcedureRef,
			e.ApprovedByPrincipalID, e.CreatedAt, e.CreatedByPrincipalID,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert kill switch event: %w", err)
	}
	return nil
}

// scopeMatch builds the "IS NULL OR = $n" clause pattern used by every
// exact-scope query in this file — a stored row's dimension is nil OR it
// equals the request's value for that dimension. When the request's own
// value for a dimension is nil, only rows with that dimension also nil can
// match (handled by passing the nil-safe equality directly).
func (s *PgStore) LatestEventForScope(ctx context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchEvent, error) {
	const query = `
		SELECT ` + eventColumns + `
		FROM kill_switch_events
		WHERE plane IS NOT DISTINCT FROM $1
		  AND domain IS NOT DISTINCT FROM $2
		  AND provider_code IS NOT DISTINCT FROM $3
		  AND tenant_id IS NOT DISTINCT FROM $4::uuid
		ORDER BY event_seq DESC
		LIMIT 1;`

	var e *domain.KillSwitchEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanEvent(tx.QueryRow(ctx, query, plane, domainName, providerCode, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest event for scope: %w", err)
	}
	return e, nil
}

// ResolveKillSwitch finds every stored scope tuple that is COMPATIBLE with
// the request (a stored dimension of NULL always matches; a stored
// non-NULL dimension must equal the request's value for that dimension —
// a request with a NIL value for a dimension therefore only matches rows
// that are ALSO nil there, since a specific incident check should never be
// widened by an unset request field), takes each matching tuple's latest
// event, and returns the most specific one that is currently ENGAGED.
// "Most specific" = most non-NULL dimensions on the stored row.
func (s *PgStore) ResolveKillSwitch(ctx context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchResolution, error) {
	const query = `
		WITH latest AS (
			SELECT DISTINCT ON (plane, domain, provider_code, tenant_id)
				kill_switch_event_id, plane, domain, provider_code, tenant_id,
				action, reason, reconciliation_procedure_ref,
				approved_by_principal_id, created_at, created_by_principal_id
			FROM kill_switch_events
			WHERE (plane IS NULL OR plane = $1)
			  AND (domain IS NULL OR domain = $2)
			  AND (provider_code IS NULL OR provider_code = $3)
			  AND (tenant_id IS NULL OR tenant_id = $4::uuid)
			ORDER BY plane, domain, provider_code, tenant_id, event_seq DESC
		)
		SELECT ` + eventColumns + `
		FROM latest
		WHERE action = 'ENGAGE'
		ORDER BY
			(plane IS NOT NULL)::int + (domain IS NOT NULL)::int
			+ (provider_code IS NOT NULL)::int + (tenant_id IS NOT NULL)::int DESC,
			created_at DESC
		LIMIT 1;`

	var e *domain.KillSwitchEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanEvent(tx.QueryRow(ctx, query, plane, domainName, providerCode, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.KillSwitchResolution{Blocked: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve kill switch: %w", err)
	}
	return &domain.KillSwitchResolution{Blocked: true, MatchedEvent: e}, nil
}

// ListCurrentStates returns the latest event for every distinct scope
// tuple ever recorded — the full operations-visibility view, regardless
// of current action (engaged or disengaged).
func (s *PgStore) ListCurrentStates(ctx context.Context) ([]domain.KillSwitchState, error) {
	const query = `
		SELECT DISTINCT ON (plane, domain, provider_code, tenant_id)
			plane, domain, provider_code, tenant_id, action, reason, created_at
		FROM kill_switch_events
		ORDER BY plane, domain, provider_code, tenant_id, event_seq DESC;`

	var out []domain.KillSwitchState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var st domain.KillSwitchState
			var action string
			if err := rows.Scan(&st.Plane, &st.Domain, &st.ProviderCode, &st.TenantID, &action, &st.Reason, &st.LatestEventAt); err != nil {
				return err
			}
			st.Action = domain.KillSwitchAction(action)
			out = append(out, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list current states: %w", err)
	}
	return out, nil
}

func (s *PgStore) ListHistoryForScope(ctx context.Context, plane, domainName, providerCode, tenantID *string) ([]domain.KillSwitchEvent, error) {
	const query = `
		SELECT ` + eventColumns + `
		FROM kill_switch_events
		WHERE plane IS NOT DISTINCT FROM $1
		  AND domain IS NOT DISTINCT FROM $2
		  AND provider_code IS NOT DISTINCT FROM $3
		  AND tenant_id IS NOT DISTINCT FROM $4::uuid
		ORDER BY event_seq DESC;`

	var out []domain.KillSwitchEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, plane, domainName, providerCode, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list history for scope: %w", err)
	}
	return out, nil
}
