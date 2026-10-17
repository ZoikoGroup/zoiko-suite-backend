package imports

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ColumnType is the type a column's values are validated and converted to.
type ColumnType string

const (
	TypeString  ColumnType = "string"
	TypeInt     ColumnType = "int"
	TypeDecimal ColumnType = "decimal"
	TypeDate    ColumnType = "date"
	TypeBool    ColumnType = "bool"
	TypeEnum    ColumnType = "enum"
)

// Column describes one column of the file.
type Column struct {
	Name        string     `json:"name"`
	Type        ColumnType `json:"type"`
	Required    bool       `json:"required"`
	MaxLen      int        `json:"max_len,omitempty"`
	Pattern     string     `json:"pattern,omitempty"`
	Enum        []string   `json:"enum,omitempty"`
	NonNegative bool       `json:"non_negative,omitempty"`
}

// Control is the control-total rule: a numeric column whose sum the sender declares and the
// import recomputes (INT-06: material imports cannot bypass control totals).
type Control struct {
	Column   string `json:"column"`
	Required bool   `json:"required"`
}

// MapRule builds one field of the command sent to the owning domain.
type MapRule struct {
	Field  string `json:"field"`
	From   string `json:"from"` // column | const | legal_entity_id | tenant_id
	Column string `json:"column,omitempty"`
	Const  any    `json:"const,omitempty"`
}

// Target is the owning-domain command an item becomes. Key names a target this deployment
// has configured: a profile cannot point the importer at an arbitrary address.
type Target struct {
	Key            string `json:"key"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	SuccessCodes   []int  `json:"success_codes,omitempty"`
	ResultRefField string `json:"result_ref_field,omitempty"`
}

// PartialPolicy decides what an invalid row means for the batch.
const (
	PartialRejectBatch = "REJECT_BATCH" // any invalid row fails the batch before preview
	PartialSkipInvalid = "SKIP_INVALID" // invalid rows are listed, skipped and reported as exceptions
)

// Spec is the certified definition of an import type.
type Spec struct {
	ImportType       string    `json:"import_type"`
	Format           string    `json:"format"` // csv | jsonl
	Delimiter        string    `json:"delimiter,omitempty"`
	Columns          []Column  `json:"columns"`
	KeyColumns       []string  `json:"key_columns,omitempty"`
	EntityColumn     string    `json:"entity_column,omitempty"`
	Control          *Control  `json:"control,omitempty"`
	Material         bool      `json:"material"`
	PartialPolicy    string    `json:"partial_policy"`
	MaxRows          int       `json:"max_rows"`
	MaxBytes         int64     `json:"max_bytes,omitempty"`
	Target           Target    `json:"target"`
	Mapping          []MapRule `json:"mapping"`
	RequireResultRef bool      `json:"require_result_ref,omitempty"`
}

// ProfileStatus is a profile's lifecycle state.
type ProfileStatus string

const (
	ProfileDraft      ProfileStatus = "DRAFT"
	ProfileCertified  ProfileStatus = "CERTIFIED"
	ProfileDeprecated ProfileStatus = "DEPRECATED"
	ProfileWithdrawn  ProfileStatus = "WITHDRAWN"
)

// Profile is a versioned, platform-published import definition.
type Profile struct {
	Key         string
	Version     int
	Status      ProfileStatus
	Spec        Spec
	SpecHash    string
	CreatedBy   string
	CertifiedBy string
	CertifiedAt *time.Time
	CreatedAt   time.Time
}

var (
	profileKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)
	columnNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	targetKeyRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)
	decimalRe    = regexp.MustCompile(`^-?[0-9]{1,18}(\.[0-9]{1,6})?$`)
	intRe        = regexp.MustCompile(`^-?[0-9]{1,18}$`)
)

// DefaultMaxRows bounds a batch when the profile names none.
const DefaultMaxRows = 100000

// Normalize applies defaults.
func (s Spec) Normalize() Spec {
	if s.Format == "" {
		s.Format = "csv"
	}
	if s.Format == "csv" && s.Delimiter == "" {
		s.Delimiter = ","
	}
	if s.PartialPolicy == "" {
		s.PartialPolicy = PartialRejectBatch
	}
	if s.MaxRows == 0 {
		s.MaxRows = DefaultMaxRows
	}
	if len(s.Target.SuccessCodes) == 0 {
		s.Target.SuccessCodes = []int{200, 201, 202, 204}
	}
	if s.Target.Method == "" {
		s.Target.Method = "POST"
	}
	return s
}

// Validate checks a specification is complete, safe and consistent.
func (s Spec) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, a...))
	}
	if strings.TrimSpace(s.ImportType) == "" {
		return bad("import_type is required")
	}
	if s.Format != "csv" && s.Format != "jsonl" {
		return bad("format must be csv or jsonl")
	}
	if s.Format == "csv" && len(s.Delimiter) != 1 {
		return bad("the csv delimiter must be one character")
	}
	if s.PartialPolicy != PartialRejectBatch && s.PartialPolicy != PartialSkipInvalid {
		return bad("partial_policy must be REJECT_BATCH or SKIP_INVALID")
	}
	if s.MaxRows < 1 || s.MaxRows > 1000000 {
		return bad("max_rows must be between 1 and 1000000")
	}
	if len(s.Columns) == 0 || len(s.Columns) > 200 {
		return bad("between 1 and 200 columns are required")
	}
	cols := map[string]Column{}
	for _, c := range s.Columns {
		if !columnNameRe.MatchString(c.Name) {
			return bad("a column name is malformed")
		}
		if _, dup := cols[c.Name]; dup {
			return bad("a column name is repeated")
		}
		switch c.Type {
		case TypeString, TypeInt, TypeDecimal, TypeDate, TypeBool:
		case TypeEnum:
			if len(c.Enum) == 0 {
				return bad("an enum column needs its values")
			}
		default:
			return bad("a column has an unknown type")
		}
		if c.Pattern != "" {
			if c.Type != TypeString {
				return bad("a pattern applies to string columns only")
			}
			if _, err := regexp.Compile(c.Pattern); err != nil {
				return bad("a column pattern does not compile")
			}
		}
		if c.MaxLen < 0 || c.MaxLen > 4096 {
			return bad("max_len must be between 0 and 4096")
		}
		cols[c.Name] = c
	}
	for _, k := range s.KeyColumns {
		if _, ok := cols[k]; !ok {
			return bad("a key column is not a column")
		}
	}
	if s.EntityColumn != "" && cols[s.EntityColumn].Type != TypeString {
		return bad("the entity column must be a string column")
	}
	if s.Control != nil {
		c, ok := cols[s.Control.Column]
		if !ok || (c.Type != TypeDecimal && c.Type != TypeInt) {
			return bad("the control column must be a decimal or int column")
		}
	}
	if s.Material {
		// A material import cannot choose to skip bad rows, run without a control total, or
		// leave its items without a domain result to reconcile against.
		if s.PartialPolicy != PartialRejectBatch {
			return bad("a material profile must reject the batch on any invalid row")
		}
		if s.Control == nil || !s.Control.Required {
			return bad("a material profile must require a control total")
		}
		if !s.RequireResultRef || s.Target.ResultRefField == "" {
			return bad("a material profile must require the domain's result reference")
		}
	}
	if !targetKeyRe.MatchString(s.Target.Key) {
		return bad("target.key is required")
	}
	switch s.Target.Method {
	case "POST", "PUT", "PATCH":
	default:
		return bad("target.method must be POST, PUT or PATCH")
	}
	if !strings.HasPrefix(s.Target.Path, "/") || strings.ContainsAny(s.Target.Path, "?#\\") ||
		strings.Contains(s.Target.Path, "://") || strings.Contains(s.Target.Path, "..") || strings.Contains(s.Target.Path, "//") {
		return bad("target.path must be an absolute path with no query, fragment or traversal")
	}
	for _, code := range s.Target.SuccessCodes {
		if code < 200 || code > 299 {
			return bad("success codes must be 2xx")
		}
	}
	if s.RequireResultRef && s.Target.ResultRefField == "" {
		return bad("require_result_ref needs target.result_ref_field")
	}
	if len(s.Mapping) == 0 || len(s.Mapping) > 200 {
		return bad("between 1 and 200 mapping rules are required")
	}
	seen := map[string]bool{}
	for _, m := range s.Mapping {
		if !columnNameRe.MatchString(m.Field) || seen[m.Field] {
			return bad("a mapping field is malformed or repeated")
		}
		seen[m.Field] = true
		switch m.From {
		case "column":
			if _, ok := cols[m.Column]; !ok {
				return bad("a mapping rule names a column that does not exist")
			}
		case "const":
			if m.Const == nil {
				return bad("a const mapping rule needs a value")
			}
		case "legal_entity_id", "tenant_id":
		default:
			return bad("a mapping rule has an unknown source")
		}
	}
	return nil
}

// ValidateProfileKey checks a profile key.
func ValidateProfileKey(k string) error {
	if !profileKeyRe.MatchString(k) {
		return fmt.Errorf("%w: profile_key is malformed", ErrValidation)
	}
	return nil
}

// Hash is SHA-256 over the canonical specification.
func (s Spec) Hash() string {
	b, _ := json.Marshal(s.Normalize())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MappingHash identifies what an item is turned into: the mapping and the command it is
// sent as. It is part of the preview fingerprint, so an approval is for this mapping.
func (s Spec) MappingHash() string {
	n := s.Normalize()
	b, _ := json.Marshal(struct {
		Mapping []MapRule `json:"m"`
		Target  Target    `json:"t"`
		Cols    []Column  `json:"c"`
	}{n.Mapping, n.Target, n.Columns})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s Spec) column(name string) (Column, bool) {
	for _, c := range s.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// ── decimals ────────────────────────────────────────────────────────────────

// ParseDecimal parses a plain decimal (no exponent, at most 6 fractional digits).
func ParseDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if !decimalRe.MatchString(s) {
		return nil, fmt.Errorf("not a plain decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("not a decimal")
	}
	return r, nil
}

// CanonicalDecimal renders a rational with no trailing zeros ("10", "10.5", "0.000001").
func CanonicalDecimal(r *big.Rat) string {
	s := r.FloatString(6)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-" || s == "-0" {
		return "0"
	}
	return s
}

// ── row validation ──────────────────────────────────────────────────────────

// ValidateRow converts one raw record (strings from a CSV, or JSON scalars) to typed values
// and reports every problem with it. The values are never echoed in an error.
func (s Spec) ValidateRow(raw map[string]any, legalEntityID string) (map[string]any, []RowError) {
	values := make(map[string]any, len(s.Columns))
	var errs []RowError
	add := func(col, code string) { errs = append(errs, RowError{Column: col, Code: code}) }

	known := make(map[string]bool, len(s.Columns))
	for _, c := range s.Columns {
		known[c.Name] = true
	}
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		add(k, CodeUnknownField)
	}

	for _, c := range s.Columns {
		v, present := raw[c.Name]
		str, isEmpty := scalarString(v)
		if !present || isEmpty {
			if c.Required {
				add(c.Name, CodeRequired)
			}
			continue
		}
		switch c.Type {
		case TypeString:
			if _, ok := v.(string); !ok {
				add(c.Name, CodeType)
				continue
			}
			if c.MaxLen > 0 && len([]rune(str)) > c.MaxLen {
				add(c.Name, CodeMaxLen)
				continue
			}
			if c.Pattern != "" && !regexp.MustCompile(c.Pattern).MatchString(str) {
				add(c.Name, CodePattern)
				continue
			}
			values[c.Name] = str
		case TypeInt:
			str = strings.TrimSpace(str)
			if !intRe.MatchString(str) {
				add(c.Name, CodeType)
				continue
			}
			r, _ := new(big.Rat).SetString(str)
			if c.NonNegative && r.Sign() < 0 {
				add(c.Name, CodeNegativeAmount)
				continue
			}
			values[c.Name] = r.Num().Int64()
		case TypeDecimal:
			r, err := ParseDecimal(str)
			if err != nil {
				add(c.Name, CodeType)
				continue
			}
			if c.NonNegative && r.Sign() < 0 {
				add(c.Name, CodeNegativeAmount)
				continue
			}
			values[c.Name] = CanonicalDecimal(r)
		case TypeDate:
			t, err := time.Parse("2006-01-02", strings.TrimSpace(str))
			if err != nil {
				add(c.Name, CodeType)
				continue
			}
			values[c.Name] = t.Format("2006-01-02")
		case TypeBool:
			switch strings.TrimSpace(str) {
			case "true":
				values[c.Name] = true
			case "false":
				values[c.Name] = false
			default:
				add(c.Name, CodeType)
			}
		case TypeEnum:
			ok := false
			for _, e := range c.Enum {
				if e == str {
					ok = true
					break
				}
			}
			if !ok {
				add(c.Name, CodeEnum)
				continue
			}
			values[c.Name] = str
		}
	}
	if s.EntityColumn != "" {
		if ev, ok := values[s.EntityColumn].(string); ok && ev != legalEntityID {
			add(s.EntityColumn, CodeCrossEntity)
		}
	}
	return values, errs
}

// scalarString renders a CSV string or a JSON scalar as text, and reports empty/null.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, strings.TrimSpace(t) == ""
	case json.Number:
		return t.String(), false
	case bool:
		if t {
			return "true", false
		}
		return "false", false
	case float64:
		return "", false // a bare float cannot be a plain decimal; caught as TYPE below
	}
	return fmt.Sprint(v), false
}

// KeyOf is the row's identity for in-file duplicate detection (empty when the profile names no
// key columns, or the row has no value for one of them).
func (s Spec) KeyOf(values map[string]any) string {
	if len(s.KeyColumns) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.KeyColumns))
	for _, k := range s.KeyColumns {
		v, ok := values[k]
		if !ok {
			return ""
		}
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, "\x1f")
}

// RowHash is SHA-256 over the row's typed values.
func RowHash(values map[string]any) string {
	b, _ := json.Marshal(values) // map keys are sorted: deterministic
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ItemKey is the owning domain's idempotency key for an item: the same batch, row and content
// always give the same key, so a retry or a rerun can never create a second domain record.
func ItemKey(batchID string, rowNo int, rowHash string) string {
	return fmt.Sprintf("imp-%s-%d-%s", strings.TrimPrefix(batchID, "imb_"), rowNo, rowHash[:12])
}

// BuildBody turns an item's values into the owning-domain command body.
func (s Spec) BuildBody(values map[string]any, tenantID, legalEntityID string) ([]byte, error) {
	body := make(map[string]any, len(s.Mapping))
	for _, m := range s.Mapping {
		switch m.From {
		case "column":
			if v, ok := values[m.Column]; ok {
				body[m.Field] = v
			}
		case "const":
			body[m.Field] = m.Const
		case "legal_entity_id":
			body[m.Field] = legalEntityID
		case "tenant_id":
			body[m.Field] = tenantID
		}
	}
	return json.Marshal(body)
}

// ExtractRef reads the owning domain's result reference (a dotted path) from its response.
func ExtractRef(path string, response []byte) string {
	if path == "" {
		return ""
	}
	var cur any
	if json.Unmarshal(response, &cur) != nil {
		return ""
	}
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[seg]
	}
	switch t := cur.(type) {
	case string:
		if len(t) > 255 {
			return ""
		}
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	}
	return ""
}
