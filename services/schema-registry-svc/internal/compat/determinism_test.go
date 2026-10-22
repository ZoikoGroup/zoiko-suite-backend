package compat_test

// The violation list is built by walking Go maps, whose iteration order is
// random. These tests pin that the result is nevertheless identical — same
// violations, same order — on every call.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/compat"
)

// manyBreaks returns an old/new schema pair that breaks in many independent
// places: removed required fields, retyped fields, newly required fields, a
// retyped nested field and a retyped array item.
func manyBreaks() (oldS, newS []byte) {
	var oldProps, newProps, oldReq, newReq []string
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("field%02d", i)
		switch i % 4 {
		case 0: // required field removed
			oldProps = append(oldProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			oldReq = append(oldReq, fmt.Sprintf("%q", name))
		case 1: // required field relaxed
			oldProps = append(oldProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			newProps = append(newProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			oldReq = append(oldReq, fmt.Sprintf("%q", name))
		case 2: // type changed
			oldProps = append(oldProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			newProps = append(newProps, fmt.Sprintf(`%q:{"type":"integer"}`, name))
		case 3: // newly required
			oldProps = append(oldProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			newProps = append(newProps, fmt.Sprintf(`%q:{"type":"string"}`, name))
			newReq = append(newReq, fmt.Sprintf("%q", name))
		}
	}
	oldProps = append(oldProps,
		`"user":{"type":"object","properties":{"role":{"type":"string"},"id":{"type":"string"}},"required":["id"]}`,
		`"tags":{"type":"array","items":{"type":"string"}}`)
	newProps = append(newProps,
		`"user":{"type":"object","properties":{"role":{"type":"integer"},"id":{"type":"string"}},"required":["id","role"]}`,
		`"tags":{"type":"array","items":{"type":"integer"}}`)
	mk := func(props, req []string) []byte {
		return []byte(fmt.Sprintf(`{"properties":{%s},"required":[%s]}`, strings.Join(props, ","), strings.Join(req, ",")))
	}
	return mk(oldProps, oldReq), mk(newProps, newReq)
}

func TestCheck_ViolationsAreDeterministic_SameSetSameOrderEveryCall(t *testing.T) {
	oldS, newS := manyBreaks()

	first, err := compat.Check(oldS, newS)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(first), 10, "the fixture must produce many independent violations")

	// Go randomises map iteration per range statement, so repeating the call
	// many times would, before the fix, produce differing orders.
	for i := 0; i < 300; i++ {
		again, err := compat.Check(oldS, newS)
		require.NoError(t, err)
		require.Equal(t, first, again, "violations must be identical, in identical order, on call %d", i)
	}
}

func TestCheck_ViolationsAreSortedByFieldPath(t *testing.T) {
	oldS, newS := manyBreaks()
	got, err := compat.Check(oldS, newS)
	require.NoError(t, err)
	assert.True(t, sort.StringsAreSorted(got), "violations are returned in a stable, sorted order: %v", got)
}

// Sorting must neither drop nor add a violation: the same fixture still reports
// every kind of break, including the nested and array-item ones.
func TestCheck_Sorting_DoesNotChangeTheViolationSet(t *testing.T) {
	oldS, newS := manyBreaks()
	got, err := compat.Check(oldS, newS)
	require.NoError(t, err)
	all := strings.Join(got, "\n")
	for _, want := range []string{
		`field "field00" was required and has been removed`,
		`field "field01" was required and is no longer required`,
		`field "field02" changed type from "string" to "integer"`,
		`field "field03" is newly required`,
		`field "user.role" changed type from "string" to "integer"`,
		`field "user.role" is newly required`,
		`field "tags" items changed type from "string" to "integer"`,
	} {
		assert.Contains(t, all, want)
	}
}
