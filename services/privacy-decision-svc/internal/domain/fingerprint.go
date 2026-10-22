package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// FingerprintVersion prefixes every input fingerprint / context hash produced
// by ComputeInputFingerprint. Legacy decisions (before this scheme) carry a
// bare 64-hex digest over a narrower input set; the prefix keeps the two
// distinguishable and the legacy value reproducible as stored evidence.
const FingerprintVersion = "v2"

type fpSubject struct {
	SubjectRef   string `json:"subject_ref"`
	SubjectClass string `json:"subject_class"`
	AgeBand      string `json:"age_band"`
	Residency    string `json:"residency"`
	Jurisdiction string `json:"jurisdiction"`
	Relationship string `json:"relationship"`
}

type fpData struct {
	DataCategories   []string `json:"data_categories"`
	DataCategory     string   `json:"data_category"`
	SensitivityFlags []string `json:"sensitivity_flags"`
	Source           string   `json:"source"`
	Classification   string   `json:"classification"`
}

type fpRecipient struct {
	RecipientRef            string `json:"recipient_ref"`
	RecipientType           string `json:"recipient_type"`
	DestinationJurisdiction string `json:"destination_jurisdiction"`
	TransferMechanismID     string `json:"transfer_mechanism_id"`
}

type fpConsent struct {
	Required  bool   `json:"required"`
	ReceiptID string `json:"receipt_id"`
}

type fpHold struct {
	RecordClass string `json:"record_class"`
	EntityRef   string `json:"entity_ref"`
}

type fpTransfer struct {
	RelationshipID          string `json:"relationship_id"`
	TransferMechanismID     string `json:"transfer_mechanism_id"`
	DestinationJurisdiction string `json:"destination_jurisdiction"`
	AssessmentCheck         bool   `json:"assessment_check"`
}

// fingerprintInput is the normalized, decision-affecting input (§13.2). Every
// field of EvaluateDecisionRequest except TenantID (supplied separately and
// verified against the request scope) appears here; the reflection test in
// fingerprint_test.go fails when a new request field is added without being
// covered.
type fingerprintInput struct {
	Version                  string       `json:"version"`
	TenantID                 string       `json:"tenant_id"`
	SubjectRef               string       `json:"subject_ref"`
	ProcessingActivityID     string       `json:"processing_activity_id"`
	ActivityVersionID        string       `json:"activity_version_id"`
	PurposeID                string       `json:"purpose_id"`
	PurposeVersionID         string       `json:"purpose_version_id"`
	ProposedOperation        string       `json:"proposed_operation"`
	SubjectContext           *fpSubject   `json:"subject_context"`
	DataContext              *fpData      `json:"data_context"`
	SecondaryPurposeID       string       `json:"secondary_purpose_id"`
	ProposedSecondaryPurpose string       `json:"proposed_secondary_purpose"`
	RecipientContext         *fpRecipient `json:"recipient_context"`
	ConsentCheck             *fpConsent   `json:"consent_check"`
	LegalHoldCheck           *fpHold      `json:"legal_hold_check"`
	TransferCheck            *fpTransfer  `json:"transfer_check"`
	DeidentificationControl  string       `json:"deidentification_control_ref"`
}

func up(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
func tr(s string) string { return strings.TrimSpace(s) }

// normList trims, upper-cases, de-duplicates, drops empties and sorts.
func normList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = up(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ComputeInputFingerprint returns "v2:<sha256 hex>" over the normalized input
// dimensions (§13.2). Enum-like values are trimmed and upper-cased, category
// and flag lists are de-duplicated and sorted, so semantically identical
// requests hash identically while any decision-affecting change alters it.
func ComputeInputFingerprint(req *EvaluateDecisionRequest, tenantID string) string {
	in := fingerprintInput{
		Version:                  FingerprintVersion,
		TenantID:                 tr(tenantID),
		SubjectRef:               tr(req.SubjectRef),
		ProcessingActivityID:     tr(req.ProcessingActivityID),
		ActivityVersionID:        tr(req.ActivityVersionID),
		PurposeID:                tr(req.PurposeID),
		PurposeVersionID:         tr(req.PurposeVersionID),
		ProposedOperation:        up(string(req.ProposedOperation)),
		SecondaryPurposeID:       tr(req.SecondaryPurposeID),
		ProposedSecondaryPurpose: tr(req.ProposedSecondaryPurpose),
		DeidentificationControl:  tr(req.DeidentificationControlRef),
	}
	if c := req.SubjectContext; c != nil {
		in.SubjectContext = &fpSubject{tr(c.SubjectRef), up(c.SubjectClass), up(c.AgeBand), up(c.Residency), up(c.Jurisdiction), tr(c.Relationship)}
	}
	if c := req.DataContext; c != nil {
		in.DataContext = &fpData{normList(c.DataCategories), up(c.DataCategory), normList(c.SensitivityFlags), up(c.Source), up(c.Classification)}
	}
	if c := req.RecipientContext; c != nil {
		in.RecipientContext = &fpRecipient{tr(c.RecipientRef), up(c.RecipientType), up(c.DestinationJurisdiction), tr(c.TransferMechanismID)}
	}
	if c := req.ConsentCheck; c != nil {
		in.ConsentCheck = &fpConsent{c.Required, tr(c.ReceiptID)}
	}
	if c := req.LegalHoldCheck; c != nil {
		in.LegalHoldCheck = &fpHold{tr(c.RecordClass), tr(c.EntityRef)}
	}
	if c := req.TransferCheck; c != nil {
		in.TransferCheck = &fpTransfer{tr(c.RelationshipID), tr(c.TransferMechanismID), up(c.DestinationJurisdiction), c.AssessmentCheck}
	}
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	return FingerprintVersion + ":" + hex.EncodeToString(sum[:])
}
