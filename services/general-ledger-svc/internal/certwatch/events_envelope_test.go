package certwatch

import (
	"encoding/json"
	"testing"
	"time"

	"zoiko.io/eventing/envelope"
)

// The consumer reads reconciliation-engine-svc's events as the shared envelope library actually serializes them, not as a fixture
// that merely resembles them: if the library's wire names ever change, this fails here, not in production.
func TestParseWithdrawal_ReadsARealCanonicalEnvelope(t *testing.T) {
	env, err := envelope.New(envelope.Spec{
		Type: "com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn", Service: "reconciliation-engine-svc", SchemaVersion: "1.0.0",
		OccurredAt: time.Now().UTC(), TenantID: tenantA, LegalEntityID: "aaaaaaaa-0000-0000-0000-00000000000a",
		AggregateType: "reconciliation-certification", AggregateID: "rct_1", CorrelationID: "corr-1", ActorID: "controller-erin",
		ResidencyRegion: "uk", Classification: envelope.Confidential,
		Data: map[string]any{
			"reliance_id": "rrl_1", "certification_id": "rct_1", "run_id": "rcr_b", "revocation_id": "rcv_2",
			"relying_service": "general-ledger-svc", "relying_object_type": "journal", "relying_object_id": "j1",
		},
	})
	if err != nil {
		t.Fatalf("the library refused the event type the producer emits: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	w, relevant, err := ParseWithdrawal(raw)
	if err != nil || !relevant || w.TenantID != tenantA || w.Ref.String() != certB || w.RevocationID != "rcv_2" || w.RelianceID != "rrl_1" {
		t.Fatalf("%+v relevant=%v err=%v\nenvelope: %s", w, relevant, err, raw)
	}
	// And a real envelope for another relying service is not this service's.
	other, _ := envelope.New(envelope.Spec{
		Type: "com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn", Service: "reconciliation-engine-svc", SchemaVersion: "1.0.0",
		OccurredAt: time.Now().UTC(), TenantID: tenantA, AggregateType: "reconciliation-certification", AggregateID: "rct_1", CorrelationID: "c",
		ActorID: "a", ResidencyRegion: "uk", Classification: envelope.Confidential,
		Data: map[string]any{"run_id": "rcr_b", "relying_service": "tax-svc"},
	})
	raw2, _ := json.Marshal(other)
	if _, relevant, err := ParseWithdrawal(raw2); relevant || err != nil {
		t.Fatalf("another relying service: relevant=%v err=%v", relevant, err)
	}
}
