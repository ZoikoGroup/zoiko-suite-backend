package domain

import (
	"context"
	"time"
)

// Command names who runs which PBC command. The handler puts it on the
// request context; the store hands it to the database, where the history
// trigger (migration 000012) records it with every state change (ST-2).
type Command struct {
	Actor, Name, CorrelationID string
}

type commandKey struct{}

// WithCommand returns ctx carrying c.
func WithCommand(ctx context.Context, c Command) context.Context {
	return context.WithValue(ctx, commandKey{}, c)
}

// CommandFrom reads the command WithCommand stored.
func CommandFrom(ctx context.Context) (Command, bool) {
	c, ok := ctx.Value(commandKey{}).(Command)
	return c, ok
}

// EvidenceRequestTransition is one append-only history row of a PBC request.
type EvidenceRequestTransition struct {
	TransitionID          string     `json:"transition_id"`
	RequestID             string     `json:"request_id"`
	FromStatus            *string    `json:"from_status,omitempty"`
	ToStatus              string     `json:"to_status"`
	Command               string     `json:"command"`
	Actor                 string     `json:"actor"`
	ChangedFields         []string   `json:"changed_fields"`
	AssignedToPrincipalID *string    `json:"assigned_to_principal_id,omitempty"`
	DueAt                 *time.Time `json:"due_at,omitempty"`
	RecordVersion         int64      `json:"record_version"`
	CorrelationID         *string    `json:"correlation_id,omitempty"`
	RecordedAt            time.Time  `json:"recorded_at"`
}

// PBCAction is one command a caller could run on a request now (ST-2).
type PBCAction struct {
	Command string `json:"command"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Role    string `json:"role"` // MANAGER or ASSIGNEE
}

// PBCAvailableActions lists the commands the request's status allows for a
// caller who is the assignee and/or holds EVIDENCE_REQUEST_MANAGE. It mirrors
// the store's state guards; the commands themselves still enforce them.
func PBCAvailableActions(r *EvidenceRequest, isManager, isAssignee bool) []PBCAction {
	base := "/v1/pbc/requests/" + r.RequestID + "/"
	open := r.Status != EvidenceRequestClosed
	var out []PBCAction
	add := func(cmd, role string) {
		out = append(out, PBCAction{Command: cmd, Method: "POST", Path: base + cmd, Role: role})
	}
	if isManager {
		if r.Status == EvidenceRequestDraft {
			add("send", "MANAGER")
		}
		if open {
			add("reassign", "MANAGER")
			add("extend-due-date", "MANAGER")
			add("notes", "MANAGER")
		}
		if r.Status == EvidenceRequestResponseReceived {
			add("clarify", "MANAGER")
		}
		if r.Status == EvidenceRequestResponseReceived || r.Status == EvidenceRequestUnderEvaluation {
			add("mark-satisfied", "MANAGER")
		}
		if open {
			add("close", "MANAGER")
		}
	}
	if isAssignee {
		if r.Status == EvidenceRequestSent {
			add("view", "ASSIGNEE")
		}
		switch r.Status {
		case EvidenceRequestSent, EvidenceRequestViewed, EvidenceRequestClarificationRequired, EvidenceRequestResponseReceived:
			add("responses", "ASSIGNEE")
		}
		if open && !isManager {
			add("notes", "ASSIGNEE")
		}
	}
	if out == nil {
		out = []PBCAction{}
	}
	return out
}
