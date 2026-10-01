package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// EvidenceAlgorithm names how the package digest is computed, so a verifier can
// reproduce it: sha256 over the canonical JSON (sorted keys, no whitespace,
// numbers preserved verbatim) of the package CONTENT.
const EvidenceAlgorithm = "sha256/canonical-json/1"

// EvidenceContent is what the digest covers (§24). Nothing that varies after
// sealing (seal time, identifiers) is inside it.
type EvidenceContent struct {
	Definition  DefinitionEvidence   `json:"definition_snapshot"`
	Run         RunEvidence          `json:"run_context"`
	Populations []PopulationEvidence `json:"population_evidence"`
	Execution   ExecutionEvidence    `json:"execution_evidence"`
	Exceptions  []ExceptionEvidence  `json:"exceptions"`
}

type DefinitionEvidence struct {
	ControlDefinitionID string   `json:"control_definition_id"`
	ControlCode         string   `json:"control_code"`
	Name                string   `json:"name"`
	OwnerRole           string   `json:"owner_role"`
	Assertions          []string `json:"assertions"`
	RiskTier            string   `json:"risk_tier"`
	Frequency           string   `json:"frequency"`
	RuleVersion         int      `json:"rule_version"`
	RuleDigest          string   `json:"rule_digest"`
}

type RunEvidence struct {
	RunID         string `json:"run_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	PeriodID      string `json:"period_id"`
	TriggerType   string `json:"trigger_type"`
	ExecutedBy    string `json:"executed_by"`
	Service       string `json:"service"`
}

type PopulationEvidence struct {
	Side           Side            `json:"side"`
	SourceSystem   string          `json:"source_system"`
	SpecRef        string          `json:"spec_ref"`
	RowCount       int             `json:"row_count"`
	ControlTotals  []CurrencyTotal `json:"control_totals"`
	PopulationHash string          `json:"population_hash"`
	Watermark      string          `json:"source_watermark"`
	Exclusions     []Exclusion     `json:"exclusions"`
}

type ExecutionEvidence struct {
	Kind              string      `json:"kind"`
	GroupMatchCount   int         `json:"group_match_count"`
	Algorithm         string      `json:"algorithm"`
	ToleranceID       string      `json:"tolerance_id"`
	AbsoluteTolerance string      `json:"absolute_tolerance"`
	DateToleranceDays int         `json:"date_tolerance_days"`
	MaterialityID     string      `json:"materiality_id"`
	MatchedCount      int         `json:"matched_count"`
	ExceptionCount    int         `json:"exception_count"`
	TotalsAgree       bool        `json:"totals_agree"`
	Result            ResultState `json:"result"`
	MatchSetDigest    string      `json:"match_set_digest"`
}

type ExceptionEvidence struct {
	ExceptionID string   `json:"exception_id"`
	Category    string   `json:"category"`
	ReasonCode  string   `json:"reason_code"`
	Severity    Severity `json:"severity"`
	Exposure    string   `json:"exposure"`
	Currency    string   `json:"currency"`
	RecordIDs   []string `json:"record_ids"`
}

// EvidencePackage is the sealed, tamper-evident record of a run execution.
type EvidencePackage struct {
	PackageID string          `json:"package_id"`
	RunID     string          `json:"run_id"`
	Content   json.RawMessage `json:"content"`
	Digest    string          `json:"digest"`
	Algorithm string          `json:"algorithm"`
	SealedAt  time.Time       `json:"sealed_at"`
	SealedBy  string          `json:"sealed_by"`
	// Verified is computed on read: does the stored content still hash to the
	// stored digest? It is never persisted.
	Verified *bool `json:"integrity_verified,omitempty"`
}

// CanonicalDigest hashes JSON in a form independent of key order, whitespace,
// and JSONB's re-serialisation: keys sorted, numbers kept as their literal text.
func CanonicalDigest(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("%w: evidence content: %v", ErrInvalidArgument, err)
	}
	canon, err := json.Marshal(v) // map keys are emitted sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SealEvidence produces the package digest over the content.
func SealEvidence(c EvidenceContent) (json.RawMessage, string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, "", err
	}
	d, err := CanonicalDigest(raw)
	if err != nil {
		return nil, "", err
	}
	return raw, d, nil
}

// VerifyEvidence recomputes the digest of stored content (Invariants 3, 13, 16).
func VerifyEvidence(content json.RawMessage, digest string) bool {
	d, err := CanonicalDigest(content)
	return err == nil && d == digest
}

// MatchSetDigest fingerprints the exact pairing output so the evidence proves
// WHICH records were matched, not just how many.
func MatchSetDigest(m []MatchResult) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return CanonicalDigest(raw)
}
