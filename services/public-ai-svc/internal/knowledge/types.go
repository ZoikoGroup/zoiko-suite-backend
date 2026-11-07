// Package knowledge holds the PUBLIC_APPROVED knowledge plane: the source
// registry record (Doc 03 §06), chunk records (§07), the ingestion state
// machine (§09), signed release manifests (§11) and release-pinned lexical
// retrieval with mandatory filters (§14, Doc 05 §18).
package knowledge

import (
	"fmt"
	"time"
)

const (
	ProductID      = "zoikosuite"
	Audience       = "PUBLIC_ANONYMOUS"
	Classification = "PUBLIC_APPROVED"
	PIIClear       = "CLEAR"
	MarketAll      = "ALL"
	KindKB         = "kb"
	SchemaVersion  = "kb-schema-1.0.0"
)

// Source is a registry record. Every field is mandatory; a missing value
// never means approved (Doc 03 §06).
type Source struct {
	SourceID         string     `json:"source_id"`
	SourceVersionID  string     `json:"source_version_id"`
	SourceType       string     `json:"source_type"`
	OriginURI        string     `json:"origin_uri"`
	URLKey           string     `json:"url_key"`
	Title            string     `json:"title"`
	ProductID        string     `json:"product_id"`
	Audience         string     `json:"audience"`
	Classification   string     `json:"classification"`
	PIIState         string     `json:"pii_state"`
	AuthorityTier    string     `json:"authority_tier"`
	SubjectType      string     `json:"subject_type"`
	OwnerID          string     `json:"owner_id"`
	ApproverIDs      []string   `json:"approver_ids"`
	MarketCodes      []string   `json:"market_codes"`
	Locale           string     `json:"locale"`
	PlanCodes        []string   `json:"plan_codes"`
	CapabilityIDs    []string   `json:"capability_ids"`
	IntentIDs        []string   `json:"intent_ids"`
	ApprovalState    string     `json:"approval_state"`
	ApprovalTS       *time.Time `json:"approval_ts"`
	EffectiveFrom    *time.Time `json:"effective_from"`
	EffectiveUntil   *time.Time `json:"effective_until"`
	ReviewDueAt      *time.Time `json:"review_due_at"`
	ExpiresAt        *time.Time `json:"expires_at"`
	ContentSHA256    string     `json:"content_sha256"`
	SigningRef       string     `json:"signing_ref"`
	License          string     `json:"license"`
	Supersedes       string     `json:"supersedes"`
	RevocationReason string     `json:"revocation_reason"`
	// Synthetic marks seed data that is not ZoikoSuite fact.
	Synthetic bool `json:"synthetic,omitempty"`
	// Domain and ModuleID place a public-KB article in the taxonomy (pending
	// the signed Services/Categories Reconciliation v2.1).
	Domain   string `json:"domain,omitempty"`
	ModuleID string `json:"module_id,omitempty"`
	// Draft is DRAFT_PENDING_REVIEW content admitted only by a dev-only
	// ingest option (KB_ALLOW_DRAFT); a production runtime refuses it.
	Draft bool `json:"draft,omitempty"`
}

// Chunk is an indexable segment. It inherits scope, validity and revocation
// from its source (Doc 03 §07).
type Chunk struct {
	ChunkID         string    `json:"chunk_id"`
	SourceID        string    `json:"source_id"`
	SourceVersionID string    `json:"source_version_id"`
	SegmentNumber   int       `json:"segment_number"`
	DocumentAnchor  string    `json:"document_anchor"`
	DisplayTitle    string    `json:"display_title"`
	URLKey          string    `json:"url_key"`
	PlainText       string    `json:"plain_text"`
	TextSHA256      string    `json:"text_sha256"`
	Classification  string    `json:"classification"`
	Audience        string    `json:"audience"`
	AuthorityTier   string    `json:"authority_tier"`
	MarketCodes     []string  `json:"market_codes"`
	Locale          string    `json:"locale"`
	PlanCodes       []string  `json:"plan_codes"`
	IntentIDs       []string  `json:"intent_ids"`
	ValidFrom       time.Time `json:"valid_from"`
	ValidUntil      time.Time `json:"valid_until"`
	ReleaseID       string    `json:"retrieval_release_id"`
	Draft           bool      `json:"draft,omitempty"`
	// Doc 03 §07 chunk record: subject and capability scope, and the review
	// deadline of high-impact content, enforced at query time (§12/§26).
	SubjectType   string     `json:"subject_type,omitempty"`
	CapabilityIDs []string   `json:"capability_ids,omitempty"`
	HighImpact    bool       `json:"high_impact,omitempty"`
	ReviewDueAt   *time.Time `json:"review_due_at,omitempty"`
}

// Route is an official destination (Doc 05 §15). URL is never shown to the
// model; the widget resolves route_key to URL from config.
type Route struct {
	RouteKey       string    `json:"route_key"`
	URL            string    `json:"url"`
	MarketCodes    []string  `json:"market_codes"`
	Locale         string    `json:"locale"`
	EffectiveFrom  time.Time `json:"effective_from"`
	EffectiveUntil time.Time `json:"effective_until"`
	ApprovalRef    string    `json:"approval_ref"`
	// RequiresClaim gates the route on a VERIFIED claim (signup_or_trial
	// requires trial eligibility, Doc 05 §15).
	RequiresClaim string `json:"requires_claim,omitempty"`
	Synthetic     bool   `json:"synthetic,omitempty"`
}

// HasDraft reports whether any chunk is DRAFT_PENDING_REVIEW content.
func (m Manifest) HasDraft() bool {
	for _, c := range m.Chunks {
		if c.Draft {
			return true
		}
	}
	return false
}

// HasSynthetic reports whether the release carries any synthetic data. A
// synthetic runtime refuses such a release unless local/dev explicitly
// allows it.
func (m Manifest) HasSynthetic() bool {
	if m.Synthetic {
		return true
	}
	for _, s := range m.Sources {
		if s.Synthetic {
			return true
		}
	}
	for _, r := range m.Routes {
		if r.Synthetic {
			return true
		}
	}
	return false
}

// Validate checks a manifest's internal consistency before it is signed or
// served (KB-008, A14): every chunk must belong to a listed, approved,
// PUBLIC_APPROVED source and carry the same labels; no orphan chunks.
func (m Manifest) Validate() error {
	if m.Kind != KindKB || m.ReleaseID == "" {
		return fmt.Errorf("manifest kind/release id invalid")
	}
	sources := map[string]Source{}
	for _, s := range m.Sources {
		if !(s.ApprovalState == "APPROVED" || (s.Draft && s.ApprovalState == ApprovalDraft)) || s.Classification != Classification || s.Audience != Audience || s.ProductID != ProductID {
			return fmt.Errorf("source %s is not an approved public source", s.SourceVersionID)
		}
		sources[s.SourceVersionID] = s
	}
	for _, c := range m.Chunks {
		s, ok := sources[c.SourceVersionID]
		if !ok {
			return fmt.Errorf("chunk %s has no source in this release", c.ChunkID)
		}
		if c.Classification != s.Classification || c.Audience != s.Audience || c.ReleaseID != m.ReleaseID {
			return fmt.Errorf("chunk %s lost its parent labels", c.ChunkID)
		}
		// KB-008: a chunk keeps its parent's authority tier and market scope,
		// and its text matches its checksum.
		if c.AuthorityTier != s.AuthorityTier || !sameSet(c.MarketCodes, s.MarketCodes) {
			return fmt.Errorf("chunk %s changed its parent's tier or market scope", c.ChunkID)
		}
		if c.TextSHA256 != sha256Hex(c.PlainText) {
			return fmt.Errorf("chunk %s text does not match its checksum", c.ChunkID)
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		if seen[x] == 0 {
			return false
		}
		seen[x]--
	}
	return true
}

// HeadingPrefix marks the start of a section inside chunk text. Retrieval and
// the extractive model use it as context; it is stripped from answers.
func HeadingPrefix(heading string) string { return "\u00a7" + heading + "\u00a7 " }

// Manifest is a signed knowledge release.
type Manifest struct {
	ReleaseID          string `json:"release_id"`
	Kind               string `json:"kind"`
	IndexSchemaVersion string `json:"index_schema_version"`
	// EmbeddingModel and Tokenizer name how the index was built (Doc 03
	// §11): "none" while retrieval is lexical only (D03-08).
	EmbeddingModel  string    `json:"embedding_model,omitempty"`
	Tokenizer       string    `json:"tokenizer,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ApprovedBy      []string  `json:"approved_by"`
	EvalRunID       string    `json:"eval_run_id"`
	ChunkPolicy     string    `json:"chunk_policy"`
	RedactionPolicy string    `json:"redaction_policy"`
	AllowedHosts    []string  `json:"allowed_hosts"`
	Sources         []Source  `json:"sources"`
	Chunks          []Chunk   `json:"chunks"`
	Routes          []Route   `json:"routes"`
	// Synonyms is the signed alias lexicon (Doc 03 §13), e.g. "ar" →
	// "accounts receivable". Keys are lower-case visitor words.
	Synonyms map[string][]string `json:"synonyms,omitempty"`
	// Synthetic marks the whole release as test/dev data.
	Synthetic bool `json:"synthetic,omitempty"`
	// CompatiblePolicyVersions lists the prompt/policy versions this release
	// was evaluated with (Doc 08 OPR-004); the runtime refuses any other.
	CompatiblePolicyVersions []string `json:"compatible_policy_versions,omitempty"`
	// Doc 03 §11 release identity: the product and audience the release is
	// for, and when it was signed. Optional on load, so releases published
	// before these fields still verify.
	ProductID string    `json:"product_id,omitempty"`
	Audience  string    `json:"audience,omitempty"`
	SignedAt  time.Time `json:"signed_at,omitempty"`
}

// TokenizerVersion names the lexical tokenizer the index is built with.
const TokenizerVersion = "lexical-v1"
