package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ZS-JUR-001 Wave 0: regime, source, interpretation and pack registries.

// Regime is a cohesive regulatory domain (VAT/GST, withholding, e-invoicing...).
type Regime struct {
	RegimeID             string    `json:"regime_id"`
	RegimeCode           string    `json:"regime_code"`
	RegimeName           string    `json:"regime_name"`
	Description          *string   `json:"description"`
	ActiveFlag           bool      `json:"active_flag"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	SchemaVersion        string    `json:"schema_version"`
}

// RegulatorySource is one row of the Source Register: evidence, not a bibliography.
type RegulatorySource struct {
	SourceID              string     `json:"source_id"`
	JurisdictionID        string     `json:"jurisdiction_id"`
	Authority             string     `json:"authority"`
	SourceType            string     `json:"source_type"`
	AuthorityLevel        string     `json:"authority_level"`
	Title                 string     `json:"title"`
	OfficialIdentifier    *string    `json:"official_identifier"`
	PublishedOn           *string    `json:"published_on"`
	EffectiveOn           *string    `json:"effective_on"`
	Location              string     `json:"location"`
	SnapshotHash          string     `json:"snapshot_hash"`
	SnapshotRef           *string    `json:"snapshot_ref"`
	Language              string     `json:"language"`
	TranslationRef        *string    `json:"translation_ref"`
	InterpretationNotes   *string    `json:"interpretation_notes"`
	ReviewedByPrincipalID *string    `json:"reviewed_by_principal_id"`
	ReviewedAt            *time.Time `json:"reviewed_at"`
	SupersededBySourceID  *string    `json:"superseded_by_source_id"`
	SupersededAt          *time.Time `json:"superseded_at"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	SchemaVersion         string     `json:"schema_version"`
}

// InterpretationRecord is the approved rationale behind a rule (s8, s20).
type InterpretationRecord struct {
	InterpretationID     string     `json:"interpretation_id"`
	JurisdictionID       string     `json:"jurisdiction_id"`
	RegimeID             *string    `json:"regime_id"`
	Subject              string     `json:"subject"`
	Decision             string     `json:"decision"`
	Rationale            string     `json:"rationale"`
	Status               string     `json:"status"`
	SourceIDs            []string   `json:"source_ids"`
	ApprovedByPrincipal  *string    `json:"approved_by_principal_id"`
	ApprovedAt           *time.Time `json:"approved_at"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	SchemaVersion        string     `json:"schema_version"`
}

// Pack is the identity of a jurisdiction pack; versions carry the content.
type Pack struct {
	PackID               string    `json:"pack_id"`
	PackRef              string    `json:"pack_ref"`
	PackName             string    `json:"pack_name"`
	Owner                string    `json:"owner"`
	SupportOwner         *string   `json:"support_owner"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	SchemaVersion        string    `json:"schema_version"`
}

// PackVersion is one immutable-once-released pack release (s5, s23).
type PackVersion struct {
	PackVersionID         string          `json:"pack_version_id"`
	PackID                string          `json:"pack_id"`
	PackRef               string          `json:"pack_ref"`
	Version               string          `json:"version"`
	Status                string          `json:"status"`
	EffectiveFrom         string          `json:"effective_from"`
	EffectiveTo           *string         `json:"effective_to"`
	Manifest              json.RawMessage `json:"manifest"`
	ManifestDigest        string          `json:"manifest_digest"`
	ArtifactDigest        *string         `json:"artifact_digest"`
	TestBundleDigest      *string         `json:"test_bundle_digest"`
	Signature             *string         `json:"signature"`
	SignatureKeyRef       *string         `json:"signature_key_ref"`
	SourceRegisterVersion *int            `json:"source_register_version"`
	CreatedAt             time.Time       `json:"created_at"`
	CreatedByPrincipalID  string          `json:"created_by_principal_id"`
	UpdatedAt             *time.Time      `json:"updated_at,omitempty"`
	UpdatedByPrincipalID  *string         `json:"updated_by_principal_id,omitempty"`
	SchemaVersion         string          `json:"schema_version"`
}

// RuleProvenance is the s7 temporal/provenance metadata of one rule version.
type RuleProvenance struct {
	JurisdictionRuleID string   `json:"jurisdiction_rule_id"`
	RegimeID           *string  `json:"regime_id"`
	InterpretationID   *string  `json:"interpretation_id"`
	SupersedesRuleID   *string  `json:"supersedes_rule_id"`
	Precedence         *int     `json:"precedence"`
	PublishedOn        *string  `json:"published_on"`
	SourceIDs          []string `json:"source_ids"`
}

// Param structs: what the store needs, with identity already resolved by the handler.

type CreateRegimeParams struct {
	RegimeCode, RegimeName string
	Description            *string
	CreatedBy              string
}

type CreateSourceParams struct {
	JurisdictionID, Authority, SourceType, AuthorityLevel, Title string
	OfficialIdentifier                                           *string
	PublishedOn, EffectiveOn                                     *string
	Location, SnapshotHash                                       string
	SnapshotRef                                                  *string
	Language                                                     string
	TranslationRef, InterpretationNotes                          *string
	CreatedBy                                                    string
}

type CreateInterpretationParams struct {
	JurisdictionID               string
	RegimeID                     *string
	Subject, Decision, Rationale string
	SourceIDs                    []string
	CreatedBy                    string
}

type CreatePackParams struct {
	PackRef, PackName, Owner string
	SupportOwner             *string
	CreatedBy                string
}

type CreatePackVersionParams struct {
	PackRef        string
	Manifest       PackManifest
	ManifestJSON   []byte
	ManifestDigest string
	CreatedBy      string
}

type SetRuleProvenanceParams struct {
	JurisdictionRuleID string
	RegimeID           *string
	InterpretationID   *string
	SupersedesRuleID   *string
	Precedence         *int
	PublishedOn        *string
	SourceIDs          []string
	ActorID            string
}

// Registry errors.
var (
	ErrRegimeNotFound           = errorString("regulatory regime not found")
	ErrSourceNotFound           = errorString("regulatory source not found")
	ErrInterpretationNotFound   = errorString("interpretation record not found")
	ErrPackNotFound             = errorString("jurisdiction pack not found")
	ErrPackVersionNotFound      = errorString("jurisdiction pack version not found")
	ErrNotIndependent           = errorString("the reviewer or approver must be a different principal from the author")
	ErrAlreadyDecided           = errorString("this record has already been reviewed, approved or superseded")
	ErrNotDraft                 = errorString("the record is past DRAFT and is immutable")
	ErrVersionNotNewer          = errorString("pack version must be greater than every existing version of the pack")
	ErrUnapprovedInterpretation = errorString("the interpretation is not approved")
	ErrSourceNotReady           = errorString("every source of the interpretation must be independently reviewed and not superseded")
	ErrInvalidReference         = errorString("a referenced record does not exist")
	ErrManifestInvalid          = errorString("pack manifest is invalid")
)

// ── Pack manifest (s5) ──────────────────────────────────────────────────────

// PackDependency pins one exact dependency (a pack, code list or schema).
type PackDependency struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
}

// PackManifest is the client-supplied part of the manifest. The server owns
// status, digests and signature, so they are not accepted here: decoding with
// DisallowUnknownFields rejects a manifest that tries to assert them.
type PackManifest struct {
	PackID                string           `json:"pack_id"`
	PackVersion           string           `json:"pack_version"`
	JurisdictionIDs       []string         `json:"jurisdiction_ids"`
	Regimes               []string         `json:"regimes"`
	EffectiveFrom         string           `json:"effective_from"`
	EffectiveTo           *string          `json:"effective_to"`
	Dependencies          []PackDependency `json:"dependencies"`
	SchemaDependencies    []PackDependency `json:"schema_dependencies"`
	SourceRegisterVersion *int             `json:"source_register_version"`
	RuleModules           []string         `json:"rule_modules"`
	// CalendarModules are calendar_version_id UUIDs and ObligationModules are
	// obligation_rule_id UUIDs (ZS-JUR-001 Wave 4); both must be PUBLISHED.
	CalendarModules   []string `json:"calendar_modules"`
	ObligationModules []string `json:"obligation_modules"`
}

var (
	packRefRe     = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z0-9-]+)+$`)
	packVersionRe = regexp.MustCompile(`^\d+(\.\d+){1,3}$`)
	digestRe      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uuidRe        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidPackRef reports whether s is a legal pack identifier (jur.gb.tax.core).
func ValidPackRef(s string) bool { return packRefRe.MatchString(s) }

// ValidDigest reports whether s is a sha256:<64 hex> digest.
func ValidDigest(s string) bool { return digestRe.MatchString(s) }

// ParseManifest decodes and structurally validates a manifest. It checks
// shape only; resolution of jurisdictions, regimes and rules against the
// registries is the store's job, and deep checks belong to the Wave 1 compiler.
func ParseManifest(raw []byte) (PackManifest, error) {
	var m PackManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	if dec.More() {
		return m, fmt.Errorf("%w: trailing data", ErrManifestInvalid)
	}
	return m, m.Validate()
}

// Validate checks the manifest's shape.
func (m PackManifest) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrManifestInvalid, fmt.Sprintf(format, a...))
	}
	if !packRefRe.MatchString(m.PackID) {
		return bad("pack_id %q must look like jur.gb.tax.core", m.PackID)
	}
	if !packVersionRe.MatchString(m.PackVersion) {
		return bad("pack_version %q must be dotted numbers such as 2026.08.1", m.PackVersion)
	}
	if len(m.JurisdictionIDs) == 0 {
		return bad("jurisdiction_ids must name at least one jurisdiction")
	}
	if len(m.Regimes) == 0 {
		return bad("regimes must name at least one regime")
	}
	if dup, ok := firstDuplicate(m.JurisdictionIDs); ok {
		return bad("jurisdiction_ids repeats %q", dup)
	}
	if dup, ok := firstDuplicate(m.Regimes); ok {
		return bad("regimes repeats %q", dup)
	}
	for _, s := range append(append([]string{}, m.JurisdictionIDs...), m.Regimes...) {
		if strings.TrimSpace(s) == "" {
			return bad("jurisdiction_ids and regimes cannot contain blank entries")
		}
	}
	from, err := time.Parse("2006-01-02", m.EffectiveFrom)
	if err != nil {
		return bad("effective_from must be a YYYY-MM-DD date")
	}
	if m.EffectiveTo != nil {
		to, err := time.Parse("2006-01-02", *m.EffectiveTo)
		if err != nil {
			return bad("effective_to must be a YYYY-MM-DD date")
		}
		if !to.After(from) {
			return bad("effective_to must be after effective_from")
		}
	}
	for _, group := range []struct {
		name string
		deps []PackDependency
	}{{"dependencies", m.Dependencies}, {"schema_dependencies", m.SchemaDependencies}} {
		seen := map[string]bool{}
		for _, d := range group.deps {
			if strings.TrimSpace(d.Ref) == "" || strings.TrimSpace(d.Version) == "" {
				return bad("%s entries need both ref and an exact version (floating dependencies are not allowed)", group.name)
			}
			if strings.ContainsAny(d.Version, "*^~><=, ") || strings.EqualFold(d.Version, "latest") {
				return bad("%s %q: version %q is not an exact pin", group.name, d.Ref, d.Version)
			}
			if seen[d.Ref] {
				return bad("%s repeats %q", group.name, d.Ref)
			}
			seen[d.Ref] = true
		}
	}
	for _, id := range m.RuleModules {
		if !uuidRe.MatchString(id) {
			return bad("rule_modules entries must be jurisdiction_rule_id UUIDs, got %q", id)
		}
	}
	for _, id := range m.CalendarModules {
		if !uuidRe.MatchString(id) {
			return bad("calendar_modules entries must be calendar_version_id UUIDs, got %q", id)
		}
	}
	for _, id := range m.ObligationModules {
		if !uuidRe.MatchString(id) {
			return bad("obligation_modules entries must be obligation_rule_id UUIDs, got %q", id)
		}
	}
	if m.SourceRegisterVersion != nil && *m.SourceRegisterVersion < 1 {
		return bad("source_register_version must be positive")
	}
	return nil
}

func firstDuplicate(in []string) (string, bool) {
	seen := map[string]bool{}
	for _, s := range in {
		if seen[s] {
			return s, true
		}
		seen[s] = true
	}
	return "", false
}

// CanonicalJSON re-encodes JSON with sorted object keys and no insignificant
// whitespace, so semantically equal documents hash identically.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map keys are emitted sorted
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DigestOf returns the sha256 digest of the canonical form of raw JSON.
func DigestOf(raw []byte) (string, error) {
	c, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CompareVersions orders two dotted-number versions numerically
// (2026.08.10 > 2026.08.9). It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// ── Source validation ───────────────────────────────────────────────────────

// ValidateSource checks the shape of a source capture request.
func ValidateSource(p CreateSourceParams) error {
	for _, f := range []struct{ name, val string }{
		{"jurisdiction_id", p.JurisdictionID}, {"authority", p.Authority}, {"source_type", p.SourceType},
		{"authority_level", p.AuthorityLevel}, {"title", p.Title}, {"location", p.Location},
	} {
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	if !digestRe.MatchString(p.SnapshotHash) {
		return fmt.Errorf("snapshot_hash must be sha256:<64 hex characters>: a source must be identifiable later")
	}
	for name, d := range map[string]*string{"published_on": p.PublishedOn, "effective_on": p.EffectiveOn} {
		if d != nil {
			if _, err := time.Parse("2006-01-02", *d); err != nil {
				return fmt.Errorf("%s must be a YYYY-MM-DD date", name)
			}
		}
	}
	return nil
}
