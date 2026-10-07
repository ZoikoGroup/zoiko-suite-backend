package query

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Cursor is the decoded contents of an opaque pagination token.
//
// It carries the tenant, the scope and a digest of the plan that produced it,
// as well as the sort position. Carrying them is the whole point: NP-57 is
// "cursor is modified to increase scope/tenant", and a cursor that held only a
// sort position would be equally valid against any scope in any tenant, so
// editing the surrounding request would move the page without invalidating
// the token.
type Cursor struct {
	TenantID string `json:"t"`
	Scope    string `json:"s"`
	// PlanDigest binds the cursor to the exact compiled plan. A caller that
	// changes its filters and reuses the cursor gets a mismatch rather than
	// page two of a different query.
	PlanDigest string `json:"p"`
	// After is the sort position from the last hit of the previous page.
	After []any `json:"a"`
	// Issued is a monotonic counter of pages served, used only to bound how
	// far a single cursor chain may walk.
	Page int `json:"n"`
}

var (
	// ErrCursorInvalid means the token did not decode or its signature did
	// not verify. NP-57's required behaviour: "signed opaque cursor fails
	// integrity validation."
	ErrCursorInvalid = errors.New("cursor failed integrity validation")
	// ErrCursorMismatch means the token verified but belongs to a different
	// tenant, scope or plan.
	ErrCursorMismatch = errors.New("cursor does not belong to this request")
)

// EncodeCursor signs and encodes a cursor.
//
// HMAC-SHA256 over the payload, then base64url of "payload.mac". Not
// encryption: the contents are not secret — a caller already knows its own
// tenant and scope — what matters is that they cannot be CHANGED. An
// encrypted-but-unauthenticated token would be worse, because it would look
// opaque while remaining malleable.
func EncodeCursor(key []byte, c Cursor) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// DecodeCursor verifies and decodes a cursor, then checks it belongs to this
// request.
//
// The signature is checked BEFORE the contents are used for anything, and with
// hmac.Equal rather than bytes.Equal — a timing-variable comparison on a MAC
// is a forgery oracle, and a cursor is attacker-supplied by definition.
func DecodeCursor(key []byte, token, tenantID, scope, planDigest string) (Cursor, error) {
	var c Cursor
	dot := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(token)-1 {
		return c, ErrCursorInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return c, ErrCursorInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return c, ErrCursorInvalid
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, ErrCursorInvalid
	}

	if err := json.Unmarshal(payload, &c); err != nil {
		return c, ErrCursorInvalid
	}

	// A verified cursor from another tenant is still refused. The signature
	// proves this service minted it, not that it was minted for this caller —
	// and a tenant that obtained another tenant's cursor (from a shared
	// screenshot, a log, a support ticket) must not be able to replay it.
	if c.TenantID != tenantID || c.Scope != scope {
		return c, ErrCursorMismatch
	}
	if planDigest != "" && c.PlanDigest != planDigest {
		// NP-56: "source authorization changes during paginated search →
		// each page/cursor enforces current policy; prior cursor does not
		// freeze permission." Binding to the plan digest is half of that; the
		// other half is that the mandatory filters are recompiled from
		// scratch for every page rather than carried in the cursor.
		return c, ErrCursorMismatch
	}
	return c, nil
}
