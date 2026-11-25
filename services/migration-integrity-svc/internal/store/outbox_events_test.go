//go:build integration

// Proof that migration-integrity-svc's evidence events reach the transactional
// outbox inside the transaction that makes the change they report, carry the
// exact pre-standard event_type consumers filter on, are not duplicated by a
// replayed request, and are actually deliverable by the relay under the
// RLS-respecting application role.
package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/eventing/outbox"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/store"
)

const (
	typeValidated  = "com.zoikosuite.migration.job.integrity-validated"
	typeViolations = "com.zoikosuite.migration.job.integrity-violations-detected"
	typeArchived   = "com.zoikosuite.migration.job.archived"
	typeRemediated = "com.zoikosuite.migration.audit-entry.remediated"
)

type outboxRow struct {
	eventType string
	payload   string
	state     string
}

// outboxRows reads through the superuser pool, so what it sees does not depend
// on the policy under test.
func outboxRows(t *testing.T, tenantID, aggregateID string) []outboxRow {
	t.Helper()
	rows, err := superPool.Query(context.Background(), `
		SELECT event_type, convert_from(payload, 'UTF8'), publish_state
		  FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2
		 ORDER BY created_at, outbox_id`, tenantID, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.eventType, &r.payload, &r.state))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func newStore() *store.PgStore {
	return store.NewPgStore(appPool, zap.NewNop(), store.WithEventRegion("uk"))
}

// createJob persists a job with invalidCount violating audit entries and
// returns it with the entry ids the store assigned.
func createJob(t *testing.T, s *store.PgStore, tenantID string, invalidCount int) *domain.MigrationJob {
	t.Helper()
	now := time.Now().UTC()
	status := domain.JobStatusCompleted
	job := &domain.MigrationJob{
		ID: uuid.NewString(), LegalEntityID: "le-uk-1", MigrationName: "fy26 ledger cutover",
		SourceSystem: "LEGACY_ERP", TargetService: "general-ledger-svc",
		TotalRecordsCount: 10, ValidRecordsCount: 10 - invalidCount, InvalidRecordsCount: invalidCount,
		IntegrityScore: float64(10-invalidCount) * 10, IntegrityScoreThreshold: 95,
		Status: status, StartedAt: &now, CompletedAt: &now,
	}
	var entries []domain.AuditEntry
	for i := 0; i < invalidCount; i++ {
		entries = append(entries, domain.AuditEntry{RecordRef: "rec-" + uuid.NewString()[:8], FieldName: "amount", ViolationType: domain.ViolationMissingRequired,
			Severity: domain.SeverityMQ2, ReasonFamily: domain.ReasonSchema, OwnerID: "owner-1"})
	}
	require.NoError(t, s.CreateJob(context.Background(), tenantID, job, nil, entries, "principal-1", "corr-1", store.IdempotencyClaim{}))
	return job
}

func TestPgStore_CreateJob_EnqueuesIntegrityValidated(t *testing.T) {
	tenantID := uuid.NewString()
	job := createJob(t, newStore(), tenantID, 0)

	rows := outboxRows(t, tenantID, job.ID)
	require.Len(t, rows, 1, "a clean job emits integrity_validated only")
	assert.Equal(t, typeValidated, rows[0].eventType)
	p := rows[0].payload
	assert.Contains(t, p, `"event_type":"migration.integrity_validated"`)
	assert.Contains(t, p, `"subject":"urn:zoikosuite:migration-job:`+job.ID+`"`)
	assert.Contains(t, p, `"legal_entity_id":"le-uk-1"`)
	assert.Contains(t, p, `"actor_id":"principal-1"`)
	assert.Contains(t, p, `"correlation_id":"corr-1"`)
	assert.Contains(t, p, `"integrity_score":100`)
	assert.Contains(t, p, `"status":"COMPLETED"`)
}

func TestPgStore_CreateJob_WithViolations_EnqueuesBothInOrder(t *testing.T) {
	tenantID := uuid.NewString()
	job := createJob(t, newStore(), tenantID, 3)

	rows := outboxRows(t, tenantID, job.ID)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{typeValidated, typeViolations}, []string{rows[0].eventType, rows[1].eventType},
		"same order the handler used to publish in")
	assert.Contains(t, rows[1].payload, `"event_type":"migration.integrity_violations_detected"`)
	assert.Contains(t, rows[1].payload, `"invalid_count":3`)
}

// The rewrite replaced "a second archive silently succeeds" with explicit
// replay protection: the same Idempotency-Key is refused inside the
// transaction (the handler then replays the stored response), and a second
// archive under a new key is an illegal transition. Either way exactly one
// job_archived event exists.
func TestPgStore_ArchiveJob_EnqueuesOnce_ReplayEmitsNothing(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	tenantID := uuid.NewString()
	job := createJob(t, s, tenantID, 0)
	claim := store.IdempotencyClaim{Key: uuid.NewString(), PrincipalID: "principal-2", Operation: "archive_job", RequestHash: "h-archive"}

	archived, err := s.ArchiveJob(ctx, tenantID, store.ArchiveCommand{JobID: job.ID, ExpectedVersion: job.Version, ActorID: "principal-2", CorrelationID: "corr-2"}, claim)
	require.NoError(t, err)
	assert.Equal(t, domain.JobStatusArchived, archived.Status)
	assert.Equal(t, job.Version+1, archived.Version)

	_, err = s.ArchiveJob(ctx, tenantID, store.ArchiveCommand{JobID: job.ID, ActorID: "principal-2", CorrelationID: "corr-3"}, claim)
	assert.ErrorIs(t, err, store.ErrIdempotencyKeyInUse, "a replayed key never applies twice")
	stored, err := s.FindIdempotentResponse(ctx, tenantID, "principal-2", claim.Key)
	require.NoError(t, err)
	require.NotNil(t, stored, "the first response is kept for replay")
	assert.Equal(t, 200, stored.Status)
	assert.Contains(t, string(stored.Body), `"status":"ARCHIVED"`)

	_, err = s.ArchiveJob(ctx, tenantID, store.ArchiveCommand{JobID: job.ID, ActorID: "principal-2", CorrelationID: "corr-4"}, store.IdempotencyClaim{})
	assert.ErrorIs(t, err, domain.ErrJobAlreadyArchived)

	var archivedRows []outboxRow
	for _, r := range outboxRows(t, tenantID, job.ID) {
		if r.eventType == typeArchived {
			archivedRows = append(archivedRows, r)
		}
	}
	require.Len(t, archivedRows, 1, "a replayed archive must not record a second job_archived")
	assert.Contains(t, archivedRows[0].payload, `"event_type":"migration.job_archived"`)
	assert.Contains(t, archivedRows[0].payload, `"actor_id":"principal-2"`)
	assert.Contains(t, archivedRows[0].payload, `"correlation_id":"corr-2"`)
	assert.Contains(t, archivedRows[0].payload, `"legal_entity_id":"le-uk-1"`)

	_, err = s.ArchiveJob(ctx, tenantID, store.ArchiveCommand{JobID: job.ID, ExpectedVersion: 1, ActorID: "p", CorrelationID: "c"}, store.IdempotencyClaim{})
	assert.ErrorIs(t, err, store.ErrVersionMismatch, "a stale If-Match is refused before the state rule")
}

func remediate(s *store.PgStore, tenantID, jobID, entryID, reason, evidence, actor, corr string, claim store.IdempotencyClaim) (*domain.AuditEntry, error) {
	entry, _, err := s.DispositionEntry(context.Background(), tenantID, store.DispositionCommand{
		JobID: jobID, EntryID: entryID, TargetStatus: domain.EntryStatusRemediated,
		Reason: reason, EvidenceRef: evidence, ActorID: actor, CorrelationID: corr,
	}, claim)
	return entry, err
}

// Ported from the pre-rewrite RemediateEntry test: the decision and its
// evidence are recorded once; a replay (same key) or a second decision (new
// key) can neither emit a second event nor rewrite the evidence.
func TestPgStore_RemediateEntry_EnqueuesOnEntry_ReplayEmitsNothing(t *testing.T) {
	s := newStore()
	tenantID := uuid.NewString()
	job := createJob(t, s, tenantID, 1)
	entryID := job.AuditEntries[0].ID
	claim := store.IdempotencyClaim{Key: uuid.NewString(), PrincipalID: "principal-3", Operation: "remediate_entry", RequestHash: "h-rem"}

	got, err := remediate(s, tenantID, job.ID, entryID, "source corrected and reloaded", "ticket-MIG-42", "principal-3", "corr-4", claim)
	require.NoError(t, err)
	assert.True(t, got.IsRemediated)
	assert.Equal(t, domain.EntryStatusRemediated, got.Status)
	assert.Equal(t, "source corrected and reloaded", got.RemediationReason)
	assert.Equal(t, "ticket-MIG-42", got.RemediationEvidenceRef)
	assert.Equal(t, "principal-3", got.RemediatedBy)
	require.NotNil(t, got.RemediatedAt)

	_, err = remediate(s, tenantID, job.ID, entryID, "a later, different story", "ticket-OTHER", "principal-3", "corr-5", claim)
	assert.ErrorIs(t, err, store.ErrIdempotencyKeyInUse, "same key: never applied twice")
	_, err = remediate(s, tenantID, job.ID, entryID, "a later, different story", "ticket-OTHER", "principal-9", "corr-6", store.IdempotencyClaim{})
	assert.ErrorIs(t, err, domain.ErrEntryAlreadyDecided, "a decision is immutable")

	rows := outboxRows(t, tenantID, entryID)
	require.Len(t, rows, 1, "aggregate is the entry (the old subject_id); a replay must not record a second remediation")
	assert.Equal(t, typeRemediated, rows[0].eventType)
	p := rows[0].payload
	assert.Contains(t, p, `"event_type":"migration.audit_entry_remediated"`)
	assert.Contains(t, p, `"subject":"urn:zoikosuite:migration-audit-entry:`+entryID+`"`)
	assert.Contains(t, p, `"legal_entity_id":"le-uk-1"`)
	assert.Contains(t, p, `"is_remediated":true`)
	assert.Contains(t, p, `"job_id":"`+job.ID+`"`)
	assert.Contains(t, p, `"remediation_reason":"source corrected and reloaded"`)
	assert.Contains(t, p, `"remediation_evidence_ref":"ticket-MIG-42"`)
	assert.Contains(t, p, `"remediated_by":"principal-3"`)
	assert.Contains(t, p, `"remediated_at":`)

	// The evidence survives a read of the job, and the job's version moved.
	read, err := s.GetJobByID(context.Background(), tenantID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.Version+1, read.Version)
	var found bool
	for _, e := range read.AuditEntries {
		if e.ID == entryID {
			found = true
			assert.Equal(t, "source corrected and reloaded", e.RemediationReason)
			assert.Equal(t, "ticket-MIG-42", e.RemediationEvidenceRef)
			assert.Equal(t, "principal-3", e.RemediatedBy)
			require.NotNil(t, e.RemediatedAt)
		}
	}
	assert.True(t, found, "the remediated entry must still be returned by the read")
}

func TestPgStore_RemediateEntry_OtherTenant_RefusedAndEmitsNothing(t *testing.T) {
	s := newStore()
	tenantID := uuid.NewString()
	job := createJob(t, s, tenantID, 1)
	entryID := job.AuditEntries[0].ID

	_, err := remediate(s, uuid.NewString(), job.ID, entryID, "r", "e", "intruder", "corr-x", store.IdempotencyClaim{})
	assert.ErrorIs(t, err, store.ErrJobNotFound, "another tenant's job is indistinguishable from a missing one")
	assert.Empty(t, outboxRows(t, tenantID, entryID))

	_, err = s.GetJobByID(context.Background(), uuid.NewString(), job.ID)
	assert.ErrorIs(t, err, store.ErrJobNotFound)
}

type capturingWriter struct {
	mu   sync.Mutex
	msgs []outbox.Message
}

func (w *capturingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msgs...)
	return nil
}

func (w *capturingWriter) Probe(context.Context) error { return nil }

// TestRelay_DeliversUnderAppRole is the end-to-end check: the relay, connected
// as the same non-superuser NOBYPASSRLS role the service runs as, must be able
// to claim and publish what the store enqueued. A tenant-only RLS policy on
// eventing_outbox passes every other test here and fails this one.
func TestRelay_DeliversUnderAppRole(t *testing.T) {
	tenantID := uuid.NewString()
	job := createJob(t, newStore(), tenantID, 0)

	w := &capturingWriter{}
	relay, err := outbox.NewRelay(appPool, w, outbox.DefaultConfig(), zap.NewNop())
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		res, err := relay.DrainOnce(context.Background())
		require.NoError(t, err)
		if res.Claimed == 0 {
			break
		}
	}

	var delivered []string
	for _, m := range w.msgs {
		if strings.Contains(string(m.Value), `"subject":"urn:zoikosuite:migration-job:`+job.ID+`"`) {
			delivered = append(delivered, string(m.Value))
		}
	}
	require.Len(t, delivered, 1, "the relay must deliver the job's event under the app role")
	assert.Contains(t, delivered[0], `"event_type":"migration.integrity_validated"`)
	rows := outboxRows(t, tenantID, job.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "published", rows[0].state)
}

// TestOutboxRLS_TenantCannotSeeAnotherTenantsEvents proves the relay exception
// did not open the table to ordinary request-path reads.
func TestOutboxRLS_TenantCannotSeeAnotherTenantsEvents(t *testing.T) {
	tenantID := uuid.NewString()
	job := createJob(t, newStore(), tenantID, 0)

	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.NewString())
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1`, job.ID).Scan(&n))
	assert.Zero(t, n)
}
