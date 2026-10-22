package events

import (
	"context"
	"encoding/json"
	"testing"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/comments-collaboration-svc/internal/domain"
)

type captureWriter struct{ msgs []kafka.Message }

func (w *captureWriter) WriteMessages(_ context.Context, m ...kafka.Message) error {
	w.msgs = append(w.msgs, m...)
	return nil
}

// comment.added carries the thread's classification, so a consumer receives
// the lineage BIZ-09 requires of every comment derivative.
func TestCommentAdded_CarriesClassificationLineage(t *testing.T) {
	w := &captureWriter{}
	p := NewKafkaPublisherWithWriter(w, "t", zap.NewNop())
	p.PublishCommentAdded(context.Background(), "corr", "actor", "tenant", "le-1",
		domain.Comment{CommentID: "c1"},
		domain.CommentThread{ThreadID: "th1", Classification: "CONFIDENTIAL"})

	if len(w.msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(w.msgs))
	}
	var evt struct {
		EventType string         `json:"event_type"`
		TenantID  string         `json:"tenant_id"`
		Payload   map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.msgs[0].Value, &evt); err != nil {
		t.Fatal(err)
	}
	if evt.EventType != "comment.added" || evt.TenantID != "tenant" {
		t.Fatalf("envelope changed unexpectedly: %+v", evt)
	}
	if evt.Payload["classification"] != "CONFIDENTIAL" {
		t.Fatalf("payload classification = %v", evt.Payload["classification"])
	}
}
