package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// ── receipt integrity (§10.1) ────────────────────────────────────────────────

// ReceiptIntegrityPrefix tags every integrity value this version of the
// service writes. Receipts written before it carry a bare 64-hex SHA-256 over
// a narrower field set (the "v1" scheme) and are recognised by the absence of
// the prefix — see IntegrityScheme. They are not rewritten: receipts are
// append-only evidence.
//
// The algorithm is unchanged (SHA-256). §33 OD-06 lists "Consent receipt
// cryptographic integrity scheme and long-term evidence verification policy"
// as an OPEN decision, so no HMAC, signature or key is introduced here — only
// the set of fields the digest covers was widened.
const ReceiptIntegrityPrefix = "v2:"

// IntegrityScheme reports which scheme produced a stored integrity value.
func IntegrityScheme(stored string) string {
	if len(stored) > len(ReceiptIntegrityPrefix) && stored[:len(ReceiptIntegrityPrefix)] == ReceiptIntegrityPrefix {
		return "v2"
	}
	if stored == "" {
		return "none"
	}
	return "v1-legacy"
}

// canonical is an unambiguous, deterministic encoder: every value is written
// as  name=<byte-length>:<value>;  so no value can masquerade as a delimiter
// or as another field, an absent optional value is distinguishable from an
// empty one, and field order is fixed by the code below.
type canonical struct{ buf []byte }

func (c *canonical) str(name, v string) {
	c.buf = append(c.buf, name...)
	c.buf = append(c.buf, '=')
	c.buf = strconv.AppendInt(c.buf, int64(len(v)), 10)
	c.buf = append(c.buf, ':')
	c.buf = append(c.buf, v...)
	c.buf = append(c.buf, ';')
}

func (c *canonical) opt(name string, v *string) {
	if v == nil {
		c.str(name, "\x00absent")
		return
	}
	c.str(name, "\x01"+*v)
}

func (c *canonical) flag(name string, v bool) { c.str(name, strconv.FormatBool(v)) }

// instant normalises to UTC and to microseconds — PostgreSQL's timestamptz
// resolution — so the digest computed at insert equals the digest recomputed
// from the stored row.
func (c *canonical) instant(name string, t *time.Time) {
	if t == nil {
		c.str(name, "\x00absent")
		return
	}
	c.str(name, "\x01"+t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano))
}

// ComputeIntegrityHash is §10.1's "receipt integrity". It covers every
// evidence field of the receipt AND its ConsentRequest:
//
//	identity       subject_ref, purpose_id, notice_version_id, action
//	capture        capture_channel, actor_principal_id, created_at
//	proxy          is_proxy, representative_subject_ref/_authority_ref/_evidence
//	affirmative    affirmative_action_type, affirmative_evidence
//	minors         age_band
//	offline import is_offline_import, import_source, import_capture_method,
//	               import_evidence_ref, imported_at
//	expiry         expires_at
//	ConsentRequest scope, data/recipient/technology context, vendor,
//	               storage_lifetime, jurisdiction_rule_ref, required_status,
//	               notice_version_id, policy_package
//
// It is deterministic: the same receipt always yields the same value, and
// changing any single covered field changes it. Fields that are not part of
// the evidence (the receipt id, tenant, correlation id, and the hash itself)
// are deliberately outside it.
func (c *ConsentReceipt) ComputeIntegrityHash() string {
	var e canonical
	e.str("scheme", "receipt-integrity/v2")

	e.str("subject_ref", c.SubjectRef)
	e.str("purpose_id", c.PurposeID)
	e.opt("notice_version_id", c.NoticeVersionID)
	e.str("action", string(c.Action))

	e.str("capture_channel", c.CaptureChannel)
	e.str("actor_principal_id", c.ActorPrincipalID)
	created := c.CreatedAt
	e.instant("created_at", &created)

	e.flag("is_proxy", c.IsProxy)
	e.opt("representative_subject_ref", c.RepresentativeSubjectRef)
	e.opt("representative_authority_ref", c.RepresentativeAuthorityRef)
	e.opt("representative_evidence", c.RepresentativeEvidence)

	e.str("affirmative_action_type", c.AffirmativeActionType)
	e.opt("affirmative_evidence", c.AffirmativeEvidence)

	e.opt("age_band", c.AgeBand)

	e.flag("is_offline_import", c.IsOfflineImport)
	e.opt("import_source", c.ImportSource)
	e.opt("import_capture_method", c.ImportCaptureMethod)
	e.opt("import_evidence_ref", c.ImportEvidenceRef)
	e.instant("imported_at", c.ImportedAt)

	e.instant("expires_at", c.ExpiresAt)

	if cr := c.ConsentRequest; cr == nil {
		e.str("consent_request", "\x00absent")
	} else {
		e.str("consent_request", "\x01present")
		e.str("cr.scope", cr.Scope)
		e.str("cr.data_context", cr.DataContext)
		e.str("cr.recipient_context", cr.RecipientContext)
		e.str("cr.technology_context", cr.TechnologyContext)
		e.str("cr.vendor", cr.Vendor)
		e.str("cr.storage_lifetime", cr.StorageLifetime)
		e.str("cr.jurisdiction_rule_ref", cr.JurisdictionRuleRef)
		e.str("cr.required_status", cr.RequiredStatus)
		e.str("cr.notice_version_id", cr.NoticeVersionID)
		e.str("cr.policy_package", cr.PolicyPackage)
	}

	sum := sha256.Sum256(e.buf)
	return ReceiptIntegrityPrefix + hex.EncodeToString(sum[:])
}

// ── §18 response shapes ──────────────────────────────────────────────────────
//
// §18's critical response fields use different names from the ones this
// service has always returned (receipt_id vs consent_receipt_id, status vs
// action, evidence_hash vs receipt_integrity_hash, withdrawal_id vs
// withdrawal_receipt_id, effective_at vs created_at, propagation_correlation
// vs propagation_correlation_id). The §18 names are added ALONGSIDE the
// existing ones, so a current consumer keeps working and a §18 consumer
// finds what the contract promises. Both name sets carry identical values.

// MarshalJSON adds §18's POST /privacy/consents critical fields:
// receipt_id, purpose/scope (purpose_id + scope), notice_version, status and
// evidence_hash.
//
// status is the receipt's own captured outcome (GRANTED or DENIED). The
// current derived status of the subject's consent — which can later become
// WITHDRAWN, EXPIRED or INVALIDATED — is a separate read (GET
// /privacy/consents), never stored on the receipt.
func (c ConsentReceipt) MarshalJSON() ([]byte, error) {
	type plain ConsentReceipt
	scope := ""
	if c.ConsentRequest != nil {
		scope = c.ConsentRequest.Scope
	}
	return json.Marshal(struct {
		plain
		ReceiptID     string        `json:"receipt_id"`
		Scope         string        `json:"scope,omitempty"`
		NoticeVersion *string       `json:"notice_version,omitempty"`
		Status        ConsentStatus `json:"status"`
		EvidenceHash  string        `json:"evidence_hash"`
	}{
		plain:         plain(c),
		ReceiptID:     c.ConsentReceiptID,
		Scope:         scope,
		NoticeVersion: c.NoticeVersionID,
		Status:        ConsentStatus(c.Action),
		EvidenceHash:  c.ReceiptIntegrityHash,
	})
}

// MarshalJSON adds §18's POST /privacy/consents/{id}/withdraw critical
// fields: withdrawal_id, effective_at and propagation_correlation.
//
// effective_at is the instant the withdrawal was recorded: §11.2 makes
// withdrawal effective from its effective time and prospective only, and this
// service records no separate, later effective instant.
func (w WithdrawalReceipt) MarshalJSON() ([]byte, error) {
	type plain WithdrawalReceipt
	return json.Marshal(struct {
		plain
		WithdrawalID           string    `json:"withdrawal_id"`
		EffectiveAt            time.Time `json:"effective_at"`
		PropagationCorrelation string    `json:"propagation_correlation"`
	}{
		plain:                  plain(w),
		WithdrawalID:           w.WithdrawalReceiptID,
		EffectiveAt:            w.CreatedAt,
		PropagationCorrelation: w.PropagationCorrelationID,
	})
}

// MarshalJSON adds §18's POST /privacy/notices/{id}/presentation-receipts
// critical field `receipt_id` (the id of the presentation receipt) beside the
// existing presentation_receipt_id, with the same value. The remaining §18
// members already travel on the receipt: subject (subject_ref), session
// (session_ref), version (notice_version_id), timestamp (created_at) and
// channel/template (channel, template_version).
func (p PresentationReceipt) MarshalJSON() ([]byte, error) {
	type plain PresentationReceipt
	return json.Marshal(struct {
		plain
		ReceiptID string `json:"receipt_id"`
	}{
		plain:     plain(p),
		ReceiptID: p.PresentationReceiptID,
	})
}

// ── §19 privacy.consent.changed ──────────────────────────────────────────────

// ConsentChangedPayload is the payload of privacy.consent.changed. §19's
// minimum: subject_ref, purpose/scope, old/new state, receipt/withdrawal ref,
// effective time.
//
// OldState and NewState are the DERIVED consent status of (subject_ref,
// purpose_id) immediately before and immediately after the change, both read
// inside the transaction that records it. They can be equal: withdrawing an
// older receipt does not alter the status a newer receipt already governs.
// The raw evidence row is kept alongside for consumers that want it.
type ConsentChangedPayload struct {
	SubjectRef string `json:"subject_ref"`
	PurposeID  string `json:"purpose_id"`
	Scope      string `json:"scope,omitempty"`

	ChangeType string        `json:"change_type"` // GRANTED | DENIED | WITHDRAWN | INVALIDATED
	OldState   ConsentStatus `json:"old_state"`
	NewState   ConsentStatus `json:"new_state"`

	ReceiptID      string `json:"receipt_id"`
	WithdrawalID   string `json:"withdrawal_id,omitempty"`
	InvalidationID string `json:"invalidation_id,omitempty"`
	// PropagationCorrelation is set for a withdrawal (§10.1).
	PropagationCorrelation string `json:"propagation_correlation,omitempty"`

	NoticeVersionID *string `json:"notice_version_id,omitempty"`
	// EffectiveTime is when the change takes effect: the capture time of a
	// grant/deny (the original consent time for an offline import, never a
	// fabricated current one), or the recording time of a withdrawal or
	// invalidation.
	EffectiveTime time.Time `json:"effective_time"`

	Receipt      *ConsentReceipt      `json:"receipt,omitempty"`
	Withdrawal   *WithdrawalReceipt   `json:"withdrawal,omitempty"`
	Invalidation *ConsentInvalidation `json:"invalidation,omitempty"`
}
