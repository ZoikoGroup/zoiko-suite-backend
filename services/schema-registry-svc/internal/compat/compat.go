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

	"zoiko.io/schema-registry-svc/internal/domain"
)

// The shape parsed here is domain.Shape, shared with the write path.
//
// This package used to declare its own identical struct. Two copies of the
// same parse is how a schema gets accepted at registration and then rejected
// by the checker on the next version — the registry's own validation and its
// own compatibility gate disagreeing about what a schema is. One definition,
// used by both.
func parse(raw json.RawMessage) (domain.Shape, error) {
	s, err := domain.ShapeOf(raw)
	if err != nil {
		return domain.Shape{}, fmt.Errorf("parse schema shape: %w", err)
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

	violations := checkProperties("", oldShape.Properties, newShape.Properties, oldShape.Required, newShape.Required)
	return violations, nil
}

func checkProperties(prefix string, oldProps, newProps map[string]domain.PropertyDef, oldReqList, newReqList []string) []string {
	oldRequired := toSet(oldReqList)
	newRequired := toSet(newReqList)

	var violations []string

	for field := range oldRequired {
		fullPath := prefix + field
		if _, stillPresent := newProps[field]; !stillPresent {
			violations = append(violations, fmt.Sprintf("field %q was required and has been removed", fullPath))
			continue
		}
		if !newRequired[field] {
			violations = append(violations, fmt.Sprintf("field %q was required and is no longer required", fullPath))
		}
	}

	for field, oldProp := range oldProps {
		fullPath := prefix + field
		newProp, stillPresent := newProps[field]
		if !stillPresent {
			continue // removing an optional field is safe
		}
		if oldProp.Type != "" && newProp.Type != "" && oldProp.Type != newProp.Type {
			violations = append(violations, fmt.Sprintf("field %q changed type from %q to %q", fullPath, oldProp.Type, newProp.Type))
			continue
		}

		// Recurse into nested object properties
		if (oldProp.Type == "object" || len(oldProp.Properties) > 0) &&
			(newProp.Type == "object" || len(newProp.Properties) > 0) {
			nested := checkProperties(fullPath+".", oldProp.Properties, newProp.Properties, oldProp.Required, newProp.Required)
			violations = append(violations, nested...)
		}

		// Recurse into array item schemas
		if oldProp.Type == "array" && newProp.Type == "array" && oldProp.Items != nil && newProp.Items != nil {
			if oldProp.Items.Type != "" && newProp.Items.Type != "" && oldProp.Items.Type != newProp.Items.Type {
				violations = append(violations, fmt.Sprintf("field %q items changed type from %q to %q", fullPath, oldProp.Items.Type, newProp.Items.Type))
			} else if (oldProp.Items.Type == "object" || len(oldProp.Items.Properties) > 0) &&
				(newProp.Items.Type == "object" || len(newProp.Items.Properties) > 0) {
				nested := checkProperties(fullPath+"[].", oldProp.Items.Properties, newProp.Items.Properties, oldProp.Items.Required, newProp.Items.Required)
				violations = append(violations, nested...)
			}
		}
	}

	for field := range newRequired {
		fullPath := prefix + field
		if !oldRequired[field] {
			violations = append(violations, fmt.Sprintf("field %q is newly required and existing producers don't populate it", fullPath))
		}
	}

	return violations
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
