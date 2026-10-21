package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// Upload reservations (ZS-SVC-S-001 §3.4) against real Postgres: the state
// machine, idempotency, and the "commit once, atomically" guarantee.

func reserveFor(t *testing.T, s *store.PgStore, ctx context.Context, docID, principal, key string, size int64) (*domain.UploadReservation, bool, error) {
	t.Helper()
	return s.CreateUploadReservation(ctx, domain.CreateUploadReservationParams{
		DocumentID: docID, ReservedByPrincipal: principal, Purpose: "amendment", ExpectedSizeBytes: size,
		DeclaredContentType: "application/pdf", IdempotencyKey: key, RequestHash: "hash-" + key + "-" + string(rune('0'+size%10)),
	})
}

func newVersionFor(label string) *domain.DocumentVersion {
	return &domain.DocumentVersion{ChecksumSHA256: sha256Hex(label), StorageKey: "blob-" + label, SizeBytes: 7,
		ContentType: "application/pdf", DetectedContentType: "application/pdf", CreatedByPrincipalID: "uploader-1"}
}

func TestReservation_CreateAndIdempotency(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-idem")

	first, replayed, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 100)
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, domain.ReservationReserved, first.CommitStatus)
	require.Equal(t, int64(100), first.ExpectedSizeBytes)
	require.Equal(t, "application/pdf", first.DeclaredContentType)

	again, replayed, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 100)
	require.NoError(t, err)
	require.True(t, replayed, "same key and request: the original reservation")
	require.Equal(t, first.ReservationID, again.ReservationID)

	_, _, err = reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 101)
	require.ErrorIs(t, err, domain.ErrIdempotencyMismatch, "same key, different request")

	other, replayed, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p2", "k1", 100)
	require.NoError(t, err)
	require.False(t, replayed, "another principal's key is its own")
	require.NotEqual(t, first.ReservationID, other.ReservationID)
}

func TestReservation_ConcurrentIdenticalRequests_CreateOne(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-concurrent-create")

	const n = 12
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "race", 100)
			errs[i] = err
			if r != nil {
				ids[i] = r.ReservationID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
	}
}

func TestReservation_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-tenant")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 100)
	require.NoError(t, err)

	_, _, err = reserveFor(t, s, otherTenantCtx(), doc.DocumentID, "p1", "k2", 100)
	require.ErrorIs(t, err, domain.ErrDocumentNotFound, "another tenant cannot reserve against this tenant's document")

	_, err = s.GetUploadReservation(otherTenantCtx(), res.ReservationID)
	require.ErrorIs(t, err, domain.ErrUploadReservationNotFound)
	_, _, _, err = s.CompleteUploadReservation(otherTenantCtx(), res.ReservationID, newVersionFor("x"), "c")
	require.ErrorIs(t, err, domain.ErrUploadReservationNotFound)
	_, err = s.CloseUploadReservation(otherTenantCtx(), res.ReservationID, domain.ReservationFailed, "x")
	require.ErrorIs(t, err, domain.ErrUploadReservationNotFound)

	for _, bad := range []string{"", "nope", "' OR 1=1 --"} {
		_, err = s.GetUploadReservation(tenantCtx(), bad)
		require.ErrorIs(t, err, domain.ErrUploadReservationNotFound, bad)
	}
	got, err := s.GetUploadReservation(tenantCtx(), res.ReservationID)
	require.NoError(t, err)
	require.Equal(t, domain.ReservationReserved, got.CommitStatus, "the other tenant's attempts changed nothing")
}

func TestReservation_Complete_CommitsTheVersionAtomically(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v1 := createTestDocument(t, s, "res-commit")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 7)
	require.NoError(t, err)

	v := newVersionFor("commit")
	got, version, already, err := s.CompleteUploadReservation(tenantCtx(), res.ReservationID, v, "corr-1")
	require.NoError(t, err)
	require.False(t, already)
	require.Equal(t, domain.ReservationCommitted, got.CommitStatus)
	require.NotNil(t, got.CommittedVersionID)
	require.Equal(t, version.DocumentVersionID, *got.CommittedVersionID)
	require.Equal(t, 2, version.Version, "the next version of the document")
	require.NotNil(t, got.CompletedAt)

	stored, err := s.FindVersionByID(tenantCtx(), version.DocumentVersionID)
	require.NoError(t, err)
	require.Equal(t, "application/pdf", stored.DetectedContentType)
	require.Equal(t, v1.DocumentID, stored.DocumentID)
	cur, err := s.FindDocumentByID(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Equal(t, 2, cur.CurrentVersion)

	var events int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id::text = $1 AND event_type = 'document_version_committed'`, doc.DocumentID).Scan(&events))
	require.Equal(t, 2, events, "the version's event is in the same transaction as the commit")
}

func TestReservation_RetriedCompletion_CommitsExactlyOneVersion(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-retry")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 7)
	require.NoError(t, err)

	first, firstVersion, already, err := s.CompleteUploadReservation(tenantCtx(), res.ReservationID, newVersionFor("a"), "c")
	require.NoError(t, err)
	require.False(t, already)
	for i := 0; i < 20; i++ {
		got, v, already, err := s.CompleteUploadReservation(tenantCtx(), res.ReservationID, newVersionFor("again"), "c")
		require.NoError(t, err)
		require.True(t, already, "retry %d", i)
		require.Equal(t, first.ReservationID, got.ReservationID)
		require.Equal(t, firstVersion.DocumentVersionID, v.DocumentVersionID)
	}
	var versions int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM document_versions WHERE document_id::text = $1`, doc.DocumentID).Scan(&versions))
	require.Equal(t, 2, versions, "the original version plus ONE committed version, after 21 completions")
}

func TestReservation_ConcurrentCompletions_CommitExactlyOneVersion(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-concurrent-complete")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 7)
	require.NoError(t, err)

	const n = 10
	ids := make([]string, n)
	fresh := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, v, already, err := s.CompleteUploadReservation(tenantCtx(), res.ReservationID, newVersionFor(strings.Repeat("x", i+1)), "c")
			errs[i] = err
			if v != nil {
				ids[i] = v.DocumentVersionID
			}
			fresh[i] = !already
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i], "every caller sees the one committed version")
		if fresh[i] {
			winners++
		}
	}
	require.Equal(t, 1, winners, "exactly one completion performed the commit")
	var versions int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM document_versions WHERE document_id::text = $1`, doc.DocumentID).Scan(&versions))
	require.Equal(t, 2, versions)
}

func TestReservation_FailedCommit_RollsBackEverything(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-rollback")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 7)
	require.NoError(t, err)

	bad := newVersionFor("rollback")
	bad.StorageKey = strings.Repeat("k", 600) // longer than the VARCHAR(500) column: the version insert fails
	_, _, _, err = s.CompleteUploadReservation(tenantCtx(), res.ReservationID, bad, "c")
	require.Error(t, err)

	got, err := s.GetUploadReservation(tenantCtx(), res.ReservationID)
	require.NoError(t, err)
	require.Equal(t, domain.ReservationReserved, got.CommitStatus, "no half-committed reservation")
	require.Nil(t, got.CommittedVersionID)
	cur, err := s.FindDocumentByID(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Equal(t, 1, cur.CurrentVersion, "the document's version counter was rolled back")
	var events int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id::text = $1 AND event_type = 'document_version_committed'`, doc.DocumentID).Scan(&events))
	require.Equal(t, 1, events, "and so was the event")

	// Still retryable with good data.
	_, v, already, err := s.CompleteUploadReservation(tenantCtx(), res.ReservationID, newVersionFor("good"), "c")
	require.NoError(t, err)
	require.False(t, already)
	require.Equal(t, 2, v.Version)
}

func TestReservation_TerminalStates_AreImmutable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-terminal")

	failed, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "kf", 7)
	require.NoError(t, err)
	closed, err := s.CloseUploadReservation(tenantCtx(), failed.ReservationID, domain.ReservationFailed, "size mismatch")
	require.NoError(t, err)
	require.Equal(t, domain.ReservationFailed, closed.CommitStatus)
	require.Equal(t, "size mismatch", closed.FailureReason)

	// Closing again changes nothing; completing a closed reservation is refused and commits nothing.
	again, err := s.CloseUploadReservation(tenantCtx(), failed.ReservationID, domain.ReservationQuarantined, "other")
	require.NoError(t, err)
	require.Equal(t, domain.ReservationFailed, again.CommitStatus)
	require.Equal(t, "size mismatch", again.FailureReason)
	_, _, _, err = s.CompleteUploadReservation(tenantCtx(), failed.ReservationID, newVersionFor("late"), "c")
	require.ErrorIs(t, err, domain.ErrUploadReservationClosed)

	quarantined, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "kq", 7)
	require.NoError(t, err)
	q, err := s.CloseUploadReservation(tenantCtx(), quarantined.ReservationID, domain.ReservationQuarantined, "EICAR")
	require.NoError(t, err)
	require.Equal(t, domain.ReservationQuarantined, q.CommitStatus)

	// A committed reservation stays committed when asked to close.
	committed, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "kc", 7)
	require.NoError(t, err)
	_, _, _, err = s.CompleteUploadReservation(tenantCtx(), committed.ReservationID, newVersionFor("ok"), "c")
	require.NoError(t, err)
	still, err := s.CloseUploadReservation(tenantCtx(), committed.ReservationID, domain.ReservationFailed, "nope")
	require.NoError(t, err)
	require.Equal(t, domain.ReservationCommitted, still.CommitStatus)

	var versions int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM document_versions WHERE document_id::text = $1`, doc.DocumentID).Scan(&versions))
	require.Equal(t, 2, versions, "the failed and quarantined reservations produced no version")

	_, err = s.CloseUploadReservation(tenantCtx(), failed.ReservationID, "COMMITTED", "x")
	require.Error(t, err, "only a failure state can be closed into")
}

func TestReservation_DatabaseGuards(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "res-guards")
	res, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k1", 7)
	require.NoError(t, err)
	_, _, _, err = s.CompleteUploadReservation(tenantCtx(), res.ReservationID, newVersionFor("g"), "c")
	require.NoError(t, err)

	ctx := context.Background()
	_, err = pool.Exec(ctx, `UPDATE upload_reservations SET commit_status = 'FAILED' WHERE reservation_id = $1`, res.ReservationID)
	require.Error(t, err, "a terminal reservation cannot be edited, even by a superuser")
	_, err = pool.Exec(ctx, `DELETE FROM upload_reservations WHERE reservation_id = $1`, res.ReservationID)
	require.Error(t, err, "reservations are never deleted")

	open, _, err := reserveFor(t, s, tenantCtx(), doc.DocumentID, "p1", "k2", 7)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE upload_reservations SET expected_size_bytes = 999 WHERE reservation_id = $1`, open.ReservationID)
	require.Error(t, err, "what the caller reserved is immutable")
	_, err = pool.Exec(ctx, `UPDATE upload_reservations SET commit_status = 'COMMITTED' WHERE reservation_id = $1`, open.ReservationID)
	require.Error(t, err, "COMMITTED requires a committed version (CHECK)")
}
