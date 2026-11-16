package events_test

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/events"
)

type captureWriter struct{ msgs []kafka.Message }

func (c *captureWriter) WriteMessages(_ context.Context, m ...kafka.Message) error {
	c.msgs = append(c.msgs, m...)
	return nil
}

// The BIZ-07 events the publisher emits must match, exactly, the message
// names declared in asyncapi.biz07.yaml — so the contract cannot drift from
// the code silently.
func TestCatalogEvents_MatchAsyncAPIContract(t *testing.T) {
	cw := &captureWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "t", cw)
	ctx := context.Background()
	o := domain.Offering{OfferingID: "o1", TenantID: "t1", LegalEntityID: "le1", SKUCode: "S"}
	v := domain.OfferingVersion{VersionID: "v1", OfferingID: "o1"}

	p.PublishOfferingCreated(ctx, "c", "a", o, v)
	p.PublishOfferingActivated(ctx, "c", "a", "t1", "le1", v)
	p.PublishOfferingSuspended(ctx, "c", "a", "t1", "le1", v)
	p.PublishOfferingRetired(ctx, "c", "a", "t1", "le1", v)
	p.PublishOfferingVersionSuperseded(ctx, "c", "a", "t1", "le1", "o1", "v0")

	var emitted []string
	for _, m := range cw.msgs {
		var env struct {
			EventType     string `json:"event_type"`
			EventVersion  string `json:"event_version"`
			SchemaVersion string `json:"schema_version"`
			SourceService string `json:"source_service"`
		}
		if err := json.Unmarshal(m.Value, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.EventVersion != "1.0" || env.SchemaVersion != "1.0" || env.SourceService != "inventory-management-svc" {
			t.Errorf("%s: envelope constants differ from contract: %+v", env.EventType, env)
		}
		emitted = append(emitted, env.EventType)
	}

	raw, err := os.ReadFile("../../asyncapi.biz07.yaml")
	if err != nil {
		t.Fatalf("read asyncapi.biz07.yaml: %v", err)
	}
	var declared []string
	for _, m := range regexp.MustCompile(`(?m)^      name: (\S+)$`).FindAllStringSubmatch(string(raw), -1) {
		declared = append(declared, m[1])
	}

	sort.Strings(emitted)
	sort.Strings(declared)
	if len(declared) != 5 || len(emitted) != 5 {
		t.Fatalf("expected 5 events each side, emitted=%v declared=%v", emitted, declared)
	}
	for i := range declared {
		if declared[i] != emitted[i] {
			t.Fatalf("contract drift: emitted=%v declared=%v", emitted, declared)
		}
	}
}
