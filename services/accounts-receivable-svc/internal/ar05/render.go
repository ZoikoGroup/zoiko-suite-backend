package ar05

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Renderer turns the immutable source document into the profile's syntax. It must be deterministic: the same document
// and profile always give the same bytes (the artifact hash is evidence). Renderers are keyed by profile syntax; a
// syntax with no renderer is refused as EINVOICE_VALIDATION_FAILED instead of being approximated.
type Renderer interface {
	Syntax() string
	ContentType() string
	Render(doc *SourceDocument, p *Profile) ([]byte, error)
}

// Renderers is the registry of renderers by syntax.
type Renderers map[string]Renderer

// DefaultRenderers has the canonical JSON renderer only. UBL_XML and AUTHORITY_SPECIFIC are NOT rendered: a UBL
// document needs party, address and tax-registration data that AR does not hold, so a "UBL-shaped" document would be
// schema-invalid and dishonest. A real renderer for those syntaxes is plugged in through Collaborators.Renderers.
func DefaultRenderers() Renderers { return Renderers{SyntaxJSON: JSONRenderer{}} }

// Syntaxes the profile registry knows.
const (
	SyntaxJSON  = "JSON"
	SyntaxUBL   = "UBL_XML"
	SyntaxOther = "AUTHORITY_SPECIFIC"
)

// For returns the renderer of a syntax or a typed "syntax not supported" validation failure.
func (r Renderers) For(syntax string) (Renderer, error) {
	if x, ok := r[syntax]; ok && x != nil {
		return x, nil
	}
	return nil, validationFailed("the profile syntax %s is not supported by this service (no renderer): the document is not rendered or transmitted", syntax)
}

// ── canonical field catalogue ────────────────────────────────────────────────

const day = "2006-01-02"

// docFields are the document-level canonical fields a profile's semantic mapping may reference. The value is derived
// from the source document only.
var docFields = map[string]func(*SourceDocument) string{
	"invoice.number":                 func(d *SourceDocument) string { return d.Number },
	"invoice.issue_date":             func(d *SourceDocument) string { return fmtDay(d.IssueDate) },
	"invoice.due_date":               func(d *SourceDocument) string { return fmtDay(d.DueDate) },
	"invoice.currency":               func(d *SourceDocument) string { return d.Currency },
	"invoice.net_total":              func(d *SourceDocument) string { return d.Net.String() },
	"invoice.tax_total":              func(d *SourceDocument) string { return d.Tax.String() },
	"invoice.gross_total":            func(d *SourceDocument) string { return d.Gross.String() },
	"invoice.seller.legal_entity_id": func(d *SourceDocument) string { return d.LegalEntityID },
	"invoice.buyer.party_id":         func(d *SourceDocument) string { return d.CustomerPartyID },
	"invoice.tax.determination_id":   func(d *SourceDocument) string { return d.TaxDeterminationID },
	"invoice.tax.rule_pack_version":  func(d *SourceDocument) string { return d.TaxRulePackVersion },
	"invoice.content_hash":           func(d *SourceDocument) string { return d.ContentHash },
}

var lineFields = map[string]func(SourceLine) string{
	"invoice.line.number":      func(l SourceLine) string { return fmt.Sprint(l.LineNumber) },
	"invoice.line.description": func(l SourceLine) string { return l.Description },
	"invoice.line.quantity":    func(l SourceLine) string { return l.Quantity.String() },
	"invoice.line.unit_price":  func(l SourceLine) string { return l.UnitPrice.String() },
	"invoice.line.net":         func(l SourceLine) string { return l.Net.String() },
	"invoice.line.tax":         func(l SourceLine) string { return l.Tax.String() },
	"invoice.line.tax_code":    func(l SourceLine) string { return l.TaxCode },
}

func fmtDay(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(day)
}

// baseRequired are the canonical fields every e-invoice needs whatever the profile maps (checked by Validate).
var baseRequired = []string{"invoice.number", "invoice.issue_date", "invoice.currency", "invoice.net_total", "invoice.tax_total",
	"invoice.gross_total", "invoice.seller.legal_entity_id", "invoice.buyer.party_id"}

// ── JSON renderer ────────────────────────────────────────────────────────────

// JSONRenderer is the deterministic canonical JSON renderer (profile syntax JSON). The output keys document elements by
// the TARGET names of the profile's semantic mapping; encoding/json writes map keys sorted, and nothing time- or
// randomness-dependent is included, so the bytes are a pure function of (document, profile).
type JSONRenderer struct{}

func (JSONRenderer) Syntax() string      { return SyntaxJSON }
func (JSONRenderer) ContentType() string { return "application/json" }

type jsonMeta struct {
	Schema       string       `json:"schema"`
	DocumentType string       `json:"document_type"`
	Profile      string       `json:"profile_code"`
	Version      string       `json:"profile_version"`
	Network      string       `json:"network"`
	Mode         string       `json:"transmission_mode"`
	Syntax       string       `json:"syntax"`
	ContentHash  string       `json:"source_content_hash"`
	Dependencies []Dependency `json:"dependencies"`
}

type jsonArtifact struct {
	Meta     jsonMeta            `json:"meta"`
	Document map[string]string   `json:"document"`
	Lines    []map[string]string `json:"lines"`
}

// Render builds the artifact. A mapping entry whose canonical field AR cannot supply is omitted here and reported by
// Validate (the artifact must not pretend to contain it).
func (JSONRenderer) Render(doc *SourceDocument, p *Profile) ([]byte, error) {
	if doc == nil || p == nil {
		return nil, fmt.Errorf("render: document and profile are required")
	}
	out := jsonArtifact{
		Meta: jsonMeta{Schema: SchemaJSONArtifact, DocumentType: doc.DocumentType, Profile: p.ProfileCode, Version: p.ProfileVersion, Network: p.Network,
			Mode: p.Mode, Syntax: SyntaxJSON, ContentHash: doc.ContentHash, Dependencies: sortedDeps(p.DependenciesOn(p.EffectiveAt))},
		Document: map[string]string{},
		Lines:    make([]map[string]string, 0, len(doc.Lines)),
	}
	for _, m := range p.SemanticMapping {
		if f, ok := docFields[m.From]; ok {
			out.Document[m.To] = f(doc)
		}
	}
	for _, l := range doc.Lines {
		row := map[string]string{}
		for _, m := range p.SemanticMapping {
			if f, ok := lineFields[m.From]; ok {
				row[m.To] = f(l)
			}
		}
		out.Lines = append(out.Lines, row)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortedDeps(in []Dependency) []Dependency {
	out := append([]Dependency{}, in...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Ref != b.Ref {
			return a.Ref < b.Ref
		}
		return a.Version < b.Version
	})
	return out
}

// Sha256Hex is the artifact / digest hash used throughout.
func Sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ── validation (structural / required-field, against the profile) ─────────────

// Issue is one validation finding.
type Issue struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidationReport is the stored result of one validation run.
type ValidationReport struct {
	Passed       bool         `json:"passed"`
	Issues       []Issue      `json:"issues"`
	Dependencies []Dependency `json:"dependencies"` // the schema / code-list versions in force that the artifact was checked against
}

// Validate checks the artifact and the source document against the profile. It is structural: required canonical fields
// are present, every mapped field is one AR can supply and is non-empty, amounts reconcile, the currency fits, the
// artifact is the profile's syntax and still hashes to the stored value. It does NOT validate against the external XSD /
// schematron / code-list contents (those packages are only recorded by version); a real renderer/validator for a
// syntax adds that.
func Validate(doc *SourceDocument, p *Profile, artifact []byte, storedArtifactHash string) ValidationReport {
	rep := ValidationReport{Dependencies: sortedDeps(p.DependenciesOn(p.EffectiveAt))}
	add := func(code, field, format string, a ...any) {
		rep.Issues = append(rep.Issues, Issue{Code: code, Field: field, Message: fmt.Sprintf(format, a...)})
	}
	if Sha256Hex(artifact) != storedArtifactHash {
		add("ARTIFACT_HASH_MISMATCH", "", "the stored artifact no longer hashes to the recorded artifact hash")
	}
	if doc.DocumentType != p.DocumentType {
		add("DOCUMENT_TYPE_MISMATCH", "document_type", "the document is a %s but the profile is for %s", doc.DocumentType, p.DocumentType)
	}
	if len(p.SemanticMapping) == 0 {
		add("PROFILE_HAS_NO_MAPPING", "", "the profile has no semantic mapping, so nothing can be rendered")
	}
	if p.Syntax == SyntaxJSON {
		var a jsonArtifact
		if err := json.Unmarshal(artifact, &a); err != nil {
			add("ARTIFACT_NOT_JSON", "", "the artifact is not valid JSON: %v", err)
		} else if a.Meta.Profile != p.ProfileCode || a.Meta.Version != p.ProfileVersion || a.Meta.ContentHash != doc.ContentHash {
			add("ARTIFACT_PROFILE_MISMATCH", "meta", "the artifact was not rendered for this profile version and document content")
		}
	}
	mapped := map[string]string{}
	for _, m := range p.SemanticMapping {
		mapped[m.From] = m.To
		_, isDoc := docFields[m.From]
		_, isLine := lineFields[m.From]
		if !isDoc && !isLine {
			add("REQUIRED_FIELD_UNSUPPORTED", m.From, "the profile requires %q (%s), which AR cannot supply", m.From, m.To)
			continue
		}
		if isDoc && strings.TrimSpace(docFields[m.From](doc)) == "" {
			add("REQUIRED_FIELD_MISSING", m.From, "%q (%s) is empty on the source document", m.From, m.To)
		}
	}
	for _, f := range baseRequired {
		if v := docFields[f](doc); strings.TrimSpace(v) == "" {
			add("REQUIRED_FIELD_MISSING", f, "%q is empty on the source document", f)
		}
		if _, ok := mapped[f]; !ok {
			add("FIELD_NOT_MAPPED", f, "the profile does not map the mandatory field %q", f)
		}
	}
	if len(doc.Lines) == 0 {
		add("NO_LINES", "invoice.line", "the document has no lines")
	}
	if !doc.Gross.FitsCurrency(doc.Currency) || !doc.Net.FitsCurrency(doc.Currency) || !doc.Tax.FitsCurrency(doc.Currency) {
		add("AMOUNT_PRECISION", "invoice.currency", "an amount has more decimals than %s allows", doc.Currency)
	}
	if doc.Net.Add(doc.Tax) != doc.Gross {
		add("TOTALS_DO_NOT_RECONCILE", "invoice.gross_total", "net %s + tax %s is not gross %s", doc.Net, doc.Tax, doc.Gross)
	}
	var ln, lt kit.Amount
	for _, l := range doc.Lines {
		ln, lt = ln.Add(l.Net), lt.Add(l.Tax)
		if l.Net.IsNegative() || strings.TrimSpace(l.Description) == "" {
			add("LINE_INVALID", fmt.Sprintf("invoice.line[%d]", l.LineNumber), "line %d needs a description and a non-negative net", l.LineNumber)
		}
	}
	if len(doc.Lines) > 0 && (ln != doc.Net || lt != doc.Tax) {
		add("LINES_DO_NOT_SUM_TO_TOTALS", "invoice.line", "lines sum to net %s / tax %s, the totals are %s / %s", ln, lt, doc.Net, doc.Tax)
	}
	if len(rep.Dependencies) == 0 {
		add("NO_DEPENDENCY_IN_FORCE", "", "no schema or code-list version of the profile is in force on %s", fmtDay(p.EffectiveAt))
	}
	rep.Passed = len(rep.Issues) == 0
	if rep.Issues == nil {
		rep.Issues = []Issue{}
	}
	return rep
}
