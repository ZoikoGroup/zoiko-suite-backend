package events

import (
	"strings"

	"zoiko.io/authorization-svc/internal/domain"
)

// DecisionEvents builds the events a recorded decision publishes, for the
// store to write to outbox_events in the decision's own transaction:
// authorization.granted or authorization.denied, plus sod.violation.detected
// when the denial was a segregation-of-duties conflict. The same rules the
// handler applied when it published directly, so moving to the outbox changes
// when an event is sent, never which.
func DecisionEvents(d domain.AccessDecisionLog) ([]domain.OutboxMessage, error) {
	tenant := tenantOf(d)
	eventType := "authorization.denied"
	if d.DecisionOutcome == "GRANTED" {
		eventType = "authorization.granted"
	}
	msg, err := buildMessage(eventType, d.CorrelationID, tenant, d.LegalEntityID, d.PrincipalID, d.AccessDecisionID, decisionPayload(d))
	if err != nil {
		return nil, err
	}
	out := []domain.OutboxMessage{{EventType: eventType, Key: string(msg.Key), Value: msg.Value, TenantID: tenant}}

	if d.DecisionOutcome != "GRANTED" && strings.HasPrefix(d.DecisionBasis, "sod:") {
		// conflict_with= names the other held action; an own-object denial
		// has no other action — the conflict is the action itself.
		conflicting := d.ActionType
		if strings.HasPrefix(d.DecisionBasis, "sod:conflict_with=") {
			conflicting = d.DecisionBasis[len("sod:conflict_with="):]
		}
		sod, err := buildMessage("sod.violation.detected", d.CorrelationID, tenant, d.LegalEntityID, d.PrincipalID, d.AccessDecisionID, sodPayload(d, conflicting))
		if err != nil {
			return nil, err
		}
		out = append(out, domain.OutboxMessage{EventType: "sod.violation.detected", Key: string(sod.Key), Value: sod.Value, TenantID: tenant})
	}
	return out, nil
}

// NewEvent builds one event in the platform envelope for the outbox — for the
// handler's own commands (authorization.cache.invalidated), which carry no
// decision row.
func NewEvent(eventType, correlationID, tenantID, actorID, key string, payload map[string]any) (domain.OutboxMessage, error) {
	msg, err := buildMessage(eventType, correlationID, tenantID, "", actorID, key, payload)
	if err != nil {
		return domain.OutboxMessage{}, err
	}
	return domain.OutboxMessage{EventType: eventType, Key: string(msg.Key), Value: msg.Value, TenantID: tenantID}, nil
}

func decisionPayload(d domain.AccessDecisionLog) map[string]any {
	return map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"action_type":        d.ActionType,
		"decision_basis":     d.DecisionBasis,
		"decided_at":         d.DecidedAt,
	}
}

func sodPayload(d domain.AccessDecisionLog, conflictingAction string) map[string]any {
	return map[string]any{
		"access_decision_id": d.AccessDecisionID,
		"principal_id":       d.PrincipalID,
		"legal_entity_id":    d.LegalEntityID,
		"candidate_action":   d.ActionType,
		"conflicting_action": conflictingAction,
		"decided_at":         d.DecidedAt,
	}
}
