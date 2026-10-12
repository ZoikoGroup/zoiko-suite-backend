// Package consumer holds the Kafka consumers of jurisdiction-rules-svc.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/telemetry"
)

// Wire names on tenant-entity-registry-svc's zoiko.entity.events topic
// (its internal/events). Doc 03 §8.2 names the same two.
const (
	EventEntityCreated             = "entity.created"
	EventEntityJurisdictionChanged = "entity.jurisdiction.changed"

	changeAssigned = "ASSIGNED"
	changeEndDated = "END_DATED"
)

// ApplicabilityStore is the consumer's whole view of the store: one write.
type ApplicabilityStore interface {
	ApplyEntityApplicability(ctx context.Context, eventID, eventType string, a domain.EntityJurisdictionApplicability) (bool, error)
}

// EntityConsumer projects entity.created (the entity's primary jurisdiction)
// and entity.jurisdiction.changed (assignments ASSIGNED / END_DATED) into
// entity_jurisdiction_applicability.
type EntityConsumer struct {
	log     *zap.Logger
	store   ApplicabilityStore
	metrics *telemetry.Metrics
}

// WithMetrics counts every consumed event by result, so a projection that has
// stopped applying (retry climbing, applied flat) can be alerted on.
func (c *EntityConsumer) WithMetrics(m *telemetry.Metrics) *EntityConsumer {
	c.metrics = m
	return c
}

func (c *EntityConsumer) count(eventType, result string) {
	if c.metrics != nil {
		c.metrics.EntityEventsTotal.WithLabelValues(eventType, result).Inc()
	}
}

func NewEntityConsumer(log *zap.Logger, st ApplicabilityStore) *EntityConsumer {
	return &EntityConsumer{log: log, store: st}
}

// inbound is the producer's envelope (tenant-entity-registry-svc
// internal/events envelope), the fields this consumer reads.
type inbound struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	ObjectID      string          `json:"object_id"`
	ObjectVersion int64           `json:"object_version"`
	EffectiveAt   time.Time       `json:"effective_at"`
	EvidenceRef   string          `json:"evidence_ref"`
	Payload       json.RawMessage `json:"payload"`
}

type entityCreatedPayload struct {
	TenantID              string `json:"tenant_id"`
	LegalEntityID         string `json:"legal_entity_id"`
	PrimaryJurisdictionID string `json:"primary_jurisdiction_id"`
}

type jurisdictionChangedPayload struct {
	LegalEntityID  string     `json:"legal_entity_id"`
	AssignmentID   string     `json:"assignment_id"`
	JurisdictionID string     `json:"jurisdiction_id"`
	AssignmentType string     `json:"assignment_type"`
	ChangeType     string     `json:"change_type"`
	EffectiveFrom  time.Time  `json:"effective_from"`
	EffectiveTo    *time.Time `json:"effective_to"`
}

// Handle applies one message. It returns an error only when the message must
// be retried (the store is unavailable). An event this consumer does not
// handle, or one that can never be applied, returns nil so the offset moves.
func (c *EntityConsumer) Handle(ctx context.Context, raw []byte) error {
	var env inbound
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("entity event: undecodable envelope — skipped", zap.Error(err), zap.Int("bytes", len(raw)))
		return nil
	}
	if env.EventType != EventEntityCreated && env.EventType != EventEntityJurisdictionChanged {
		return nil
	}

	a, err := project(env)
	if err != nil {
		c.log.Error("entity event: cannot be applied — skipped",
			zap.String("event_id", env.EventID), zap.String("event_type", env.EventType), zap.Error(err))
		c.count(env.EventType, "skipped")
		return nil
	}
	if a == nil {
		return nil
	}

	applied, err := c.store.ApplyEntityApplicability(ctx, env.EventID, env.EventType, *a)
	switch {
	case errors.Is(err, domain.ErrInvalidEvent):
		c.log.Error("entity event: rejected by the store — skipped",
			zap.String("event_id", env.EventID), zap.Error(err))
		c.count(env.EventType, "skipped")
		return nil
	case err != nil:
		c.count(env.EventType, "retry")
		return err
	}
	if applied {
		c.count(env.EventType, "applied")
	} else {
		c.count(env.EventType, "duplicate")
	}
	c.log.Debug("entity event consumed",
		zap.String("event_id", env.EventID), zap.String("event_type", env.EventType),
		zap.String("applicability_id", a.ApplicabilityID), zap.Bool("applied", applied))
	return nil
}

// project maps an envelope to the applicability row it implies. A nil row
// with a nil error means the event says nothing about applicability.
func project(env inbound) (*domain.EntityJurisdictionApplicability, error) {
	if env.EventID == "" {
		return nil, fmt.Errorf("%w: no event_id to de-duplicate on", domain.ErrInvalidEvent)
	}
	// The tenant comes only from the envelope: a row with a guessed tenant
	// would answer another tenant's queries.
	if _, err := uuid.Parse(env.TenantID); err != nil {
		return nil, fmt.Errorf("%w: tenant_id %q", domain.ErrInvalidEvent, env.TenantID)
	}
	version := env.ObjectVersion
	if version < 1 {
		version = 1
	}

	switch env.EventType {
	case EventEntityCreated:
		var p entityCreatedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("%w: payload: %v", domain.ErrInvalidEvent, err)
		}
		if p.TenantID != "" && p.TenantID != env.TenantID {
			return nil, fmt.Errorf("%w: payload tenant %q differs from envelope tenant %q", domain.ErrInvalidEvent, p.TenantID, env.TenantID)
		}
		entity := firstNonEmpty(p.LegalEntityID, env.LegalEntityID)
		if entity == "" {
			return nil, fmt.Errorf("%w: no legal_entity_id", domain.ErrInvalidEvent)
		}
		if strings.TrimSpace(p.PrimaryJurisdictionID) == "" {
			return nil, nil
		}
		if _, err := uuid.Parse(p.PrimaryJurisdictionID); err != nil {
			return nil, fmt.Errorf("%w: primary_jurisdiction_id %q", domain.ErrInvalidEvent, p.PrimaryJurisdictionID)
		}
		if env.EffectiveAt.IsZero() {
			return nil, fmt.Errorf("%w: no effective_at", domain.ErrInvalidEvent)
		}
		return &domain.EntityJurisdictionApplicability{
			ApplicabilityID:     "primary:" + entity,
			TenantID:            env.TenantID,
			LegalEntityID:       entity,
			JurisdictionID:      p.PrimaryJurisdictionID,
			ApplicabilityType:   domain.ApplicabilityEntityPrimary,
			EffectiveFrom:       env.EffectiveAt,
			SourceObjectVersion: version,
		}, nil

	case EventEntityJurisdictionChanged:
		var p jurisdictionChangedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("%w: payload: %v", domain.ErrInvalidEvent, err)
		}
		entity := firstNonEmpty(p.LegalEntityID, env.LegalEntityID)
		assignment := firstNonEmpty(p.AssignmentID, env.ObjectID)
		if entity == "" || assignment == "" {
			return nil, fmt.Errorf("%w: legal_entity_id and assignment_id are required", domain.ErrInvalidEvent)
		}
		if _, err := uuid.Parse(p.JurisdictionID); err != nil {
			return nil, fmt.Errorf("%w: jurisdiction_id %q", domain.ErrInvalidEvent, p.JurisdictionID)
		}
		if p.EffectiveFrom.IsZero() {
			return nil, fmt.Errorf("%w: no effective_from", domain.ErrInvalidEvent)
		}
		switch p.ChangeType {
		case changeAssigned:
		case changeEndDated:
			if p.EffectiveTo == nil {
				return nil, fmt.Errorf("%w: END_DATED without effective_to", domain.ErrInvalidEvent)
			}
		default:
			return nil, fmt.Errorf("%w: change_type %q", domain.ErrInvalidEvent, p.ChangeType)
		}
		a := &domain.EntityJurisdictionApplicability{
			ApplicabilityID:     assignment,
			TenantID:            env.TenantID,
			LegalEntityID:       entity,
			JurisdictionID:      p.JurisdictionID,
			ApplicabilityType:   firstNonEmpty(p.AssignmentType, "UNSPECIFIED"),
			EffectiveFrom:       p.EffectiveFrom,
			EffectiveTo:         p.EffectiveTo,
			SourceObjectVersion: version,
		}
		if env.EvidenceRef != "" {
			ref := env.EvidenceRef
			a.EvidenceRef = &ref
		}
		return a, nil
	}
	return nil, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// Run consumes until ctx ends. A message is committed only after it has been
// applied (or deliberately skipped): ReadMessage's auto-commit would lose an
// event whose apply failed. A store outage retries the same message with
// backoff instead of moving past it.
func (c *EntityConsumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("entity consumer: reader close failed", zap.Error(err))
		}
	}()
	c.log.Info("entity consumer started")

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.log.Info("entity consumer stopping")
				return
			}
			c.log.Warn("entity consumer: fetch failed — retrying", zap.Error(err))
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}

		backoff := time.Second
		for {
			err := c.Handle(ctx, msg.Value)
			if err == nil {
				break
			}
			c.log.Error("entity consumer: apply failed — retrying the same message",
				zap.String("key", string(msg.Key)), zap.Int64("offset", msg.Offset), zap.Error(err))
			if !sleep(ctx, backoff) {
				c.log.Info("entity consumer stopping with an unapplied message — not committed, so it is redelivered")
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.log.Error("entity consumer: commit failed (the message will be redelivered and de-duplicated)", zap.Error(err))
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
