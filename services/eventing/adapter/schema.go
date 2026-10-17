package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// ErrSchemaInvalid is returned when a document does not satisfy the authority's schema. Its message lists the
// failing paths and rules, never the offending values: a filing carries taxpayer data.
var ErrSchemaInvalid = errors.New("document does not satisfy the authority schema")

// A deliberately small JSON-Schema subset, enough to certify an authority's submission shape without a
// dependency: type (object, array, string, number, integer, boolean, null), required, properties,
// additionalProperties (false only), items, enum, pattern, minLength, maxLength, minimum, maximum.
// Anything outside it in a certified schema is refused at certification time, so a profile can never
// silently rely on a rule that is not enforced.
type schemaNode struct {
	Type                 string                 `json:"type"`
	Required             []string               `json:"required"`
	Properties           map[string]*schemaNode `json:"properties"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
	Items                *schemaNode            `json:"items"`
	Enum                 []json.RawMessage      `json:"enum"`
	Pattern              string                 `json:"pattern"`
	MinLength            *int                   `json:"minLength"`
	MaxLength            *int                   `json:"maxLength"`
	Minimum              *json.Number           `json:"minimum"`
	Maximum              *json.Number           `json:"maximum"`
	re                   *regexp.Regexp
}

var schemaTypes = map[string]bool{"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true, "null": true}

// CompileSchema parses and checks a schema. Unknown keywords are refused.
func CompileSchema(raw json.RawMessage) (*schemaNode, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: schema is empty", ErrSpecInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var n schemaNode
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("%w: schema: %v", ErrSpecInvalid, err)
	}
	if err := n.check("$", 0); err != nil {
		return nil, err
	}
	return &n, nil
}

func (n *schemaNode) check(path string, depth int) error {
	bad := func(m string) error { return fmt.Errorf("%w: schema %s: %s", ErrSpecInvalid, path, m) }
	if depth > 12 {
		return bad("nested too deeply")
	}
	if n.Type != "" && !schemaTypes[n.Type] {
		return bad("unknown type")
	}
	if n.Pattern != "" {
		re, err := regexp.Compile(n.Pattern)
		if err != nil {
			return bad("pattern does not compile")
		}
		n.re = re
	}
	if n.AdditionalProperties != nil && *n.AdditionalProperties {
		return bad("additionalProperties may only be false")
	}
	for k, p := range n.Properties {
		if p == nil {
			return bad("a property has no definition")
		}
		if err := p.check(path+"."+k, depth+1); err != nil {
			return err
		}
	}
	if n.Items != nil {
		if err := n.Items.check(path+"[]", depth+1); err != nil {
			return err
		}
	}
	return nil
}

// ValidateDocument checks a JSON document against the schema.
func (n *schemaNode) ValidateDocument(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("%w: not JSON", ErrSchemaInvalid)
	}
	var problems []string
	n.validate("$", v, &problems)
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	if len(problems) > 10 {
		problems = append(problems[:10], fmt.Sprintf("and %d more", len(problems)-10))
	}
	return fmt.Errorf("%w: %s", ErrSchemaInvalid, strings.Join(problems, "; "))
}

func (n *schemaNode) validate(path string, v any, out *[]string) {
	add := func(rule string) { *out = append(*out, path+" "+rule) }
	if n.Type != "" {
		ok := false
		switch n.Type {
		case "object":
			_, ok = v.(map[string]any)
		case "array":
			_, ok = v.([]any)
		case "string":
			_, ok = v.(string)
		case "boolean":
			_, ok = v.(bool)
		case "null":
			ok = v == nil
		case "number":
			_, ok = v.(json.Number)
		case "integer":
			if num, isNum := v.(json.Number); isNum {
				r, good := new(big.Rat).SetString(num.String())
				ok = good && r.IsInt()
			}
		}
		if !ok {
			add("must be of type " + n.Type)
			return
		}
	}
	if len(n.Enum) > 0 {
		got, _ := json.Marshal(v)
		found := false
		for _, e := range n.Enum {
			var ev any
			dec := json.NewDecoder(bytes.NewReader(e))
			dec.UseNumber()
			_ = dec.Decode(&ev)
			if b, _ := json.Marshal(ev); bytes.Equal(b, got) {
				found = true
			}
		}
		if !found {
			add("is not one of the allowed values")
		}
	}
	switch t := v.(type) {
	case string:
		if n.MinLength != nil && len([]rune(t)) < *n.MinLength {
			add("is shorter than allowed")
		}
		if n.MaxLength != nil && len([]rune(t)) > *n.MaxLength {
			add("is longer than allowed")
		}
		if n.re != nil && !n.re.MatchString(t) {
			add("does not match the required pattern")
		}
	case json.Number:
		r, ok := new(big.Rat).SetString(t.String())
		if ok {
			if n.Minimum != nil {
				if m, ok := new(big.Rat).SetString(n.Minimum.String()); ok && r.Cmp(m) < 0 {
					add("is below the minimum")
				}
			}
			if n.Maximum != nil {
				if m, ok := new(big.Rat).SetString(n.Maximum.String()); ok && r.Cmp(m) > 0 {
					add("is above the maximum")
				}
			}
		}
	case map[string]any:
		for _, req := range n.Required {
			if _, ok := t[req]; !ok {
				*out = append(*out, path+"."+req+" is required")
			}
		}
		for k, val := range t {
			if p, ok := n.Properties[k]; ok {
				p.validate(path+"."+k, val, out)
			} else if n.AdditionalProperties != nil && !*n.AdditionalProperties {
				*out = append(*out, path+"."+k+" is not allowed")
			}
		}
	case []any:
		if n.Items != nil {
			for i, el := range t {
				n.Items.validate(fmt.Sprintf("%s[%d]", path, i), el, out)
			}
		}
	}
}

// CanonicalHash is the fingerprint of an approved document: SHA-256 (hex) of its canonical form, which is the
// document decoded with exact numbers and re-encoded with sorted keys and no insignificant whitespace. The TAX
// domain computes the same value for the subject it approved, so the adapter can tell a changed document from
// the approved one.
func CanonicalHash(doc []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("document is not JSON")
	}
	if dec.More() {
		return "", fmt.Errorf("document has trailing content")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return hashBytes(b), nil
}
