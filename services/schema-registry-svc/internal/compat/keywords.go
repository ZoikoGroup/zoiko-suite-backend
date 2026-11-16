package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"zoiko.io/schema-registry-svc/internal/domain"
)

// keywordViolations compares the validation keywords of one schema node.
//
// ZS-EVENT-001 section 12 treats a change that can reject a previously valid
// event, or that consumers may not tolerate, as a major (breaking) change:
// tightening a range, adding or changing a pattern or format, adding an enum
// value (consumers must tolerate unknown values for that to be minor, and the
// registry cannot prove they do), removing an enum value, or forbidding extra
// properties. Relaxing validation (dropping a bound, pattern or enum) is
// compatible. The registry cannot prove what consumers tolerate, so enum
// growth is refused and must be registered under compatibility_mode NONE as a
// recorded, controlled migration.
//
// Values are parsed leniently: a keyword whose value is not of the expected
// JSON type is not comparable and produces no violation, so an unusual schema
// that is already stored can still be evolved.
func keywordViolations(path string, o, n domain.Keywords) []string {
	var v []string
	v = append(v, enumViolations(path, o.Enum, n.Enum)...)

	// Lower bounds tighten when raised or newly added.
	for _, k := range []struct {
		name     string
		old, new json.RawMessage
	}{
		{"minimum", o.Minimum, n.Minimum},
		{"exclusiveMinimum", o.ExclusiveMinimum, n.ExclusiveMinimum},
		{"minLength", o.MinLength, n.MinLength},
		{"minItems", o.MinItems, n.MinItems},
	} {
		if tightened(k.old, k.new, true) {
			v = append(v, fmt.Sprintf("field %q %s was tightened from %s to %s", path, k.name, show(k.old), show(k.new)))
		}
	}
	// Upper bounds tighten when lowered or newly added.
	for _, k := range []struct {
		name     string
		old, new json.RawMessage
	}{
		{"maximum", o.Maximum, n.Maximum},
		{"exclusiveMaximum", o.ExclusiveMaximum, n.ExclusiveMaximum},
		{"maxLength", o.MaxLength, n.MaxLength},
		{"maxItems", o.MaxItems, n.MaxItems},
	} {
		if tightened(k.old, k.new, false) {
			v = append(v, fmt.Sprintf("field %q %s was tightened from %s to %s", path, k.name, show(k.old), show(k.new)))
		}
	}

	for _, k := range []struct {
		name     string
		old, new json.RawMessage
	}{
		{"pattern", o.Pattern, n.Pattern},
		{"format", o.Format, n.Format},
	} {
		if present(k.new) && !bytes.Equal(compact(k.old), compact(k.new)) {
			v = append(v, fmt.Sprintf("field %q %s was added or changed (%s -> %s); this can reject previously valid values", path, k.name, show(k.old), show(k.new)))
		}
	}

	if forbidsExtra(n.AdditionalProperties) && !forbidsExtra(o.AdditionalProperties) {
		v = append(v, fmt.Sprintf("field %q additionalProperties now forbids properties that were previously allowed", path))
	}
	return v
}

func enumViolations(path string, oldEnum, newEnum []json.RawMessage) []string {
	if len(newEnum) == 0 {
		return nil // dropping an enum relaxes validation
	}
	if len(oldEnum) == 0 {
		return []string{fmt.Sprintf("field %q gained an enum, which tightens validation", path)}
	}
	oldSet, newSet := valueSet(oldEnum), valueSet(newEnum)
	var v []string
	for _, val := range sortedKeys(newSet) {
		if !oldSet[val] {
			v = append(v, fmt.Sprintf("field %q enum value %s was added; consumers may not tolerate unknown values, so this needs a major version or compatibility_mode NONE", path, val))
		}
	}
	for _, val := range sortedKeys(oldSet) {
		if !newSet[val] {
			v = append(v, fmt.Sprintf("field %q enum value %s was removed", path, val))
		}
	}
	return v
}

func valueSet(vals []json.RawMessage) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, raw := range vals {
		out[string(compact(raw))] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tightened reports whether a numeric bound moved to reject more values. A bound
// present in the new schema but not the old always tightens; a bound that is
// absent in the new one relaxes. Non-numeric values are not comparable.
func tightened(oldRaw, newRaw json.RawMessage, lower bool) bool {
	nv, nok := number(newRaw)
	if !nok {
		return false
	}
	ov, ook := number(oldRaw)
	if !ook {
		return len(oldRaw) == 0 // newly added
	}
	if lower {
		return nv > ov
	}
	return nv < ov
}

func number(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// forbidsExtra reports additionalProperties: false.
func forbidsExtra(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "false"
}

func compact(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return bytes.TrimSpace(raw)
	}
	return b.Bytes()
}

func show(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(none)"
	}
	return string(compact(raw))
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
