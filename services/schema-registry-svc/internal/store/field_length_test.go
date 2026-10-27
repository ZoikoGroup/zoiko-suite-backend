package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

// An over-long Idempotency-Key is a field error (ErrFieldTooLong), which the
// handler maps to 400, not an unclassified store failure.
func TestPgStore_Insert_OverlongIdempotencyKey_IsAFieldError(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Insert(context.Background(), &domain.EventSchema{
		EventName: "long.key.probe", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema:     json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
		IdempotencyKey: strings.Repeat("k", 300),
	}, 0)
	require.ErrorIs(t, err, domain.ErrFieldTooLong)
}
