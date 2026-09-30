package domain

import "encoding/json"

// ZS-SVC-Y-001 §3.3 is labelled a NON-BYPASSABLE semantic rule: "NCD must
// never expose one generic sent=true status." The register stores PENDING /
// SENT / FAILED / PENDING_UNKNOWN, and SENT meant two different things — "a
// provider accepted it" for EMAIL, "it is in the recipient's inbox" for
// IN_APP — while the API, the console and every consumer saw the same word.
//
// The stored value is unchanged (every query, constraint and the retry worker
// key on it). What leaves the service is the precise proposition: the JSON
// `status` of a notification is its DeliveryState, and `stored_status` keeps
// the raw column for anyone reconciling against the database.

// Precise delivery states for the register.
const (
	DeliveryPending          = "PENDING"
	DeliveryRetryScheduled   = "RETRY_SCHEDULED"
	DeliveryProviderAccepted = "PROVIDER_ACCEPTED"
	DeliveryToInbox          = "DELIVERED_TO_INBOX"
	DeliveryUnknown          = "DELIVERY_UNKNOWN"
	DeliveryFailed           = "FAILED"
)

// DeliveryStateOf names what the row's evidence actually supports.
func DeliveryStateOf(n Notification) string {
	switch n.Status {
	case StatusSent:
		if n.Channel == ChannelInApp {
			return DeliveryToInbox
		}
		return DeliveryProviderAccepted
	case StatusFailed:
		return DeliveryFailed
	case StatusPendingUnknown:
		return DeliveryUnknown
	case StatusPending:
		if n.NextAttemptAt != nil {
			return DeliveryRetryScheduled
		}
		return DeliveryPending
	}
	return n.Status
}

// EvidenceStatementOf is the one-line claim a reader may make from the state.
func EvidenceStatementOf(n Notification) string {
	switch DeliveryStateOf(n) {
	case DeliveryToInbox:
		if n.ReadAt != nil {
			return "in the recipient's authenticated inbox and opened by them; not an acknowledgment"
		}
		return "in the recipient's authenticated inbox; not proof they have read it"
	case DeliveryProviderAccepted:
		return "a mail provider accepted the message; not proof of delivery to the mailbox, reading or legal service"
	case DeliveryUnknown:
		return "the provider's answer was lost; the attempt must be reconciled before any resend"
	case DeliveryFailed:
		return "delivery concluded without success"
	}
	return "delivery has not concluded"
}

// StoredStatusFor maps a precise state (or a stored value) back to the
// column value — for the ?status= filter and for clients decoding responses.
func StoredStatusFor(s string) string {
	switch s {
	case DeliveryProviderAccepted, DeliveryToInbox:
		return StatusSent
	case DeliveryUnknown:
		return StatusPendingUnknown
	case DeliveryRetryScheduled:
		return StatusPending
	}
	return s
}

type notificationAlias Notification

// MarshalJSON exposes the precise state as `status` (§3.3).
func (n Notification) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		notificationAlias
		Status          string `json:"status"`
		DeliveryState   string `json:"delivery_state"`
		StoredStatus    string `json:"stored_status"`
		EvidenceClaim   string `json:"evidence_claim"`
		LegallyServed   string `json:"legally_served"`
	}{
		notificationAlias: notificationAlias(n),
		Status:            DeliveryStateOf(n),
		DeliveryState:     DeliveryStateOf(n),
		StoredStatus:      n.Status,
		EvidenceClaim:     EvidenceStatementOf(n),
		LegallyServed:     "NOT_DETERMINED_BY_NCD",
	})
}

// UnmarshalJSON accepts either vocabulary and restores the stored value.
func (n *Notification) UnmarshalJSON(b []byte) error {
	var aux struct {
		notificationAlias
		Status       string `json:"status"`
		StoredStatus string `json:"stored_status"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*n = Notification(aux.notificationAlias)
	n.Status = StoredStatusFor(aux.Status)
	if aux.StoredStatus != "" {
		n.Status = aux.StoredStatus
	}
	return nil
}
