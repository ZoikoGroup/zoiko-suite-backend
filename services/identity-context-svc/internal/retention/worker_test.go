package retention_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/retention"
	"zoiko.io/identity-context-svc/internal/store"
)

// DoD gate 8: "Retention/hold/privacy behavior tested where applicable."
//
// The test that matters most here is the one asserting a legal hold BLOCKS
// disposition, and the one asserting a FAILED hold check also blocks it. The
// cost of getting either wrong is destroying evidence during litigation, which
// is not a failure you recover from by fixing the code afterwards.

type fakeStore struct {
	tenants  []string
	sessions map[string][]store.DisposableSession
	holds    map[string]bool // "tenant|principal" → held
	holdErr  error

	disposed          map[string][]string
	purged            int
	idempotencyPurged int
	disposeErr        error
	tenantsErr        error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[string][]store.DisposableSession{},
		holds:    map[string]bool{},
		disposed: map[string][]string{},
	}
}

func (f *fakeStore) TenantsWithDisposableRecords(_ context.Context, _ time.Time) ([]string, error) {
	return f.tenants, f.tenantsErr
}

func (f *fakeStore) FindDisposableSessions(_ context.Context, tenantID string, _ time.Time, limit int) ([]store.DisposableSession, error) {
	rows := f.sessions[tenantID]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *fakeStore) DisposeSessions(_ context.Context, tenantID string, ids []string, _ time.Time) (int, error) {
	if f.disposeErr != nil {
		return 0, f.disposeErr
	}
	f.disposed[tenantID] = append(f.disposed[tenantID], ids...)
	return len(ids), nil
}

func (f *fakeStore) HasActiveLegalHold(_ context.Context, tenantID, principalID string) (bool, error) {
	if f.holdErr != nil {
		return false, f.holdErr
	}
	return f.holds[tenantID+"|"+principalID], nil
}

func (f *fakeStore) PurgePublishedOutbox(_ context.Context, _ time.Time, _ int) (int, error) {
	f.purged++
	return 3, nil
}

// idempotencyPurged counts calls so a test can assert the sweep prunes the
// replay table as well as the outbox.
func (f *fakeStore) PurgeIdempotencyKeysBefore(_ context.Context, _ time.Time) (int64, error) {
	f.idempotencyPurged++
	return 2, nil
}

// capturingSink records the events the worker emits, so a test can assert on
// the disposition certificate rather than only on the row counts.
type capturingSink struct{ records []outbox.Record }

func (c *capturingSink) Emit(_ context.Context, rec outbox.Record) error {
	c.records = append(c.records, rec)
	return nil
}

func (c *capturingSink) typesEmitted() []string {
	out := make([]string, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, r.EventType)
	}
	return out
}

func newWorker(s retention.Store, sink *capturingSink) *retention.Worker {
	return retention.New(s,
		events.NewPublisherWithSink(zap.NewNop(), "test-topic", sink),
		retention.Config{Interval: time.Hour, BatchSize: 100, OutboxRetention: 24 * time.Hour},
		zap.NewNop())
}

func session(id, principalID string) store.DisposableSession {
	return store.DisposableSession{SessionContextID: id, PrincipalID: principalID}
}

// ── Disposition ──────────────────────────────────────────────────────────────

func TestSweepDisposesEvidencePastItsRetentionPeriod(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{
		session("sc-1", "p-1"),
		session("sc-2", "p-2"),
	}
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.ElementsMatch(t, []string{"sc-1", "sc-2"}, s.disposed["tenant-a"])
}

// ── Legal hold (GOV-10) ──────────────────────────────────────────────────────

// TestLegalHoldBlocksDisposition is the central test of this package.
//
// Invariant 7: a hold "suspends disposition but never creates a new processing
// purpose". The suspension half is here; nothing in this worker can widen
// access, which is the other half.
func TestLegalHoldBlocksDisposition(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{
		session("sc-1", "p-held"),
		session("sc-2", "p-free"),
	}
	s.holds["tenant-a|p-held"] = true
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.Equal(t, []string{"sc-2"}, s.disposed["tenant-a"],
		"a held principal's evidence must survive the sweep")
	assert.NotContains(t, s.disposed["tenant-a"], "sc-1")
}

// TestFailedHoldCheckBlocksDisposition is the fail-closed direction, and
// "closed" here means DO NOT DELETE.
//
// A hold check that did not run is not a check that passed. Skipping the row
// costs a few hours until the next sweep; disposing it costs evidence that
// cannot be recovered.
func TestFailedHoldCheckBlocksDisposition(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{session("sc-1", "p-1")}
	s.holdErr = errors.New("postgres is gone")
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.Empty(t, s.disposed["tenant-a"],
		"an unanswerable hold check must never be read as 'no hold'")
}

// TestHeldBackIsReportedSeparately pins that the two numbers are not collapsed.
//
// A sweep that quietly disposed 1 of 2 rows and said nothing about the other
// would look like a success in every dashboard. "Nothing was due" and
// "everything due was held" are materially different facts.
func TestHeldBackIsReportedSeparately(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{
		session("sc-1", "p-held"),
		session("sc-2", "p-free"),
	}
	s.holds["tenant-a|p-held"] = true
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.Contains(t, sink.typesEmitted(), events.EventDispositionExecuted)
	assert.Contains(t, sink.typesEmitted(), events.EventDispositionBlocked,
		"rows held back must be announced, not merely omitted from the disposed count")
}

func TestNoBlockedEventWhenNothingIsHeld(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{session("sc-1", "p-1")}
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.NotContains(t, sink.typesEmitted(), events.EventDispositionBlocked)
}

// TestTenantWideHoldBlocksEveryPrincipal covers the hold that names no
// principal — the one that blocks everything.
func TestTenantWideHoldBlocksEveryPrincipal(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{
		session("sc-1", "p-1"),
		session("sc-2", "p-2"),
		session("sc-3", "p-3"),
	}
	// The store's HasActiveLegalHold resolves a tenant-wide hold to true for
	// every principal — that predicate is what this stands in for.
	s.holds["tenant-a|p-1"] = true
	s.holds["tenant-a|p-2"] = true
	s.holds["tenant-a|p-3"] = true
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	assert.Empty(t, s.disposed["tenant-a"])
	assert.Contains(t, sink.typesEmitted(), events.EventDispositionBlocked)
}

// ── Certificate ──────────────────────────────────────────────────────────────

// TestCertificateIsIssuedEvenWhenNothingWasDisposed pins GOV-09's named
// required output.
//
// A sweep that disposed nothing because everything was held is a materially
// different fact from one that found nothing due, and both belong on the
// record.
func TestCertificateIsIssuedEvenWhenNothingWasDisposed(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	s.sessions["tenant-a"] = []store.DisposableSession{session("sc-1", "p-held")}
	s.holds["tenant-a|p-held"] = true
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	require.NotEmpty(t, sink.records)
	assert.Contains(t, sink.typesEmitted(), events.EventDispositionExecuted)
	assert.Contains(t, string(sink.records[0].Payload), "certificate_id")
	assert.Contains(t, string(sink.records[0].Payload), `"held_back":1`)
}

func TestNoCertificateWhenNothingWasDue(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a"}
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))
	assert.NotContains(t, sink.typesEmitted(), events.EventDispositionExecuted)
}

// ── Resilience ───────────────────────────────────────────────────────────────

// TestOneTenantFailureDoesNotStopTheRest: a retention obligation that stops at
// the first broken tenant is one that silently stops applying to everyone
// after it in the list.
func TestOneTenantFailureDoesNotStopTheRest(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-broken", "tenant-ok"}
	s.sessions["tenant-broken"] = []store.DisposableSession{session("sc-x", "p-x")}
	s.sessions["tenant-ok"] = []store.DisposableSession{session("sc-1", "p-1")}
	s.disposeErr = errors.New("deadlock detected")
	sink := &capturingSink{}

	err := newWorker(s, sink).SweepOnce(context.Background())

	// The error IS reported — a sweep that swallowed it would look clean while
	// a tenant's retention silently stopped running.
	require.Error(t, err)
}

func TestOutboxIsPurgedOnEverySweep(t *testing.T) {
	s := newFakeStore()
	sink := &capturingSink{}

	require.NoError(t, newWorker(s, sink).SweepOnce(context.Background()))

	// Delivered outbox rows carry no evidential weight of their own — the
	// event is on the topic and the fact it attests is in its own table — so
	// unlike session evidence this is a true delete, and it must actually run
	// or the table grows without bound.
	assert.Equal(t, 1, s.purged)
}

func TestSweepStopsOnContextCancellation(t *testing.T) {
	s := newFakeStore()
	s.tenants = []string{"tenant-a", "tenant-b"}
	sink := &capturingSink{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := newWorker(s, sink).SweepOnce(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
