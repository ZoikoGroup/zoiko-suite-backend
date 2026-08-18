package store_test

// Store-level coverage for the gaps closed in the 17 Aug 2026 pass: the paging
// the reads never had, and the schema invariants 000003 states.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

func seedVersions(t *testing.T, s interface {
	Insert(context.Context, *domain.EventSchema, int) (*domain.EventSchema, error)
}, eventName string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := s.Insert(ctx, &domain.EventSchema{
			EventName:         eventName,
			JSONSchema:        json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
			CompatibilityMode: domain.CompatibilityBackward,
		}, i)
		require.NoError(t, err, "seed version %d", i+1)
	}
}

// Both reads were unbounded: every version of an event, and every event name
// on the platform, in one response, forever.
func TestPgStore_Versions_IsPaged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedVersions(t, s, "paging.probe.event", 5)

	page1, err := s.Versions(ctx, "paging.probe.event", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, 1, page1[0].Version, "versions come back oldest first")
	assert.Equal(t, 2, page1[1].Version)

	page3, err := s.Versions(ctx, "paging.probe.event", 2, 4)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	assert.Equal(t, 5, page3[0].Version)

	// (event_name, version) is the primary key, so the ordering is already a
	// total order — the pages must partition the history, never repeat a row.
	beyond, err := s.Versions(ctx, "paging.probe.event", 2, 99)
	require.NoError(t, err)
	assert.Empty(t, beyond)
}

func TestPgStore_EventNames_IsPaged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		seedVersions(t, s, "catalogue.probe.e"+strconv.Itoa(i), 1)
	}

	first, err := s.EventNames(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, first, 2)

	second, err := s.EventNames(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, second, 2)

	for _, a := range first {
		assert.NotContains(t, second, a, "event name %q appeared on both pages", a)
	}
}

// json_schema is JSONB, so Postgres guaranteed well-formed JSON and nothing
// more — `123` and `null` are valid JSONB. The service's own check was
// json.Valid, which accepts them too, so a first version could be stored as a
// number and the event could then never be evolved.
func TestPgStore_Insert_NonObjectSchema_IsRefusedByTheSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, raw := range []string{`123`, `"a string"`, `null`, `[]`} {
		_, err := s.Insert(ctx, &domain.EventSchema{
			EventName:         "invariant.probe.event",
			JSONSchema:        json.RawMessage(raw),
			CompatibilityMode: domain.CompatibilityBackward,
		}, 0)
		assert.Error(t, err, "the schema accepted json_schema %s", raw)
	}
}

func TestPgStore_Insert_EmptyObjectSchema_IsRefusedByTheSchema(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Insert(context.Background(), &domain.EventSchema{
		EventName:         "invariant.probe.empty",
		JSONSchema:        json.RawMessage(`{}`),
		CompatibilityMode: domain.CompatibilityBackward,
	}, 0)
	assert.Error(t, err, "the schema accepted {} — a contract that permits every payload")
}

// The event name is the primary key of a canonical registry, and was a
// free-text field.
func TestPgStore_Insert_MalformedEventName_IsRefusedByTheSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"NotLowercase.event", "nodots", "trailing."} {
		_, err := s.Insert(ctx, &domain.EventSchema{
			EventName:         name,
			JSONSchema:        json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
			CompatibilityMode: domain.CompatibilityBackward,
		}, 0)
		assert.Error(t, err, "the schema accepted event name %q", name)
	}
}

// The service refuses any compatibility mode it cannot enforce rather than
// defaulting it, so the column should only ever hold BACKWARD or NONE.
func TestPgStore_Insert_UnknownCompatibilityMode_IsRefusedByTheSchema(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Insert(context.Background(), &domain.EventSchema{
		EventName:         "invariant.probe.mode",
		JSONSchema:        json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
		CompatibilityMode: "FORWARD",
	}, 0)
	assert.Error(t, err, "the schema accepted a compatibility mode the service cannot enforce")
}

// An over-long name used to reach Postgres, die there as SQLSTATE 22001, and
// come back as 503 "schema store unavailable" — an outage status for a name
// that was simply too long.
func TestPgStore_Insert_OverlongEventName_IsAFieldErrorNotAnOutage(t *testing.T) {
	s := newTestStore(t)
	long := "a." + string(make([]byte, 0))
	for i := 0; i < 300; i++ {
		long += "b"
	}
	_, err := s.Insert(context.Background(), &domain.EventSchema{
		EventName:         long,
		JSONSchema:        json.RawMessage(`{"properties":{"a":{"type":"string"}}}`),
		CompatibilityMode: domain.CompatibilityBackward,
	}, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrFieldTooLong,
		"an over-long field must be a field error, not a generic store failure")
}
