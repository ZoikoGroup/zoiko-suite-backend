package domain

import (
	"errors"
	"time"
)

var (
	ErrFeedNotFound  = errors.New("data feed subscription not found")
	ErrEventNotFound = errors.New("data feed event not found")
)

const (
	FeedTypeMarketData  = "MARKET_DATA"
	FeedTypeCreditScore = "CREDIT_SCORE"
	FeedTypeCompanyInfo = "COMPANY_INFO"
	FeedTypeFXRate      = "FX_RATE"
	FeedTypeESG         = "ESG_DATA"

	FeedStatusActive   = "ACTIVE"
	FeedStatusInactive = "INACTIVE"
)

type DataFeedSubscription struct {
	FeedID        string    `json:"feed_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Provider      string    `json:"provider"`
	FeedType      string    `json:"feed_type"`
	Symbol        string    `json:"symbol,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type DataFeedEvent struct {
	EventID   string                 `json:"event_id"`
	FeedID    string                 `json:"feed_id"`
	TenantID  string                 `json:"tenant_id"`
	EventType string                 `json:"event_type"`
	Payload   map[string]interface{} `json:"payload"`
	ReceivedAt time.Time             `json:"received_at"`
}

type CreateSubscriptionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Provider      string `json:"provider"`
	FeedType      string `json:"feed_type"`
	Symbol        string `json:"symbol"`
}

type IngestEventRequest struct {
	FeedID    string                 `json:"feed_id"`
	EventType string                 `json:"event_type"`
	Payload   map[string]interface{} `json:"payload"`
}
