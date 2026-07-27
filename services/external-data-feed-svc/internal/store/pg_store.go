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
	_, err := p.pool.Exec(ctx, query, sub.FeedID, sub.TenantID, sub.LegalEntityID, sub.Provider, sub.FeedType, sub.Symbol, sub.Status, sub.CreatedAt, sub.UpdatedAt)
	return err
}

func (p *PgStore) GetSubscriptionByID(ctx context.Context, id string) (*domain.DataFeedSubscription, error) {
	query := `
		SELECT feed_id, tenant_id, legal_entity_id, provider, feed_type, COALESCE(symbol,''), status, created_at, updated_at
		FROM data_feed_subscriptions WHERE feed_id = $1
	`
	var sub domain.DataFeedSubscription
	err := p.pool.QueryRow(ctx, query, id).Scan(&sub.FeedID, &sub.TenantID, &sub.LegalEntityID, &sub.Provider, &sub.FeedType, &sub.Symbol, &sub.Status, &sub.CreatedAt, &sub.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFeedNotFound
		}
		return nil, err
	}
	return &sub, nil
}

func (p *PgStore) ListSubscriptions(ctx context.Context, legalEntityID string) ([]domain.DataFeedSubscription, error) {
	query := `
		SELECT feed_id, tenant_id, legal_entity_id, provider, feed_type, COALESCE(symbol,''), status, created_at, updated_at
		FROM data_feed_subscriptions
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := make([]domain.DataFeedSubscription, 0)
	for rows.Next() {
		var sub domain.DataFeedSubscription
		if err := rows.Scan(&sub.FeedID, &sub.TenantID, &sub.LegalEntityID, &sub.Provider, &sub.FeedType, &sub.Symbol, &sub.Status, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, sub)
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
	_, err := p.pool.Exec(ctx, query, event.EventID, event.FeedID, event.TenantID, event.EventType, payloadBytes, event.ReceivedAt)
	return err
}

func (p *PgStore) ListEvents(ctx context.Context, feedID string) ([]domain.DataFeedEvent, error) {
	query := `
		SELECT event_id, feed_id, tenant_id, event_type, payload, received_at
		FROM data_feed_events
		WHERE ($1 = '' OR feed_id = $1)
		ORDER BY received_at DESC
		LIMIT 500
	`
	rows, err := p.pool.Query(ctx, query, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := make([]domain.DataFeedEvent, 0)
	for rows.Next() {
		var ev domain.DataFeedEvent
		var payloadRaw []byte
		if err := rows.Scan(&ev.EventID, &ev.FeedID, &ev.TenantID, &ev.EventType, &payloadRaw, &ev.ReceivedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(payloadRaw, &ev.Payload)
		res = append(res, ev)
	}
	return res, nil
}
