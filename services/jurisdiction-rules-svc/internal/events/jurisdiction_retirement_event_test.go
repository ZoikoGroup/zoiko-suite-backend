package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// REF-01 RetireJurisdiction: jurisdiction.deactivated (the spec's
// JurisdictionRetired) carries the retirement evidence, so a consumer holding
// its own copy of the registry learns why, on what source and what replaces it.

func deactivatedPayload(t *testing.T, j domain.Jurisdiction) map[string]any {
	t.Helper()
	p := NewOutboxPublisher(zap.NewNop(), nil, "topic")
	ctx := WithBatch(context.Background())
	if err := p.PublishJurisdictionDeactivated(ctx, j, "corr-1"); err != nil {
		t.Fatal(err)
	}
	evs := pendingOf(t, ctx)
	if len(evs) != 1 || evs[0].EventType != EventJurisdictionDeactivated {
		t.Fatalf("pending = %+v, want one %s", evs, EventJurisdictionDeactivated)
	}
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(evs[0].Payload, &env); err != nil {
		t.Fatal(err)
	}
	return env.Payload
}

func TestDeactivatedEvent_CarriesTheRetirementEvidence(t *testing.T) {
	reason, ev, succ := "state dissolved", "iso-newsletter-3", "00000000-0000-4000-8000-000000000002"
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := deactivatedPayload(t, domain.Jurisdiction{
		JurisdictionID: "j-1", JurisdictionCode: "OLD", ActiveFlag: false, EffectiveTo: &to,
		CreatedByPrincipalID: "creator", RowVersion: 3, DataClassification: "PUBLIC",
		RetirementReason: &reason, RetirementEvidenceRef: &ev, SupersededByJurisdictionID: &succ,
	})
	for key, want := range map[string]any{
		"retirement_reason": reason, "retirement_evidence_ref": ev, "superseded_by_jurisdiction_id": succ,
		"jurisdiction_code": "OLD", "active_flag": false,
	} {
		if got[key] != want {
			t.Errorf("payload[%s] = %v, want %v", key, got[key], want)
		}
	}
}

// A bare retirement still publishes, with the evidence fields empty rather than absent.
func TestDeactivatedEvent_WithNoEvidenceStillPublishes(t *testing.T) {
	got := deactivatedPayload(t, domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "X", CreatedByPrincipalID: "c", DataClassification: "PUBLIC"})
	for _, key := range []string{"retirement_reason", "retirement_evidence_ref", "superseded_by_jurisdiction_id"} {
		v, present := got[key]
		if !present || v != nil {
			t.Errorf("payload[%s] = %v (present=%v), want present and null", key, v, present)
		}
	}
}
