// BNK-02's own persistence — consent/token/health lifecycle for bank
// connections. Kept separate from pg_store.go's original connection CRUD
// to keep this capability's own scope self-contained, same pattern used
// for every other capability added to an existing service in this build.
package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/banking-connector-svc/internal/domain"
)

const bnk02Columns = `
	connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at,
	bank_account_id, provider_ref, consent_scope, token_lease_ref, token_expires_at, health_status, region`

func scanConnection(row pgx.Row, c *domain.BankConnection) error {
	return row.Scan(&c.ConnectionID, &c.TenantID, &c.LegalEntityID, &c.BankName, &c.BIC, &c.AccountNumber, &c.Currency, &c.Status, &c.CreatedAt, &c.UpdatedAt,
		&c.BankAccountID, &c.ProviderRef, &c.ConsentScope, &c.TokenLeaseRef, &c.TokenExpiresAt, &c.HealthStatus, &c.Region)
}

func (p *PgStore) recordConnectionEvent(ctx context.Context, tx pgx.Tx, tenantID, connectionID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO bank_connection_events (event_id, connection_id, tenant_id, event_type, detail, actor_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		uuid.New().String(), connectionID, tenantID, eventType, detail, actorPrincipalID)
	return err
}

// InitiateConnection is BNK-02's real entry point — a request record in
// REQUESTED status, before any provider/user authorization has happened.
// Idempotent on (tenant_id, correlation_id).
func (p *PgStore) InitiateConnection(ctx context.Context, params domain.InitiateConnectionParams) (*domain.BankConnection, bool, error) {
	tenantID := params.TenantID
	created := false
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		requestedScope := params.RequestedScope
		if requestedScope == nil {
			requestedScope = []string{}
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_connections (
				connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status,
				bank_account_id, provider_ref, consent_scope, region, correlation_id, created_by_principal_id
			) VALUES ($1,$2,$3,$4,'','',$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id <> '' DO NOTHING
			RETURNING `+bnk02Columns,
			uuid.New().String(), tenantID, params.LegalEntityID, params.BankName, "USD", domain.ConnStatusRequested,
			params.BankAccountID, params.ProviderRef, requestedScope, params.Region, params.CorrelationID, params.CreatedByPrincipalID)
		if err := scanConnection(row, &c); err == nil {
			created = true
			return p.recordConnectionEvent(ctx, tx, tenantID, c.ConnectionID, domain.EventConnectionRequested, "", params.CreatedByPrincipalID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return scanConnection(tx.QueryRow(ctx, `SELECT `+bnk02Columns+` FROM bank_connections WHERE tenant_id=$1 AND correlation_id=$2`, tenantID, params.CorrelationID), &c)
	})
	if err != nil {
		return nil, false, err
	}
	return &c, created, nil
}

// notFoundOrInvalidConnection disambiguates a CAS zero-rows-updated
// result: either the connection doesn't exist (in this tenant), or it
// exists but isn't in a state that permits the attempted transition.
func (p *PgStore) notFoundOrInvalidConnection(ctx context.Context, connectionID string) error {
	c, err := p.GetConnectionByID(ctx, connectionID)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			return domain.ErrConnectionNotFound
		}
		return err
	}
	if c == nil {
		return domain.ErrConnectionNotFound
	}
	return domain.ErrInvalidConnectionTransition
}

func (p *PgStore) CompleteConnectionAuthorization(ctx context.Context, params domain.CompleteConnectionAuthorizationParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		grantedScope := params.GrantedScope
		if grantedScope == nil {
			grantedScope = []string{}
		}
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET status=$3, token_lease_ref=$4, token_expires_at=$5, consent_scope=$6, health_status='HEALTHY', updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('REQUESTED','AUTHORIZING')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, domain.ConnStatusAuthorizing, params.TokenLeaseRef, params.TokenExpiresAt, grantedScope)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionAuthorized, "", params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) ActivateConnection(ctx context.Context, params domain.ActivateConnectionParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		// token_lease_ref<>'' is defense-in-depth alongside the status
		// guard: ReconnectProvider clears it when re-entering AUTHORIZING
		// from RECONSENT_REQUIRED specifically so this can't succeed again
		// on stale consent until a real CompleteConnectionAuthorization
		// sets a fresh one.
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET status=$3, updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status='AUTHORIZING' AND token_lease_ref<>''
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, domain.ConnStatusActive)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionActivated, "", params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) RefreshConnection(ctx context.Context, params domain.RefreshConnectionParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET token_lease_ref=$3, token_expires_at=$4, health_status='HEALTHY', status=$5, updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('ACTIVE','DEGRADED')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, params.NewTokenLeaseRef, params.NewTokenExpiresAt, domain.ConnStatusActive)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionRefreshed, "", params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) TriggerReconsent(ctx context.Context, params domain.TriggerReconsentParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET status=$3, health_status='DEGRADED', updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('ACTIVE','DEGRADED','SUSPENDED')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, domain.ConnStatusReconsentRequired)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionReconsentTrigger, params.Reason, params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) SuspendConnection(ctx context.Context, params domain.SuspendConnectionParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET status=$3, suspend_reason=$4, updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('ACTIVE','DEGRADED')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, domain.ConnStatusSuspended, params.Reason)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionSuspended, params.Reason, params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

// RevokeConnection is the DB-enforced proof of the spec's own named
// negative path "revoked consent still used" — REVOKED is terminal
// (migration 003's reject_revoked_connection_mutation trigger blocks any
// further mutation, including a reactivation attempt), not just a status
// value a later call could flip back.
func (p *PgStore) RevokeConnection(ctx context.Context, params domain.RevokeConnectionParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET status=$3, revoke_reason=$4, token_lease_ref='', updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status <> 'REVOKED'
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, domain.ConnStatusRevoked, params.Reason)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionRevoked, params.Reason, params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

// ReconnectProvider's target status depends on WHY the connection needs
// reconnecting. DEGRADED/SUSPENDED are operational states — the provider
// session itself is still valid, so recovery goes straight back to ACTIVE,
// unchanged from before. RECONSENT_REQUIRED means the bank/provider is
// demanding fresh consent — jumping straight to ACTIVE from there would
// silently reactivate a connection whose authorization is no longer
// valid, the same class of gap the doc's "revoked consent cannot be
// silently reactivated" SoD rule targets for RevokeConnection. So
// RECONSENT_REQUIRED instead re-enters the real authorization step
// (-> AUTHORIZING), and only CompleteConnectionAuthorization/
// ActivateConnection — which actually capture a new token lease/granted
// scope — can bring it back to ACTIVE.
func (p *PgStore) ReconnectProvider(ctx context.Context, params domain.ReconnectProviderParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET
				status=CASE WHEN status='RECONSENT_REQUIRED' THEN 'AUTHORIZING' ELSE 'ACTIVE' END,
				-- Clearing the stale credential here (not just relabeling the
				-- state) is what actually prevents ActivateConnection from
				-- reactivating on old consent — see ActivateConnection's own
				-- token_lease_ref<>'' guard, and this function's doc comment.
				token_lease_ref=CASE WHEN status='RECONSENT_REQUIRED' THEN '' ELSE token_lease_ref END,
				token_expires_at=CASE WHEN status='RECONSENT_REQUIRED' THEN NULL ELSE token_expires_at END,
				health_status='HEALTHY', updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('DEGRADED','SUSPENDED','RECONSENT_REQUIRED')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionReconnected, "", params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) RotateConnectionCredential(ctx context.Context, params domain.RotateConnectionCredentialParams) (*domain.BankConnection, error) {
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_connections SET token_lease_ref=$3, token_expires_at=$4, updated_at=now()
			WHERE connection_id=$1 AND tenant_id=$2 AND status IN ('ACTIVE','DEGRADED')
			RETURNING `+bnk02Columns,
			params.ConnectionID, params.TenantID, params.NewTokenLeaseRef, params.NewTokenExpiresAt)
		if err := scanConnection(row, &c); err != nil {
			return err
		}
		return p.recordConnectionEvent(ctx, tx, params.TenantID, params.ConnectionID, domain.EventConnectionCredentialRotated, "", params.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, p.notFoundOrInvalidConnection(ctx, params.ConnectionID)
	}
	return &c, err
}

func (p *PgStore) ListConnectionEvents(ctx context.Context, tenantID, connectionID string) ([]domain.ConnectionEvent, error) {
	res := make([]domain.ConnectionEvent, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, connection_id, tenant_id, event_type, detail, actor_principal_id, created_at
			FROM bank_connection_events WHERE tenant_id=$1 AND connection_id=$2 ORDER BY created_at ASC`, tenantID, connectionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ConnectionEvent
			if err := rows.Scan(&e.EventID, &e.ConnectionID, &e.TenantID, &e.EventType, &e.Detail, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			res = append(res, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CreateRegionPolicy adds one allowed-region entry for a legal entity.
// idx_bank_region_policies_tenant_entity_region (migration 006) enforces
// uniqueness — a duplicate is rejected rather than silently accepted as a
// no-op, so a caller can tell "already allowed" apart from "just added."
func (p *PgStore) CreateRegionPolicy(ctx context.Context, params domain.CreateRegionPolicyParams) (*domain.BankRegionPolicy, error) {
	var rp domain.BankRegionPolicy
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_region_policies (policy_id, tenant_id, legal_entity_id, region, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING policy_id, tenant_id, legal_entity_id, region, created_by_principal_id, created_at`,
			uuid.New().String(), params.TenantID, params.LegalEntityID, params.Region, params.ActorPrincipalID)
		return row.Scan(&rp.PolicyID, &rp.TenantID, &rp.LegalEntityID, &rp.Region, &rp.CreatedByPrincipalID, &rp.CreatedAt)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.ErrRegionPolicyAlreadyExists
		}
		return nil, err
	}
	return &rp, nil
}

// IsRegionAllowed reports whether region is permitted for a legal entity.
// A legal entity with NO configured policy rows has no restriction at all
// — this returns true unconditionally in that case, so enforcement is
// opt-in per legal entity rather than retroactively deny-by-default for
// every legal entity that predates migration 006.
func (p *PgStore) IsRegionAllowed(ctx context.Context, tenantID, legalEntityID, region string) (bool, error) {
	var totalPolicies, matchingPolicies int
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM bank_region_policies WHERE tenant_id=$1 AND legal_entity_id=$2`, tenantID, legalEntityID).Scan(&totalPolicies); err != nil {
			return err
		}
		if totalPolicies == 0 {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM bank_region_policies WHERE tenant_id=$1 AND legal_entity_id=$2 AND region=$3`, tenantID, legalEntityID, region).Scan(&matchingPolicies)
	})
	if err != nil {
		return false, err
	}
	if totalPolicies == 0 {
		return true, nil
	}
	return matchingPolicies > 0, nil
}

// HasRegionPolicy reports whether a legal entity has any configured
// bank_region_policies rows at all — used only to make the fail-open
// "unrestricted" path in enforceRegionPolicy visible in telemetry (a
// deliberate product decision, not a bug: see IsRegionAllowed's own doc
// comment), never to change IsRegionAllowed's own enforcement decision.
func (p *PgStore) HasRegionPolicy(ctx context.Context, tenantID, legalEntityID string) (bool, error) {
	var total int
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM bank_region_policies WHERE tenant_id=$1 AND legal_entity_id=$2`, tenantID, legalEntityID).Scan(&total)
	})
	if err != nil {
		return false, err
	}
	return total > 0, nil
}
