// Package knowledge holds the PUBLIC_APPROVED knowledge plane: the source
// registry record (Doc 03 §06), chunk records (§07), the ingestion state
// machine (§09), signed release manifests (§11) and release-pinned lexical
// retrieval with mandatory filters (§14, Doc 05 §18).
package knowledge

import "time"

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
}

// HeadingPrefix marks the start of a section inside chunk text. Retrieval and
// the extractive model use it as context; it is stripped from answers.
func HeadingPrefix(heading string) string { return "\u00a7" + heading + "\u00a7 " }

// Manifest is a signed knowledge release.
type Manifest struct {
	ReleaseID          string    `json:"release_id"`
	Kind               string    `json:"kind"`
	IndexSchemaVersion string    `json:"index_schema_version"`
	CreatedAt          time.Time `json:"created_at"`
	ApprovedBy         []string  `json:"approved_by"`
	EvalRunID          string    `json:"eval_run_id"`
	ChunkPolicy        string    `json:"chunk_policy"`
	RedactionPolicy    string    `json:"redaction_policy"`
	AllowedHosts       []string  `json:"allowed_hosts"`
	Sources            []Source  `json:"sources"`
	Chunks             []Chunk   `json:"chunks"`
	Routes             []Route   `json:"routes"`
}
