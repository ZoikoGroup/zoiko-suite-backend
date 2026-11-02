package store

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// ─── MemoryStore ──────────────────────────────────────────────────────────────

// MemoryStore is a handler-test double only. It has no outbox, so it must
// never back the running service: evidence and its events would both be
// lost. It mirrors PgStore's semantics — tenant isolation, state machine,
// versions, idempotent replay — so handler tests exercise the real rules.
type MemoryStore struct {
	mu          sync.Mutex
	jobs        map[string]*domain.MigrationJob
	idempotency map[string]StoredResponse
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]*domain.MigrationJob{}, idempotency: map[string]StoredResponse{}}
}

func idempotencyMapKey(tenantID, principalID, key string) string {
	return tenantID + "\x00" + principalID + "\x00" + key
}

// keyTaken reports whether the claim's key already completed a request
// (PgStore's claim step: a concurrent request committed first).
func (m *MemoryStore) keyTaken(tenantID string, claim IdempotencyClaim) bool {
	if claim.IsZero() {
		return false
	}
	_, taken := m.idempotency[idempotencyMapKey(tenantID, claim.PrincipalID, claim.Key)]
	return taken
}

// remember stores a response for replay. Callers check keyTaken first,
// before changing any state.
func (m *MemoryStore) remember(tenantID string, claim IdempotencyClaim, status int, response any) error {
	if claim.IsZero() {
		return nil
	}
	mapKey := idempotencyMapKey(tenantID, claim.PrincipalID, claim.Key)
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	m.idempotency[mapKey] = StoredResponse{Operation: claim.Operation, RequestHash: claim.RequestHash, Status: status, Body: body}
	return nil
}

// FindIdempotentResponse implements Store.
func (m *MemoryStore) FindIdempotentResponse(_ context.Context, tenantID, principalID, key string) (*StoredResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.idempotency[idempotencyMapKey(tenantID, principalID, key)]
	if !ok {
		return nil, nil
	}
	return &stored, nil
}

// CreateJob implements Store.
func (m *MemoryStore) CreateJob(_ context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry, actorID, _ string, claim IdempotencyClaim) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyTaken(tenantID, claim) {
		return ErrIdempotencyKeyInUse
	}
	prepareNewJob(job, tenantID, actorID, checks, entries, time.Now().UTC())
	if err := m.remember(tenantID, claim, http.StatusCreated, job); err != nil {
		return err
	}
	m.jobs[job.ID] = deepCopyJob(job)
	return nil
}

// GetJobByID implements Store.
func (m *MemoryStore) GetJobByID(_ context.Context, tenantID, id string) (*domain.MigrationJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.TenantID != tenantID {
		return nil, ErrJobNotFound
	}
	copied := deepCopyJob(job)
	copied.OpenQuarantineCount = domain.CountOpenEntries(copied.AuditEntries)
	return copied, nil
}

// ListJobs implements Store: summaries only (no checks, no entries).
func (m *MemoryStore) ListJobs(_ context.Context, tenantID string, filter ListFilter) ([]domain.MigrationJob, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	matching := []domain.MigrationJob{}
	for _, job := range m.jobs {
		if job.TenantID != tenantID || job.LegalEntityID != filter.LegalEntityID {
			continue
		}
		if filter.Status != "" && string(job.Status) != filter.Status {
			continue
		}
		summary := *job
		summary.OpenQuarantineCount = domain.CountOpenEntries(job.AuditEntries)
		summary.IntegrityChecks = nil
		summary.AuditEntries = nil
		matching = append(matching, summary)
	}
	sort.Slice(matching, func(a, b int) bool {
		if matching[a].CreatedAt.Equal(matching[b].CreatedAt) {
			return matching[a].ID < matching[b].ID
		}
		return matching[a].CreatedAt.After(matching[b].CreatedAt)
	})
	if filter.Offset >= len(matching) {
		return []domain.MigrationJob{}, false, nil
	}
	page := matching[filter.Offset:]
	hasMore := len(page) > filter.Limit
	if hasMore {
		page = page[:filter.Limit]
	}
	return page, hasMore, nil
}

// ArchiveJob implements Store.
func (m *MemoryStore) ArchiveJob(_ context.Context, tenantID string, cmd ArchiveCommand, claim IdempotencyClaim) (*domain.MigrationJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyTaken(tenantID, claim) {
		return nil, ErrIdempotencyKeyInUse
	}
	job, ok := m.jobs[cmd.JobID]
	if !ok || job.TenantID != tenantID {
		return nil, ErrJobNotFound
	}
	if cmd.ExpectedVersion != 0 && cmd.ExpectedVersion != job.Version {
		return nil, ErrVersionMismatch
	}
	if err := domain.CanArchive(job.Status); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	job.Status = domain.JobStatusArchived
	job.ArchivedAt = &now
	job.ArchivedBy = cmd.ActorID
	job.Version++
	job.UpdatedAt = now

	summary := *job
	summary.OpenQuarantineCount = domain.CountOpenEntries(job.AuditEntries)
	summary.IntegrityChecks = nil
	summary.AuditEntries = nil
	if err := m.remember(tenantID, claim, http.StatusOK, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

// DispositionEntry implements Store.
func (m *MemoryStore) DispositionEntry(_ context.Context, tenantID string, cmd DispositionCommand, claim IdempotencyClaim) (*domain.AuditEntry, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyTaken(tenantID, claim) {
		return nil, 0, ErrIdempotencyKeyInUse
	}
	job, ok := m.jobs[cmd.JobID]
	if !ok || job.TenantID != tenantID {
		return nil, 0, ErrJobNotFound
	}
	if cmd.ExpectedVersion != 0 && cmd.ExpectedVersion != job.Version {
		return nil, 0, ErrVersionMismatch
	}
	entry := job.FindEntry(cmd.EntryID)
	if entry == nil {
		return nil, 0, ErrEntryNotFound
	}
	if err := domain.CanDisposition(job.Status, entry.Status); err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	applyDecision(entry, cmd, now)
	job.Version++
	job.UpdatedAt = now

	decided := *entry
	if err := m.remember(tenantID, claim, http.StatusOK, &decided); err != nil {
		return nil, 0, err
	}
	return &decided, job.Version, nil
}

// deepCopyJob copies the slices so callers cannot mutate stored state.
func deepCopyJob(job *domain.MigrationJob) *domain.MigrationJob {
	copied := *job
	copied.IntegrityChecks = append([]domain.IntegrityCheck(nil), job.IntegrityChecks...)
	copied.AuditEntries = append([]domain.AuditEntry(nil), job.AuditEntries...)
	if job.ControlTotals != nil {
		totals := *job.ControlTotals
		copied.ControlTotals = &totals
	}
	return &copied
}
