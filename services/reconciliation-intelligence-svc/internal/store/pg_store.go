package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// appendResolutionNote combines the existing note (typically the machine's
// heuristic rationale, recorded when the item was generated) with a human's
// decision note. Neither PgStore nor MemoryStore's ApplyResolution may
// simply overwrite: the common case is a human approving with no note at
// all, and blank human input must not erase why the system made the
// recommendation in the first place.
func appendResolutionNote(existing, human string) string {
	if human == "" {
		return existing
	}
	if existing == "" {
		return human
	}
	return existing + " | decision: " + human
}

type Store interface {
	// CreateJob enqueues "reconciliation.analyzed" (and, when the job found
	// unmatched items, "reconciliation.unmatched_flagged") in the same
	// transaction as the insert.
	CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.ReconciliationSuggestion, actorID, correlationID string) error
	GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error)
	ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error)
	// ApplyResolution enqueues "reconciliation.resolution_recommended" in
	// the same transaction as the update.
	ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.SuggestionStatus, notes, legalEntityID, actorID, correlationID string) (*domain.ReconciliationSuggestion, error)
	ArchiveJob(ctx context.Context, tenantID, id string) error
}

type PgStore struct {
	pool *pgxpool.Pool

	// eventRegion is the residencyregion stamped on every reconciliation
	// event. Empty means no region was configured, and every event-emitting
	// write then fails (envelope.New refuses it) rather than emitting an
	// event with a guessed region.
	eventRegion string
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (config EVENT_RESIDENCY_REGION).
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func NewPgStore(pool *pgxpool.Pool, opts ...Option) *PgStore {
	s := &PgStore{pool: pool}
	for _, o := range opts {
		o(s)
	}
	return s
}

// enqueueReconciliationEvent writes one reconciliation lifecycle event to
// the transactional outbox inside tx, the transaction that made the change
// it reports (ZS-EVENT-001 §6).
func (s *PgStore) enqueueReconciliationEvent(ctx context.Context, tx pgx.Tx, fact, legacyType, subjectID, tenantID, legalEntityID, actorID, correlationID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.reconciliation.job." + fact,
		LegacyType:      legacyType,
		Service:         "reconciliation-intelligence-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   "reconciliation-job",
		AggregateID:     subjectID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		// Reconciliation findings are financial records of a tenant's books.
		Classification: envelope.Confidential,
		Data:           data,
	})
	if err != nil {
		return fmt.Errorf("build reconciliation.%s event: %w", fact, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue reconciliation.%s event: %w", fact, err)
	}
	return nil
}

func (s *PgStore) CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.ReconciliationSuggestion, actorID, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}

	job.ID = uuid.New().String()
	job.TenantID = tenantID
	job.AnalyzedAt = time.Now()
	job.CreatedAt = time.Now()

	// Empty optional values are stored as NULL, not '': a run-sourced job has
	// no second source system, and a legacy job has no run.
	queryJob := `
		INSERT INTO reconciliation_jobs (
			id, tenant_id, legal_entity_id, job_name, source_system_a, source_system_b,
			total_processed_count, matched_count, unmatched_count, reconciliation_rate, status, analyzed_at, created_at,
			bank_reconciliation_run_id
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12, $13, NULLIF($14, '')::uuid)`

	_, err = tx.Exec(ctx, queryJob,
		job.ID, tenantID, job.LegalEntityID, job.JobName, string(job.SourceSystemA), string(job.SourceSystemB),
		job.TotalProcessedCount, job.MatchedCount, job.UnmatchedCount, job.ReconciliationRate, job.Status,
		job.AnalyzedAt, job.CreatedAt, job.BankReconciliationRunID,
	)
	if err != nil {
		return fmt.Errorf("insert reconciliation_job failed: %w", err)
	}

	queryItem := `
		INSERT INTO reconciliation_unmatched_items (
			id, tenant_id, legal_entity_id, job_id, source_record_a, source_record_b, amount_a, amount_b,
			discrepancy_amount, discrepancy_type, confidence_score, heuristic_rule, tolerance_version,
			recommendation, suggestion_status, resolution_notes, created_at, updated_at,
			bank_reconciliation_run_id, statement_line_id, counterpart_statement_line_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric, $10, $11, $12, $13, $14, $15, $16, $17, $18,
			NULLIF($19, '')::uuid, NULLIF($20, '')::uuid, NULLIF($21, '')::uuid)`

	for i := range items {
		item := &items[i]
		item.ID = uuid.New().String()
		item.TenantID = tenantID
		// The suggestion carries its own legal entity (A9), not only its job.
		item.LegalEntityID = job.LegalEntityID
		item.JobID = job.ID
		item.CreatedAt = time.Now()
		item.UpdatedAt = time.Now()

		_, err := tx.Exec(ctx, queryItem,
			item.ID, tenantID, item.LegalEntityID, job.ID, item.SourceRecordA, item.SourceRecordB, item.AmountA, item.AmountB,
			item.DiscrepancyAmount, string(item.DiscrepancyType), item.ConfidenceScore,
			nullableHeuristicRule(item.HeuristicRule), nullableToleranceVersion(item.ToleranceVersion), string(item.Recommendation),
			string(item.SuggestionStatus), item.ResolutionNotes, item.CreatedAt, item.UpdatedAt,
			item.BankReconciliationRunID, item.StatementLineID, item.CounterpartStatementLineID,
		)
		if err != nil {
			return fmt.Errorf("insert reconciliation_unmatched_item failed: %w", err)
		}
	}

	job.UnmatchedItems = items

	if err := s.enqueueReconciliationEvent(ctx, tx, "analyzed", "reconciliation.analyzed",
		job.ID, tenantID, job.LegalEntityID, actorID, correlationID, map[string]any{
			"job_id":                     job.ID,
			"tenant_id":                  tenantID,
			"legal_entity_id":            job.LegalEntityID,
			"job_name":                   job.JobName,
			"bank_reconciliation_run_id": job.BankReconciliationRunID,
			"source_system_a":            string(job.SourceSystemA),
			"source_system_b":            string(job.SourceSystemB),
			"total_processed_count":      job.TotalProcessedCount,
			// matched_count / reconciliation_rate are null on a run-sourced
			// job: match state is not measured here (see
			// domain.ReconciliationJob).
			"matched_count":       job.MatchedCount,
			"unmatched_count":     job.UnmatchedCount,
			"reconciliation_rate": job.ReconciliationRate,
			"status":              job.Status,
		}); err != nil {
		return err
	}

	if job.UnmatchedCount > 0 {
		if err := s.enqueueReconciliationEvent(ctx, tx, "unmatched-flagged", "reconciliation.unmatched_flagged",
			job.ID, tenantID, job.LegalEntityID, actorID, correlationID, map[string]any{
				"job_id":          job.ID,
				"unmatched_count": job.UnmatchedCount,
				"rate":            job.ReconciliationRate,
			}); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var j domain.ReconciliationJob
	if err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM reconciliation_jobs WHERE id = $1`, id), &j); err != nil {
		return nil, fmt.Errorf("reconciliation job not found: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT `+itemColumns+` FROM reconciliation_unmatched_items WHERE job_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("query unmatched items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.ReconciliationSuggestion
		if err := scanItem(rows, &item); err != nil {
			return nil, fmt.Errorf("scan unmatched item: %w", err)
		}
		j.UnmatchedItems = append(j.UnmatchedItems, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unmatched items: %w", err)
	}

	return &j, nil
}

// Column lists shared by every read, so a new column cannot be added to one
// SELECT and missed in another. Nullable text/uuid columns are read as empty
// strings (legacy jobs have no run; run-sourced jobs have no second source
// system).
const jobColumns = `id, tenant_id, legal_entity_id, job_name, source_system_a, COALESCE(source_system_b, ''),
	total_processed_count, matched_count, unmatched_count, reconciliation_rate, status, analyzed_at, created_at,
	COALESCE(bank_reconciliation_run_id::text, '')`

const itemColumns = `id, tenant_id, legal_entity_id, job_id, source_record_a, source_record_b, amount_a, amount_b,
	discrepancy_amount, discrepancy_type, confidence_score, COALESCE(heuristic_rule, ''),
	COALESCE(tolerance_version, 0), recommendation, suggestion_status,
	COALESCE(resolution_notes, ''), created_at, updated_at,
	COALESCE(bank_reconciliation_run_id::text, ''), COALESCE(statement_line_id::text, ''),
	COALESCE(counterpart_statement_line_id::text, '')`

func scanJob(row pgx.Row, j *domain.ReconciliationJob) error {
	return row.Scan(
		&j.ID, &j.TenantID, &j.LegalEntityID, &j.JobName, &j.SourceSystemA, &j.SourceSystemB,
		&j.TotalProcessedCount, &j.MatchedCount, &j.UnmatchedCount, &j.ReconciliationRate, &j.Status, &j.AnalyzedAt, &j.CreatedAt,
		&j.BankReconciliationRunID,
	)
}

func scanItem(row pgx.Row, item *domain.ReconciliationSuggestion) error {
	if err := row.Scan(
		&item.ID, &item.TenantID, &item.LegalEntityID, &item.JobID, &item.SourceRecordA, &item.SourceRecordB,
		&item.AmountA, &item.AmountB, &item.DiscrepancyAmount, &item.DiscrepancyType, &item.ConfidenceScore,
		&item.HeuristicRule, &item.ToleranceVersion,
		&item.Recommendation, &item.SuggestionStatus, &item.ResolutionNotes, &item.CreatedAt, &item.UpdatedAt,
		&item.BankReconciliationRunID, &item.StatementLineID, &item.CounterpartStatementLineID,
	); err != nil {
		return err
	}
	return canonicalizeItemAmounts(item)
}

func (s *PgStore) ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	query := `SELECT ` + jobColumns + ` FROM reconciliation_jobs WHERE tenant_id = $1`

	args := []interface{}{tenantID}
	if legalEntityID != "" {
		args = append(args, legalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
	}
	if sourceA != "" {
		args = append(args, sourceA)
		query += fmt.Sprintf(" AND source_system_a = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY analyzed_at DESC"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.ReconciliationJob
	for rows.Next() {
		var j domain.ReconciliationJob
		if err := scanJob(rows, &j); err != nil {
			return nil, fmt.Errorf("scan reconciliation job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reconciliation jobs: %w", err)
	}

	return jobs, nil
}

func (s *PgStore) ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.SuggestionStatus, notes, legalEntityID, actorID, correlationID string) (*domain.ReconciliationSuggestion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var item domain.ReconciliationSuggestion
	queryItem := `SELECT ` + itemColumns + `
		FROM reconciliation_unmatched_items WHERE id = $1 AND job_id = $2
		FOR UPDATE`

	if err := scanItem(tx.QueryRow(ctx, queryItem, itemID, jobID), &item); err != nil {
		return nil, fmt.Errorf("unmatched item not found: %w", err)
	}
	if err := domain.ValidateResolutionTransition(item.SuggestionStatus, status); err != nil {
		return nil, err
	}

	// Append rather than overwrite. item.ResolutionNotes at this point still
	// holds whatever AnalyzeUnmatchedStatementLines recorded as the
	// machine's rationale for the recommendation (e.g. "heuristic rule:
	// discrepancy $32.00 is below the $50.00 write-off tolerance"). A human
	// applying a decision without typing their own note — the common case —
	// must not silently erase that rationale; it is the only record of why
	// the system proposed this in the first place, and it is most valuable
	// at exactly the moment a decision is made.
	combinedNotes := appendResolutionNote(item.ResolutionNotes, notes)

	item.SuggestionStatus = status
	item.ResolutionNotes = combinedNotes
	item.UpdatedAt = time.Now()

	updateQuery := `UPDATE reconciliation_unmatched_items SET suggestion_status = $1, resolution_notes = $2, updated_at = $3 WHERE id = $4`
	_, err = tx.Exec(ctx, updateQuery, string(status), combinedNotes, item.UpdatedAt, itemID)
	if err != nil {
		return nil, err
	}

	if err := s.enqueueReconciliationEvent(ctx, tx, "resolution-recommended", "reconciliation.resolution_recommended",
		itemID, tenantID, legalEntityID, actorID, correlationID, map[string]any{
			"item_id": item.ID,
			"job_id":  item.JobID,
			// The authoritative record the decision is about; "" on items
			// from jobs analyzed before migration 000003.
			"bank_reconciliation_run_id":    item.BankReconciliationRunID,
			"statement_line_id":             item.StatementLineID,
			"counterpart_statement_line_id": item.CounterpartStatementLineID,
			// suggestion_status is canonical; resolution_status is the
			// pre-rename alias kept for one release (A9/B5). Both carry the
			// same value, so they cannot disagree.
			"suggestion_status": string(item.SuggestionStatus),
			"resolution_status": string(item.SuggestionStatus),
			"resolution_notes":  item.ResolutionNotes,
		}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *PgStore) ArchiveJob(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}

	cmd, err := tx.Exec(ctx, "UPDATE reconciliation_jobs SET status = 'ARCHIVED' WHERE id = $1", id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("reconciliation job not found")
	}

	return tx.Commit(ctx)
}

// nullableHeuristicRule stores an unset rule as NULL. That is only possible
// for rows written before migration 000005 or for an item a test constructs
// by hand; a produced suggestion always names its rule. NULL keeps "not
// recorded" distinct from an empty rule name.
func nullableHeuristicRule(rule domain.HeuristicRule) any {
	if rule == "" {
		return nil
	}
	return string(rule)
}

// nullableToleranceVersion stores an unset version as NULL for the same
// reason: 0 is not a policy version that was ever approved.
func nullableToleranceVersion(version int) any {
	if version == 0 {
		return nil
	}
	return version
}

// canonicalizeItemAmounts rewrites the NUMERIC(18,4) text Postgres returns
// ("32.0000") into the canonical decimal form the analyze response used
// ("32"), so one amount reads the same however it is fetched.
func canonicalizeItemAmounts(item *domain.ReconciliationSuggestion) error {
	for _, f := range []*string{&item.AmountA, &item.AmountB, &item.DiscrepancyAmount} {
		r, err := domain.ParseAmount(*f)
		if err != nil {
			return fmt.Errorf("stored amount on item %s: %w", item.ID, err)
		}
		*f = domain.CanonicalAmount(r)
	}
	return nil
}

// MemoryStore is a test double for handler tests only. cmd/server never
// serves from it: an unreachable database is a startup failure, not a
// reason to hold human decisions in process memory.
type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]*domain.ReconciliationJob
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		jobs: make(map[string]*domain.ReconciliationJob),
	}
}

// CreateJob never emits an event: MemoryStore is a test double with no
// transaction to enqueue an outbox row into.
func (m *MemoryStore) CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.ReconciliationSuggestion, actorID, correlationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job.ID = uuid.New().String()
	job.TenantID = tenantID
	job.AnalyzedAt = time.Now()
	job.CreatedAt = time.Now()

	for i := range items {
		items[i].ID = uuid.New().String()
		items[i].TenantID = tenantID
		items[i].LegalEntityID = job.LegalEntityID
		items[i].JobID = job.ID
		items[i].CreatedAt = time.Now()
		items[i].UpdatedAt = time.Now()
	}

	job.UnmatchedItems = items
	m.jobs[job.ID] = job
	return nil
}

func (m *MemoryStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	j, exists := m.jobs[id]
	if !exists || j.TenantID != tenantID {
		return nil, fmt.Errorf("reconciliation job not found")
	}
	return j, nil
}

func (m *MemoryStore) ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []domain.ReconciliationJob
	for _, j := range m.jobs {
		if j.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && j.LegalEntityID != legalEntityID {
			continue
		}
		if sourceA != "" && string(j.SourceSystemA) != sourceA {
			continue
		}
		if status != "" && j.Status != status {
			continue
		}
		result = append(result, *j)
	}
	return result, nil
}

func (m *MemoryStore) ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.SuggestionStatus, notes, legalEntityID, actorID, correlationID string) (*domain.ReconciliationSuggestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, exists := m.jobs[jobID]
	if !exists || j.TenantID != tenantID {
		return nil, fmt.Errorf("reconciliation job not found")
	}

	for i := range j.UnmatchedItems {
		if j.UnmatchedItems[i].ID == itemID {
			if err := domain.ValidateResolutionTransition(j.UnmatchedItems[i].SuggestionStatus, status); err != nil {
				return nil, err
			}
			// Append rather than overwrite — mirrors PgStore exactly. See its
			// comment: the machine's rationale must survive a human's
			// empty-note approval.
			j.UnmatchedItems[i].SuggestionStatus = status
			j.UnmatchedItems[i].ResolutionNotes = appendResolutionNote(j.UnmatchedItems[i].ResolutionNotes, notes)
			j.UnmatchedItems[i].UpdatedAt = time.Now()
			return &j.UnmatchedItems[i], nil
		}
	}

	return nil, fmt.Errorf("unmatched item not found")
}

func (m *MemoryStore) ArchiveJob(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, exists := m.jobs[id]
	if !exists || j.TenantID != tenantID {
		return fmt.Errorf("reconciliation job not found")
	}

	j.Status = "ARCHIVED"
	return nil
}
