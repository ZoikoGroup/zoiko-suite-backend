package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/comments-collaboration-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every
// published event carries event name, event version, timestamp, tenant
// ID, legal entity ID, actor ID, correlation ID, source service, and
// payload schema version.
type envelope struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer
// — narrowed to an interface so tests can assert envelope content
// without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type KafkaPublisher struct {
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: w, topic: topic, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

// BIZ-09's own named events: "CommentAdded; CommentEdited;
// CommentModerated; MentionCreated; ThreadResolved." All five are
// implemented as of Wave 2.

func (p *KafkaPublisher) PublishCommentAdded(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment, t domain.CommentThread) {
	p.emit(ctx, "comment.added", correlationID, actorID, tenantID, legalEntityID, c.CommentID, map[string]any{
		"comment_id": c.CommentID, "thread_id": t.ThreadID,
		"linked_object_type": t.LinkedObjectType, "linked_object_id": t.LinkedObjectID,
	})
}

func (p *KafkaPublisher) PublishCommentEdited(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment) {
	p.emit(ctx, "comment.edited", correlationID, actorID, tenantID, legalEntityID, c.CommentID, map[string]any{
		"comment_id": c.CommentID, "current_version_id": c.CurrentVersionID,
	})
}

func (p *KafkaPublisher) PublishCommentModerated(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, c domain.Comment) {
	p.emit(ctx, "comment.moderated", correlationID, actorID, tenantID, legalEntityID, c.CommentID, map[string]any{
		"comment_id": c.CommentID, "moderation_reason": c.ModerationReason,
	})
}

func (p *KafkaPublisher) PublishMentionCreated(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, m domain.Mention) {
	p.emit(ctx, "mention.created", correlationID, actorID, tenantID, legalEntityID, m.CommentID, map[string]any{
		"mention_id": m.MentionID, "comment_id": m.CommentID, "mentioned_principal_id": m.MentionedPrincipalID,
	})
}

func (p *KafkaPublisher) PublishThreadResolved(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, t domain.CommentThread) {
	p.emit(ctx, "thread.resolved", correlationID, actorID, tenantID, legalEntityID, t.ThreadID, map[string]any{
		"thread_id": t.ThreadID, "resolution_note": t.ResolutionNote,
	})
}

func (p *KafkaPublisher) emit(ctx context.Context, eventType, correlationID, actorID, tenantID, legalEntityID, entityID string, payload map[string]any) {
	evt := envelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "comments-collaboration-svc",
		EntityID:      entityID,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		p.logger.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(entityID), Value: data}); err != nil {
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", eventType), zap.Error(err))
	}
}
