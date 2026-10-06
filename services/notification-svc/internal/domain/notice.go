package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ZS-SVC-Y-001 NCD-05 (sections 8.1 to 8.5): the regulated notice package.
//
// What this proves: what the platform prepared, attempted, transmitted, observed and
// received as acknowledgement. What it never claims (8.5): that service was legally
// effective. No status here means "legal service complete", and provider acceptance alone
// never advances a notice past DELIVERY_IN_PROGRESS.

// Notice lifecycle (Figure 8).
const (
	NoticePrepared          = "PREPARED"
	NoticeReady             = "READY"
	NoticeDeliveryInProgess = "DELIVERY_IN_PROGRESS"
	NoticeDeliveryEvidenced = "DELIVERY_EVIDENCED"
	NoticeException         = "EXCEPTION"
	NoticeSatisfiedByPolicy = "SATISFIED_BY_POLICY"
	NoticeAckPending        = "ACK_PENDING"
	NoticeAcknowledged      = "ACKNOWLEDGED"
	NoticeDeclined          = "DECLINED"
	NoticeExpired           = "EXPIRED"
	NoticeDisputed          = "DISPUTED"
)

// Acknowledgement requirements.
const (
	AckNone             = "NONE"
	AckReceipt          = "RECEIPT"
	AckAcceptDecline    = "ACCEPTANCE_DECLINE"
	AckMethodAuthAction = "AUTHENTICATED_ACTION"
)

// Recipient actions on a notice.
const (
	ActionAcknowledge = "ACKNOWLEDGE"
	ActionAccept      = "ACCEPT"
	ActionDecline     = "DECLINE"
	ActionDispute     = "DISPUTE"
)

// Evidence classes at which a communication is a regulated notice (E3 and E4).
var RegulatedEvidenceClasses = []string{"E3", "E4"}

var (
	ErrNoticeNotFound        = errors.New("notice not found")
	ErrNoticeInvalid         = errors.New("notice is invalid")
	ErrNoticeState           = errors.New("notice is not in a state that allows this")
	ErrNotNoticeRecipient    = errors.New("only the notice's recipient may respond to it")
	ErrNoticeAlreadyAnswered = errors.New("this notice has already been answered")
	ErrAckNotRequired        = errors.New("this notice does not call for an acknowledgement")
	ErrOperatorAckRefused    = errors.New("an operator cannot acknowledge on a recipient's behalf")
)

// NoticeProblem explains what is wrong with a notice.
type NoticeProblem struct{ Msg string }

func (p NoticeProblem) Error() string { return p.Msg }
func (p NoticeProblem) Unwrap() error { return ErrNoticeInvalid }

// Notice is one exact version of a regulated notice.
type Notice struct {
	NoticeID             string     `json:"notice_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	LineageID            string     `json:"lineage_id"`
	VersionNumber        int        `json:"version_number"`
	SupersedesNoticeID   *string    `json:"supersedes_notice_id,omitempty"`
	CorrectionReason     *string    `json:"correction_reason,omitempty"`
	IntentVersionID      string     `json:"intent_version_id"`
	RecipientPrincipalID string     `json:"recipient_principal_id"`
	RecipientCapacity    string     `json:"recipient_capacity"`
	Channel              string     `json:"channel"`
	Locale               string     `json:"locale"`
	Subject              string     `json:"subject"`
	Body                 string     `json:"body"`
	ContentHash          string     `json:"content_hash"`
	PolicyRef            string     `json:"policy_ref"`
	EffectiveDate        *string    `json:"effective_date,omitempty"`
	AckRequirement       string     `json:"ack_requirement"`
	DeadlineAt           *time.Time `json:"deadline_at,omitempty"`
	Status               string     `json:"status"`
	NotificationID       *string    `json:"notification_id,omitempty"`
	SupersededByNoticeID *string    `json:"superseded_by_notice_id,omitempty"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
}

// NoticeTransition is one entry of a notice's append-only history.
type NoticeTransition struct {
	EventID          string    `json:"event_id"`
	NoticeID         string    `json:"notice_id"`
	FromStatus       string    `json:"from_status,omitempty"`
	ToStatus         string    `json:"to_status"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Reason           string    `json:"reason"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// NoticeAck is a recipient's response to one notice version.
type NoticeAck struct {
	AckID            string    `json:"ack_id"`
	NoticeID         string    `json:"notice_id"`
	Action           string    `json:"action"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Method           string    `json:"method"`
	Comment          string    `json:"comment,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// CreateNoticeParams prepares a notice (or, with Supersedes set, a correcting version).
type CreateNoticeParams struct {
	LegalEntityID        string
	IntentVersionID      string
	RecipientPrincipalID string
	Locale               string
	Subject              string
	Body                 string
	PolicyRef            string
	EffectiveDate        *string
	AckRequirement       string
	DeadlineAt           *time.Time
	CreatedByPrincipalID string

	// Set only for a correction.
	SupersedesNoticeID string
	CorrectionReason   string
}

const (
	maxNoticeSubject = 255
	maxNoticeBody    = 200_000
)

// NoticeContentHash fixes the exact content served: the same subject, body and locale
// always hash the same, and any change hashes differently. Fields are length-prefixed
// through JSON so no two different contents can share a serialization.
func NoticeContentHash(subject, body, locale string) string {
	raw, _ := json.Marshal([]string{subject, body, locale})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateNotice checks everything about a notice that can be known without storing it,
// against the intent version it is sent under.
func ValidateNotice(p CreateNoticeParams, iv *IntentVersion, now time.Time) error {
	if p.RecipientPrincipalID == "" || p.LegalEntityID == "" {
		return NoticeProblem{"recipient_principal_id and legal_entity_id are required"}
	}
	if iv == nil || iv.VersionID != p.IntentVersionID {
		return NoticeProblem{"the notice must be sent under a communication intent version"}
	}
	if iv.LegalEntityID != p.LegalEntityID {
		return NoticeProblem{"the intent belongs to a different legal entity"}
	}
	if !inSet(RegulatedEvidenceClasses, iv.EvidenceClass) {
		return NoticeProblem{fmt.Sprintf("a regulated notice needs an intent of evidence class %s; this intent is %s",
			strings.Join(RegulatedEvidenceClasses, " or "), iv.EvidenceClass)}
	}
	if !iv.ChannelAllowed(ChannelEmail) {
		return NoticeProblem{"the intent does not allow the EMAIL channel, the only route regulated notices have"}
	}
	if !ValidDirectPathClass(iv.PurposeClass) {
		return NoticeProblem{"the intent's purpose class " + iv.PurposeClass + " is not available on the direct send path"}
	}
	if strings.TrimSpace(p.Subject) == "" || strings.TrimSpace(p.Body) == "" {
		return NoticeProblem{"subject and body are required"}
	}
	if len(p.Subject) > maxNoticeSubject || len(p.Body) > maxNoticeBody {
		return NoticeProblem{fmt.Sprintf("subject is at most %d and body at most %d characters", maxNoticeSubject, maxNoticeBody)}
	}
	if strings.TrimSpace(p.PolicyRef) == "" || len(p.PolicyRef) > 255 {
		return NoticeProblem{"policy_ref names the legal or policy basis (a PDC rule reference) and is required"}
	}
	if !inSet([]string{AckNone, AckReceipt, AckAcceptDecline}, p.AckRequirement) {
		return NoticeProblem{"ack_requirement must be one of NONE, RECEIPT, ACCEPTANCE_DECLINE"}
	}
	if p.AckRequirement != AckNone && p.DeadlineAt == nil {
		return NoticeProblem{"a notice that calls for a response needs a deadline_at (owned by the workflow that requires it)"}
	}
	if p.DeadlineAt != nil && !p.DeadlineAt.After(now) {
		return NoticeProblem{"deadline_at must be in the future"}
	}
	if p.EffectiveDate != nil {
		if _, err := time.Parse("2006-01-02", *p.EffectiveDate); err != nil {
			return NoticeProblem{"effective_date is written YYYY-MM-DD"}
		}
	}
	return nil
}

// AckOutcome maps a recipient action to the notice status it produces, or an error when the
// action does not fit what the notice asked for.
func AckOutcome(requirement, action string) (string, error) {
	switch requirement {
	case AckNone:
		return "", ErrAckNotRequired
	case AckReceipt:
		if action == ActionAcknowledge {
			return NoticeAcknowledged, nil
		}
	case AckAcceptDecline:
		switch action {
		case ActionAccept:
			return NoticeAcknowledged, nil
		case ActionDecline:
			return NoticeDeclined, nil
		}
	}
	if action == ActionDispute {
		return NoticeDisputed, nil
	}
	return "", NoticeProblem{fmt.Sprintf("action %q does not fit a notice that requires %s", action, requirement)}
}

// NoticeProgress is what a notice should do next given the delivery facts.
type NoticeProgress struct {
	To     string
	Reason string
}

// DecideNoticeProgress is the one rule for moving a notice forward, kept pure so it can be
// tested without a database. It returns ok=false when the notice should stay where it is.
//
// The delivery threshold is the heart of the doctrine: a provider accepting a message
// (notification SENT) is NOT enough. The receiving mail server must have accepted it
// (MAILBOX_ACCEPTED), and a bounce or a failed delivery is an exception, never evidence.
func DecideNoticeProgress(n *Notice, notificationStatus, notificationFailure string, facts []DeliveryEvidence, now time.Time) (NoticeProgress, bool) {
	switch n.Status {
	case NoticeDeliveryInProgess:
		bounced, accepted := false, false
		for _, f := range facts {
			switch f.Fact {
			case EvidenceBounced:
				bounced = true
			case EvidenceMailboxAccepted:
				accepted = true
			}
		}
		switch {
		case notificationStatus == StatusFailed:
			return NoticeProgress{NoticeException, "the delivery failed: " + notificationFailure}, true
		case bounced:
			return NoticeProgress{NoticeException, "the message bounced (permanent failure reported by the receiving server)"}, true
		case notificationStatus == StatusSent && accepted:
			return NoticeProgress{NoticeDeliveryEvidenced, "the receiving mail server accepted the message (MAILBOX_ACCEPTED); this does not prove the recipient saw it"}, true
		case n.DeadlineAt != nil && !now.Before(*n.DeadlineAt):
			return NoticeProgress{NoticeException, "the deadline passed before delivery could be evidenced"}, true
		}
	case NoticeDeliveryEvidenced:
		if n.AckRequirement == AckNone {
			return NoticeProgress{NoticeSatisfiedByPolicy, "no acknowledgement is required; satisfied by policy, which is not a finding of legal service"}, true
		}
		return NoticeProgress{NoticeAckPending, "delivery evidenced; waiting for the recipient's response"}, true
	case NoticeAckPending:
		if n.DeadlineAt != nil && !now.Before(*n.DeadlineAt) {
			return NoticeProgress{NoticeExpired, "no response by the deadline; the workflow decides what follows, and no acknowledgement is assumed"}, true
		}
	}
	return NoticeProgress{}, false
}
