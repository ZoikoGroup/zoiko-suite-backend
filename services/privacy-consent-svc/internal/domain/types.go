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
//   - PRV-03 (privacy-decision-svc) provides runtime purpose-binding
//     decisions that callers evaluate before processing personal data.
//     This service (PRV-02) provides the authoritative evidence store
//     and derived status resolution ("what did this subject assert about
//     this purpose") which PRV-03 queries at runtime.
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
	NoticeVersionID string              `json:"notice_version_id"`
	NoticeID        string              `json:"notice_id"`
	Locale          string              `json:"locale"`
	Audience        string              `json:"audience"`
	ContentHash     string              `json:"content_hash"`
	VersionStatus   NoticeVersionStatus `json:"version_status"`
	EffectiveFrom   *time.Time          `json:"effective_from,omitempty"`
	// EffectiveTo closes the §10.1 "effective window" when a successor is
	// published — set, in the same transaction as the supersession, to the
	// successor's EffectiveFrom. Nil while this version is still PUBLISHED
	// (or was never published), which is a window still open, not a missing
	// fact.
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	SupersedesVersionID   *string    `json:"supersedes_version_id,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
	// PublishedByPrincipalID/PublishedAt are the §10.1 "publisher" evidence —
	// distinct from CreatedByPrincipalID (the maker) and ApprovedByPrincipalID
	// (the checker). Before this, PublishNoticeVersion recorded no actor at
	// all for the act of publishing, so the SoD check that refuses the maker
	// or the approver from publishing produced a row with no record of who
	// actually did.
	PublishedByPrincipalID *string    `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
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
	SessionRef            *string   `json:"session_ref,omitempty"`
	TemplateVersion       *string   `json:"template_version,omitempty"`
	DeliveryEvidence      *string   `json:"delivery_evidence,omitempty"`
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
// (subject_ref, purpose_id) pair — see ResolveConsentStatus. §6's
// "Orthogonal State Architecture" names six values; WITHDRAWN, EXPIRED and
// INVALIDATED are three distinct terminal facts, never collapsed into one.
type ConsentStatus string

const (
	ConsentStatusNotRequested ConsentStatus = "NOT_REQUESTED"
	ConsentStatusGranted      ConsentStatus = "GRANTED"
	ConsentStatusDenied       ConsentStatus = "DENIED"
	ConsentStatusWithdrawn    ConsentStatus = "WITHDRAWN"
	// ConsentStatusExpired is derived from ConsentReceipt.ExpiresAt — a
	// time-boxed grant that has run out. Distinct from withdrawal (the
	// subject acted) and invalidation (the platform acted).
	ConsentStatusExpired ConsentStatus = "EXPIRED"
	// ConsentStatusInvalidated is set by a ConsentInvalidation record — a
	// platform-initiated correction (e.g. a compromised capture channel or a
	// fraud finding), never a subject-initiated act. See WithdrawConsent vs
	// InvalidateConsent.
	ConsentStatusInvalidated ConsentStatus = "INVALIDATED"
)

// ConsentRequest is the documented object (§10.1) representing the request
// context presented to the subject: exact purpose, scope, data/recipient/technology
// context, required/optional status, notice version, policy package. Distinct
// from the receipt itself, and persisted as its own evidence row (migration
// 000005) — it used to be accepted on the wire and silently discarded before
// the INSERT, so nothing was ever actually preserved from it.
//
// Vendor/StorageLifetime/JurisdictionRuleRef are §11.1's web/mobile tracking
// scope: "model storage/access technology, purpose, vendor, lifetime and
// jurisdictional rule reference."
type ConsentRequest struct {
	PurposeID           string `json:"purpose_id"`
	Scope               string `json:"scope,omitempty"`
	DataContext         string `json:"data_context,omitempty"`
	RecipientContext    string `json:"recipient_context,omitempty"`
	TechnologyContext   string `json:"technology_context,omitempty"`
	Vendor              string `json:"vendor,omitempty"`
	StorageLifetime     string `json:"storage_lifetime,omitempty"`
	JurisdictionRuleRef string `json:"jurisdiction_rule_ref,omitempty"`
	RequiredStatus      string `json:"required_status,omitempty"` // REQUIRED | OPTIONAL
	NoticeVersionID     string `json:"notice_version_id,omitempty"`
	PolicyPackage       string `json:"policy_package,omitempty"`
}

// ConsentReceipt is append-only evidence of one grant/deny event —
// PRV-I07: purpose/scope specific, never a universal boolean. Never
// updated or deleted once written (migration 000002's trigger).
// Includes Proxy / Authorized Representative evidence (§11.1) and
// Affirmative-action evidence (§10.1).
type ConsentReceipt struct {
	ConsentReceiptID           string          `json:"consent_receipt_id"`
	TenantID                   *string         `json:"tenant_id,omitempty"`
	SubjectRef                 string          `json:"subject_ref"`
	PurposeID                  string          `json:"purpose_id"`
	NoticeVersionID            *string         `json:"notice_version_id,omitempty"`
	Action                     ConsentAction   `json:"action"`
	CaptureChannel             string          `json:"capture_channel"`
	ActorPrincipalID           string          `json:"actor_principal_id"`
	CorrelationID              string          `json:"correlation_id,omitempty"`
	CreatedAt                  time.Time       `json:"created_at"`
	IsProxy                    bool            `json:"is_proxy"`
	RepresentativeSubjectRef   *string         `json:"representative_subject_ref,omitempty"`
	RepresentativeAuthorityRef *string         `json:"representative_authority_ref,omitempty"`
	RepresentativeEvidence     *string         `json:"representative_evidence,omitempty"`
	AffirmativeActionType      string          `json:"affirmative_action_type"`
	AffirmativeEvidence        *string         `json:"affirmative_evidence,omitempty"`
	ConsentRequest             *ConsentRequest `json:"consent_request,omitempty"`

	// ReceiptIntegrityHash is §10.1's "receipt integrity": a SHA-256 over the
	// receipt's own material fields, computed once at insert time and never
	// recomputed. It lets a later reader detect whether a row that claims to
	// be this receipt actually is — append-only triggers already stop an
	// in-place edit, but this also catches a row reconstructed by a restore
	// or a migration that didn't go through RecordConsent.
	ReceiptIntegrityHash string `json:"receipt_integrity_hash"`

	// AgeBand is §11.1 "children/minors": age/age-band evidence, never a
	// single hard-coded threshold.
	AgeBand *string `json:"age_band,omitempty"`

	// IsOfflineImport and the Import* fields are §11.1 "offline/imported
	// consent": source, capture method, original evidence reference and
	// migration provenance — never a fabricated current-platform timestamp.
	// CreatedAt is still "when this service recorded the row"; ImportedAt is
	// "when the consent actually happened," which can predate CreatedAt by
	// years for a migrated book of business.
	IsOfflineImport     bool       `json:"is_offline_import"`
	ImportSource        *string    `json:"import_source,omitempty"`
	ImportCaptureMethod *string    `json:"import_capture_method,omitempty"`
	ImportEvidenceRef   *string    `json:"import_evidence_ref,omitempty"`
	ImportedAt          *time.Time `json:"imported_at,omitempty"`

	// ExpiresAt backs ConsentStatusExpired. Nil means the grant does not
	// time-box itself.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	// PropagationCorrelationID is §10.1's "downstream propagation correlation
	// IDs": a single id this service stamps on the withdrawal and on the
	// privacy.consent.changed event it publishes, so a downstream consumer
	// that fans a withdrawal out to other systems can be traced back to this
	// one row. Generated server-side at withdrawal time, not caller-supplied
	// — this service cannot fabricate a real external propagation id before
	// propagation has happened, it can only mint the id that identifies it.
	PropagationCorrelationID string `json:"propagation_correlation_id"`
}

// ConsentInvalidation is append-only evidence that a ConsentReceipt was
// invalidated by the platform — distinct from WithdrawalReceipt (a
// subject-initiated act) per §6's orthogonal state list. A compromised
// capture channel or a confirmed fraud finding invalidates a grant the
// subject never asked to end; recording that as a "withdrawal" would
// misattribute the act to the wrong party in every downstream audit trail.
type ConsentInvalidation struct {
	ConsentInvalidationID    string    `json:"consent_invalidation_id"`
	TenantID                 *string   `json:"tenant_id,omitempty"`
	ConsentReceiptID         string    `json:"consent_receipt_id"`
	InvalidatedByPrincipalID string    `json:"invalidated_by_principal_id"`
	Reason                   string    `json:"reason"`
	Channel                  string    `json:"channel"`
	CreatedAt                time.Time `json:"created_at"`
}

// ConsentResolution is the read-side answer to "what is the current
// status of this subject's consent for this purpose" — computed from the
// evidence, never itself stored.
type ConsentResolution struct {
	SubjectRef          string               `json:"subject_ref"`
	PurposeID           string               `json:"purpose_id"`
	Status              ConsentStatus        `json:"status"`
	LatestReceipt       *ConsentReceipt      `json:"latest_receipt,omitempty"`
	WithdrawalReceipt   *WithdrawalReceipt   `json:"withdrawal_receipt,omitempty"`
	ConsentInvalidation *ConsentInvalidation `json:"consent_invalidation,omitempty"`
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
	SubjectRef       string `json:"subject_ref"`
	Channel          string `json:"channel"`
	Locale           string `json:"locale"`
	SessionRef       string `json:"session_ref,omitempty"`
	TemplateVersion  string `json:"template_version,omitempty"`
	DeliveryEvidence string `json:"delivery_evidence,omitempty"`
}

type RecordConsentRequest struct {
	TenantID                   string          `json:"tenant_id,omitempty"`
	SubjectRef                 string          `json:"subject_ref"`
	PurposeID                  string          `json:"purpose_id"`
	NoticeVersionID            string          `json:"notice_version_id,omitempty"`
	Action                     string          `json:"action"`
	CaptureChannel             string          `json:"capture_channel"`
	IsProxy                    bool            `json:"is_proxy,omitempty"`
	RepresentativeSubjectRef   string          `json:"representative_subject_ref,omitempty"`
	RepresentativeAuthorityRef string          `json:"representative_authority_ref,omitempty"`
	RepresentativeEvidence     string          `json:"representative_evidence,omitempty"`
	AffirmativeActionType      string          `json:"affirmative_action_type,omitempty"`
	AffirmativeEvidence        string          `json:"affirmative_evidence,omitempty"`
	ConsentRequest             *ConsentRequest `json:"consent_request,omitempty"`

	// AgeBand/offline-import/expiry — see ConsentReceipt's own doc comment
	// for what each backs.
	AgeBand             string     `json:"age_band,omitempty"`
	IsOfflineImport     bool       `json:"is_offline_import,omitempty"`
	ImportSource        string     `json:"import_source,omitempty"`
	ImportCaptureMethod string     `json:"import_capture_method,omitempty"`
	ImportEvidenceRef   string     `json:"import_evidence_ref,omitempty"`
	ImportedAt          *time.Time `json:"imported_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
}

type WithdrawConsentRequest struct {
	Channel string `json:"channel"`
}

// InvalidateConsentRequest backs POST /privacy/consents/{id}/invalidate —
// the platform-initiated counterpart to WithdrawConsentRequest. Reason is
// mandatory: an invalidation with no stated reason is not evidence, and §1
// treats "stable error/evidence" as the minimum bar for any privileged
// correction.
type InvalidateConsentRequest struct {
	Reason  string `json:"reason"`
	Channel string `json:"channel"`
}

type SetPreferenceRequest struct {
	TenantID         string `json:"tenant_id,omitempty"`
	SubjectRef       string `json:"subject_ref"`
	ChannelOrPurpose string `json:"channel_or_purpose"`
	Value            string `json:"value"`
	Source           string `json:"source"`
}

// ── Idempotency (§18.1) ──────────────────────────────────────────────────────

type IdempotencyRecord struct {
	Key          string
	TenantID     string
	Endpoint     string
	RequestHash  string
	ResponseCode int
	ResponseBody []byte
	CreatedAt    time.Time
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrNoticeNotFound        = errorString("notice not found")
	ErrNoticeVersionNotFound = errorString("notice version not found")
	// ErrNoticeVersionInvalid is §32 PRV-008 NOTICE_VERSION_INVALID: a
	// presentation or consent receipt referenced a notice version that is not
	// a currently PUBLISHED version of its notice (draft, approved, withdrawn,
	// superseded, malformed id, or nonexistent). §6: "Receipt always binds
	// exact published notice version."
	ErrNoticeVersionInvalid            = errorString("PRV-008: notice_version_id must name a currently PUBLISHED notice version")
	ErrInvalidNoticeTransition         = errorString("invalid notice version status transition")
	ErrConsentReceiptNotFound          = errorString("consent receipt not found")
	ErrAlreadyWithdrawn                = errorString("consent receipt is already withdrawn")
	ErrAlreadyInvalidated              = errorString("consent receipt is already invalidated")
	ErrPurposeNotRegistered            = errorString("purpose is not a registered, published purpose")
	ErrStoreUnavailable                = errorString("privacy-consent store unavailable")
	ErrIdempotencyConflict             = errorString("idempotency key reused with different request payload")
	ErrIdempotencyKeyRequired          = errorString("Idempotency-Key header is required on this operation (§18.1)")
	ErrCorrelationIDRequired           = errorString("X-Correlation-ID header is required on this operation (§18.1)")
	ErrRepresentativeAuthorityRequired = errorString("proxy consent requires representative_subject_ref and representative_authority_ref")
	ErrNoticeSelfApprovalForbidden     = errorString("creator of notice version cannot approve it")
	ErrNoticeSelfPublishForbidden      = errorString("creator of notice version cannot publish it")
	ErrNoticeApproverCannotPublish     = errorString("approver of notice version cannot also publish it")
	ErrTenantScopeRequired             = errorString("X-Tenant-Id header is required — the gateway sets it from a verified identity envelope")
	ErrTenantScopeMismatch             = errorString("tenant_id does not match the verified X-Tenant-Id")
	ErrIdentityMissing                 = errorString("X-Principal-Id header is required — the gateway sets it from a verified identity envelope")
)
