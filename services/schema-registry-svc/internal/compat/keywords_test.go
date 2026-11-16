package compat_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/compat"
)

func wrap(prop string) []byte {
	return []byte(`{"properties":{"f":` + prop + `},"required":["f"]}`)
}

func check(t *testing.T, oldProp, newProp string) []string {
	t.Helper()
	v, err := compat.Check(wrap(oldProp), wrap(newProp))
	require.NoError(t, err)
	return v
}

// EV-T07: an optional field is compatible; EV-T08: string -> number is not.
func TestKeywords_BreakingChanges(t *testing.T) {
	cases := map[string][2]string{
		"enum value added":        {`{"type":"string","enum":["A","B"]}`, `{"type":"string","enum":["A","B","C"]}`},
		"enum value removed":      {`{"type":"string","enum":["A","B"]}`, `{"type":"string","enum":["A"]}`},
		"enum newly added":        {`{"type":"string"}`, `{"type":"string","enum":["A"]}`},
		"minimum raised":          {`{"type":"number","minimum":0}`, `{"type":"number","minimum":1}`},
		"minimum newly added":     {`{"type":"number"}`, `{"type":"number","minimum":0}`},
		"maximum lowered":         {`{"type":"number","maximum":100}`, `{"type":"number","maximum":50}`},
		"maxLength newly added":   {`{"type":"string"}`, `{"type":"string","maxLength":10}`},
		"minItems raised":         {`{"type":"array","minItems":1}`, `{"type":"array","minItems":2}`},
		"pattern added":           {`{"type":"string"}`, `{"type":"string","pattern":"^A"}`},
		"pattern changed":         {`{"type":"string","pattern":"^A"}`, `{"type":"string","pattern":"^B"}`},
		"format changed":          {`{"type":"string","format":"date"}`, `{"type":"string","format":"date-time"}`},
		"extra properties barred": {`{"type":"object"}`, `{"type":"object","additionalProperties":false}`},
	}
	for name, c := range cases {
		assert.NotEmpty(t, check(t, c[0], c[1]), name)
	}
}

func TestKeywords_CompatibleChanges(t *testing.T) {
	cases := map[string][2]string{
		"enum dropped":         {`{"type":"string","enum":["A","B"]}`, `{"type":"string"}`},
		"enum reordered":       {`{"type":"string","enum":["A","B"]}`, `{"type":"string","enum":["B","A"]}`},
		"minimum lowered":      {`{"type":"number","minimum":5}`, `{"type":"number","minimum":1}`},
		"minimum dropped":      {`{"type":"number","minimum":5}`, `{"type":"number"}`},
		"maximum raised":       {`{"type":"number","maximum":5}`, `{"type":"number","maximum":9}`},
		"pattern dropped":      {`{"type":"string","pattern":"^A"}`, `{"type":"string"}`},
		"pattern unchanged":    {`{"type":"string","pattern":"^A"}`, `{"type":"string","pattern":"^A"}`},
		"extra props allowed":  {`{"type":"object","additionalProperties":false}`, `{"type":"object"}`},
		"unchanged everything": {`{"type":"string","enum":["A"],"maxLength":5}`, `{"type":"string","enum":["A"],"maxLength":5}`},
	}
	for name, c := range cases {
		assert.Empty(t, check(t, c[0], c[1]), name)
	}
}

func TestKeywords_NestedAndItemsAndRoot(t *testing.T) {
	oldS := []byte(`{"properties":{"o":{"type":"object","properties":{"s":{"type":"string","enum":["A"]}}},
		"a":{"type":"array","items":{"type":"string","maxLength":5}}}}`)
	newS := []byte(`{"properties":{"o":{"type":"object","properties":{"s":{"type":"string","enum":["A","B"]}}},
		"a":{"type":"array","items":{"type":"string","maxLength":3}}},"additionalProperties":false}`)
	v, err := compat.Check(oldS, newS)
	require.NoError(t, err)
	joined := ""
	for _, s := range v {
		joined += s + "\n"
	}
	assert.Contains(t, joined, `"o.s" enum value "B" was added`)
	assert.Contains(t, joined, `"a[]" maxLength was tightened`)
	assert.Contains(t, joined, `"$" additionalProperties`)
}

// An odd value already in a stored schema must not make it unevolvable.
func TestKeywords_LenientOnOddValues(t *testing.T) {
	v, err := compat.Check(wrap(`{"type":"number","minimum":"x"}`), wrap(`{"type":"number","minimum":"y"}`))
	require.NoError(t, err)
	assert.Empty(t, v)
}

func TestKeywords_DeterministicOrder(t *testing.T) {
	o := wrap(`{"type":"string","enum":["A","B","C"]}`)
	n := wrap(`{"type":"string","enum":["D","E","F"]}`)
	first, _ := compat.Check(o, n)
	for i := 0; i < 20; i++ {
		again, _ := compat.Check(o, n)
		require.Equal(t, first, again)
	}
}
