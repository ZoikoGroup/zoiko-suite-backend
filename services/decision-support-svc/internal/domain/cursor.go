package domain

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Page size bounds for GET /v1/recommendations.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// EncodeCursor renders the keyset position of rec as an opaque cursor.
func EncodeCursor(rec Recommendation) string {
	raw := rec.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + rec.RecommendationID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor.
func DecodeCursor(cursor string) (*PageCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, false
	}
	createdAtText, recommendationID, found := strings.Cut(string(raw), "|")
	if !found {
		return nil, false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtText)
	if err != nil {
		return nil, false
	}
	if _, err := uuid.Parse(recommendationID); err != nil {
		return nil, false
	}
	return &PageCursor{CreatedAt: createdAt, RecommendationID: recommendationID}, true
}
