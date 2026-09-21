package query

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var key = []byte("cursor-test-key-at-least-thirty-two-bytes")

func TestCursor_RoundTrips(t *testing.T) {
	in := Cursor{TenantID: "t-1", Scope: "obligation", PlanDigest: "d-1",
		After: []any{1.5, "ob-9"}, Page: 2}

	token, err := EncodeCursor(key, in)
	require.NoError(t, err)

	out, err := DecodeCursor(key, token, "t-1", "obligation", "d-1")
	require.NoError(t, err)
	assert.Equal(t, in.TenantID, out.TenantID)
	assert.Equal(t, in.Page, out.Page)
	assert.Equal(t, []any{1.5, "ob-9"}, out.After)
}

// NP-57, the core case: the payload is readable, so an attacker CAN see what
// to change — and changing it invalidates the token, which is the property
// that matters. Encryption would hide the contents and leave them malleable.
func TestCursor_TamperedPayloadFailsIntegrity(t *testing.T) {
	token, err := EncodeCursor(key, Cursor{TenantID: "t-1", Scope: "obligation",
		PlanDigest: "d-1", After: []any{1.0}})
	require.NoError(t, err)

	dot := strings.LastIndex(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	require.NoError(t, err)

	var c Cursor
	require.NoError(t, json.Unmarshal(payload, &c))
	c.TenantID = "victim-tenant"
	forged, err := json.Marshal(c)
	require.NoError(t, err)

	tampered := base64.RawURLEncoding.EncodeToString(forged) + token[dot:]
	_, err = DecodeCursor(key, tampered, "victim-tenant", "obligation", "d-1")
	assert.ErrorIs(t, err, ErrCursorInvalid)
}

func TestCursor_TamperedSignatureFails(t *testing.T) {
	token, err := EncodeCursor(key, Cursor{TenantID: "t-1", Scope: "obligation", PlanDigest: "d-1"})
	require.NoError(t, err)

	dot := strings.LastIndex(token, ".")
	tampered := token[:dot+1] + base64.RawURLEncoding.EncodeToString([]byte("not-a-real-mac"))
	_, err = DecodeCursor(key, tampered, "t-1", "obligation", "d-1")
	assert.ErrorIs(t, err, ErrCursorInvalid)
}

// A cursor signed with a different key does not verify — which is what makes
// key rotation a cursor invalidation rather than a silent trust extension.
func TestCursor_ForeignKeyFails(t *testing.T) {
	token, err := EncodeCursor([]byte("some-other-key-thirty-two-bytes-long!!"),
		Cursor{TenantID: "t-1", Scope: "obligation", PlanDigest: "d-1"})
	require.NoError(t, err)

	_, err = DecodeCursor(key, token, "t-1", "obligation", "d-1")
	assert.ErrorIs(t, err, ErrCursorInvalid)
}

// A VALID cursor belonging to another tenant is still refused. The signature
// proves this service minted it, not that it was minted for this caller.
func TestCursor_ValidButForAnotherTenantIsRefused(t *testing.T) {
	token, err := EncodeCursor(key, Cursor{TenantID: "tenant-a", Scope: "obligation", PlanDigest: "d-1"})
	require.NoError(t, err)

	_, err = DecodeCursor(key, token, "tenant-b", "obligation", "d-1")
	assert.ErrorIs(t, err, ErrCursorMismatch)
}

func TestCursor_ValidButForAnotherScopeIsRefused(t *testing.T) {
	token, err := EncodeCursor(key, Cursor{TenantID: "t-1", Scope: "obligation", PlanDigest: "d-1"})
	require.NoError(t, err)

	_, err = DecodeCursor(key, token, "t-1", "employee", "d-1")
	assert.ErrorIs(t, err, ErrCursorMismatch)
}

func TestCursor_MalformedTokensAreRejected(t *testing.T) {
	for _, token := range []string{"", ".", "abc", "abc.", ".abc", "not-base64!.also-not"} {
		_, err := DecodeCursor(key, token, "t-1", "obligation", "d-1")
		assert.ErrorIs(t, err, ErrCursorInvalid, "token %q", token)
	}
}
