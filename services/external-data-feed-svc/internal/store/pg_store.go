package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
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

func (p *PgStore) CreateSubscription(ctx context.Context, sub *domain.DataFeedSubscription) error {
	tenantID := middleware.GetTenantID(ctx)
	if sub.FeedID == "" {
		sub.FeedID = uuid.New().String()
	}
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	sub.TenantID = tenantID

	query := `
		INSERT INTO data_feed_subscriptions (feed_id, tenant_id, legal_entity_id, provider, feed_type, symbol, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, sub.FeedID, sub.TenantID, sub.LegalEntityID, sub.Provider, sub.FeedType, sub.Symbol, sub.Status, sub.CreatedAt, sub.UpdatedAt)
		return err
	})
}

// GetSubscriptionByID looks up a subscription scoped to the caller's
// tenant.
//
// The tenant predicate is new. This query was `WHERE feed_id = $1` alone,
// so any caller holding (or guessing) a feed_id could read another
// tenant's provider, feed_type and symbol — i.e. which external data
// sources that tenant pays for and what instruments it tracks. Returns
// ErrFeedNotFound rather than a distinct forbidden error, so a probe
// cannot confirm that another tenant's feed_id exists.
func (p *PgStore) GetSubscriptionByID(ctx context.Context, id string) (*domain.DataFeedSubscription, error) {
	query := `
		SELECT feed_id, tenant_id, legal_entity_id, provider, feed_type, COALESCE(symbol,''), status, created_at, updated_at
		FROM data_feed_subscriptions
		WHERE feed_id = $1
		  AND tenant_id = $2
	`
	var sub domain.DataFeedSubscription
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&sub.FeedID, &sub.TenantID, &sub.LegalEntityID, &sub.Provider, &sub.FeedType, &sub.Symbol, &sub.Status, &sub.CreatedAt, &sub.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFeedNotFound
		}
		return nil, err
	}
	return &sub, nil
}

// ListSubscriptions lists the caller's own tenant's subscriptions,
// optionally narrowed to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's subscriptions.
func (p *PgStore) ListSubscriptions(ctx context.Context, legalEntityID string) ([]domain.DataFeedSubscription, error) {
	query := `
		SELECT feed_id, tenant_id, legal_entity_id, provider, feed_type, COALESCE(symbol,''), status, created_at, updated_at
		FROM data_feed_subscriptions
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.DataFeedSubscription, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sub domain.DataFeedSubscription
			if err := rows.Scan(&sub.FeedID, &sub.TenantID, &sub.LegalEntityID, &sub.Provider, &sub.FeedType, &sub.Symbol, &sub.Status, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
				return err
			}
			res = append(res, sub)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *PgStore) IngestEvent(ctx context.Context, event *domain.DataFeedEvent) error {
	tenantID := middleware.GetTenantID(ctx)
	if event.EventID == "" {
		event.EventID = uuid.New().String()
	}
	event.ReceivedAt = time.Now().UTC()
	event.TenantID = tenantID

	payloadBytes, _ := json.Marshal(event.Payload)
	query := `
		INSERT INTO data_feed_events (event_id, feed_id, tenant_id, event_type, payload, received_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, event.EventID, event.FeedID, event.TenantID, event.EventType, payloadBytes, event.ReceivedAt)
		return err
	})
}

// ListEvents lists feed events for the caller's tenant, optionally
// narrowed to one feed.
//
// The tenant predicate is new, and this method's self-disabling filter was
// the worst of the three: the ONLY predicate was `($1 = '' OR feed_id =
// $1)` on feed_id itself, so calling it with no feed_id returned up to 500
// of EVERY tenant's feed events — including the payload JSONB, i.e. the
// actual market/credit/company data flowing through their subscriptions.
// The feed_id dimension stays optional; the tenant dimension never is.
func (p *PgStore) ListEvents(ctx context.Context, feedID string) ([]domain.DataFeedEvent, error) {
	query := `
		SELECT event_id, feed_id, tenant_id, event_type, payload, received_at
		FROM data_feed_events
		WHERE tenant_id = $2
		  AND ($1 = '' OR feed_id = $1)
		ORDER BY received_at DESC
		LIMIT 500
	`
	res := make([]domain.DataFeedEvent, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, feedID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev domain.DataFeedEvent
			var payloadRaw []byte
			if err := rows.Scan(&ev.EventID, &ev.FeedID, &ev.TenantID, &ev.EventType, &payloadRaw, &ev.ReceivedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(payloadRaw, &ev.Payload)
			res = append(res, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
