package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

// EventService is the producing service name carried on every event.
const EventService = "connectivity-api-bridge-svc"

type PgStore struct {
	pool        *pgxpool.Pool
	eventRegion string
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by every event this store
// enqueues (config EVENT_RESIDENCY_REGION).
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func NewPgStore(pool *pgxpool.Pool, opts ...Option) *PgStore {
	s := &PgStore{pool: pool, eventRegion: "uk"}
	for _, o := range opts {
		o(s)
	}
	return s
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so the tenant_isolation_policy added in migration
// 002_add_rls.sql has a value to enforce against.
//
// The tenant is read from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a request body or query
// parameter. GetTenantID returns "" when absent rather than a fabricated
// default, and "" matches no real tenant_id — so a call that somehow
// arrives without a verified tenant sees and writes nothing, instead of
// operating on a synthetic "default-tenant" bucket shared with every other
// such caller.
func (p *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", middleware.GetTenantID(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ── Column lists ──────────────────────────────────────────────────────────────

const bridgeCols = `bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status,
	created_at, updated_at, purpose, provider_key, provider_profile_version, COALESCE(credential_ref, ''),
	credential_verified_at, consent_expires_at, destination_host, destination_region, sync_mode, scopes, version,
	config_fingerprint, approved_fingerprint, approved_version, created_by, last_changed_by, approved_by,
	approved_at, status_reason, legacy_unapproved`

func scanBridge(row pgx.Row, b *domain.ApiBridge) error {
	err := row.Scan(&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType,
		&b.Status, &b.CreatedAt, &b.UpdatedAt, &b.Purpose, &b.ProviderKey, &b.ProviderProfileVersion, &b.CredentialRef,
		&b.CredentialVerifiedAt, &b.ConsentExpiresAt, &b.DestinationHost, &b.DestinationRegion, &b.SyncMode, &b.Scopes,
		&b.Version, &b.ConfigFingerprint, &b.ApprovedFingerprint, &b.ApprovedVersion, &b.CreatedBy, &b.LastChangedBy,
		&b.ApprovedBy, &b.ApprovedAt, &b.StatusReason, &b.LegacyUnapproved)
	if err != nil {
		return err
	}
	if b.Scopes == nil {
		b.Scopes = []string{}
	}
	return nil
}

func (p *PgStore) insertConnector(ctx context.Context, tx pgx.Tx, b *domain.ApiBridge, idemKey, idemHash string) error {
	var key, hash any
	if idemKey != "" {
		key, hash = idemKey, idemHash
	}
	var cred any
	if b.CredentialRef != "" {
		cred = b.CredentialRef
	}
	if b.Scopes == nil {
		b.Scopes = []string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO api_bridges (bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status,
			created_at, updated_at, purpose, provider_key, provider_profile_version, credential_ref, credential_verified_at,
			consent_expires_at, destination_host, destination_region, sync_mode, scopes, version, config_fingerprint,
			approved_fingerprint, approved_version, created_by, last_changed_by, approved_by, approved_at, status_reason,
			legacy_unapproved, idempotency_key, idempotency_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)`,
		b.BridgeID, b.TenantID, b.LegalEntityID, b.BridgeName, b.Protocol, b.EndpointURL, b.AuthType, b.Status,
		b.CreatedAt, b.UpdatedAt, b.Purpose, b.ProviderKey, b.ProviderProfileVersion, cred, b.CredentialVerifiedAt,
		b.ConsentExpiresAt, b.DestinationHost, b.DestinationRegion, defaultStr(b.SyncMode, domain.SyncPush), b.Scopes, b.Version,
		b.ConfigFingerprint, b.ApprovedFingerprint, b.ApprovedVersion, b.CreatedBy, b.LastChangedBy, b.ApprovedBy,
		b.ApprovedAt, b.StatusReason, b.LegacyUnapproved, key, hash)
	return err
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *PgStore) prepareNew(ctx context.Context, b *domain.ApiBridge) {
	b.TenantID = middleware.GetTenantID(ctx)
	if b.BridgeID == "" {
		b.BridgeID = uuid.New().String()
	}
	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt = now, now
	if b.Status == "" {
		b.Status = domain.StatusDraft
	}
	if b.Version < 1 {
		b.Version = 1
	}
	if b.SyncMode == "" {
		b.SyncMode = domain.SyncPush
	}
	if b.DestinationHost == "" {
		b.DestinationHost = domain.EndpointHost(b.EndpointURL)
	}
}

// CreateBridge is the pre-governance entry point, kept for existing callers.
func (p *PgStore) CreateBridge(ctx context.Context, b *domain.ApiBridge) error {
	p.prepareNew(ctx, b)
	return p.withTenant(ctx, func(tx pgx.Tx) error { return p.insertConnector(ctx, tx, b, "", "") })
}

// GetBridgeByID looks up a bridge scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE bridge_id = $1`
// alone, so any caller holding (or guessing) a bridge_id could read
// another tenant's integration configuration — endpoint_url and auth_type
// included. Returns ErrBridgeNotFound rather than a distinct forbidden
// error, so a probe cannot confirm that another tenant's bridge_id exists.
func (p *PgStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	var b domain.ApiBridge
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanBridge(tx.QueryRow(ctx,
			`SELECT `+bridgeCols+` FROM api_bridges WHERE bridge_id = $1 AND tenant_id = $2`,
			id, middleware.GetTenantID(ctx)), &b)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBridgeNotFound
		}
		return nil, err
	}
	return &b, nil
}

// ListBridges lists the caller's own tenant's bridges, optionally narrowed
// to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's bridges. Narrowing within your own tenant is a legitimate
// choice; skipping the tenant dimension never is.
func (p *PgStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	res := make([]domain.ApiBridge, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+bridgeCols+` FROM api_bridges
			WHERE tenant_id = $2 AND ($1 = '' OR legal_entity_id = $1)
			ORDER BY created_at DESC`, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b domain.ApiBridge
			if err := scanBridge(rows, &b); err != nil {
				return err
			}
			res = append(res, b)
		}
		return rows.Err()
	})
	return res, err
}

func (p *PgStore) RecordIngestion(ctx context.Context, log *domain.IngestionLog) error {
	if log.LogID == "" {
		log.LogID = uuid.New().String()
	}
	log.IngestedAt = time.Now().UTC()
	log.TenantID = middleware.GetTenantID(ctx)
	return p.withTenant(ctx, func(tx pgx.Tx) error { return insertIngestion(ctx, tx, log) })
}

func insertIngestion(ctx context.Context, tx pgx.Tx, log *domain.IngestionLog) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ingestion_logs (log_id, bridge_id, tenant_id, payload_summary, ingestion_status, error_message, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		log.LogID, log.BridgeID, log.TenantID, log.PayloadSummary, log.IngestionStatus, log.ErrorMessage, log.IngestedAt)
	return err
}

// ListIngestionLogs is scoped to the caller's tenant.
func (p *PgStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	res := make([]domain.IngestionLog, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT log_id, bridge_id, tenant_id, payload_summary, ingestion_status, COALESCE(error_message, ''), ingested_at
			FROM ingestion_logs WHERE bridge_id = $1 AND tenant_id = $2 ORDER BY ingested_at DESC`,
			bridgeID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.IngestionLog
			if err := rows.Scan(&l.LogID, &l.BridgeID, &l.TenantID, &l.PayloadSummary, &l.IngestionStatus, &l.ErrorMessage, &l.IngestedAt); err != nil {
				return err
			}
			res = append(res, l)
		}
		return rows.Err()
	})
	return res, err
}

// ── Governance ────────────────────────────────────────────────────────────────

func (p *PgStore) CreateConnector(ctx context.Context, b *domain.ApiBridge, idemKey, idemHash string, fx *domain.Effects) (bool, error) {
	p.prepareNew(ctx, b)
	created := true
	attempt := func() error {
		created = true
		return p.withTenant(ctx, func(tx pgx.Tx) error {
			if idemKey != "" {
				var existing domain.ApiBridge
				var storedHash string
				err := tx.QueryRow(ctx, `SELECT `+bridgeCols+`, COALESCE(idempotency_hash, '') FROM api_bridges
					WHERE tenant_id = $1 AND idempotency_key = $2`, b.TenantID, idemKey).
					Scan(scanTargets(&existing, &storedHash)...)
				switch {
				case err == nil:
					if storedHash != idemHash {
						return domain.ErrIdempotencyConflict
					}
					if existing.Scopes == nil {
						existing.Scopes = []string{}
					}
					*b = existing
					created = false
					return nil
				case !errors.Is(err, pgx.ErrNoRows):
					return err
				}
			}
			if err := p.insertConnector(ctx, tx, b, idemKey, idemHash); err != nil {
				return err
			}
			return p.applyEffects(ctx, tx, b, nil, fx)
		})
	}
	err := attempt()
	if idemKey != "" && pgCode(err) == "23505" {
		// A concurrent request with the same key committed first. Its row is
		// now visible, so the lookup at the top of the retry finds it.
		err = attempt()
	}
	return created, err
}

func scanTargets(b *domain.ApiBridge, extra *string) []any {
	return []any{&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType,
		&b.Status, &b.CreatedAt, &b.UpdatedAt, &b.Purpose, &b.ProviderKey, &b.ProviderProfileVersion, &b.CredentialRef,
		&b.CredentialVerifiedAt, &b.ConsentExpiresAt, &b.DestinationHost, &b.DestinationRegion, &b.SyncMode, &b.Scopes,
		&b.Version, &b.ConfigFingerprint, &b.ApprovedFingerprint, &b.ApprovedVersion, &b.CreatedBy, &b.LastChangedBy,
		&b.ApprovedBy, &b.ApprovedAt, &b.StatusReason, &b.LegacyUnapproved, extra}
}

func (p *PgStore) MutateConnector(ctx context.Context, id string, fn domain.MutateFunc) (*domain.ApiBridge, error) {
	var out domain.ApiBridge
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var b domain.ApiBridge
		err := scanBridge(tx.QueryRow(ctx,
			`SELECT `+bridgeCols+` FROM api_bridges WHERE bridge_id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, middleware.GetTenantID(ctx)), &b)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBridgeNotFound
		}
		if err != nil {
			return err
		}
		health, err := p.readHealth(ctx, tx, id)
		if err != nil {
			return err
		}
		before := b
		before.Scopes = append([]string(nil), b.Scopes...)
		priorHealth := *health

		fx, err := fn(&b, health)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, b) {
			if err := p.updateConnector(ctx, tx, &b, before.Version); err != nil {
				return err
			}
		}
		if err := p.applyEffects(ctx, tx, &b, &priorHealth, fx); err != nil {
			return err
		}
		out = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgStore) updateConnector(ctx context.Context, tx pgx.Tx, b *domain.ApiBridge, fromVersion int64) error {
	var cred any
	if b.CredentialRef != "" {
		cred = b.CredentialRef
	}
	tag, err := tx.Exec(ctx, `
		UPDATE api_bridges SET
			bridge_name=$3, protocol=$4, endpoint_url=$5, auth_type=$6, status=$7, updated_at=$8, purpose=$9,
			provider_key=$10, provider_profile_version=$11, credential_ref=$12, credential_verified_at=$13,
			consent_expires_at=$14, destination_host=$15, destination_region=$16, sync_mode=$17, scopes=$18, version=$19,
			config_fingerprint=$20, approved_fingerprint=$21, approved_version=$22, last_changed_by=$23, approved_by=$24,
			approved_at=$25, status_reason=$26, legacy_unapproved=$27
		WHERE bridge_id=$1 AND tenant_id=$2 AND version=$28`,
		b.BridgeID, b.TenantID, b.BridgeName, b.Protocol, b.EndpointURL, b.AuthType, b.Status, b.UpdatedAt, b.Purpose,
		b.ProviderKey, b.ProviderProfileVersion, cred, b.CredentialVerifiedAt, b.ConsentExpiresAt, b.DestinationHost,
		b.DestinationRegion, b.SyncMode, b.Scopes, b.Version, b.ConfigFingerprint, b.ApprovedFingerprint, b.ApprovedVersion,
		b.LastChangedBy, b.ApprovedBy, b.ApprovedAt, b.StatusReason, b.LegacyUnapproved, fromVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		// The row is locked FOR UPDATE, so this is a bug, not a race.
		return fmt.Errorf("connector %s changed under its own lock", b.BridgeID)
	}
	return nil
}

func (p *PgStore) readHealth(ctx context.Context, tx pgx.Tx, id string) (*domain.Health, error) {
	h := &domain.Health{ConnectorID: id, TenantID: middleware.GetTenantID(ctx), State: domain.HealthHealthy}
	err := tx.QueryRow(ctx, `
		SELECT state, reason_code, detail, consecutive_failures, last_checked_at, last_success_at, updated_at
		FROM connector_health WHERE connector_id = $1 AND tenant_id = $2`, id, h.TenantID).
		Scan(&h.State, &h.ReasonCode, &h.Detail, &h.ConsecutiveFailures, &h.LastCheckedAt, &h.LastSuccessAt, &h.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, nil
	}
	return h, err
}

// applyEffects writes the evidence rows and outbox events a mutation produced,
// in the caller's transaction.
func (p *PgStore) applyEffects(ctx context.Context, tx pgx.Tx, b *domain.ApiBridge, priorHealth *domain.Health, fx *domain.Effects) error {
	if fx == nil {
		return nil
	}
	now := time.Now().UTC()
	if c := fx.Change; c != nil {
		_, err := tx.Exec(ctx, `
			INSERT INTO connector_changes (change_id, connector_id, tenant_id, from_version, to_version, change_kind, changed_by,
				changed_fields, scopes_added, scopes_removed, prior_fingerprint, new_fingerprint, reason, changed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			uuid.New().String(), b.BridgeID, b.TenantID, c.FromVersion, c.ToVersion, c.Kind, c.ChangedBy,
			nz(c.ChangedFields), nz(c.ScopesAdded), nz(c.ScopesRemoved), c.PriorFingerprint, c.NewFingerprint, c.Reason, now)
		if err != nil {
			return fmt.Errorf("record connector change: %w", err)
		}
	}
	if a := fx.Approval; a != nil {
		_, err := tx.Exec(ctx, `
			INSERT INTO connector_approvals (approval_id, connector_id, tenant_id, connector_version, fingerprint, decision,
				requested_by, decided_by, reason, decided_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			uuid.New().String(), b.BridgeID, b.TenantID, a.ConnectorVersion, a.Fingerprint, a.Decision,
			a.RequestedBy, a.DecidedBy, a.Reason, now)
		if err != nil {
			return fmt.Errorf("record connector approval: %w", err)
		}
	}
	if h := fx.Health; h != nil {
		if err := p.writeHealth(ctx, tx, b, priorHealth, h, now); err != nil {
			return err
		}
	}
	if l := fx.Ingestion; l != nil {
		if l.LogID == "" {
			l.LogID = uuid.New().String()
		}
		l.TenantID, l.BridgeID, l.IngestedAt = b.TenantID, b.BridgeID, now
		if err := insertIngestion(ctx, tx, l); err != nil {
			return fmt.Errorf("record ingestion: %w", err)
		}
	}
	for _, ev := range fx.Events {
		if ev.AggregateID == "" {
			ev.AggregateID = b.BridgeID
		}
		if ev.LegalEntityID == "" {
			ev.LegalEntityID = b.LegalEntityID
		}
		if ev.AggregateVersion == nil {
			v := b.Version
			ev.AggregateVersion = &v
		}
		if err := p.enqueue(ctx, tx, b.TenantID, ev); err != nil {
			return err
		}
	}
	return nil
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (p *PgStore) writeHealth(ctx context.Context, tx pgx.Tx, b *domain.ApiBridge, prior *domain.Health, h *domain.HealthUpdate, now time.Time) error {
	from := domain.HealthHealthy
	if prior != nil {
		from = prior.State
	}
	var lastSuccess any
	if h.Success {
		lastSuccess = now
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO connector_health (connector_id, tenant_id, state, reason_code, detail, consecutive_failures,
			last_checked_at, last_success_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$7)
		ON CONFLICT (connector_id) DO UPDATE SET state = EXCLUDED.state, reason_code = EXCLUDED.reason_code,
			detail = EXCLUDED.detail, consecutive_failures = EXCLUDED.consecutive_failures,
			last_checked_at = EXCLUDED.last_checked_at,
			last_success_at = COALESCE(EXCLUDED.last_success_at, connector_health.last_success_at),
			updated_at = EXCLUDED.updated_at`,
		b.BridgeID, b.TenantID, h.State, h.ReasonCode, h.Detail, h.ConsecutiveFailures, now, lastSuccess)
	if err != nil {
		return fmt.Errorf("record connector health: %w", err)
	}
	if from != h.State {
		_, err = tx.Exec(ctx, `
			INSERT INTO connector_health_transitions (transition_id, connector_id, tenant_id, from_state, to_state, reason_code, detail, at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			uuid.New().String(), b.BridgeID, b.TenantID, from, h.State, h.ReasonCode, h.Detail, now)
		if err != nil {
			return fmt.Errorf("record health transition: %w", err)
		}
	}
	return nil
}

// BuildEnvelope validates and builds the canonical envelope for an event. Both
// stores call it, so the in-memory store refuses exactly what Postgres would.
func BuildEnvelope(region, tenantID string, ev domain.OutboxEvent) (*envelope.Envelope, error) {
	aggType := ev.AggregateType
	if aggType == "" {
		aggType = "connector"
	}
	data := ev.Data
	if data == nil {
		data = map[string]any{} // the envelope requires a JSON object; an event with nothing to say still says it happened
	}
	return envelope.New(envelope.Spec{
		Type:             "com.zoikosuite.connectivity." + ev.Type,
		LegacyType:       ev.LegacyType,
		Service:          EventService,
		SchemaVersion:    "1.0.0",
		OccurredAt:       time.Now().UTC(),
		TenantID:         tenantID,
		LegalEntityID:    ev.LegalEntityID,
		AggregateType:    aggType,
		AggregateID:      ev.AggregateID,
		AggregateVersion: ev.AggregateVersion,
		CorrelationID:    ev.CorrelationID,
		ActorID:          ev.ActorID,
		ResidencyRegion:  region,
		// Connector facts name external systems and approvals; none carries a
		// secret or a payload body.
		Classification: envelope.Confidential,
		Data:           data,
	})
}

// enqueue writes a domain event to the outbox in the caller's transaction.
func (p *PgStore) enqueue(ctx context.Context, tx pgx.Tx, tenantID string, ev domain.OutboxEvent) error {
	env, err := BuildEnvelope(p.eventRegion, tenantID, ev)
	if err != nil {
		return fmt.Errorf("build %s event: %w", ev.Type, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", ev.Type, err)
	}
	return nil
}
