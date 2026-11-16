// Package schemainfer derives a draft JSON Schema for an event's `data` from
// payloads a producer actually emitted (ZS-EVENT-001 section 12.1: "JSON
// Schema ... golden valid payloads").
//
// It exists because hand-deriving a schema per event type from producer code
// does not scale to a catalogue of hundreds, and a schema invented without
// looking at real payloads is worse than none. What it produces is an OBSERVED
// contract, and says so in its description:
//
//   - a field is `required` only if it was present in every sample, and only
//     when there were at least MinSamples samples (one sample proves nothing
//     about which fields are optional);
//   - a type is stated only if every sample agreed on it (integer widens to
//     number; a null or a mix drops the type rather than guess);
//   - format date-time is stated only if every string sample parsed as RFC 3339.
//
// A human must still review the result: the samples decide how strict it is.
// The output stays inside the subset schema-registry-svc compares (type,
// properties, required, items), so it can be registered as is.
package schemainfer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// DefaultMinSamples is the fewest samples from which `required` is stated.
const DefaultMinSamples = 2

// Options tunes Infer.
type Options struct {
	// MinSamples: below this many samples no field is marked required.
	MinSamples int
	// Description is the schema's description; Infer appends the sample count.
	Description string
}

type node struct {
	n           int // samples seen at this node
	types       map[string]int
	allDate     bool
	anyString   bool
	allIntegral bool
	props       map[string]*node
	propCount   map[string]int
	items       *node
}

func newNode() *node {
	return &node{types: map[string]int{}, allDate: true, allIntegral: true,
		props: map[string]*node{}, propCount: map[string]int{}}
}

func (n *node) add(v any) {
	n.n++
	switch x := v.(type) {
	case nil:
		n.types["null"]++
	case bool:
		n.types["boolean"]++
	case json.Number:
		n.types["number"]++
		if _, err := x.Int64(); err != nil {
			n.allIntegral = false
		}
	case string:
		n.types["string"]++
		n.anyString = true
		if _, err := time.Parse(time.RFC3339Nano, x); err != nil {
			n.allDate = false
		}
	case []any:
		n.types["array"]++
		if n.items == nil {
			n.items = newNode()
		}
		for _, el := range x {
			n.items.add(el)
		}
	case map[string]any:
		n.types["object"]++
		for k, child := range x {
			c := n.props[k]
			if c == nil {
				c = newNode()
				n.props[k] = c
			}
			n.propCount[k]++
			c.add(child)
		}
	}
}

// Infer builds the schema from samples, each a JSON object (an event's data).
func Infer(samples []json.RawMessage, opt Options) ([]byte, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("schemainfer: no samples")
	}
	if opt.MinSamples <= 0 {
		opt.MinSamples = DefaultMinSamples
	}
	root := newNode()
	for i, raw := range samples {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("schemainfer: sample %d is not JSON: %w", i, err)
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, fmt.Errorf("schemainfer: sample %d is not a JSON object", i)
		}
		root.add(v)
	}
	desc := opt.Description
	if desc != "" {
		desc += " "
	}
	desc += fmt.Sprintf("OBSERVED contract inferred from %d emitted payload(s): a field is required only if present in every sample (and only when there were at least %d samples), and a type is stated only where all samples agreed. Review before relying on it.", len(samples), opt.MinSamples)
	out := render(root, opt.MinSamples)
	out["description"] = desc
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func render(n *node, minSamples int) map[string]any {
	out := map[string]any{}
	if t := stableType(n); t != "" {
		out["type"] = t
	}
	if n.anyString && n.allDate && len(n.types) == 1 {
		out["format"] = "date-time"
	}
	if len(n.props) > 0 {
		props := map[string]any{}
		var required []string
		for k, c := range n.props {
			props[k] = render(c, minSamples)
			if n.propCount[k] == n.n && n.n >= minSamples && len(n.types) == 1 {
				required = append(required, k)
			}
		}
		out["properties"] = props
		if len(required) > 0 {
			sort.Strings(required)
			out["required"] = required
		}
	}
	if n.items != nil && n.items.n > 0 {
		out["items"] = render(n.items, minSamples)
	}
	return out
}

// stableType returns the JSON Schema type all samples agreed on, or "" when
// they did not (including any null).
func stableType(n *node) string {
	if len(n.types) != 1 {
		return ""
	}
	for t := range n.types {
		switch t {
		case "null":
			return ""
		case "number":
			if n.allIntegral {
				return "integer"
			}
			return "number"
		}
		return t
	}
	return ""
}
