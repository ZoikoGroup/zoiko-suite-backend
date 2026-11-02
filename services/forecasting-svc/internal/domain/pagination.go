package domain

import (
	"encoding/base64"
	"strings"
	"time"
)

// Cursor pagination for every list endpoint.
//
// Lists are ordered newest first by (created_at DESC, id DESC). The cursor is an
// opaque base64 string encoding the (created_at, id) of the last row returned;
// the next page starts strictly after it. Offset pagination is avoided because
// it skips or repeats rows when new records arrive between page requests.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// PageRequest is the decoded ?limit=&cursor= pair.
type PageRequest struct {
	Limit           int
	AfterCreatedAt  time.Time // zero when there is no cursor
	AfterID         string
	HasCursorFilter bool
}

// EncodeCursor builds the opaque cursor for the row (createdAt, id).
func EncodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor.
func DecodeCursor(cursor string) (time.Time, string, error) {
	invalid := NewValidationError("cursor", "cursor is not a valid page cursor")
	rawBytes, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", invalid
	}
	parts := strings.SplitN(string(rawBytes), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", invalid
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", invalid
	}
	return createdAt, parts[1], nil
}
