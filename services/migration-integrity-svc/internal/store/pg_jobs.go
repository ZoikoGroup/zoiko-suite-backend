package store

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"zoiko.io/migration-integrity-svc/internal/domain"
)

// ─── Column lists and scanners ────────────────────────────────────────────────

// jobColumns is the one SELECT list for a job row, shared by get, list and
// the row-locking reads, so every reader scans the same shape (scanJob).
// NUMERIC amounts are read as ::text — never through float64.
const jobColumns = `
	j.id, j.tenant_id, j.legal_entity_id, j.migration_name, j.source_system, j.target_service,
	COALESCE(j.requested_by, ''),
	j.total_records_count, j.valid_records_count, j.invalid_records_count,
	j.integrity_score::float8, j.integrity_score_threshold::float8, j.blocker_count,
	j.status, j.version, j.started_at, j.completed_at, j.archived_at, COALESCE(j.archived_by, ''),
	j.created_at, j.updated_at,
	COALESCE(j.control_amount_field, ''), j.source_record_count, j.target_record_count,
	j.source_amount_total::text, j.target_amount_total::text,
	COALESCE(j.source_control_hash, ''), COALESCE(j.target_control_hash, ''), j.control_totals_matched,
	(SELECT COUNT(*) FROM migration_audit_entries open_entry
	  WHERE open_entry.job_id = j.id AND open_entry.tenant_id = j.tenant_id
	    AND open_entry.status = 'QUARANTINED')`

func scanJob(row pgx.Row) (*domain.MigrationJob, error) {
	var job domain.MigrationJob
	var amountField, sourceHash, targetHash string
	var sourceCount, targetCount *int
	var sourceAmount, targetAmount *string
	var matched *bool
	err := row.Scan(
		&job.ID, &job.TenantID, &job.LegalEntityID, &job.MigrationName, &job.SourceSystem, &job.TargetService,
		&job.RequestedBy,
		&job.TotalRecordsCount, &job.ValidRecordsCount, &job.InvalidRecordsCount,
		&job.IntegrityScore, &job.IntegrityScoreThreshold, &job.BlockerCount,
		&job.Status, &job.Version, &job.StartedAt, &job.CompletedAt, &job.ArchivedAt, &job.ArchivedBy,
		&job.CreatedAt, &job.UpdatedAt,
		&amountField, &sourceCount, &targetCount,
		&sourceAmount, &targetAmount,
		&sourceHash, &targetHash, &matched,
		&job.OpenQuarantineCount,
	)
	if err != nil {
		return nil, err
	}
	// Jobs recorded before control totals existed have none; they read as
	// "no control_totals" rather than as a fabricated zero reconciliation.
	if sourceCount != nil && targetCount != nil && sourceAmount != nil && targetAmount != nil && matched != nil {
		job.ControlTotals = &domain.ControlTotals{
			AmountField:       amountField,
			SourceRecordCount: *sourceCount,
			TargetRecordCount: *targetCount,
			SourceAmountTotal: canonicalStoredAmount(*sourceAmount),
			TargetAmountTotal: canonicalStoredAmount(*targetAmount),
			SourceControlHash: sourceHash,
			TargetControlHash: targetHash,
			Matched:           *matched,
		}
	}
	return &job, nil
}

// canonicalStoredAmount renders a NUMERIC(28,6) text value ("300.250000")
// the way the API renders amounts ("300.25").
func canonicalStoredAmount(text string) string {
	value, ok := domain.ParseDecimal(text)
	if !ok {
		return text
	}
	return domain.FormatDecimal(value)
}

const checkColumns = `id, tenant_id, job_id, check_name, check_type, records_checked, records_passed,
	records_failed, severity, COALESCE(detail, ''), created_at`

const entryColumns = `id, tenant_id, job_id, record_ref, COALESCE(field_name, ''), COALESCE(source_value, ''),
	COALESCE(target_value, ''), violation_type, status, is_remediated, severity, reason_family,
	COALESCE(owner_id, ''), COALESCE(remediation_reason, ''), COALESCE(remediation_evidence_ref, ''),
	COALESCE(remediated_by, ''), remediated_at, waiver_expires_at, created_at`

func scanEntry(row pgx.Row) (*domain.AuditEntry, error) {
	var e domain.AuditEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.JobID, &e.RecordRef, &e.FieldName, &e.SourceValue,
		&e.TargetValue, &e.ViolationType, &e.Status, &e.IsRemediated, &e.Severity, &e.ReasonFamily,
		&e.OwnerID, &e.RemediationReason, &e.RemediationEvidenceRef,
		&e.RemediatedBy, &e.RemediatedAt, &e.WaiverExpiresAt, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ─── CreateJob ────────────────────────────────────────────────────────────────

// CreateJob implements Store. Steps: claim key → insert job → insert checks →
// insert entries → enqueue integrity_validated (+ violations_detected when
// anything was quarantined) → record response.
func (s *PgStore) CreateJob(ctx context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry, actorID, correlationID string, claim IdempotencyClaim) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, tenantID, claim); err != nil {
			return err
		}

		now := time.Now().UTC()
		prepareNewJob(job, tenantID, actorID, checks, entries, now)

		if err := insertJob(ctx, tx, job); err != nil {
			return err
		}
		for i := range job.IntegrityChecks {
			if err := insertCheck(ctx, tx, &job.IntegrityChecks[i]); err != nil {
				return err
			}
		}
		for i := range job.AuditEntries {
			if err := insertEntry(ctx, tx, &job.AuditEntries[i]); err != nil {
				return err
			}
		}

		if err := s.enqueueJobValidatedEvents(ctx, tx, tenantID, actorID, correlationID, job); err != nil {
			return err
		}
		return completeIdempotencyKey(ctx, tx, tenantID, claimWithStatus(claim, http.StatusCreated), job)
	})
}

// prepareNewJob assigns ids, tenant, timestamps and initial state to a job
// and its children, so the response and the stored rows are identical.
func prepareNewJob(job *domain.MigrationJob, tenantID, actorID string, checks []domain.IntegrityCheck, entries []domain.AuditEntry, now time.Time) {
	job.TenantID = tenantID
	if job.RequestedBy == "" {
		job.RequestedBy = actorID
	}
	job.Version = 1
	job.CreatedAt = now
	job.UpdatedAt = now
	for i := range checks {
		checks[i].ID = uuid.NewString()
		checks[i].TenantID = tenantID
		checks[i].JobID = job.ID
		checks[i].CreatedAt = now
	}
	for i := range entries {
		entries[i].ID = uuid.NewString()
		entries[i].TenantID = tenantID
		entries[i].JobID = job.ID
		entries[i].CreatedAt = now
		if entries[i].Status == "" {
			entries[i].Status = domain.EntryStatusQuarantined
		}
		entries[i].IsRemediated = entries[i].Status == domain.EntryStatusRemediated
	}
	job.IntegrityChecks = checks
	job.AuditEntries = entries
	job.OpenQuarantineCount = domain.CountOpenEntries(entries)
}

func insertJob(ctx context.Context, tx pgx.Tx, job *domain.MigrationJob) error {
	var amountField, sourceAmount, targetAmount, sourceHash, targetHash *string
	var sourceCount, targetCount *int
	var matched *bool
	if totals := job.ControlTotals; totals != nil {
		amountField, sourceCount, targetCount = &totals.AmountField, &totals.SourceRecordCount, &totals.TargetRecordCount
		sourceAmount, targetAmount = &totals.SourceAmountTotal, &totals.TargetAmountTotal
		sourceHash, targetHash, matched = nullIfEmpty(totals.SourceControlHash), &totals.TargetControlHash, &totals.Matched
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO migration_jobs
		  (id, tenant_id, legal_entity_id, migration_name, source_system, target_service, requested_by,
		   total_records_count, valid_records_count, invalid_records_count, integrity_score,
		   integrity_score_threshold, blocker_count, status, version, started_at, completed_at,
		   created_at, updated_at,
		   control_amount_field, source_record_count, target_record_count,
		   source_amount_total, target_amount_total, source_control_hash, target_control_hash,
		   control_totals_matched)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
		        $20,$21,$22,$23::numeric,$24::numeric,$25,$26,$27)`,
		job.ID, job.TenantID, job.LegalEntityID, job.MigrationName, job.SourceSystem, job.TargetService, job.RequestedBy,
		job.TotalRecordsCount, job.ValidRecordsCount, job.InvalidRecordsCount, job.IntegrityScore,
		job.IntegrityScoreThreshold, job.BlockerCount, string(job.Status), job.Version, job.StartedAt, job.CompletedAt,
		job.CreatedAt, job.UpdatedAt,
		amountField, sourceCount, targetCount, sourceAmount, targetAmount, sourceHash, targetHash, matched,
	)
	if err != nil {
		return fmt.Errorf("insert migration_job: %w", err)
	}
	return nil
}

func insertCheck(ctx context.Context, tx pgx.Tx, c *domain.IntegrityCheck) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO migration_integrity_checks
		  (id, tenant_id, job_id, check_name, check_type, records_checked, records_passed, records_failed,
		   severity, detail, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		c.ID, c.TenantID, c.JobID, c.CheckName, string(c.CheckType), c.RecordsChecked, c.RecordsPassed,
		c.RecordsFailed, string(c.Severity), c.Detail, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert integrity_check: %w", err)
	}
	return nil
}

func insertEntry(ctx context.Context, tx pgx.Tx, e *domain.AuditEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO migration_audit_entries
		  (id, tenant_id, job_id, record_ref, field_name, source_value, target_value, violation_type,
		   status, is_remediated, severity, reason_family, owner_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14)`,
		e.ID, e.TenantID, e.JobID, e.RecordRef, e.FieldName, e.SourceValue, e.TargetValue,
		string(e.ViolationType), string(e.Status), e.IsRemediated, string(e.Severity), string(e.ReasonFamily),
		e.OwnerID, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert audit_entry: %w", err)
	}
	return nil
}

func (s *PgStore) enqueueJobValidatedEvents(ctx context.Context, tx pgx.Tx, tenantID, actorID, correlationID string, job *domain.MigrationJob) error {
	controlTotalsMatched := job.ControlTotals == nil || job.ControlTotals.Matched
	validated := migrationEvent{
		TypeSegment: "job", Fact: "integrity-validated", LegacyType: "migration.integrity_validated",
		AggregateType: aggregateJob, AggregateID: job.ID, LegalEntityID: job.LegalEntityID,
		Data: map[string]any{
			"job_id":                 job.ID,
			"integrity_score":        job.IntegrityScore,
			"status":                 string(job.Status),
			"blocker_count":          job.BlockerCount,
			"control_totals_matched": controlTotalsMatched,
			"quarantined_count":      len(job.AuditEntries),
		},
	}
	if err := s.enqueueEvent(ctx, tx, tenantID, actorID, correlationID, validated); err != nil {
		return err
	}
	if len(job.AuditEntries) == 0 {
		return nil
	}
	violations := migrationEvent{
		TypeSegment: "job", Fact: "integrity-violations-detected", LegacyType: "migration.integrity_violations_detected",
		AggregateType: aggregateJob, AggregateID: job.ID, LegalEntityID: job.LegalEntityID,
		Data: map[string]any{
			"job_id":            job.ID,
			"invalid_count":     job.InvalidRecordsCount,
			"quarantined_count": len(job.AuditEntries),
			"blocker_count":     job.BlockerCount,
		},
	}
	return s.enqueueEvent(ctx, tx, tenantID, actorID, correlationID, violations)
}

// ─── GetJobByID ───────────────────────────────────────────────────────────────

// GetJobByID implements Store.
func (s *PgStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.MigrationJob, error) {
	var job *domain.MigrationJob
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		job, err = readJob(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		if job.IntegrityChecks, err = readChecks(ctx, tx, tenantID, id); err != nil {
			return err
		}
		job.AuditEntries, err = readEntries(ctx, tx, tenantID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// readJob reads one job row; forUpdate takes the row lock the transitions
// need. The tenant predicate is explicit as well as enforced by RLS.
func readJob(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (*domain.MigrationJob, error) {
	query := `SELECT ` + jobColumns + ` FROM migration_jobs j WHERE j.id = $1 AND j.tenant_id = $2`
	if forUpdate {
		query += ` FOR UPDATE OF j`
	}
	job, err := scanJob(tx.QueryRow(ctx, query, id, tenantID))
	if isNoRows(err) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read migration_job: %w", err)
	}
	return job, nil
}

func readChecks(ctx context.Context, tx pgx.Tx, tenantID, jobID string) ([]domain.IntegrityCheck, error) {
	rows, err := tx.Query(ctx, `SELECT `+checkColumns+` FROM migration_integrity_checks
		WHERE job_id = $1 AND tenant_id = $2 ORDER BY created_at, check_type`, jobID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query integrity_checks: %w", err)
	}
	defer rows.Close()
	var checks []domain.IntegrityCheck
	for rows.Next() {
		var c domain.IntegrityCheck
		if err := rows.Scan(&c.ID, &c.TenantID, &c.JobID, &c.CheckName, &c.CheckType, &c.RecordsChecked,
			&c.RecordsPassed, &c.RecordsFailed, &c.Severity, &c.Detail, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan integrity_check: %w", err)
		}
		checks = append(checks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate integrity_checks: %w", err)
	}
	return checks, nil
}

func readEntries(ctx context.Context, tx pgx.Tx, tenantID, jobID string) ([]domain.AuditEntry, error) {
	rows, err := tx.Query(ctx, `SELECT `+entryColumns+` FROM migration_audit_entries
		WHERE job_id = $1 AND tenant_id = $2 ORDER BY created_at, id`, jobID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query audit_entries: %w", err)
	}
	defer rows.Close()
	var entries []domain.AuditEntry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit_entry: %w", err)
		}
		entries = append(entries, *entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit_entries: %w", err)
	}
	return entries, nil
}

// ─── ListJobs ─────────────────────────────────────────────────────────────────

// ListJobs implements Store. It fetches Limit+1 rows to learn whether a next
// page exists without a separate COUNT.
func (s *PgStore) ListJobs(ctx context.Context, tenantID string, filter ListFilter) ([]domain.MigrationJob, bool, error) {
	jobs := []domain.MigrationJob{}
	hasMore := false
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + jobColumns + ` FROM migration_jobs j
			WHERE j.tenant_id = $1 AND j.legal_entity_id = $2 AND ($3 = '' OR j.status = $3)
			ORDER BY j.created_at DESC, j.id
			LIMIT $4 OFFSET $5`
		rows, err := tx.Query(ctx, query, tenantID, filter.LegalEntityID, filter.Status, filter.Limit+1, filter.Offset)
		if err != nil {
			return fmt.Errorf("query migration_jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			job, err := scanJob(rows)
			if err != nil {
				return fmt.Errorf("scan migration_job: %w", err)
			}
			jobs = append(jobs, *job)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, err
	}
	if len(jobs) > filter.Limit {
		jobs = jobs[:filter.Limit]
		hasMore = true
	}
	return jobs, hasMore, nil
}

// ─── ArchiveJob ───────────────────────────────────────────────────────────────

// ArchiveJob implements Store. Steps: claim key → lock job → If-Match →
// state rule (not already archived) → set ARCHIVED, archived_at/by, bump
// version → enqueue job_archived → record response. Checks and entries are
// untouched: archive is a soft delete and the evidence stays readable.
func (s *PgStore) ArchiveJob(ctx context.Context, tenantID string, cmd ArchiveCommand, claim IdempotencyClaim) (*domain.MigrationJob, error) {
	var archived *domain.MigrationJob
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, tenantID, claim); err != nil {
			return err
		}
		job, err := readJob(ctx, tx, tenantID, cmd.JobID, true)
		if err != nil {
			return err
		}
		if cmd.ExpectedVersion != 0 && cmd.ExpectedVersion != job.Version {
			return ErrVersionMismatch
		}
		if err := domain.CanArchive(job.Status); err != nil {
			return err
		}

		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE migration_jobs
			   SET status = 'ARCHIVED', archived_at = $3, archived_by = $4,
			       version = version + 1, updated_at = $3
			 WHERE id = $1 AND tenant_id = $2`, cmd.JobID, tenantID, now, cmd.ActorID)
		if err != nil {
			return fmt.Errorf("archive migration_job: %w", err)
		}
		job.Status = domain.JobStatusArchived
		job.ArchivedAt = &now
		job.ArchivedBy = cmd.ActorID
		job.Version++
		job.UpdatedAt = now

		event := migrationEvent{
			TypeSegment: "job", Fact: "archived", LegacyType: "migration.job_archived",
			AggregateType: aggregateJob, AggregateID: job.ID, LegalEntityID: job.LegalEntityID,
			Data: map[string]any{"job_id": job.ID, "archived_by": cmd.ActorID, "archived_at": now, "version": job.Version},
		}
		if err := s.enqueueEvent(ctx, tx, tenantID, cmd.ActorID, cmd.CorrelationID, event); err != nil {
			return err
		}
		archived = job
		return completeIdempotencyKey(ctx, tx, tenantID, claimWithStatus(claim, http.StatusOK), job)
	})
	if err != nil {
		return nil, err
	}
	return archived, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func claimWithStatus(claim IdempotencyClaim, status int) IdempotencyClaim {
	claim.ResponseStatus = status
	return claim
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
