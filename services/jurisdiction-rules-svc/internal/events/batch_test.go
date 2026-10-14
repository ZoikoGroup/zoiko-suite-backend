package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	svcenvelope "zoiko.io/jurisdiction-rules-svc/internal/envelope"
)

func pendingOf(t *testing.T, ctx context.Context) []OutboxPendingEvent {
	t.Helper()
	b := batchFrom(ctx)
	if b == nil {
		t.Fatal("context carries no batch")
	}
	return b.events
}

// Audit gap 5: the pending buffer was one slice on the shared publisher, so
// one request's FlushPending wrote whatever any other request had buffered —
// into the wrong transaction. Each transaction scope now holds only its own.
func TestOutboxPublisher_BatchesAreIsolatedPerTransaction(t *testing.T) {
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	rule := domain.JurisdictionRule{JurisdictionRuleID: "r-1", JurisdictionID: "j-1", DataClassification: "INTERNAL", CreatedByPrincipalID: "maker"}

	var wg sync.WaitGroup
	ctxs := make([]context.Context, 50)
	for i := range ctxs {
		ctxs[i] = WithBatch(context.Background())
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			if err := p.PublishRuleUpdated(ctx, rule, "c"); err != nil {
				t.Error(err)
			}
		}(ctxs[i])
	}
	wg.Wait()
	for i, ctx := range ctxs {
		if n := len(pendingOf(t, ctx)); n != 1 {
			t.Fatalf("transaction %d holds %d events, want exactly its own 1", i, n)
		}
	}
}

// With no transaction scope there is nothing for the event to commit with;
// refusing is the only answer that cannot lose or misattribute it.
func TestOutboxPublisher_RefusesAPublishOutsideABatch(t *testing.T) {
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	err := p.PublishRuleUpdated(context.Background(), domain.JurisdictionRule{}, "c")
	if !errors.Is(err, ErrNoBatch) {
		t.Fatalf("want ErrNoBatch, got %v", err)
	}
}

// The activated event was built by the rule.updated builder and carried
// event_type "jurisdiction.rule.updated" in its envelope. The envelope also
// lacked classification, payload_hash and the causing request id.
func TestOutboxPublisher_EnvelopeNamesTheEventAndCarriesIntegrityFields(t *testing.T) {
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{RequestID: "req-9"})
	ctx = WithBatch(ctx)
	rule := domain.JurisdictionRule{JurisdictionRuleID: "r-1", JurisdictionID: "j-1", DataClassification: "INTERNAL", CreatedByPrincipalID: "maker"}

	if err := p.PublishRuleUpdated(ctx, rule, "c"); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishRuleActivated(ctx, rule, "c"); err != nil {
		t.Fatal(err)
	}

	want := []string{EventRuleUpdated, EventRuleActivated}
	for i, e := range pendingOf(t, ctx) {
		var env struct {
			EventType      string          `json:"event_type"`
			RequestID      string          `json:"request_id"`
			Classification string          `json:"classification"`
			PayloadHash    string          `json:"payload_hash"`
			Payload        json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(e.Payload, &env); err != nil {
			t.Fatal(err)
		}
		if e.EventType != want[i] || env.EventType != want[i] {
			t.Errorf("event %d: outbox type %q, envelope type %q, want %q", i, e.EventType, env.EventType, want[i])
		}
		sum := sha256.Sum256(env.Payload)
		if env.PayloadHash != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Errorf("event %d: payload_hash %q does not match the payload", i, env.PayloadHash)
		}
		if env.Classification != "INTERNAL" || env.RequestID != "req-9" {
			t.Errorf("event %d: classification %q request_id %q, want INTERNAL / req-9", i, env.Classification, env.RequestID)
		}
	}
}

// Row 64: the envelope carries the aggregate's version after the change, so a
// consumer can drop a stale or duplicate event.
func TestOutboxPublisher_EnvelopeCarriesAggregateVersion(t *testing.T) {
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	ctx := WithBatch(context.Background())
	if err := p.PublishRuleActivated(ctx, domain.JurisdictionRule{JurisdictionRuleID: "r-1", RowVersion: 7, PrecedenceLevel: 2, RuleVersion: 3}, "c"); err != nil {
		t.Fatal(err)
	}
	var env struct {
		AggregateVersion int64 `json:"aggregate_version"`
		Payload          struct {
			PrecedenceLevel int `json:"precedence_level"`
			RuleVersion     int `json:"rule_version"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(pendingOf(t, ctx)[0].Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.AggregateVersion != 7 || env.Payload.PrecedenceLevel != 2 || env.Payload.RuleVersion != 3 {
		t.Fatalf("want aggregate_version 7 with precedence 2 and rule_version 3 in the payload, got %+v", env)
	}
}
