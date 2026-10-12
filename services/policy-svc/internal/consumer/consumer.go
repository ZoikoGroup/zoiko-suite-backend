// Package consumer provides Kafka consumers for external events that
// affect policy evaluation — entity.created, role.updated,
// authority.delegated. These drive cache invalidation and internal state
// updates (03-microservices.md §8.1 "Consumed Events").
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// EventHandler is the interface for handling specific event types.
type EventHandler interface {
	HandleEntityCreated(ctx context.Context, event EntityCreatedEvent) error
	HandleRoleUpdated(ctx context.Context, event RoleUpdatedEvent) error
	HandleAuthorityDelegated(ctx context.Context, event AuthorityDelegatedEvent) error
}

// EntityCreatedEvent represents the entity.created event.
type EntityCreatedEvent struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	EmittedAt     string `json:"emitted_at"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// RoleUpdatedEvent represents the role.updated event.
type RoleUpdatedEvent struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	EmittedAt     string `json:"emitted_at"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// AuthorityDelegatedEvent represents the authority.delegated event.
type AuthorityDelegatedEvent struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	EmittedAt     string `json:"emitted_at"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// ConsumerConfig holds configuration for the consumer.
type ConsumerConfig struct {
	Brokers  []string
	GroupID  string
	Topics   []string
	MinBytes int
	MaxBytes int
}

// DefaultConsumerConfig returns a sensible default config.
func DefaultConsumerConfig(brokers []string, groupID string) ConsumerConfig {
	return ConsumerConfig{
		Brokers:  brokers,
		GroupID:  groupID,
		Topics:   []string{"zoiko.entity.events", "zoiko.access-control.events", "zoiko.delegated-authority.events"},
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10MB
	}
}

// topicReader holds a reader for a single topic.
type topicReader struct {
	topic  string
	reader *kafka.Reader
}

// Consumer is the Kafka consumer for policy-relevant events.
type Consumer struct {
	readers  []*topicReader
	handler  EventHandler
	log      *zap.Logger
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewConsumer constructs a new event consumer.
func NewConsumer(cfg ConsumerConfig, handler EventHandler, log *zap.Logger) *Consumer {
	readers := make([]*topicReader, len(cfg.Topics))
	for i, topic := range cfg.Topics {
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers:        cfg.Brokers,
			GroupID:        cfg.GroupID,
			Topic:          topic,
			MinBytes:       cfg.MinBytes,
			MaxBytes:       cfg.MaxBytes,
			CommitInterval: 5 * time.Second,
			StartOffset:    kafka.LastOffset,
			Logger:         kafka.LoggerFunc(func(s string, args ...interface{}) { log.Debug("kafka consumer", zap.String("msg", s), zap.String("topic", topic)) }),
			ErrorLogger:    kafka.LoggerFunc(func(s string, args ...interface{}) { log.Error("kafka consumer error", zap.String("msg", s), zap.String("topic", topic)) }),
		})
		readers[i] = &topicReader{topic: topic, reader: reader}
	}

	return &Consumer{
		readers: readers,
		handler: handler,
		log:     log,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start begins consuming events in a background goroutine.
func (c *Consumer) Start(ctx context.Context) {
	go c.run(ctx)
}

// Stop signals the consumer to stop and waits for it to finish.
func (c *Consumer) Stop() {
	close(c.stopCh)
	<-c.doneCh
	for _, tr := range c.readers {
		tr.reader.Close()
	}
}

// run is the main consumption loop.
func (c *Consumer) run(ctx context.Context) {
	defer close(c.doneCh)

	c.log.Info("policy-svc event consumer started",
		zap.Int("reader_count", len(c.readers)),
		zap.String("group_id", c.readers[0].reader.Config().GroupID),
	)

	// Run a consumer loop for each reader
	for _, tr := range c.readers {
		tr := tr // capture for goroutine
		go c.runReader(ctx, tr)
	}

	// Wait for stop signal
	<-c.stopCh
	c.log.Info("policy-svc event consumer stopped")
}

// runReader runs the consumption loop for a single topic reader.
func (c *Consumer) runReader(ctx context.Context, tr *topicReader) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		default:
			msg, err := tr.reader.FetchMessage(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				c.log.Error("consumer: fetch message failed",
					zap.String("topic", tr.topic),
					zap.Error(err),
				)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if err := c.handleMessage(ctx, msg, tr.topic); err != nil {
				c.log.Error("consumer: handle message failed",
					zap.String("topic", tr.topic),
					zap.Int("partition", msg.Partition),
					zap.Int64("offset", msg.Offset),
					zap.Error(err),
				)
				// Don't commit offset on handling failure — will retry
				continue
			}

			if err := tr.reader.CommitMessages(ctx, msg); err != nil {
				c.log.Error("consumer: commit message failed",
					zap.String("topic", tr.topic),
					zap.Error(err),
				)
			}
		}
	}
}

// handleMessage routes the message to the appropriate handler based on topic and event type.
func (c *Consumer) handleMessage(ctx context.Context, msg kafka.Message, topic string) error {
	var baseEvent struct {
		EventType string `json:"event_type"`
	}

	if err := json.Unmarshal(msg.Value, &baseEvent); err != nil {
		return fmt.Errorf("unmarshal base event: %w", err)
	}

	switch {
	case topic == "zoiko.entity.events" && baseEvent.EventType == "entity.created":
		var event EntityCreatedEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			return fmt.Errorf("unmarshal entity.created: %w", err)
		}
		return c.handler.HandleEntityCreated(ctx, event)

	case topic == "zoiko.access-control.events" && baseEvent.EventType == "role.updated":
		var event RoleUpdatedEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			return fmt.Errorf("unmarshal role.updated: %w", err)
		}
		return c.handler.HandleRoleUpdated(ctx, event)

	case topic == "zoiko.delegated-authority.events" && baseEvent.EventType == "authority.delegated":
		var event AuthorityDelegatedEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			return fmt.Errorf("unmarshal authority.delegated: %w", err)
		}
		return c.handler.HandleAuthorityDelegated(ctx, event)

	default:
		// Ignore unknown event types/topics
		c.log.Debug("consumer: ignoring unknown event",
			zap.String("topic", topic),
			zap.String("event_type", baseEvent.EventType),
		)
		return nil
	}
}