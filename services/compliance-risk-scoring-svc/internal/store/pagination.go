package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Page limits.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// PageCursor is the keyset position after the last row returned: rows are
// ordered by (created_at DESC, id DESC), so the next page continues with
// rows strictly "before" this pair.
type PageCursor struct {
	CreatedAt time.Time
	ID        string
}

// PageRequest asks for up to Limit rows after Cursor (nil = first page).
type PageRequest struct {
	Limit  int
	Cursor *PageCursor
}

// EncodeCursor makes an opaque, URL-safe cursor token.
func EncodeCursor(cursor PageCursor) string {
	raw := cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token from EncodeCursor.
func DecodeCursor(token string) (*PageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errors.New("cursor is not valid")
	}
	timestampText, id, found := strings.Cut(string(raw), "|")
	if !found || id == "" {
		return nil, errors.New("cursor is not valid")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, timestampText)
	if err != nil {
		return nil, errors.New("cursor is not valid")
	}
	return &PageCursor{CreatedAt: createdAt, ID: id}, nil
}
