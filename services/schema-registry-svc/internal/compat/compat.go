// Package compat enforces the "controlled schema evolution discipline"
// required by docs/architecture/04-data-model.md §2.12.
//
// Scope (v1, documented): analysis is top-level only — it reads a schema's
// `properties` map (field name -> declared `type`) and `required` list.
// Nested object/array structure is not analyzed. This is a deliberate v1
// boundary, not an accident: it catches the violations that actually break
// existing producers/consumers (a field silently vanishing, being
// downgraded from required, or changing type) without building a full
// JSON Schema diff engine nobody asked for yet.
package compat

import (
	"encoding/json"
	"fmt"
)

type shape struct {
	Properties map[string]struct {
		Type string `json:"type"`
	} `json:"properties"`
	Required []string `json:"required"`
}

func parse(raw json.RawMessage) (shape, error) {
	var s shape
	if err := json.Unmarshal(raw, &s); err != nil {
		return shape{}, fmt.Errorf("parse schema shape: %w", err)
	}
	return s, nil
}

// Check compares a proposed new schema against the current latest schema and
// returns a human-readable violation for every breaking change found. An
// empty result means the new schema is a safe evolution of the old one.
//
// A change is breaking when:
//   - a field required in the old schema is missing from the new schema's
//     properties entirely
//   - a field required in the old schema is no longer required in the new
//     schema
//   - a field present in both schemas has a different declared `type`
//   - a field is newly added to `required` in the new schema that wasn't
//     required in the old schema (existing producers don't populate it yet)
//
// Adding a new optional field, or removing a field that was never required,
// is always safe.
func Check(oldRaw, newRaw json.RawMessage) ([]string, error) {
	oldShape, err := parse(oldRaw)
	if err != nil {
		return nil, fmt.Errorf("old schema: %w", err)
	}
	newShape, err := parse(newRaw)
	if err != nil {
		return nil, fmt.Errorf("new schema: %w", err)
	}

	oldRequired := toSet(oldShape.Required)
	newRequired := toSet(newShape.Required)

	var violations []string

	for field := range oldRequired {
		if _, stillPresent := newShape.Properties[field]; !stillPresent {
			violations = append(violations, fmt.Sprintf("field %q was required and has been removed", field))
			continue
		}
		if !newRequired[field] {
			violations = append(violations, fmt.Sprintf("field %q was required and is no longer required", field))
		}
	}

	for field, oldProp := range oldShape.Properties {
		newProp, stillPresent := newShape.Properties[field]
		if !stillPresent {
			continue // removing an optional field is safe
		}
		if oldProp.Type != "" && newProp.Type != "" && oldProp.Type != newProp.Type {
			violations = append(violations, fmt.Sprintf("field %q changed type from %q to %q", field, oldProp.Type, newProp.Type))
		}
	}

	for field := range newRequired {
		if !oldRequired[field] {
			violations = append(violations, fmt.Sprintf("field %q is newly required and existing producers don't populate it", field))
		}
	}

	return violations, nil
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
