package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

// The lifecycle consumer — §8.3's role.assigned, employment.changed and
// entity.scope.updated, which four passes recorded as having no producer.
//
// The envelopes below are the REAL shapes, copied from the producers:
//
//	principal.status.changed     identity-context-svc/internal/events/publisher.go
//	role.* / permission.bundle.* access-control-svc/internal/events/publisher.go
//	entity.*                     tenant-entity-registry-svc/internal/events/publisher.go
//
// Written out rather than built with a helper that invents a shape, because the
// whole defect this file closes was a shape mismatch: the consumer that did not
// exist was reasoned about from the spec's event names instead of from what the
// producers publish.

type lifecycleStub struct {
	projected  []domain.ProjectPrincipalStatusParams
	projectErr error

	invalidatedTenants []string
}

func (s *lifecycleStub) ProjectPrincipalStatus(_ context.Context, params domain.ProjectPrincipalStatusParams) (*domain.PrincipalStatusProjection, error) {
	if s.projectErr != nil {
		return nil, s.projectErr
	}
	s.projected = append(s.projected, params)
	return &domain.PrincipalStatusProjection{
		PrincipalID: params.PrincipalID,
		TenantID:    params.TenantID,
		Status:      params.Status,
	}, nil
}

func (s *lifecycleStub) InvalidateGrantSourcesForTenant(tenantID string) {
	s.invalidatedTenants = append(s.invalidatedTenants, tenantID)
}

func newLifecycle(store *lifecycleStub) *events.LifecycleConsumer {
	return events.NewLifecycleConsumer(zap.NewNop(), store)
}

// lifecycleEnvelope builds the platform event envelope (Doc 03 §19) with a payload.
func lifecycleEnvelope(t *testing.T, eventID, eventType, tenantID string, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	out, err := json.Marshal(map[string]any{
		"event_id":       eventID,
		"event_type":     eventType,
		"tenant_id":      tenantID,
		"correlation_id": "corr-" + eventID,
		"payload":        json.RawMessage(raw),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

// ── principal.status.changed → the layer-0 projection ───────────────────────

// identity-context-svc's exact payload: principal_id, tenant_id, new_status,
// actor, correlation_id.
func TestLifecycle_PrincipalSuspended_IsProjected(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-1", "principal.status.changed", "t-1", map[string]any{
		"principal_id":   "p-9",
		"tenant_id":      "t-1",
		"new_status":     "SUSPENDED",
		"actor":          "p-admin",
		"correlation_id": "corr-e-1",
	}))

	if len(store.projected) != 1 {
		t.Fatalf("projected %d rows, want 1", len(store.projected))
	}
	got := store.projected[0]
	if got.PrincipalID != "p-9" {
		t.Errorf("principal_id = %q", got.PrincipalID)
	}
	if got.TenantID != "t-1" {
		t.Errorf("tenant_id = %q", got.TenantID)
	}
	if got.Status != "SUSPENDED" {
		t.Errorf("status = %q", got.Status)
	}
	// Provenance from the constant, not from the wire: source_service is
	// provenance, and taking it from the message would let a mislabelled
	// producer claim to be identity-context-svc.
	if got.SourceService != events.LifecycleSourceService {
		t.Errorf("source_service = %q, want %q", got.SourceService, events.LifecycleSourceService)
	}
}

func TestLifecycle_PrincipalReinstated_IsProjected(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-2", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"tenant_id":    "t-1",
		"new_status":   "ACTIVE",
	}))

	if len(store.projected) != 1 || store.projected[0].Status != domain.PrincipalStatusActive {
		t.Fatalf("projected = %+v, want one ACTIVE row — reinstatement has to project too, or an unsuspension never takes effect",
			store.projected)
	}
}

func TestLifecycle_StatusIsUpperCased(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-3", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "  suspended  ",
	}))

	if len(store.projected) != 1 {
		t.Fatalf("projected %d rows, want 1", len(store.projected))
	}
	// Layer 0 compares against the literal "ACTIVE", so a lower-case "active"
	// arriving from a producer would be read as NOT active and deny that
	// principal everything.
	if store.projected[0].Status != "SUSPENDED" {
		t.Fatalf("status = %q, want SUSPENDED — trimmed and upper-cased", store.projected[0].Status)
	}
}

// A lower-case "active" is the dangerous case in the other direction: without
// normalising, layer 0 would deny a principal that upstream just reinstated.
func TestLifecycle_LowerCaseActiveNormalisesToActive(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-4", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "active",
	}))

	if len(store.projected) != 1 || store.projected[0].Status != domain.PrincipalStatusActive {
		t.Fatalf("status = %+v — an un-normalised 'active' would be read as not-ACTIVE and deny a reinstated principal everything",
			store.projected)
	}
}

// The tenant comes from the PAYLOAD first, then the envelope: identity-context-svc
// puts it in both, and the payload names the tenant the status is about.
func TestLifecycle_TenantFallsBackToEnvelope(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-5", "principal.status.changed", "t-envelope", map[string]any{
		"principal_id": "p-9",
		"new_status":   "DISABLED",
	}))

	if len(store.projected) != 1 || store.projected[0].TenantID != "t-envelope" {
		t.Fatalf("projected = %+v, want the envelope tenant when the payload names none", store.projected)
	}
}

// No tenant anywhere is REFUSED, not defaulted. principal_status_projection.tenant_id
// is NOT NULL and every read carries a tenant predicate, so a guessed tenant
// would either write a row no policy can match or suspend a principal in the
// wrong tenant.
func TestLifecycle_NoTenantAnywhereIsRefused(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-6", "principal.status.changed", "", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	}))

	if len(store.projected) != 0 {
		t.Fatalf("projected %+v — a tenantless status event must not be written", store.projected)
	}
}

// An EMPTY status is refused, and this is the one place in this consumer where
// fail-closed is the wrong direction: layer 0 denies anything that is not
// exactly ACTIVE, so storing "" would deny that principal everything — caused
// by a malformed event rather than by a decision.
func TestLifecycle_EmptyStatusIsRefusedRatherThanDenyingEverything(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-7", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "",
	}))

	if len(store.projected) != 0 {
		t.Fatalf("projected %+v — an empty status would be read as not-ACTIVE and deny this principal every action",
			store.projected)
	}
}

func TestLifecycle_NoPrincipalIDIsRefused(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-8", "principal.status.changed", "t-1", map[string]any{
		"new_status": "SUSPENDED",
	}))

	if len(store.projected) != 0 {
		t.Fatalf("projected %+v with no principal_id", store.projected)
	}
}

// A stale event is not an error. ProjectPrincipalStatus refuses to overwrite a
// newer status, and re-applying an old SUSPENDED after a newer ACTIVE would
// re-suspend a reinstated principal — silently, with the only symptom being a
// person unable to work.
func TestLifecycle_StaleStatusIsNotTreatedAsFailure(t *testing.T) {
	store := &lifecycleStub{projectErr: domain.ErrPrincipalStatusStale}
	c := newLifecycle(store)

	// Handle must not panic and must not retry; the outcome the stream is
	// converging on already holds.
	c.Handle(context.Background(), lifecycleEnvelope(t, "e-9", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	}))
}

func TestLifecycle_ProjectionFailureIsSurvived(t *testing.T) {
	store := &lifecycleStub{projectErr: domain.ErrStoreUnavailable}
	c := newLifecycle(store)

	// A failure here means the principal stays fully authorized, which is why
	// the consumer logs it at Error with that consequence named — but it must
	// not stall the partition.
	c.Handle(context.Background(), lifecycleEnvelope(t, "e-10", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	}))
}

// A status change never invalidates the grant namespaces: it changes one
// principal's standing and nothing about what any role grants. The
// principal-status cache namespace is invalidated by the STORE decorator, not
// by the consumer.
func TestLifecycle_StatusChangeDoesNotInvalidateGrants(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-11", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	}))

	if len(store.invalidatedTenants) != 0 {
		t.Fatalf("invalidated %v — a suspension changes no role's grant set, and throwing away a whole tenant's cached grants for it would cost the hot path for nothing",
			store.invalidatedTenants)
	}
}

// ── the grant-graph events → cross-replica cache invalidation ───────────────

// Every one of these is published by a service that has ALREADY written
// through this service's admin API on one replica. The event is how the other
// replicas find out — which is what turns a TTL-length staleness window on the
// authorization path into broker latency.
func TestLifecycle_GrantGraphEventsInvalidateTheirTenant(t *testing.T) {
	eventTypes := []string{
		// access-control-svc
		"role.created",
		"role.updated",
		"permission.bundle.updated",
		// tenant-entity-registry-svc
		"entity.status.changed",
		"entity.hierarchy.changed",
		"entity.jurisdiction.changed",
		"entity.updated",
	}

	for _, et := range eventTypes {
		t.Run(et, func(t *testing.T) {
			store := &lifecycleStub{}
			c := newLifecycle(store)

			c.Handle(context.Background(), lifecycleEnvelope(t, "e-"+et, et, "t-1", map[string]any{"role_definition_id": "rd-1"}))

			if len(store.invalidatedTenants) != 1 || store.invalidatedTenants[0] != "t-1" {
				t.Fatalf("invalidated %v, want [t-1]", store.invalidatedTenants)
			}
			if len(store.projected) != 0 {
				t.Fatalf("projected %+v — these events change no principal's standing", store.projected)
			}
		})
	}
}

// The asymmetry that matters: a tenantless grant-graph event invalidates
// EVERYTHING rather than being refused. Over-invalidating costs one database
// read; under-invalidating leaves a replica serving a grant set an
// authoritative service has just changed, for up to the TTL, on the
// authorization path.
func TestLifecycle_TenantlessGrantGraphEventInvalidatesEveryTenant(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-20", "permission.bundle.updated", "", map[string]any{
		"bundle_id": "b-1",
	}))

	if len(store.invalidatedTenants) != 1 {
		t.Fatalf("invalidated %v, want exactly one call", store.invalidatedTenants)
	}
	// The empty string is how cache.Store.invalidate expresses "every tenant",
	// via its global generation counter — the same treatment a platform-wide
	// SoD rule gets.
	if store.invalidatedTenants[0] != "" {
		t.Fatalf("invalidated %q, want the empty tenant (= every tenant)", store.invalidatedTenants[0])
	}
}

// ── dispatch, dedupe and robustness ─────────────────────────────────────────

// The three topics carry plenty this service has no interest in. Skipped
// silently — at this volume a log line per uninteresting event is how the log
// stops being readable — and, critically, without stalling the offset.
func TestLifecycle_UninterestingEventsAreSkipped(t *testing.T) {
	ignored := []string{
		"identity.authentication.succeeded",
		"session.invalidated",
		"identity.context.resolved",
		"tenant.created",
		"workspace.created",
		"position.created",
		"employee.hired",
		// Explicitly NOT consumed: the payload names an employee_id and there
		// is no employee-to-principal mapping anywhere on the estate, so this
		// service cannot tell whose authority it is about. Consuming it on a
		// guessed join would end the wrong principal's grants.
		"employee.terminated",
		"employee.status.changed",
	}

	for _, et := range ignored {
		t.Run(et, func(t *testing.T) {
			store := &lifecycleStub{}
			c := newLifecycle(store)

			c.Handle(context.Background(), lifecycleEnvelope(t, "e-"+et, et, "t-1", map[string]any{
				"employee_id": "emp-1",
				"tenant_id":   "t-1",
				"new_status":  "TERMINATED",
			}))

			if len(store.projected) != 0 {
				t.Errorf("projected %+v for %s", store.projected, et)
			}
			if len(store.invalidatedTenants) != 0 {
				t.Errorf("invalidated %v for %s", store.invalidatedTenants, et)
			}
		})
	}
}

func TestLifecycle_DedupesOnEventID(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	msg := lifecycleEnvelope(t, "e-same", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	})
	c.Handle(context.Background(), msg)
	c.Handle(context.Background(), msg)
	c.Handle(context.Background(), msg)

	if len(store.projected) != 1 {
		t.Fatalf("projected %d times for 3 redeliveries of one event id, want 1", len(store.projected))
	}
}

// An event with no id is HANDLED, not dropped: the store writes are idempotent,
// and dropping a real suspension because its producer omitted a field is the
// worse failure.
func TestLifecycle_MissingEventIDIsStillHandled(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	for i := 0; i < 2; i++ {
		c.Handle(context.Background(), lifecycleEnvelope(t, "", "principal.status.changed", "t-1", map[string]any{
			"principal_id": "p-9",
			"new_status":   "SUSPENDED",
		}))
	}

	if len(store.projected) != 2 {
		t.Fatalf("projected %d times, want 2 — an event with no id cannot be deduplicated and must not be dropped",
			len(store.projected))
	}
}

func TestLifecycle_UndecodableInputIsSurvived(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), []byte("{not json"))
	c.Handle(context.Background(), []byte(""))
	c.Handle(context.Background(), nil)
	// A consumed event type with a payload that is not an object.
	c.Handle(context.Background(), []byte(`{"event_id":"e-x","event_type":"principal.status.changed","tenant_id":"t-1","payload":"a string"}`))

	if len(store.projected) != 0 || len(store.invalidatedTenants) != 0 {
		t.Fatalf("malformed input reached the store: projected=%+v invalidated=%v",
			store.projected, store.invalidatedTenants)
	}
}

// status_changed_at is not in identity-context-svc's current payload. It is
// declared so that a producer which starts sending it gets replay protection
// with no change here — nil means "upstream did not say", which
// ProjectPrincipalStatus handles by applying the write.
func TestLifecycle_StatusChangedAtIsForwardedWhenPresent(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	at := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	c.Handle(context.Background(), lifecycleEnvelope(t, "e-30", "principal.status.changed", "t-1", map[string]any{
		"principal_id":      "p-9",
		"new_status":        "SUSPENDED",
		"status_changed_at": at.Format(time.RFC3339),
	}))

	if len(store.projected) != 1 {
		t.Fatalf("projected %d rows, want 1", len(store.projected))
	}
	if store.projected[0].StatusChangedAt == nil {
		t.Fatal("status_changed_at was dropped — without it the store cannot refuse a stale replay")
	}
	if !store.projected[0].StatusChangedAt.Equal(at) {
		t.Errorf("status_changed_at = %v, want %v", store.projected[0].StatusChangedAt, at)
	}
}

func TestLifecycle_AbsentStatusChangedAtIsNilNotZeroTime(t *testing.T) {
	store := &lifecycleStub{}
	c := newLifecycle(store)

	c.Handle(context.Background(), lifecycleEnvelope(t, "e-31", "principal.status.changed", "t-1", map[string]any{
		"principal_id": "p-9",
		"new_status":   "SUSPENDED",
	}))

	if len(store.projected) != 1 {
		t.Fatalf("projected %d rows, want 1", len(store.projected))
	}
	// nil, not the zero time. A zero time would order BEFORE every stored
	// value, so the store's staleness check would refuse every event from a
	// producer that does not send the field — which is the current producer.
	if store.projected[0].StatusChangedAt != nil {
		t.Fatalf("status_changed_at = %v, want nil — the zero time would make every event from the current producer look stale",
			store.projected[0].StatusChangedAt)
	}
}

// The union is what the reader subscribes on behalf of. Asserted so that
// adding an event type to one of the two maps without the other being reachable
// is visible.
func TestLifecycle_ConsumedEventTypesCoversBothGroups(t *testing.T) {
	got := events.LifecycleConsumedEventTypes()

	for _, et := range []string{
		"principal.status.changed",
		"role.created",
		"role.updated",
		"permission.bundle.updated",
		"entity.status.changed",
		"entity.hierarchy.changed",
		"entity.jurisdiction.changed",
		"entity.updated",
	} {
		if !got[et] {
			t.Errorf("%s is not in LifecycleConsumedEventTypes", et)
		}
	}

	// The delegation consumer's events belong to the OTHER consumer, on
	// another topic and another group. Overlap would mean one event handled
	// twice by two dedupe sets that cannot see each other.
	for _, et := range []string{"authority.delegated", "authority.revoked", "authority.expired"} {
		if got[et] {
			t.Errorf("%s is claimed by both consumers", et)
		}
	}

	if got["employee.terminated"] {
		t.Error("employee.terminated is consumed — its payload names an employee_id that nothing on this estate maps to a principal")
	}
}
