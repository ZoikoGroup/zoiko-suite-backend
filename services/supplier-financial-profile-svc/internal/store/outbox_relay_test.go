package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
	"zoiko.io/supplier-financial-profile-svc/internal/outbox"
)

type capturePublisher struct{ got map[string][]byte }

func (c *capturePublisher) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	c.got[id] = payload
	return nil
}

// TestOutboxRelay_PublishesCommandEvents proves the rows written by commands
// are relayable: the envelope carries the spec event name, profile id and
// tenant, and rows are marked published exactly once.
func TestOutboxRelay_PublishesCommandEvents(t *testing.T) {
	s, pool := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	p, err := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: uuid.New().String(), SupplierRef: "sup"}, "m", nil)
	if err != nil {
		t.Fatal(err)
	}

	pub := &capturePublisher{got: map[string][]byte{}}
	outbox.NewRelay(s.pool, pub, 0, 500, zap.NewNop()).RelayOnce(context.Background())

	found := map[string]bool{}
	for _, raw := range pub.got {
		var env struct {
			EventType     string `json:"event_type"`
			EntityID      string `json:"entity_id"`
			TenantID      string `json:"tenant_id"`
			SourceService string `json:"source_service"`
			ActorID       string `json:"actor_id"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.EntityID == p.ProfileID {
			if env.TenantID != tenant || env.SourceService != "supplier-financial-profile-svc" || env.ActorID != "m" {
				t.Fatalf("bad envelope: %+v", env)
			}
			found[env.EventType] = true
		}
	}
	if !found["SupplierFinancialProfileCreated"] || !found["supplier_financial_profile.created"] {
		t.Fatalf("relay did not publish the create events: %v", found)
	}
	var unpublished int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL`, p.ProfileID).Scan(&unpublished)
	if unpublished != 0 {
		t.Fatalf("expected the rows to be marked published, %d left", unpublished)
	}
}
