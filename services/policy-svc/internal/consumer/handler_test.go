package consumer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// recorder counts InvalidateTenant calls per tenant.
type recorder struct{ calls map[string]int }

func newRecorder() *recorder { return &recorder{calls: map[string]int{}} }

func (r *recorder) InvalidateTenant(tenantID string) { r.calls[tenantID]++ }

func (r *recorder) total() int {
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

const tenantA = "11111111-1111-1111-1111-111111111111"

// TestHandler_EachConsumedEventRefreshesItsTenant covers Doc 03 §8.1's three
// consumed events: each one drops the named tenant's cached resolution
// candidates, and only that tenant's.
func TestHandler_EachConsumedEventRefreshesItsTenant(t *testing.T) {
	ctx := context.Background()
	for name, fire := range map[string]func(h *Handler) error{
		"entity.created": func(h *Handler) error {
			return h.HandleEntityCreated(ctx, EntityCreatedEvent{EventID: "e1", TenantID: tenantA})
		},
		"role.updated": func(h *Handler) error {
			return h.HandleRoleUpdated(ctx, RoleUpdatedEvent{EventID: "e2", TenantID: tenantA})
		},
		"authority.delegated": func(h *Handler) error {
			return h.HandleAuthorityDelegated(ctx, AuthorityDelegatedEvent{EventID: "e3", TenantID: tenantA})
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder()
			if err := fire(NewHandler(rec, zap.NewNop())); err != nil {
				t.Fatalf("handler returned %v; a cache refresh never fails", err)
			}
			if rec.calls[tenantA] != 1 || rec.total() != 1 {
				t.Fatalf("invalidations = %v, want exactly one for %s", rec.calls, tenantA)
			}
		})
	}
}

// TestHandler_NoTenantIsANoOp: a platform event names no tenant, so there is
// nothing tenant-scoped to drop, and it must not be treated as an error (an
// error would stall the partition on redelivery).
func TestHandler_NoTenantIsANoOp(t *testing.T) {
	rec := newRecorder()
	h := NewHandler(rec, zap.NewNop())
	if err := h.HandleEntityCreated(context.Background(), EntityCreatedEvent{EventID: "e1"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if rec.total() != 0 {
		t.Fatalf("invalidations = %v, want none", rec.calls)
	}
}

// TestHandler_DuplicateDeliveryIsIdempotent (GCP domain negative path #29):
// at-least-once delivery may hand the same event over twice. Both succeed and
// leave the cache in the same state.
func TestHandler_DuplicateDeliveryIsIdempotent(t *testing.T) {
	rec := newRecorder()
	h := NewHandler(rec, zap.NewNop())
	ev := RoleUpdatedEvent{EventID: "dup", TenantID: tenantA}
	for i := 0; i < 2; i++ {
		if err := h.HandleRoleUpdated(context.Background(), ev); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if rec.calls[tenantA] != 2 {
		t.Fatalf("invalidations = %v; each delivery clears the (already empty) entry", rec.calls)
	}
}

// spy records which handler method handleMessage routed to.
type spy struct{ got []string }

func (s *spy) HandleEntityCreated(_ context.Context, e EntityCreatedEvent) error {
	s.got = append(s.got, "entity.created:"+e.TenantID)
	return nil
}

func (s *spy) HandleRoleUpdated(_ context.Context, e RoleUpdatedEvent) error {
	s.got = append(s.got, "role.updated:"+e.TenantID)
	return nil
}

func (s *spy) HandleAuthorityDelegated(_ context.Context, e AuthorityDelegatedEvent) error {
	s.got = append(s.got, "authority.delegated:"+e.TenantID)
	return nil
}

func message(t *testing.T, eventType string) kafka.Message {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event_id":       "ev-1",
		"event_type":     eventType,
		"event_version":  "1",
		"tenant_id":      tenantA,
		"source_service": "test",
		"payload":        map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kafka.Message{Value: b}
}

// TestHandleMessage_RoutesByTopicAndType checks the topic names against the
// producers' (tenant-entity-registry-svc, access-control-svc,
// delegated-authority-svc KAFKA_EVENTS_TOPIC defaults) and that an event only
// counts on its own topic.
func TestHandleMessage_RoutesByTopicAndType(t *testing.T) {
	cases := []struct {
		topic, eventType, want string
	}{
		{"zoiko.entity.events", "entity.created", "entity.created:" + tenantA},
		{"zoiko.access-control.events", "role.updated", "role.updated:" + tenantA},
		{"zoiko.delegated-authority.events", "authority.delegated", "authority.delegated:" + tenantA},
		// Same event type on the wrong topic, or another type on the right
		// topic: ignored, and not an error (nothing to retry).
		{"zoiko.access-control.events", "entity.created", ""},
		{"zoiko.entity.events", "entity.updated", ""},
	}
	for _, tc := range cases {
		t.Run(tc.topic+"/"+tc.eventType, func(t *testing.T) {
			s := &spy{}
			c := &Consumer{handler: s, log: zap.NewNop()}
			if err := c.handleMessage(context.Background(), message(t, tc.eventType), tc.topic); err != nil {
				t.Fatalf("err = %v", err)
			}
			switch {
			case tc.want == "" && len(s.got) != 0:
				t.Fatalf("routed %v, want ignored", s.got)
			case tc.want != "" && (len(s.got) != 1 || s.got[0] != tc.want):
				t.Fatalf("routed %v, want [%s]", s.got, tc.want)
			}
		})
	}
}

// TestHandleMessage_MalformedIsAnError: an unreadable record is reported, so
// the offset is not committed and the failure is visible.
func TestHandleMessage_MalformedIsAnError(t *testing.T) {
	c := &Consumer{handler: &spy{}, log: zap.NewNop()}
	if err := c.handleMessage(context.Background(), kafka.Message{Value: []byte("{not json")}, "zoiko.entity.events"); err == nil {
		t.Fatal("malformed record accepted")
	}
}

// TestDefaultConsumerConfig_SubscribesToTheProducersTopics guards against a
// rename on either side silently disconnecting the consumer.
func TestDefaultConsumerConfig_SubscribesToTheProducersTopics(t *testing.T) {
	cfg := DefaultConsumerConfig([]string{"kafka:9094"}, "policy-svc")
	want := map[string]bool{"zoiko.entity.events": true, "zoiko.access-control.events": true, "zoiko.delegated-authority.events": true}
	if len(cfg.Topics) != len(want) {
		t.Fatalf("topics = %v", cfg.Topics)
	}
	for _, topic := range cfg.Topics {
		if !want[topic] {
			t.Fatalf("unexpected topic %q in %v", topic, cfg.Topics)
		}
	}
}
