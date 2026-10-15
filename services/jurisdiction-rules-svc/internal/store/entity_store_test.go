package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Doc 03 §8.2 consumed events, projected: redelivery is a no-op, an
// out-of-order ASSIGNED cannot reopen an END_DATED assignment, reads are
// windowed by effective date and never cross tenants.
func TestPgStore_EntityApplicabilityProjection(t *testing.T) {
	s, _, ctx := newTestStore(t)
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	entity := uuid.New().String()
	jur := mustCreateJurisdiction(t, s, ctx, "EA", nil).JurisdictionID
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	assigned := domain.EntityJurisdictionApplicability{
		ApplicabilityID: "asg-1", TenantID: tenantA, LegalEntityID: entity, JurisdictionID: jur,
		ApplicabilityType: "TAX_ONLY", EffectiveFrom: from, SourceObjectVersion: 1,
	}
	ended := assigned
	ended.EffectiveTo = &to
	ended.SourceObjectVersion = 2

	if ok, err := s.ApplyEntityApplicability(ctx, "evt-a", "entity.jurisdiction.changed", assigned); err != nil || !ok {
		t.Fatalf("ASSIGNED: applied=%v err=%v", ok, err)
	}
	if ok, err := s.ApplyEntityApplicability(ctx, "evt-a", "entity.jurisdiction.changed", assigned); err != nil || ok {
		t.Fatalf("redelivered event must be a no-op: applied=%v err=%v", ok, err)
	}
	if ok, err := s.ApplyEntityApplicability(ctx, "evt-e", "entity.jurisdiction.changed", ended); err != nil || !ok {
		t.Fatalf("END_DATED: applied=%v err=%v", ok, err)
	}
	// The same ASSIGNED fact re-published under a new event id (a producer
	// replay) arrives after END_DATED: version 1 < 2, so it must not reopen.
	if ok, err := s.ApplyEntityApplicability(ctx, "evt-a-replay", "entity.jurisdiction.changed", assigned); err != nil || ok {
		t.Fatalf("a stale version must not overwrite a newer one: applied=%v err=%v", ok, err)
	}

	in := func(tenant string, at time.Time) int {
		t.Helper()
		rows, err := s.ListEntityJurisdictions(ctx, tenant, entity, at)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if n := in(tenantA, from.Add(24*time.Hour)); n != 1 {
		t.Errorf("inside the assignment period: want 1 jurisdiction, got %d", n)
	}
	if n := in(tenantA, to.Add(time.Hour)); n != 0 {
		t.Errorf("after END_DATED: want 0, got %d (the stale ASSIGNED reopened it?)", n)
	}
	if n := in(tenantB, from.Add(24*time.Hour)); n != 0 {
		t.Errorf("another tenant must see nothing, got %d", n)
	}
	if rows, err := s.ListEntityJurisdictions(ctx, "not-a-uuid", "nor-this", from); err != nil || len(rows) != 0 {
		t.Errorf("malformed ids are a non-match, not an error: %v / %d rows", err, len(rows))
	}

	// A tenant cannot hijack another tenant's row by reusing its id.
	hijack := assigned
	hijack.TenantID = tenantB
	hijack.SourceObjectVersion = 9
	if ok, err := s.ApplyEntityApplicability(ctx, "evt-h", "entity.jurisdiction.changed", hijack); err != nil || ok {
		t.Fatalf("a row keyed to tenant A must not be overwritten by tenant B: applied=%v err=%v", ok, err)
	}

	// An inverted period is a bad event: ErrInvalidEvent, recorded as consumed.
	bad := assigned
	bad.ApplicabilityID = "asg-bad"
	early := from.Add(-time.Hour)
	bad.EffectiveTo = &early
	if _, err := s.ApplyEntityApplicability(ctx, "evt-bad", "entity.jurisdiction.changed", bad); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatalf("inverted period: want ErrInvalidEvent, got %v", err)
	}
	if ok, err := s.ApplyEntityApplicability(ctx, "evt-bad", "entity.jurisdiction.changed", bad); err != nil || ok {
		t.Fatalf("a rejected event is recorded and not retried: applied=%v err=%v", ok, err)
	}
}
