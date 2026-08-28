// Package domain defines the canonical types for privacy-consent-svc —
// PRV-02, "Notice, Consent & Preference Evidence Service", from
// ZS-SVC-W-001 (Privacy/Consent/Purpose/Data Rights Control).
//
// v1 scope, documented rather than hidden (same doctrine as
// privacy-purpose-registry-svc's PRV-01):
//
//   - This is the second of five ZS-SVC-W-001 services. It depends on
//     PRV-01 (privacy-purpose-registry-svc) for purpose validity — every
//     ConsentReceipt's purpose_id is checked against a REAL call to that
//     service's GET /privacy/purposes/{id}, not trusted as an opaque
//     string. A purpose that doesn't resolve to a currently PUBLISHED
//     version is rejected (PRV-001/PRV-006-shaped error), never silently
//     accepted.
//   - Every evidence table (consent_receipts, withdrawal_receipts,
//     presentation_receipts, preference_assertions) is genuinely
//     append-only — enforced by a database trigger blocking ALL updates
//     and deletes, not just publish-once-then-immutable like PRV-01's
//     purpose_versions. PRV-I10/I11: withdrawing consent NEVER deletes or
//     edits the original grant; it adds a new fact (a WithdrawalReceipt)
//     that changes the CURRENT resolved status without touching history.
//   - "Current consent status" for (subject_ref, purpose_id) is a
//     DERIVED READ, not a stored column: the latest ConsentReceipt for
//     that pair, downgraded to WITHDRAWN if any WithdrawalReceipt
//     references it. There is no mutable "status" field anywhere in this
//     schema — see ResolveConsentStatus.
//   - PreferenceAssertion is deliberately a separate table with no
//     relationship to ConsentReceipt at the schema level (PRV-I12:
//     preferences never automatically become consent or lawful basis).
//   - PRV-03 (the runtime purpose-binding decision endpoint most other
//     services would actually call before processing personal data) is
//     NOT built. This service lets a caller ask "what did this subject
//     say about this purpose" — it does not yet provide the single
//     "am I allowed right now" decision PRV-03 would centralize.
package domain

import "time"

// ── Notice ───────────────────────────────────────────────────────────────────

type NoticeVersionStatus string

const (
	NoticeStatusDraft      NoticeVersionStatus = "DRAFT"
	NoticeStatusApproved   NoticeVersionStatus = "APPROVED"
	NoticeStatusPublished  NoticeVersionStatus = "PUBLISHED"
	NoticeStatusSuperseded NoticeVersionStatus = "SUPERSEDED"
	NoticeStatusWithdrawn  NoticeVersionStatus = "WITHDRAWN"
)

var noticeTransitions = map[NoticeVersionStatus][]NoticeVersionStatus{
	NoticeStatusDraft:     {NoticeStatusApproved},
	NoticeStatusApproved:  {NoticeStatusPublished},
	NoticeStatusPublished: {NoticeStatusWithdrawn}, // SUPERSEDED is a side effect of publishing a successor, not a direct action — see store.PublishNoticeVersion
}

func ValidNoticeTransition(from, to NoticeVersionStatus) bool {
	for _, allowed := range noticeTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Notice is the stable identity. TenantID nil = platform-wide notice
// (Zoiko as independent controller), same doctrine as every nullable-
// tenant stable-identity table in this platform.
type Notice struct {
	NoticeID             string    `json:"notice_id"`
	TenantID             *string   `json:"tenant_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// NoticeVersion. ContentHash stands in for the real rendered notice
// content — this service does not own document storage (§3.1: DRC owns
// document bytes), so it records a hash/reference, matching evidence-
// manifest-svc's own doctrine of storing a pointer/snapshot rather than
// re-implementing another service's job.
type NoticeVersion struct {
	NoticeVersionID       string              `json:"notice_version_id"`
	NoticeID              string              `json:"notice_id"`
	Locale                string              `json:"locale"`
	Audience              string              `json:"audience"`
	ContentHash           string              `json:"content_hash"`
	VersionStatus         NoticeVersionStatus `json:"version_status"`
	EffectiveFrom         *time.Time          `json:"effective_from,omitempty"`
	SupersedesVersionID   *string             `json:"supersedes_version_id,omitempty"`
	ApprovedByPrincipalID *string             `json:"approved_by_principal_id,omitempty"`
	CreatedAt             time.Time           `json:"created_at"`
	CreatedByPrincipalID  string              `json:"created_by_principal_id"`
}

// PresentationReceipt is append-only evidence that a notice version was
// shown to a subject — PRV-I08: presentation and consent are separate
// evidence objects, recorded independently.
type PresentationReceipt struct {
	PresentationReceiptID string    `json:"presentation_receipt_id"`
	TenantID              *string   `json:"tenant_id,omitempty"`
	NoticeVersionID       string    `json:"notice_version_id"`
	SubjectRef            string    `json:"subject_ref"`
	Channel               string    `json:"channel"`
	Locale                string    `json:"locale"`
	CreatedAt             time.Time `json:"created_at"`
}

// ── Consent ──────────────────────────────────────────────────────────────────

type ConsentAction string

const (
	ConsentActionGranted ConsentAction = "GRANTED"
	ConsentActionDenied  ConsentAction = "DENIED"
)

func (a ConsentAction) Valid() bool {
	return a == ConsentActionGranted || a == ConsentActionDenied
}

// ConsentStatus is the DERIVED (never stored) resolved state for a
// (subject_ref, purpose_id) pair — see ResolveConsentStatus.
type ConsentStatus string

const (
	ConsentStatusNotRequested ConsentStatus = "NOT_REQUESTED"
	ConsentStatusGranted      ConsentStatus = "GRANTED"
	ConsentStatusDenied       ConsentStatus = "DENIED"
	ConsentStatusWithdrawn    ConsentStatus = "WITHDRAWN"
)

// ConsentReceipt is append-only evidence of one grant/deny event —
// PRV-I07: purpose/scope specific, never a universal boolean. Never
// updated or deleted once written (migration 000002's trigger).
type ConsentReceipt struct {
	ConsentReceiptID string        `json:"consent_receipt_id"`
	TenantID         *string       `json:"tenant_id,omitempty"`
	SubjectRef       string        `json:"subject_ref"`
	PurposeID        string        `json:"purpose_id"`
	NoticeVersionID  *string       `json:"notice_version_id,omitempty"`
	Action           ConsentAction `json:"action"`
	CaptureChannel   string        `json:"capture_channel"`
	ActorPrincipalID string        `json:"actor_principal_id"`
	CorrelationID    string        `json:"correlation_id,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

// WithdrawalReceipt is append-only evidence that a specific ConsentReceipt
// was withdrawn. PRV-I10: withdrawal never deletes its own evidence (the
// original ConsentReceipt row is untouched). PRV-I11: withdrawal affects
// future processing only — this row is a new fact, not a correction.
type WithdrawalReceipt struct {
	WithdrawalReceiptID    string    `json:"withdrawal_receipt_id"`
	TenantID               *string   `json:"tenant_id,omitempty"`
	ConsentReceiptID       string    `json:"consent_receipt_id"`
	WithdrawnByPrincipalID string    `json:"withdrawn_by_principal_id"`
	Channel                string    `json:"channel"`
	CreatedAt              time.Time `json:"created_at"`
}

// ConsentResolution is the read-side answer to "what is the current
// status of this subject's consent for this purpose" — computed from the
// evidence, never itself stored.
type ConsentResolution struct {
	SubjectRef        string             `json:"subject_ref"`
	PurposeID         string             `json:"purpose_id"`
	Status            ConsentStatus      `json:"status"`
	LatestReceipt     *ConsentReceipt    `json:"latest_receipt,omitempty"`
	WithdrawalReceipt *WithdrawalReceipt `json:"withdrawal_receipt,omitempty"`
}

// ── Preference ───────────────────────────────────────────────────────────────

type PreferenceValue string

const (
	PreferenceEnabled       PreferenceValue = "ENABLED"
	PreferenceDisabled      PreferenceValue = "DISABLED"
	PreferenceUnset         PreferenceValue = "UNSET"
	PreferenceNotApplicable PreferenceValue = "NOT_APPLICABLE"
)

func (v PreferenceValue) Valid() bool {
	switch v {
	case PreferenceEnabled, PreferenceDisabled, PreferenceUnset, PreferenceNotApplicable:
		return true
	}
	return false
}

// PreferenceAssertion is append-only, and deliberately has NO
// relationship to ConsentReceipt at the schema level — PRV-I12:
// preferences never automatically become consent or lawful basis. "Current"
// preference is the latest assertion for (subject_ref, channel_or_purpose).
type PreferenceAssertion struct {
	PreferenceAssertionID string          `json:"preference_assertion_id"`
	TenantID              *string         `json:"tenant_id,omitempty"`
	SubjectRef            string          `json:"subject_ref"`
	ChannelOrPurpose      string          `json:"channel_or_purpose"`
	Value                 PreferenceValue `json:"value"`
	Source                string          `json:"source"`
	CreatedAt             time.Time       `json:"created_at"`
}

// ── Request DTOs ─────────────────────────────────────────────────────────────

type CreateNoticeRequest struct {
	TenantID    string `json:"tenant_id,omitempty"`
	Locale      string `json:"locale"`
	Audience    string `json:"audience"`
	ContentHash string `json:"content_hash"`
}

type CreateNoticeVersionRequest struct {
	ParentVersionID string `json:"parent_version_id"`
	Locale          string `json:"locale"`
	Audience        string `json:"audience"`
	ContentHash     string `json:"content_hash"`
}

type RecordPresentationRequest struct {
	SubjectRef string `json:"subject_ref"`
	Channel    string `json:"channel"`
	Locale     string `json:"locale"`
}

type RecordConsentRequest struct {
	TenantID        string `json:"tenant_id,omitempty"`
	SubjectRef      string `json:"subject_ref"`
	PurposeID       string `json:"purpose_id"`
	NoticeVersionID string `json:"notice_version_id,omitempty"`
	Action          string `json:"action"`
	CaptureChannel  string `json:"capture_channel"`
}

type WithdrawConsentRequest struct {
	Channel string `json:"channel"`
}

type SetPreferenceRequest struct {
	TenantID         string `json:"tenant_id,omitempty"`
	SubjectRef       string `json:"subject_ref"`
	ChannelOrPurpose string `json:"channel_or_purpose"`
	Value            string `json:"value"`
	Source           string `json:"source"`
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrNoticeNotFound          = errorString("notice not found")
	ErrNoticeVersionNotFound   = errorString("notice version not found")
	ErrInvalidNoticeTransition = errorString("invalid notice version status transition")
	ErrConsentReceiptNotFound  = errorString("consent receipt not found")
	ErrAlreadyWithdrawn        = errorString("consent receipt is already withdrawn")
	ErrPurposeNotRegistered    = errorString("purpose is not a registered, published purpose")
	ErrStoreUnavailable        = errorString("privacy-consent store unavailable")
)
