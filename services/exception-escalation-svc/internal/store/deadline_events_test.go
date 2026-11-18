package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

const evPrefix = "com.zoikosuite.business.deadline."

// Each BIZ-08 event is written in the same transaction as the change it announces, to the shared
// eventing outbox. Payload schemas are checked for every row by verifyEventContracts.
func TestDeadlineEvents_WrittenWithEachChange(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))

	// created + completed
	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-ev", Title: "Close books", DueAt: time.Now().UTC().Add(200 * time.Hour),
		CreatedByPrincipalID: "creator-1", CorrelationID: "corr-1",
	})
	require.NoError(t, err)
	_, err = s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1", CorrelationID: "corr-2"})
	require.NoError(t, err)
	require.Equal(t, []string{evPrefix + "created", evPrefix + "completed"}, eventingTypes(t, pool, d.DeadlineID))

	var envJSON string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE aggregate_id=$1 AND event_type=$2`, d.DeadlineID, evPrefix+"completed").Scan(&envJSON))

	// mirrored, then superseded by a new source version: created, created (new), superseded (old)
	p := domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-ev", Title: "VAT", DueAt: time.Now().UTC().Add(300 * time.Hour),
		SourceType: "TAX", SourceRef: "vat-ev-1", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	}
	first, _, err := s.MirrorAuthoritativeDeadline(ctx(), p)
	require.NoError(t, err)
	p.SourceVersion = "v2"
	second, supersededID, err := s.MirrorAuthoritativeDeadline(ctx(), p)
	require.NoError(t, err)
	require.Equal(t, first.DeadlineID, supersededID)
	require.Equal(t, []string{evPrefix + "created", evPrefix + "superseded"}, eventingTypes(t, pool, first.DeadlineID))
	require.Equal(t, []string{evPrefix + "created"}, eventingTypes(t, pool, second.DeadlineID))

	// A replay of the same version writes no second event.
	_, _, err = s.MirrorAuthoritativeDeadline(ctx(), p)
	require.NoError(t, err)
	require.Equal(t, []string{evPrefix + "created"}, eventingTypes(t, pool, second.DeadlineID))
	assert.Contains(t, envJSON, "corr-2", "the correlation id is carried onto the event")
	assert.Contains(t, envJSON, "owner-1", "the actor is carried onto the event")
}

// due_soon and overdue are committed with the notified stamp, once, so a failed Kafka publish
// can no longer lose them (the handler used to publish after the stamp committed).
func TestDeadlineEvents_DueSoonAndOverdueOncePerCrossing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))

	soon, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{TenantID: "default", LegalEntityID: "le-ev2", Title: "soon", DueAt: time.Now().UTC().Add(time.Hour), CreatedByPrincipalID: "c"})
	require.NoError(t, err)
	late, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{TenantID: "default", LegalEntityID: "le-ev2", Title: "late", DueAt: time.Now().UTC().Add(-time.Hour), CreatedByPrincipalID: "c"})
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		_, _, err = s.ListUpcoming(ctx(), domain.ListUpcomingParams{TenantID: "default", LegalEntityID: "le-ev2", ActorPrincipalID: "reader-1", CorrelationID: "c-up"})
		require.NoError(t, err)
		_, _, err = s.ListOverdue(ctx(), domain.ListOverdueParams{TenantID: "default", LegalEntityID: "le-ev2", ActorPrincipalID: "reader-1", CorrelationID: "c-od"})
		require.NoError(t, err)
	}
	require.Equal(t, []string{evPrefix + "created", evPrefix + "became-due-soon"}, eventingTypes(t, pool, soon.DeadlineID))
	require.Equal(t, []string{evPrefix + "created", evPrefix + "became-overdue"}, eventingTypes(t, pool, late.DeadlineID))
}

// A store with no event region cannot build the envelope, so the write is refused and rolled
// back: no deadline, no event.
func TestDeadlineEvents_MissingRegionRollsTheWriteBack(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool) // no WithEventRegion

	_, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{TenantID: "default", LegalEntityID: "le-r", Title: "x", DueAt: time.Now().UTC().Add(time.Hour), CreatedByPrincipalID: "c"})
	require.Error(t, err)

	var deadlines, events int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM deadlines`).Scan(&deadlines))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&events))
	assert.Zero(t, deadlines, "the deadline must not exist when its event could not be written")
	assert.Zero(t, events)

	// An overdue stamp is rolled back with its event too.
	good := store.NewPgStore(pool, store.WithEventRegion("local"))
	late, err := good.CreateDeadline(ctx(), domain.CreateDeadlineParams{TenantID: "default", LegalEntityID: "le-r", Title: "late", DueAt: time.Now().UTC().Add(-time.Hour), CreatedByPrincipalID: "c"})
	require.NoError(t, err)
	_, _, err = s.ListOverdue(ctx(), domain.ListOverdueParams{TenantID: "default", LegalEntityID: "le-r"})
	require.Error(t, err)
	got, err := good.GetDeadline(ctx(), late.DeadlineID)
	require.NoError(t, err)
	assert.Nil(t, got.OverdueNotifiedAt, "a failed event must not leave the deadline marked as notified")
}
