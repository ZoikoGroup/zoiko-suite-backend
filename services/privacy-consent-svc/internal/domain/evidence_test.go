package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sp(s string) *string { return &s }

// fullReceipt sets EVERY evidence field, so that each mutation below is
// measured against a receipt in which nothing is "absent".
func fullReceipt() *ConsentReceipt {
	created := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	imported := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	return &ConsentReceipt{
		ConsentReceiptID: "rcpt-1", TenantID: sp("tenant-1"), CorrelationID: "corr-1", ReceiptIntegrityHash: "ignored",
		SubjectRef: "subject-1", PurposeID: "purpose-1", NoticeVersionID: sp("notice-v1"), Action: ConsentActionGranted,
		CaptureChannel: "web", ActorPrincipalID: "actor-1", CreatedAt: created,
		IsProxy: true, RepresentativeSubjectRef: sp("rep-1"), RepresentativeAuthorityRef: sp("auth-1"), RepresentativeEvidence: sp("ev-1"),
		AffirmativeActionType: "EXPLICIT_CHECKBOX", AffirmativeEvidence: sp("aff-1"),
		AgeBand:         sp("ADULT"),
		IsOfflineImport: true, ImportSource: sp("legacy-crm"), ImportCaptureMethod: sp("paper-form"),
		ImportEvidenceRef: sp("scan-991"), ImportedAt: &imported,
		ExpiresAt: &expires,
		ConsentRequest: &ConsentRequest{
			PurposeID: "purpose-1", Scope: "email", DataContext: "contact", RecipientContext: "processor",
			TechnologyContext: "cookie", Vendor: "acme", StorageLifetime: "12m", JurisdictionRuleRef: "rule-1",
			RequiredStatus: "OPTIONAL", NoticeVersionID: "notice-v1", PolicyPackage: "pkg-1",
		},
	}
}

func TestIntegrityHash_IsDeterministicAndVersioned(t *testing.T) {
	a, b := fullReceipt().ComputeIntegrityHash(), fullReceipt().ComputeIntegrityHash()
	if a != b {
		t.Fatalf("not deterministic: %s vs %s", a, b)
	}
	if IntegrityScheme(a) != "v2" || !strings.HasPrefix(a, ReceiptIntegrityPrefix) || len(a) != len(ReceiptIntegrityPrefix)+64 {
		t.Fatalf("unexpected value shape: %q", a)
	}
	if IntegrityScheme("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") != "v1-legacy" || IntegrityScheme("") != "none" {
		t.Fatalf("legacy/empty values must be recognised as such")
	}
}

// Every field the spec's §10.1 / the audit lists must move the digest.
func TestIntegrityHash_EveryProtectedFieldChangesIt(t *testing.T) {
	base := fullReceipt().ComputeIntegrityHash()
	other := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	mutations := map[string]func(c *ConsentReceipt){
		// identity
		"subject_ref":       func(c *ConsentReceipt) { c.SubjectRef = "subject-2" },
		"purpose_id":        func(c *ConsentReceipt) { c.PurposeID = "purpose-2" },
		"notice_version_id": func(c *ConsentReceipt) { c.NoticeVersionID = sp("notice-v2") },
		"action":            func(c *ConsentReceipt) { c.Action = ConsentActionDenied },
		// capture
		"capture_channel":    func(c *ConsentReceipt) { c.CaptureChannel = "mobile" },
		"actor_principal_id": func(c *ConsentReceipt) { c.ActorPrincipalID = "actor-2" },
		"created_at":         func(c *ConsentReceipt) { c.CreatedAt = c.CreatedAt.Add(time.Microsecond) },
		// proxy
		"is_proxy":                    func(c *ConsentReceipt) { c.IsProxy = false },
		"representative_subject_ref":  func(c *ConsentReceipt) { c.RepresentativeSubjectRef = sp("rep-2") },
		"representative_authority_ref": func(c *ConsentReceipt) { c.RepresentativeAuthorityRef = sp("auth-2") },
		"representative_evidence":     func(c *ConsentReceipt) { c.RepresentativeEvidence = sp("ev-2") },
		// affirmative-action evidence
		"affirmative_action_type": func(c *ConsentReceipt) { c.AffirmativeActionType = "TOGGLE" },
		"affirmative_evidence":    func(c *ConsentReceipt) { c.AffirmativeEvidence = sp("aff-2") },
		// minors
		"age_band": func(c *ConsentReceipt) { c.AgeBand = sp("MINOR") },
		// offline import provenance
		"is_offline_import":     func(c *ConsentReceipt) { c.IsOfflineImport = false },
		"import_source":         func(c *ConsentReceipt) { c.ImportSource = sp("other-crm") },
		"import_capture_method": func(c *ConsentReceipt) { c.ImportCaptureMethod = sp("phone") },
		"import_evidence_ref":   func(c *ConsentReceipt) { c.ImportEvidenceRef = sp("scan-992") },
		"imported_at":           func(c *ConsentReceipt) { c.ImportedAt = &other },
		// expiry
		"expires_at": func(c *ConsentReceipt) { c.ExpiresAt = &other },
		// ConsentRequest: presence and every field
		"consent_request (removed)":    func(c *ConsentReceipt) { c.ConsentRequest = nil },
		"cr.scope":                     func(c *ConsentReceipt) { c.ConsentRequest.Scope = "sms" },
		"cr.data_context":              func(c *ConsentReceipt) { c.ConsentRequest.DataContext = "x" },
		"cr.recipient_context":         func(c *ConsentReceipt) { c.ConsentRequest.RecipientContext = "x" },
		"cr.technology_context":        func(c *ConsentReceipt) { c.ConsentRequest.TechnologyContext = "x" },
		"cr.vendor":                    func(c *ConsentReceipt) { c.ConsentRequest.Vendor = "x" },
		"cr.storage_lifetime":          func(c *ConsentReceipt) { c.ConsentRequest.StorageLifetime = "x" },
		"cr.jurisdiction_rule_ref":     func(c *ConsentReceipt) { c.ConsentRequest.JurisdictionRuleRef = "x" },
		"cr.required_status":           func(c *ConsentReceipt) { c.ConsentRequest.RequiredStatus = "REQUIRED" },
		"cr.notice_version_id":         func(c *ConsentReceipt) { c.ConsentRequest.NoticeVersionID = "notice-v9" },
		"cr.policy_package":            func(c *ConsentReceipt) { c.ConsentRequest.PolicyPackage = "x" },
	}
	for name, mutate := range mutations {
		c := fullReceipt()
		mutate(c)
		if got := c.ComputeIntegrityHash(); got == base {
			t.Errorf("changing %s did not change the integrity hash", name)
		}
	}
}

// Absent and empty are different facts; field boundaries cannot be forged.
func TestIntegrityHash_DistinguishesAbsentFromEmpty_AndResistsBoundaryShifts(t *testing.T) {
	absent, empty := fullReceipt(), fullReceipt()
	absent.AffirmativeEvidence, empty.AffirmativeEvidence = nil, sp("")
	if absent.ComputeIntegrityHash() == empty.ComputeIntegrityHash() {
		t.Errorf("an absent value and an empty value must not hash alike")
	}
	a, b := fullReceipt(), fullReceipt()
	a.SubjectRef, a.PurposeID = "ab", "c"
	b.SubjectRef, b.PurposeID = "a", "bc"
	if a.ComputeIntegrityHash() == b.ComputeIntegrityHash() {
		t.Errorf("moving a character across a field boundary must change the hash")
	}
}

// What is not evidence must NOT move the hash — otherwise recomputing it from
// a stored row (which has an id, a tenant and a correlation id) could not match.
func TestIntegrityHash_IgnoresNonEvidenceFields_AndNormalisesTime(t *testing.T) {
	base := fullReceipt().ComputeIntegrityHash()
	c := fullReceipt()
	c.ConsentReceiptID, c.TenantID, c.CorrelationID, c.ReceiptIntegrityHash = "other", sp("other"), "other", "other"
	if c.ConsentRequest != nil {
		c.ConsentRequest.PurposeID = "different-but-derived"
	}
	if c.ComputeIntegrityHash() != base {
		t.Errorf("id/tenant/correlation/stored-hash must not affect the digest")
	}
	// PostgreSQL stores microseconds; a nanosecond residue and a different zone
	// are the same instant.
	d := fullReceipt()
	d.CreatedAt = d.CreatedAt.Add(999 * time.Nanosecond).In(time.FixedZone("x", 5*3600))
	if d.ComputeIntegrityHash() != base {
		t.Errorf("sub-microsecond residue and zone representation must not change the digest")
	}
}

// Guard: a new ConsentReceipt field must be consciously classified. If you add
// one, either put it in the digest (and in TestIntegrityHash_EveryProtected…)
// or list it here as deliberately excluded.
func TestIntegrityHash_EveryReceiptFieldIsClassified(t *testing.T) {
	covered := map[string]bool{
		"SubjectRef": true, "PurposeID": true, "NoticeVersionID": true, "Action": true,
		"CaptureChannel": true, "ActorPrincipalID": true, "CreatedAt": true,
		"IsProxy": true, "RepresentativeSubjectRef": true, "RepresentativeAuthorityRef": true, "RepresentativeEvidence": true,
		"AffirmativeActionType": true, "AffirmativeEvidence": true, "AgeBand": true,
		"IsOfflineImport": true, "ImportSource": true, "ImportCaptureMethod": true, "ImportEvidenceRef": true, "ImportedAt": true,
		"ExpiresAt": true, "ConsentRequest": true,
	}
	excluded := map[string]string{
		"ConsentReceiptID":     "identifier assigned at insert; not evidence",
		"TenantID":             "ownership, enforced by RLS; not part of §10.1's receipt integrity",
		"CorrelationID":        "request tracing; not evidence",
		"ReceiptIntegrityHash": "the digest itself",
	}
	rt := reflect.TypeOf(ConsentReceipt{})
	for i := 0; i < rt.NumField(); i++ {
		n := rt.Field(i).Name
		_, isCovered := covered[n]
		_, isExcluded := excluded[n]
		if !isCovered && !isExcluded {
			t.Errorf("ConsentReceipt.%s is neither covered by the integrity hash nor listed as excluded — decide which", n)
		}
		if isCovered && isExcluded {
			t.Errorf("%s is listed as both covered and excluded", n)
		}
	}
}

// ── §18 response shapes ──────────────────────────────────────────────────────

func TestConsentReceiptJSON_CarriesSection18Fields_AndKeepsTheExistingOnes(t *testing.T) {
	c := fullReceipt()
	c.ReceiptIntegrityHash = c.ComputeIntegrityHash()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		// §18: receipt_id, purpose/scope, notice_version, status, evidence_hash
		"receipt_id": "rcpt-1", "purpose_id": "purpose-1", "scope": "email", "notice_version": "notice-v1",
		"status": "GRANTED", "evidence_hash": c.ReceiptIntegrityHash,
		// the names consumers already use
		"consent_receipt_id": "rcpt-1", "action": "GRANTED", "receipt_integrity_hash": c.ReceiptIntegrityHash, "notice_version_id": "notice-v1",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	// A DENIED receipt reports status DENIED.
	c.Action = ConsentActionDenied
	raw, _ = json.Marshal(c)
	_ = json.Unmarshal(raw, &m)
	if m["status"] != "DENIED" {
		t.Errorf("status = %v, want DENIED", m["status"])
	}
	// Round-trips into the struct consumers decode into.
	var back ConsentReceipt
	if err := json.Unmarshal(raw, &back); err != nil || back.ConsentReceiptID != "rcpt-1" || back.Action != ConsentActionDenied {
		t.Errorf("round trip failed: %v %+v", err, back)
	}
}

func TestWithdrawalJSON_CarriesSection18Fields_AndKeepsTheExistingOnes(t *testing.T) {
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	w := WithdrawalReceipt{WithdrawalReceiptID: "w-1", ConsentReceiptID: "rcpt-1", CreatedAt: at, PropagationCorrelationID: "prop-1", Channel: "web"}
	raw, _ := json.Marshal(w)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for k, v := range map[string]any{
		"withdrawal_id": "w-1", "effective_at": "2026-05-06T07:08:09Z", "propagation_correlation": "prop-1",
		"withdrawal_receipt_id": "w-1", "created_at": "2026-05-06T07:08:09Z", "propagation_correlation_id": "prop-1",
	} {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}
