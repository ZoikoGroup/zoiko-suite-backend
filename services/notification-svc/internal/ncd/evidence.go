package ncd

import (
	"strings"
	"time"
)

// ── §6.2 attempt state machine ──────────────────────────────────────────────

// transitions mirrors migration 000018's ncd_attempt_transition trigger. The
// trigger is the enforcement; this copy lets the store decide, before it
// writes, whether an incoming fact moves the attempt or only adds evidence.
var transitions = map[string][]string{
	AttemptCreated:    {AttemptSubmitting, AttemptFailed},
	AttemptSubmitting: {AttemptAccepted, AttemptPending, AttemptUnknown, AttemptDelivered, AttemptFailed, AttemptBounced},
	AttemptAccepted:   {AttemptPending, AttemptDelivered, AttemptFailed, AttemptBounced},
	AttemptPending:    {AttemptDelivered, AttemptFailed, AttemptBounced},
	AttemptUnknown:    {AttemptAccepted, AttemptDelivered, AttemptFailed, AttemptBounced},
	AttemptDelivered:  {AttemptBounced},
}

// CanTransition reports whether from → to is a §6.2 transition.
func CanTransition(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Terminal reports whether an attempt state admits no further move except
// the DELIVERED → BOUNCED correction.
func Terminal(state string) bool {
	return state == AttemptFailed || state == AttemptBounced
}

// ── §3.3 "sent" is never overloaded ─────────────────────────────────────────

// DeliveryState is the precise transport term for an attempt state. There is
// deliberately no "SENT": each value names exactly one proposition of §3.3's
// table, and nothing here claims human receipt or legal service.
func DeliveryState(channel, attemptState string) string {
	switch attemptState {
	case AttemptCreated:
		return "CREATED"
	case AttemptSubmitting:
		return "SUBMITTED_TO_PROVIDER"
	case AttemptAccepted:
		return "PROVIDER_ACCEPTED"
	case AttemptPending:
		return "PROVIDER_PENDING"
	case AttemptUnknown:
		return "DELIVERY_UNKNOWN"
	case AttemptDelivered:
		if channel == ChannelInApp {
			return "DELIVERED_TO_INBOX"
		}
		if channel == ChannelEmail {
			return "DELIVERED_TO_MAILBOX"
		}
		return "DELIVERED_TO_NETWORK"
	case AttemptFailed:
		return "FAILED"
	case AttemptBounced:
		return "BOUNCED"
	}
	return "NOT_ATTEMPTED"
}

// stateRank orders attempt states by the strength of the transport claim, for
// summarizing a communication with several attempts.
func stateRank(s string) int {
	switch s {
	case AttemptDelivered:
		return 6
	case AttemptAccepted:
		return 5
	case AttemptPending:
		return 4
	case AttemptUnknown:
		return 3
	case AttemptSubmitting:
		return 2
	case AttemptCreated:
		return 1
	case AttemptBounced, AttemptFailed:
		return 0
	}
	return -1
}

// Claims is the §3.3 evidence summary of one communication: each proposition
// separately, with what supports it. LegallyServed is never computed.
type Claims struct {
	DeliveryState       string `json:"delivery_state"`
	SubmittedToProvider bool   `json:"submitted_to_provider"`
	ProviderAccepted    bool   `json:"provider_accepted"`
	Delivered           bool   `json:"delivered"`
	OpenedOrDisplayed   bool   `json:"opened_or_displayed"`
	OpenSignalOnly      bool   `json:"open_signal_only"`
	Acknowledged        bool   `json:"acknowledged"`
	AcknowledgmentState string `json:"acknowledgment_state"`
	LegallyServed       string `json:"legally_served"`
	Statement           string `json:"statement"`
}

// Summarize computes Claims from attempts, evidence and acknowledgment state.
func Summarize(attempts []Attempt, evidence []Evidence, ackState string) Claims {
	c := Claims{DeliveryState: "NOT_ATTEMPTED", LegallyServed: LegalSufficiencyNotDetermined, AcknowledgmentState: ackState}
	best := -2
	for _, a := range attempts {
		if a.State != AttemptCreated {
			c.SubmittedToProvider = true
		}
		if r := stateRank(a.State); r > best {
			best = r
			c.DeliveryState = DeliveryState(a.Channel, a.State)
		}
	}
	for _, e := range evidence {
		switch e.EvidenceType {
		case "PROVIDER_ACCEPTED":
			c.ProviderAccepted = true
		case "MAILBOX_ACCEPTED", "NETWORK_DELIVERY_STATUS", "IN_APP_DELIVERED", "PUSH_ACCEPTED":
			c.ProviderAccepted = true
			c.Delivered = true
		case "SHOWN", "OPENED", "ACTIONED":
			c.OpenedOrDisplayed = true
		case "OPEN_SIGNAL":
			// NP-37/38: a pixel is non-authoritative in both directions.
			c.OpenSignalOnly = !c.OpenedOrDisplayed
		case "ACKNOWLEDGED":
			c.Acknowledged = true
		case "UNUSED":
		}
	}
	switch {
	case c.Acknowledged:
		c.Statement = "acknowledged by an authenticated actor against the exact notice version; legal sufficiency is determined outside NCD"
	case c.OpenedOrDisplayed:
		c.Statement = "displayed to the authenticated recipient; not an acknowledgment"
	case c.Delivered:
		c.Statement = "delivered to the recipient's mailbox, network or inbox; not proof anyone read it"
	case c.ProviderAccepted:
		c.Statement = "a provider accepted the message; not proof of delivery, reading or service"
	default:
		c.Statement = "no transport claim is supported yet"
	}
	return c
}

// ── §7.1 evidence normalization ─────────────────────────────────────────────

// Normalized is what one raw provider or interaction event means.
type Normalized struct {
	EvidenceType    string
	NormalizedState string
	Confidence      string
	DoesNotProve    string
	// AttemptState is the state the attempt should move to, or "" when the
	// fact adds evidence without a transport transition (an open, a click,
	// a complaint).
	AttemptState string
	// Suppression is the canonical suppression the fact creates, or "".
	Suppression      string
	SuppressionScope string // purpose scope
}

// NormalizeProviderEvent maps a provider callback's event type to an evidence
// fact. Unknown kinds return ok=false and change nothing.
func NormalizeProviderEvent(channel, kind string) (Normalized, bool) {
	switch strings.ToLower(kind) {
	case "accepted", "queued":
		return Normalized{"PROVIDER_ACCEPTED", "ACCEPTED", "HIGH",
			"mailbox/device delivery, human receipt, legal service", AttemptAccepted, "", ""}, true
	case "rejected":
		return Normalized{"PROVIDER_REJECTED", "REJECTED", "HIGH",
			"anything about the recipient's endpoint beyond the provider's own refusal", AttemptFailed, "", ""}, true
	case "delivered":
		t := "MAILBOX_ACCEPTED"
		if channel == ChannelSMS {
			t = "NETWORK_DELIVERY_STATUS"
		} else if channel == ChannelPush {
			t = "PUSH_ACCEPTED"
		}
		return Normalized{t, "DELIVERED", "MEDIUM",
			"human access, reading, comprehension, identity or legal service", AttemptDelivered, "", ""}, true
	case "deferred":
		return Normalized{"DEFERRED", "PENDING", "MEDIUM",
			"eventual delivery or failure", AttemptPending, "", ""}, true
	case "bounced", "hard_bounce":
		return Normalized{"BOUNCED", "HARD_BOUNCE", "HIGH",
			"that the recipient refused the content; only that the endpoint is unusable", AttemptBounced, SuppHardBounce, "ALL"}, true
	case "soft_bounce", "soft_bounced":
		return Normalized{"DEFERRED", "SOFT_BOUNCE", "MEDIUM",
			"a permanent endpoint failure", AttemptPending, SuppSoftBounce, "ALL"}, true
	case "complaint", "spam_report":
		return Normalized{"COMPLAINT", "COMPLAINT", "HIGH",
			"non-receipt; a complaint implies the message arrived", "", SuppComplaint, string(PurposeMarketing)}, true
	case "token_invalid", "invalid_endpoint":
		return Normalized{"TOKEN_INVALID", "ENDPOINT_INVALID", "HIGH",
			"anything about the recipient beyond the endpoint being invalid", AttemptFailed, SuppEndpointInvalid, "ALL"}, true
	case "opened", "open":
		return Normalized{"OPEN_SIGNAL", "OPEN_SIGNAL", "LOW",
			"human reading, identity or acknowledgment; may be a proxy or prefetch (NP-38), and its absence proves nothing (NP-37)", "", "", ""}, true
	case "clicked", "click":
		return Normalized{"LINK_ACTION", "LINK_ACTION", "LOW",
			"the actor's identity or an acknowledgment (NP-39)", "", "", ""}, true
	}
	return Normalized{}, false
}

// SubmitOutcome is what a transport reported for one submission.
type SubmitOutcome struct {
	Accepted          bool
	DeliveredNow      bool // IN_APP: recording is delivery to the inbox
	Unknown           bool
	Retryable         bool
	Reason            string
	ProviderName      string
	ProviderMessageID string
}

// NormalizeSubmit maps a synchronous submission result to attempt state and
// evidence (§7.1 "API submit response").
func NormalizeSubmit(channel string, o SubmitOutcome) Normalized {
	switch {
	case o.Unknown:
		return Normalized{"PROVIDER_UNKNOWN", "UNKNOWN", "LOW",
			"either delivery or non-delivery; the original attempt must be reconciled before any resend (INV-12)", AttemptUnknown, "", ""}
	case o.DeliveredNow && channel == ChannelInApp:
		return Normalized{"IN_APP_DELIVERED", "DELIVERED", "HIGH",
			"that the recipient has seen or read it", AttemptDelivered, "", ""}
	case o.Accepted:
		return Normalized{"PROVIDER_ACCEPTED", "ACCEPTED", "HIGH",
			"mailbox/device delivery, human receipt, legal service", AttemptAccepted, "", ""}
	default:
		return Normalized{"PROVIDER_REJECTED", "REJECTED", "HIGH",
			"anything beyond the provider's own refusal", AttemptFailed, "", ""}
	}
}

// ResolutionDue is how long an UNKNOWN attempt may stay unresolved before it
// becomes a human exception (§6.2 "unresolved until expiry → EXCEPTION").
const ResolutionDue = 30 * time.Minute
