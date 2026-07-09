package compat_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/compat"
)

func TestCheck_AddingOptionalField_IsSafe(t *testing.T) {
	oldSchema := []byte(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`)
	newSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"session_id":{"type":"string"}},"required":["principal_id"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	assert.Empty(t, violations)
}

func TestCheck_RemovingOptionalField_IsSafe(t *testing.T) {
	oldSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"note":{"type":"string"}},"required":["principal_id"]}`)
	newSchema := []byte(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	assert.Empty(t, violations)
}

func TestCheck_RemovingRequiredField_IsBreaking(t *testing.T) {
	oldSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id","tenant_id"]}`)
	newSchema := []byte(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], `"tenant_id"`)
	assert.Contains(t, violations[0], "removed")
}

func TestCheck_DowngradingRequiredToOptional_IsBreaking(t *testing.T) {
	oldSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id","tenant_id"]}`)
	newSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], `"tenant_id"`)
	assert.Contains(t, violations[0], "no longer required")
}

func TestCheck_ChangingFieldType_IsBreaking(t *testing.T) {
	oldSchema := []byte(`{"properties":{"risk_score":{"type":"integer"}},"required":["risk_score"]}`)
	newSchema := []byte(`{"properties":{"risk_score":{"type":"string"}},"required":["risk_score"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], `"risk_score"`)
	assert.Contains(t, violations[0], "integer")
	assert.Contains(t, violations[0], "string")
}

func TestCheck_AddingNewRequiredField_IsBreaking(t *testing.T) {
	oldSchema := []byte(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`)
	newSchema := []byte(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id","tenant_id"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0], `"tenant_id"`)
	assert.Contains(t, violations[0], "newly required")
}

func TestCheck_IdenticalSchema_IsSafe(t *testing.T) {
	s := []byte(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`)

	violations, err := compat.Check(s, s)

	require.NoError(t, err)
	assert.Empty(t, violations)
}

func TestCheck_MultipleViolations_AllReported(t *testing.T) {
	oldSchema := []byte(`{"properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`)
	newSchema := []byte(`{"properties":{"a":{"type":"integer"}},"required":["a"]}`)

	violations, err := compat.Check(oldSchema, newSchema)

	require.NoError(t, err)
	assert.Len(t, violations, 2) // b removed, a type changed
}

func TestCheck_MalformedSchema_ReturnsError(t *testing.T) {
	_, err := compat.Check([]byte(`not json`), []byte(`{}`))
	require.Error(t, err)
}
