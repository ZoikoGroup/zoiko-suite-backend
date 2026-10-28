// Package mapping is the declarative source-to-canonical transform of ZS-SVC-T-001 (§8.2, OD-03).
//
// A mapping is DATA: a versioned, hashed JSON specification interpreted by this engine. Nothing in a mapping can
// execute code, call out, or read anything but the one source document it is given, which is what makes the
// guarantees below checkable:
//
//   - deterministic: the same source document and the same specification produce byte-identical canonical output;
//   - no invented facts: a default exists only where the contract declares it non-business (a constant tag, say),
//     and never for an amount, date, enumeration, identifier or any other business fact;
//   - no silent loss: a decimal that would lose digits, an unknown enumeration value, a date without a zone, a
//     string that does not fit — each stops the mapping and names the rule, rather than rounding, truncating or
//     defaulting;
//   - minimized: source fields the specification does not name never reach the canonical payload;
//   - traceable: every canonical field records the source pointer and rule it came from.
//
// Issues carry a quarantine class, a stable code and the field names involved. They never carry values: a
// failed mapping is reported to operators and must not become a second copy of the sensitive payload (I-28).
package mapping

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// QuarantineClass is why a source document was held (§8.3).
type QuarantineClass string

const (
	ClassSchema             QuarantineClass = "SCHEMA"
	ClassMapping            QuarantineClass = "MAPPING"
	ClassAuthContext        QuarantineClass = "AUTH_CONTEXT"
	ClassDataPolicy         QuarantineClass = "DATA_POLICY"
	ClassDomainPrecondition QuarantineClass = "DOMAIN_PRECONDITION"
)

// Field types.
const (
	TypeString   = "string"
	TypeInteger  = "integer"
	TypeDecimal  = "decimal"
	TypeBoolean  = "boolean"
	TypeDate     = "date"
	TypeDatetime = "datetime"
	TypeEnum     = "enum"
)

// Limits bound what one evaluation may consume.
const (
	DefaultMaxSourceBytes = 8 << 20
	maxDepth              = 64
	maxDecimalDigits      = 38
	maxFields             = 512
)

// ErrSpecInvalid is returned for a mapping specification that cannot be compiled.
var ErrSpecInvalid = errors.New("mapping specification is invalid")

// Spec is a mapping specification.
type Spec struct {
	// CanonicalContract names the domain contract the output satisfies. The domain consumer accepts it at
	// publication; the engine only records it.
	CanonicalContract string  `json:"canonical_contract"`
	Fields            []Field `json:"fields"`
	// ProhibitedSourcePaths lists source pointers that must never arrive. A document carrying one is held
	// before anything is mapped (NP-12).
	ProhibitedSourcePaths []string `json:"prohibited_source_paths,omitempty"`
}

// Field is one canonical field.
type Field struct {
	// To is the canonical location, a JSON pointer ("/amount", "/payer/name").
	To string `json:"to"`
	// From is the source location, a JSON pointer. It is empty only for a constant.
	From string `json:"from,omitempty"`
	Type string `json:"type"`

	Required  bool `json:"required,omitempty"`
	Trim      bool `json:"trim,omitempty"`
	MaxLength *int `json:"max_length,omitempty"`

	// Scale is the most fractional digits a decimal may carry. A source value with more significant digits is
	// refused, not rounded.
	Scale *int `json:"scale,omitempty"`

	// Enum maps source values to canonical ones, exactly (case-sensitive). A source value not listed is
	// quarantined unless UnknownDefault says otherwise.
	Enum           map[string]string `json:"enum,omitempty"`
	UnknownDefault *string           `json:"unknown_default,omitempty"`

	// KeepSourceOffset also emits "<to>_source_offset" for a datetime, so the canonical UTC instant keeps the
	// offset the source expressed.
	KeepSourceOffset bool `json:"keep_source_offset,omitempty"`

	// Const and DefaultIfMissing are contract-declared non-business defaults. Both need NonBusinessDefault, and
	// only a string or boolean may be defaulted at all: an amount, date, enumeration or identifier is never
	// invented.
	Const              *string `json:"const,omitempty"`
	DefaultIfMissing   *string `json:"default_if_missing,omitempty"`
	NonBusinessDefault bool    `json:"non_business_default,omitempty"`
}

// Issue is one reason a document was held. It never contains a value from the document.
type Issue struct {
	Class  QuarantineClass `json:"class"`
	Code   string          `json:"code"`
	Field  string          `json:"field,omitempty"`  // canonical pointer
	Source string          `json:"source,omitempty"` // source pointer
}

// Lineage ties one canonical field to its source (I-10).
type Lineage struct {
	CanonicalField   string `json:"canonical_field"`
	SourcePath       string `json:"source_path,omitempty"`
	Rule             string `json:"rule"`
	MappingVersionID string `json:"mapping_version_id,omitempty"`
	Defaulted        bool   `json:"defaulted,omitempty"`
}

// Result is the outcome of one evaluation. When Issues is non-empty there is no Canonical output at all: a
// partial canonical payload is never handed anywhere.
type Result struct {
	Canonical     json.RawMessage `json:"canonical,omitempty"`
	CanonicalHash string          `json:"canonical_hash,omitempty"`
	Lineage       []Lineage       `json:"lineage,omitempty"`
	Issues        []Issue         `json:"issues,omitempty"`
	// DroppedTopLevel counts source members the specification does not name.
	DroppedTopLevel int `json:"dropped_top_level"`
}

// OK reports whether the mapping produced canonical output.
func (r *Result) OK() bool { return len(r.Issues) == 0 }

// Compiled is a validated specification ready to evaluate.
type Compiled struct {
	spec Spec
	hash string
}

var (
	pointerRE = regexp.MustCompile(`^(/([^/~]|~[01])*)+$`)
	// a canonical payload never carries a credential, whatever the source called it
	secretish = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|authorization|credential)`)
)

// Compile validates spec. Unknown keys in the JSON form are refused by CompileJSON, so a field that is not
// enforced can never be silently ignored.
func Compile(spec Spec) (*Compiled, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(format, a...))
	}
	if strings.TrimSpace(spec.CanonicalContract) == "" {
		return nil, bad("canonical_contract is required")
	}
	if len(spec.Fields) == 0 || len(spec.Fields) > maxFields {
		return nil, bad("fields must hold between 1 and %d entries", maxFields)
	}
	seen := map[string]bool{}
	var tos []string
	for i, f := range spec.Fields {
		at := fmt.Sprintf("fields[%d]", i)
		if !pointerRE.MatchString(f.To) {
			return nil, bad("%s.to is not a JSON pointer", at)
		}
		if seen[f.To] {
			return nil, bad("%s.to repeats %s", at, f.To)
		}
		seen[f.To] = true
		tos = append(tos, f.To)
		if secretish.MatchString(f.To) {
			return nil, bad("%s.to would carry a credential into the canonical payload", at)
		}
		hasDefault := f.Const != nil || f.DefaultIfMissing != nil
		if f.Const != nil && f.From != "" {
			return nil, bad("%s is a constant and must not name a source", at)
		}
		if f.Const == nil && !pointerRE.MatchString(f.From) {
			return nil, bad("%s.from is not a JSON pointer", at)
		}
		if f.Const != nil && f.DefaultIfMissing != nil {
			return nil, bad("%s sets both const and default_if_missing", at)
		}
		if hasDefault {
			if !f.NonBusinessDefault {
				return nil, bad("%s: a default must be declared non_business_default (no invented business facts)", at)
			}
			if f.Type != TypeString && f.Type != TypeBoolean {
				return nil, bad("%s: only a string or boolean may be defaulted, never %s", at, f.Type)
			}
			if f.Type == TypeBoolean {
				if v := derefStr(f.Const, f.DefaultIfMissing); v != "true" && v != "false" {
					return nil, bad("%s: a boolean default is true or false", at)
				}
			}
			if f.Required && f.DefaultIfMissing != nil {
				return nil, bad("%s: a field with a default is not required", at)
			}
		} else if f.NonBusinessDefault {
			return nil, bad("%s declares non_business_default without a default", at)
		}
		switch f.Type {
		case TypeString:
			if f.MaxLength != nil && *f.MaxLength < 1 {
				return nil, bad("%s.max_length must be positive", at)
			}
		case TypeInteger, TypeBoolean, TypeDate:
		case TypeDatetime:
		case TypeDecimal:
			if f.Scale == nil || *f.Scale < 0 || *f.Scale > 18 {
				return nil, bad("%s: a decimal declares scale between 0 and 18", at)
			}
		case TypeEnum:
			if len(f.Enum) == 0 {
				return nil, bad("%s: an enum lists its mapped values", at)
			}
		default:
			return nil, bad("%s.type %q is not supported", at, f.Type)
		}
		if f.Type != TypeEnum && (len(f.Enum) > 0 || f.UnknownDefault != nil) {
			return nil, bad("%s: enum settings on a non-enum field", at)
		}
		if f.Type != TypeDecimal && f.Scale != nil {
			return nil, bad("%s: scale on a non-decimal field", at)
		}
		if f.Type != TypeDatetime && f.KeepSourceOffset {
			return nil, bad("%s: keep_source_offset on a non-datetime field", at)
		}
		if f.Type != TypeString && (f.MaxLength != nil || f.Trim) {
			return nil, bad("%s: string settings on a non-string field", at)
		}
	}
	// one canonical location may not sit inside another (/a and /a/b)
	sort.Strings(tos)
	for i := 1; i < len(tos); i++ {
		if strings.HasPrefix(tos[i], tos[i-1]+"/") {
			return nil, bad("canonical %s nests inside %s", tos[i], tos[i-1])
		}
	}
	for i, p := range spec.ProhibitedSourcePaths {
		if !pointerRE.MatchString(p) {
			return nil, bad("prohibited_source_paths[%d] is not a JSON pointer", i)
		}
	}
	canon, err := CanonicalJSON(spec)
	if err != nil {
		return nil, bad("specification cannot be hashed")
	}
	sum := sha256.Sum256(canon)
	return &Compiled{spec: spec, hash: hex.EncodeToString(sum[:])}, nil
}

// CompileJSON parses raw (refusing unknown keys) and compiles it.
func CompileJSON(raw []byte) (*Compiled, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: specification is empty", ErrSpecInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSpecInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing content", ErrSpecInvalid)
	}
	return Compile(s)
}

// Hash is the transform hash: SHA-256 of the specification's canonical form. Two mappings with the same hash
// transform identically.
func (c *Compiled) Hash() string { return c.hash }

// Spec returns the specification.
func (c *Compiled) Spec() Spec { return c.spec }

// Evaluate maps one source document. mappingVersionID is recorded in the lineage. maxBytes ≤ 0 means the default.
func (c *Compiled) Evaluate(source []byte, mappingVersionID string, maxBytes int) *Result {
	res := &Result{}
	issue := func(cl QuarantineClass, code, field, src string) {
		res.Issues = append(res.Issues, Issue{Class: cl, Code: code, Field: field, Source: src})
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxSourceBytes
	}
	if len(source) > maxBytes {
		issue(ClassSchema, "source_too_large", "", "")
		return res
	}
	doc, err := decodeExact(source)
	if err != nil {
		issue(ClassSchema, "source_not_json", "", "")
		return res
	}
	if depth(doc, 0) > maxDepth {
		issue(ClassSchema, "source_too_deep", "", "")
		return res
	}
	obj, isObj := doc.(map[string]any)
	if !isObj {
		issue(ClassSchema, "source_not_object", "", "")
		return res
	}

	// prohibited data is stopped before anything is mapped, and nothing is emitted (NP-12)
	for _, p := range c.spec.ProhibitedSourcePaths {
		if v, ok := lookup(doc, p); ok && v != nil {
			issue(ClassDataPolicy, "prohibited_field", "", p)
		}
	}
	if len(res.Issues) > 0 {
		return res
	}

	out := map[string]any{}
	named := map[string]bool{}
	for _, f := range c.spec.Fields {
		if f.From != "" {
			if first := firstSegment(f.From); first != "" {
				named[first] = true
			}
		}
		val, lin, iss := c.field(doc, f, mappingVersionID)
		if iss != nil {
			res.Issues = append(res.Issues, *iss)
			continue
		}
		if lin == nil { // optional and absent
			continue
		}
		setPointer(out, f.To, val)
		res.Lineage = append(res.Lineage, *lin)
		if f.Type == TypeDatetime && f.KeepSourceOffset {
			if off, ok := val.(datetimeValue); ok {
				setPointer(out, f.To+"_source_offset", off.offset)
			}
		}
	}
	for k := range obj {
		if !named[k] {
			res.DroppedTopLevel++
		}
	}
	if len(res.Issues) > 0 {
		res.Lineage = nil
		return res
	}
	flatten(out)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		issue(ClassMapping, "canonical_unencodable", "", "")
		return res
	}
	res.Canonical = json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
	sum := sha256.Sum256(res.Canonical)
	res.CanonicalHash = hex.EncodeToString(sum[:])
	return res
}

// datetimeValue carries the canonical UTC instant and the source offset to the writer; flatten replaces it
// with the plain string before encoding.
type datetimeValue struct {
	utc    string
	offset string
}

func flatten(m map[string]any) {
	for k, v := range m {
		switch t := v.(type) {
		case datetimeValue:
			m[k] = t.utc
		case map[string]any:
			flatten(t)
		}
	}
}

func (c *Compiled) field(doc any, f Field, mvID string) (any, *Lineage, *Issue) {
	mk := func(cl QuarantineClass, code string) (any, *Lineage, *Issue) {
		return nil, nil, &Issue{Class: cl, Code: code, Field: f.To, Source: f.From}
	}
	lin := &Lineage{CanonicalField: f.To, SourcePath: f.From, Rule: f.Type, MappingVersionID: mvID}

	if f.Const != nil {
		lin.Rule, lin.Defaulted = "const", true
		return typedDefault(f, *f.Const), lin, nil
	}
	raw, present := lookup(doc, f.From)
	if raw == nil { // absent and JSON null are both "no value"
		present = false
	}
	if present {
		if s, ok := raw.(string); ok && f.Type == TypeString && f.Trim && strings.TrimSpace(s) == "" {
			present = false
		}
	}
	if !present {
		switch {
		case f.DefaultIfMissing != nil:
			lin.Rule, lin.Defaulted = "default", true
			return typedDefault(f, *f.DefaultIfMissing), lin, nil
		case f.Required:
			// a missing business fact is never invented (NP-11)
			return mk(ClassMapping, "required_missing")
		default:
			return nil, nil, nil
		}
	}

	switch f.Type {
	case TypeString:
		s, ok := raw.(string)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		if f.Trim {
			s = strings.TrimSpace(s)
		}
		if f.MaxLength != nil && len([]rune(s)) > *f.MaxLength {
			return mk(ClassMapping, "value_too_long")
		}
		return s, lin, nil

	case TypeBoolean:
		b, ok := raw.(bool)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		return b, lin, nil

	case TypeInteger:
		n, ok := integerOf(raw)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		return n, lin, nil

	case TypeDecimal:
		txt, ok := decimalText(raw)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		out, code := formatDecimal(txt, *f.Scale)
		if code != "" {
			return mk(ClassMapping, code)
		}
		return out, lin, nil

	case TypeDate:
		s, ok := raw.(string)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		t, err := time.Parse("2006-01-02", s)
		if err != nil || t.Format("2006-01-02") != s {
			return mk(ClassMapping, "date_invalid")
		}
		return s, lin, nil

	case TypeDatetime:
		s, ok := raw.(string)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			// RFC 3339 requires an offset; a bare local time is ambiguous and is refused, not assumed to be UTC
			return mk(ClassMapping, "datetime_invalid_or_zoneless")
		}
		_, off := t.Zone()
		return datetimeValue{utc: t.UTC().Format(time.RFC3339Nano), offset: offsetText(off)}, lin, nil

	case TypeEnum:
		s, ok := raw.(string)
		if !ok {
			return mk(ClassMapping, "type_mismatch")
		}
		if v, ok := f.Enum[s]; ok {
			return v, lin, nil
		}
		if f.UnknownDefault != nil {
			lin.Defaulted = true
			return *f.UnknownDefault, lin, nil
		}
		return mk(ClassMapping, "enum_unmapped")
	}
	return mk(ClassMapping, "type_unsupported")
}

func typedDefault(f Field, s string) any {
	if f.Type == TypeBoolean {
		return s == "true"
	}
	return s
}

func derefStr(a, b *string) string {
	if a != nil {
		return *a
	}
	if b != nil {
		return *b
	}
	return ""
}

func offsetText(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("%s%02d:%02d", sign, sec/3600, (sec%3600)/60)
}

func integerOf(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	return 0, false
}

var decimalRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// decimalText accepts a JSON number or a decimal string, never a float, and never an exponent form.
func decimalText(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = t
	default:
		return "", false
	}
	return s, decimalRE.MatchString(s)
}

// formatDecimal renders txt with exactly scale fractional digits, or names why it cannot: a value with more
// significant fractional digits than scale is refused rather than rounded (NP-10).
func formatDecimal(txt string, scale int) (string, string) {
	if len(txt) > maxDecimalDigits+2 { // sign and point

		return "", "decimal_too_large"
	}
	r, ok := new(big.Rat).SetString(txt)
	if !ok {
		return "", "decimal_invalid"
	}
	shift := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(shift))
	if !scaled.IsInt() {
		return "", "precision_loss"
	}
	return r.FloatString(scale), ""
}

// ── JSON helpers ─────────────────────────────────────────────────────────────

func decodeExact(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing content")
	}
	return v, nil
}

func depth(v any, d int) int {
	if d > maxDepth {
		return d
	}
	max := d
	switch t := v.(type) {
	case map[string]any:
		for _, c := range t {
			if n := depth(c, d+1); n > max {
				max = n
			}
		}
	case []any:
		for _, c := range t {
			if n := depth(c, d+1); n > max {
				max = n
			}
		}
	}
	return max
}

func unescapePointer(s string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}

func segments(ptr string) []string {
	parts := strings.Split(ptr[1:], "/")
	for i := range parts {
		parts[i] = unescapePointer(parts[i])
	}
	return parts
}

func firstSegment(ptr string) string {
	if len(ptr) < 2 {
		return ""
	}
	return segments(ptr)[0]
}

// lookup resolves an RFC 6901 pointer.
func lookup(doc any, ptr string) (any, bool) {
	cur := doc
	for _, seg := range segments(ptr) {
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) || (len(seg) > 1 && seg[0] == '0') {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func setPointer(root map[string]any, ptr string, v any) {
	segs := segments(ptr)
	cur := root
	for _, s := range segs[:len(segs)-1] {
		next, ok := cur[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[s] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

// CanonicalJSON is the specification's canonical form: sorted keys, no insignificant whitespace, no HTML
// escaping, so a hash does not depend on how the author laid the file out.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	generic, err := decodeExact(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
