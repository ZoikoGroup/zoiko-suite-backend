package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/leave-absence-svc/internal/domain"
)

// BuildWFPEnvelope renders a WFP-02 event in the platform envelope (Doc 03 §19).
// It runs at WRITE time, inside the transaction that changes state, so the
// envelope carries the actor and correlation of the request that caused the
// change. The outbox stores the result verbatim.
func BuildWFPEnvelope(eventType, correlationID, causationID, actorID, tenantID, legalEntityID, jurisdiction string, payload any) ([]byte, error) {
	if !domain.EventCatalogue[eventType] {
		return nil, fmt.Errorf("event type %q is not in the WFP-02 catalogue", eventType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	return json.Marshal(struct {
		envelope
		CausationID  string `json:"causation_id,omitempty"`
		Jurisdiction string `json:"jurisdiction,omitempty"`
	}{
		envelope: envelope{
			EventID:       uuid.NewString(),
			EventType:     eventType,
			EventVersion:  "1.0",
			EmittedAt:     time.Now().UTC(),
			SchemaVersion: "1.0",
			SourceService: "leave-absence-svc",
			TenantID:      tenantID,
			LegalEntityID: legalEntityID,
			ActorID:       actorID,
			CorrelationID: correlationID,
			Payload:       raw,
		},
		CausationID:  causationID,
		Jurisdiction: jurisdiction,
	})
}
