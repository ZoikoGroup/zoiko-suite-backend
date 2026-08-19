// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires (event_version, tenant_id, legal_entity_id,
// actor_id), sourced from real data on domain.JournalHeader — not left
// empty or fabricated.
package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decodeOne(t *testing.T, w *fakeWriter) envelope {
	t.Helper()
	require.Len(t, w.msgs, 1)
	var env envelope
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	return env
}

func header() domain.JournalHeader {
	postedBy := "poster-1"
	validatedBy := "validator-1"
	reversedBy := "reverser-1"
	return domain.JournalHeader{
		JournalID:              "journal-1",
		TenantID:               "tenant-1",
		LegalEntityID:          "entity-1",
		FiscalPeriod:           "2026-08",
		CorrelationID:          "corr-1",
		CreatedByPrincipalID:   "creator-1",
		ValidatedByPrincipalID: &validatedBy,
		PostedByPrincipalID:    &postedBy,
		ReversedByPrincipalID:  &reversedBy,
		CreatedAt:              time.Now().UTC(),
	}
}

func TestPublishJournalCreated_EnvelopeCarriesCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.general-ledger.events", w)

	p.PublishJournalCreated(context.Background(), header())

	env := decodeOne(t, w)
	assert.Equal(t, "journal.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "general-ledger-svc", env.SourceService)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID, "journal.created must attribute the CREATOR, not a later lifecycle actor")
}

// TestPublishJournalPosted_EnvelopeCarriesPoster_NotCreator is the important
// one: before this fix, journal.posted's envelope carried no actor at all.
// It must specifically be the POSTER, since posting can legitimately be a
// different principal than whoever created the draft — attributing the
// wrong actor on a financial-ledger event is exactly the kind of "no
// silent state change" violation Doc 01 §2.10 exists to prevent.
func TestPublishJournalPosted_EnvelopeCarriesPoster_NotCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.general-ledger.events", w)
	h := header()

	p.PublishJournalPosted(context.Background(), h)

	env := decodeOne(t, w)
	assert.Equal(t, "journal.posted", env.EventType)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "poster-1", env.ActorID)
	assert.NotEqual(t, h.CreatedByPrincipalID, env.ActorID)
}

func TestPublishJournalValidated_EnvelopeCarriesValidator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.general-ledger.events", w)

	p.PublishJournalValidated(context.Background(), header())

	env := decodeOne(t, w)
	assert.Equal(t, "validator-1", env.ActorID)
}

func TestPublishJournalReversed_EnvelopeCarriesReverser(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.general-ledger.events", w)

	p.PublishJournalReversed(context.Background(), header(), "journal-2")

	env := decodeOne(t, w)
	assert.Equal(t, "journal.reversed", env.EventType)
	assert.Equal(t, "reverser-1", env.ActorID)
}

// TestPublishJournalPosted_NilActorPointer_OmitsRatherThanPanics proves the
// pre-posting lifecycle stages (where PostedByPrincipalID is still nil) are
// handled safely — this should not realistically happen (posting implies
// PostedByPrincipalID was just set), but a nil pointer must never panic the
// publish path.
func TestPublishJournalPosted_NilActorPointer_OmitsRatherThanPanics(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.general-ledger.events", w)
	h := header()
	h.PostedByPrincipalID = nil

	assert.NotPanics(t, func() {
		p.PublishJournalPosted(context.Background(), h)
	})
	env := decodeOne(t, w)
	assert.Empty(t, env.ActorID)
}
