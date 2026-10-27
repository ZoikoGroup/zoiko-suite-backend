package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// State is the state of one dimension of an integration outcome.
type State string

const (
	StateNotApplicable State = "NOT_APPLICABLE"
	StatePending       State = "PENDING"
	StateSucceeded     State = "SUCCEEDED"
	StateFailed        State = "FAILED"
	StateUnknown       State = "UNKNOWN"
)

func (s State) valid() bool {
	switch s {
	case StateNotApplicable, StatePending, StateSucceeded, StateFailed, StateUnknown:
		return true
	}
	return false
}

// Outcome keeps the six separate facts ZS-SVC-M-001 §0 says "integration
// success" is made of. They are never collapsed into one boolean: a 202 from a
// provider is Transport only, and says nothing about Provider or Domain.
type Outcome struct {
	Transport      State `json:"transport"`      // wire exchange completed
	Schema         State `json:"schema"`         // contract/schema validity
	Mapping        State `json:"mapping"`        // canonical mapping
	Provider       State `json:"provider"`       // external authority/provider state
	Domain         State `json:"domain"`         // downstream domain acceptance
	Reconciliation State `json:"reconciliation"` // population/total reconciliation
}

func (o Outcome) all() []State {
	return []State{o.Transport, o.Schema, o.Mapping, o.Provider, o.Domain, o.Reconciliation}
}

// Validate refuses an outcome with an unset or unknown dimension. An unset
// dimension must be NOT_APPLICABLE said out loud, not left blank, so a missing
// reconciliation can never read as "nothing to reconcile".
func (o Outcome) Validate() error {
	names := []string{"transport", "schema", "mapping", "provider", "domain", "reconciliation"}
	for i, s := range o.all() {
		if !s.valid() {
			return fmt.Errorf("outcome dimension %s has invalid state %q", names[i], s)
		}
	}
	return nil
}

// Overall is the single rolled-up status. Precedence is deliberately
// pessimistic: any FAILED wins, then UNKNOWN, then PENDING. Only an outcome
// whose every applicable dimension SUCCEEDED, with at least one applicable
// dimension, is CERTIFIED. A success that hides a reconciliation break is
// therefore impossible to express (INT-NP-048, invariant 25).
func (o Outcome) Overall() string {
	var failed, unknown, pending, succeeded bool
	for _, s := range o.all() {
		switch s {
		case StateFailed:
			failed = true
		case StateUnknown:
			unknown = true
		case StatePending:
			pending = true
		case StateSucceeded:
			succeeded = true
		}
	}
	switch {
	case failed:
		return "FAILED"
	case unknown:
		return "UNKNOWN"
	case pending:
		return "PENDING"
	case succeeded:
		return "CERTIFIED"
	}
	return "PENDING" // nothing applicable and nothing succeeded: not certified
}

// Certified reports Overall() == "CERTIFIED".
func (o Outcome) Certified() bool { return o.Overall() == "CERTIFIED" }

// Evidence is one attempt/batch/message evidence record (§9
// IntegrationEvidencePackage). It carries hashes and references, never bodies:
// the payload lives in restricted raw-evidence storage and is addressed by
// RawRef, so routine telemetry cannot leak it (INT-CTRL-035, INT-NP-047).
type Evidence struct {
	EvidenceID string `json:"evidence_id"`
	Kind       string `json:"kind"` // e.g. "provider_attempt", "import_batch", "webhook_delivery"

	SubjectType    string `json:"subject_type"`
	SubjectID      string `json:"subject_id"`
	SubjectVersion int64  `json:"subject_version,omitempty"`

	TenantID      string `json:"tenant_id"`
	CorrelationID string `json:"correlation_id"`
	CausationID   string `json:"causation_id,omitempty"`
	TraceID       string `json:"trace_id,omitempty"`

	SourceSystem      string `json:"source_system"`
	DestinationSystem string `json:"destination_system,omitempty"`

	ContractVersion string `json:"contract_version"`
	ProfileVersion  string `json:"profile_version,omitempty"`

	PayloadHash  string `json:"payload_hash,omitempty"`  // exact content sent/received
	ResponseHash string `json:"response_hash,omitempty"` // exact response/callback received
	RawRef       string `json:"raw_ref,omitempty"`       // pointer into restricted raw-evidence storage

	Outcome Outcome `json:"outcome"`
	Overall string  `json:"overall"`

	// Refs links to authoritative records (domain command id, provider
	// reference, attempt id, reconciliation run id). Values are identifiers.
	Refs map[string]string `json:"refs,omitempty"`

	RecordedAt time.Time `json:"recorded_at"`
}

// EvidenceSpec is what a caller supplies; the rest is derived from its Context.
type EvidenceSpec struct {
	Kind           string
	SubjectType    string
	SubjectID      string
	SubjectVersion int64
	ProfileVersion string
	PayloadHash    string
	ResponseHash   string
	RawRef         string
	Outcome        Outcome
	Refs           map[string]string
}

// NewEvidence builds an evidence record from the trusted context. It refuses a
// record whose Refs hold something that looks like a secret or a body.
func NewEvidence(c Context, s EvidenceSpec, now time.Time) (Evidence, error) {
	if s.Kind == "" || s.SubjectType == "" || s.SubjectID == "" {
		return Evidence{}, errors.New("evidence needs kind, subject_type and subject_id")
	}
	if c.TenantID == "" || c.CorrelationID == "" {
		return Evidence{}, errors.New("evidence needs a resolved tenant and correlation id")
	}
	if err := s.Outcome.Validate(); err != nil {
		return Evidence{}, err
	}
	for _, h := range []string{s.PayloadHash, s.ResponseHash} {
		if h != "" && !hashPattern.MatchString(h) {
			return Evidence{}, errors.New("hashes must be sha256:<64 hex>")
		}
	}
	for k, v := range s.Refs {
		if len(v) > maxRefLen || containsSecret(k, v) {
			return Evidence{}, fmt.Errorf("ref %q must be a short identifier, not a secret or body", k)
		}
	}
	return Evidence{
		EvidenceID:        uuid.NewString(),
		Kind:              s.Kind,
		SubjectType:       s.SubjectType,
		SubjectID:         s.SubjectID,
		SubjectVersion:    s.SubjectVersion,
		TenantID:          c.TenantID,
		CorrelationID:     c.CorrelationID,
		CausationID:       c.CausationID,
		TraceID:           c.TraceID,
		SourceSystem:      c.SourceSystem,
		DestinationSystem: c.DestinationSystem,
		ContractVersion:   c.ContractVersion,
		ProfileVersion:    s.ProfileVersion,
		PayloadHash:       s.PayloadHash,
		ResponseHash:      s.ResponseHash,
		RawRef:            s.RawRef,
		Outcome:           s.Outcome,
		Overall:           s.Outcome.Overall(),
		Refs:              s.Refs,
		RecordedAt:        now.UTC(),
	}, nil
}

const maxRefLen = 256

var hashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// HashBytes returns "sha256:<hex>" of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashJSON hashes v's JSON encoding. encoding/json sorts map keys, so equal
// values hash equally.
func HashJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// ---- redaction (INT-CTRL-035) ----------------------------------------------

// Redacted replaces a sensitive value in telemetry.
const Redacted = "[REDACTED]"

var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|authorization|api[-_]?key|private[-_]?key|credential|cookie|signature|bearer|client[-_]?secret|refresh|iban|account[-_]?number|card|cvv|ssn|tax[-_]?id)`)

var sensitiveValue = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`), // JWT
	regexp.MustCompile(`(?i)(password|secret|token|api[-_]?key)\s*[=:]\s*\S+`),
}

func containsSecret(key, value string) bool {
	if sensitiveKey.MatchString(key) {
		return true
	}
	return ScrubString(value) != value
}

// ScrubString removes bearer tokens, JWTs, private keys and key=value secrets
// from free text.
func ScrubString(s string) string {
	for _, re := range sensitiveValue {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// Redact returns a deep copy of v (as decoded JSON: maps, slices, scalars) with
// values under sensitive keys replaced and sensitive text scrubbed. It is for
// logs and metrics; raw evidence is stored separately under access control.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveKey.MatchString(k) {
				out[k] = Redacted
				continue
			}
			out[k] = Redact(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Redact(val)
		}
		return out
	case string:
		return ScrubString(t)
	default:
		return v
	}
}

// RedactJSON redacts a JSON document. A document that is not valid JSON is
// scrubbed as text and truncated, never returned as-is.
func RedactJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		s := ScrubString(string(raw))
		if len(s) > 256 {
			s = s[:256] + "…"
		}
		return []byte(strings.TrimSpace(s))
	}
	out, err := json.Marshal(Redact(v))
	if err != nil {
		return []byte(Redacted)
	}
	return out
}
