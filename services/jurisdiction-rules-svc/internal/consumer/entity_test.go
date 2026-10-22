package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

type fakeStore struct {
	calls []domain.EntityJurisdictionApplicability
	ids   []string
	err   error
}

func (f *fakeStore) ApplyEntityApplicability(_ context.Context, eventID, _ string, a domain.EntityJurisdictionApplicability) (bool, error) {
	f.calls = append(f.calls, a)
	f.ids = append(f.ids, eventID)
	return f.err == nil, f.err
}

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	entity = "22222222-2222-2222-2222-222222222222"
	jur    = "33333333-3333-3333-3333-333333333333"
)

// envelope mirrors tenant-entity-registry-svc's internal/events envelope.
func envelope(t *testing.T, eventType, tenantID string, version int64, payload map[string]any) []byte {
	t.Helper()
	p, _ := json.Marshal(payload)
	b, _ := json.Marshal(map[string]any{
		"event_id": "evt-1", "event_type": eventType, "event_version": "1.0",
		"emitted_at": time.Now().UTC(), "schema_version": "1.0", "source_service": "tenant-entity-registry-svc",
		"tenant_id": tenantID, "legal_entity_id": entity, "jurisdiction": jur, "correlation_id": "c",
		"object_id": "asg-1", "object_version": version, "effective_at": "2026-01-01T00:00:00Z",
		"recorded_at": time.Now().UTC(), "evidence_ref": "registry-extract-9", "payload": json.RawMessage(p),
	})
	return b
}

func TestEntityConsumer_ProjectsBothEvents(t *testing.T) {
	st := &fakeStore{}
	c := NewEntityConsumer(zap.NewNop(), st)

	if err := c.Handle(context.Background(), envelope(t, EventEntityCreated, tenant, 1, map[string]any{
		"tenant_id": tenant, "legal_entity_id": entity, "primary_jurisdiction_id": jur, "entity_code": "E1",
	})); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(context.Background(), envelope(t, EventEntityJurisdictionChanged, tenant, 2, map[string]any{
		"legal_entity_id": entity, "assignment_id": "asg-1", "jurisdiction_id": jur, "assignment_type": "TAX_ONLY",
		"change_type": "END_DATED", "effective_from": "2026-01-01T00:00:00Z", "effective_to": "2026-06-01T00:00:00Z",
	})); err != nil {
		t.Fatal(err)
	}
	if len(st.calls) != 2 {
		t.Fatalf("want 2 projections, got %d", len(st.calls))
	}
	p := st.calls[0]
	if p.ApplicabilityID != "primary:"+entity || p.ApplicabilityType != domain.ApplicabilityEntityPrimary || p.TenantID != tenant || p.JurisdictionID != jur {
		t.Errorf("entity.created projection wrong: %+v", p)
	}
	a := st.calls[1]
	if a.ApplicabilityID != "asg-1" || a.ApplicabilityType != "TAX_ONLY" || a.EffectiveTo == nil || a.SourceObjectVersion != 2 || a.EvidenceRef == nil || *a.EvidenceRef != "registry-extract-9" {
		t.Errorf("entity.jurisdiction.changed projection wrong: %+v", a)
	}
}

// Events that can never be applied are skipped (nil, so the offset moves)
// and never reach the store: a guessed tenant would answer another tenant.
func TestEntityConsumer_SkipsWhatCannotBeApplied(t *testing.T) {
	st := &fakeStore{}
	c := NewEntityConsumer(zap.NewNop(), st)
	bad := map[string][]byte{
		"no tenant": envelope(t, EventEntityCreated, "", 1, map[string]any{"legal_entity_id": entity, "primary_jurisdiction_id": jur}),
		"payload tenant differs": envelope(t, EventEntityCreated, tenant, 1, map[string]any{
			"tenant_id": "44444444-4444-4444-4444-444444444444", "legal_entity_id": entity, "primary_jurisdiction_id": jur}),
		"malformed jurisdiction": envelope(t, EventEntityJurisdictionChanged, tenant, 1, map[string]any{
			"legal_entity_id": entity, "assignment_id": "a", "jurisdiction_id": "nope", "change_type": "ASSIGNED", "effective_from": "2026-01-01T00:00:00Z"}),
		"end-dated without end": envelope(t, EventEntityJurisdictionChanged, tenant, 2, map[string]any{
			"legal_entity_id": entity, "assignment_id": "a", "jurisdiction_id": jur, "change_type": "END_DATED", "effective_from": "2026-01-01T00:00:00Z"}),
		"unknown change type": envelope(t, EventEntityJurisdictionChanged, tenant, 2, map[string]any{
			"legal_entity_id": entity, "assignment_id": "a", "jurisdiction_id": jur, "change_type": "MOVED", "effective_from": "2026-01-01T00:00:00Z"}),
		"not json":    []byte("{"),
		"other event": envelope(t, "entity.status.changed", tenant, 1, map[string]any{}),
	}
	for name, raw := range bad {
		if err := c.Handle(context.Background(), raw); err != nil {
			t.Errorf("%s: want nil (skip), got %v", name, err)
		}
	}
	if len(st.calls) != 0 {
		t.Fatalf("nothing unappliable may reach the store, got %d calls", len(st.calls))
	}
}

// A store outage must NOT be swallowed: Run retries the same message and
// does not commit it.
func TestEntityConsumer_StoreOutageIsRetried(t *testing.T) {
	st := &fakeStore{err: domain.ErrStoreUnavailable}
	c := NewEntityConsumer(zap.NewNop(), st)
	err := c.Handle(context.Background(), envelope(t, EventEntityCreated, tenant, 1, map[string]any{
		"tenant_id": tenant, "legal_entity_id": entity, "primary_jurisdiction_id": jur}))
	if !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Fatalf("want the outage returned for retry, got %v", err)
	}
	st.err = domain.ErrInvalidEvent
	if err := c.Handle(context.Background(), envelope(t, EventEntityCreated, tenant, 1, map[string]any{
		"tenant_id": tenant, "legal_entity_id": entity, "primary_jurisdiction_id": jur})); err != nil {
		t.Fatalf("a store-rejected event is skipped, not retried; got %v", err)
	}
}
