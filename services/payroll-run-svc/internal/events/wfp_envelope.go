package events

import (
	"encoding/json"
	"fmt"
	"time"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// BuildWFPEnvelope renders a WFP event in the platform envelope (Doc 03 §19).
//
// It is called at WRITE time, inside the transaction that changes state, so the
// envelope carries the actor and correlation of the request that caused the
// change rather than whatever the relay happens to know when it drains. The
// outbox stores the result verbatim.
func BuildWFPEnvelope(eventType, correlationID, causationID, actorID, tenantID, legalEntityID, jurisdiction string, payload any) ([]byte, error) {
	if !domain.EventCatalogue[eventType] {
		return nil, fmt.Errorf("event type %q is not in the WFP catalogue", eventType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	type wfpEnvelope struct {
		envelope
		CausationID string `json:"causation_id,omitempty"`
	}
	return json.Marshal(wfpEnvelope{
		envelope: envelope{
			EventType:     eventType,
			EventVersion:  "1.0",
			EmittedAt:     time.Now().UTC(),
			SchemaVersion: "1.0",
			SourceService: "payroll-run-svc",
			CorrelationID: correlationID,
			TenantID:      tenantID,
			LegalEntityID: legalEntityID,
			ActorID:       actorID,
			Jurisdiction:  jurisdiction,
			Payload:       raw,
		},
		CausationID: causationID,
	})
}
