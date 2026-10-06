package domain

import "time"

// ZS-SVC-Y-001 NCD-04 section 7.1: provider telemetry is normalized into explicit evidence
// facts with a known strength and stated limits, and is "never promoted into claims it
// cannot support". Nothing in this vocabulary says a person saw, read or acknowledged a
// message: those facts need an authenticated human response (NCD-05), and no provider
// callback can produce one.

// Evidence facts a provider callback can establish for an email.
const (
	EvidenceMailboxAccepted = "MAILBOX_ACCEPTED" // the receiving mail server took the message
	EvidenceBounced         = "BOUNCED"          // permanent failure (hard bounce)
	EvidenceDeferred        = "DEFERRED"         // temporary failure (soft or transient bounce)
	EvidenceRejected        = "REJECTED"         // the provider dropped it before sending
	EvidenceComplaint       = "COMPLAINT"        // the recipient reported abuse
	EvidenceUnsubscribed    = "UNSUBSCRIBED"     // the recipient opted out
)

// Strength says how far a fact can be trusted as evidence of delivery.
const (
	StrengthProviderLevel   = "PROVIDER_LEVEL"   // the provider's own action
	StrengthMailboxLevel    = "MAILBOX_LEVEL"    // a mail server's answer; not proof anyone read it
	StrengthRecipientSignal = "RECIPIENT_SIGNAL" // the recipient did something (can be forged or automated)
)

// EvidenceFacts and EvidenceStrengths are the closed sets (the database CHECKs match).
var (
	EvidenceFacts     = []string{EvidenceMailboxAccepted, EvidenceBounced, EvidenceDeferred, EvidenceRejected, EvidenceComplaint, EvidenceUnsubscribed}
	EvidenceStrengths = []string{StrengthProviderLevel, StrengthMailboxLevel, StrengthRecipientSignal}
)

// evidenceLimits are the stated limits of each fact, shown with it so no reader has to
// remember what a MAILBOX_ACCEPTED does not prove.
var evidenceLimits = map[string]string{
	EvidenceMailboxAccepted: "The receiving mail server accepted the message. It does not prove the recipient saw it.",
	EvidenceBounced:         "The message could not be delivered. The endpoint is unusable until corrected.",
	EvidenceDeferred:        "Delivery was temporarily refused. It may still be delivered or may later bounce.",
	EvidenceRejected:        "The provider refused to send the message. Nothing reached the recipient's server.",
	EvidenceComplaint:       "The recipient (or their provider) reported the message as abuse. This is a signal, not a verified fact.",
	EvidenceUnsubscribed:    "The recipient asked not to receive this kind of message.",
}

// EvidenceLimits returns the stated limits of a fact.
func EvidenceLimits(fact string) string { return evidenceLimits[fact] }

// NormalizeEvidence maps a provider callback to a normalized fact and its strength. The
// event type and the bounce class use the webhook package's spellings; an event this
// vocabulary cannot honestly describe returns ok=false and is not recorded as evidence.
func NormalizeEvidence(eventType, bounceClass string) (fact, strength string, ok bool) {
	switch eventType {
	case "DELIVERED":
		return EvidenceMailboxAccepted, StrengthMailboxLevel, true
	case "BOUNCE":
		if bounceClass == "SOFT" || bounceClass == "TRANSIENT" {
			return EvidenceDeferred, StrengthMailboxLevel, true
		}
		return EvidenceBounced, StrengthMailboxLevel, true
	case "DROPPED":
		return EvidenceRejected, StrengthProviderLevel, true
	case "COMPLAINT":
		return EvidenceComplaint, StrengthRecipientSignal, true
	case "UNSUBSCRIBE":
		return EvidenceUnsubscribed, StrengthRecipientSignal, true
	}
	return "", "", false
}

// DeliveryEvidence is one normalized fact about one attempt. It is append-only: a later
// callback adds a fact, it never edits an earlier one, so a late correction is visible as
// a later fact and not as a rewritten history.
type DeliveryEvidence struct {
	EvidenceID     string    `json:"evidence_id"`
	TenantID       string    `json:"tenant_id"`
	NotificationID string    `json:"notification_id"`
	AttemptID      string    `json:"attempt_id"`
	SourceEventID  string    `json:"source_event_id"`
	Provider       string    `json:"provider"`
	Fact           string    `json:"fact"`
	Strength       string    `json:"strength"`
	Limits         string    `json:"limits"`
	Diagnostic     string    `json:"diagnostic,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// MaxDiagnosticLength bounds the provider's free text we keep: complaint feedback can
// carry personal data and is minimized (7.3), so we keep a short code, never a payload.
const MaxDiagnosticLength = 200
