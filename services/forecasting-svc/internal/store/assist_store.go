package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/outbox"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 assist jobs, suggestions, evidence and planner decisions.
//
// Nothing in this file writes to forecast_models or forecast_projections. The
// only read of forecast_models is the provenance snapshot taken when a planner
// accepts a suggestion for a target forecast.
// ─────────────────────────────────────────────────────────────────────────────

const assistJobColumns = `
	job_id, tenant_id, legal_entity_id, domain_name, model_provider, model_version, plan_version,
	planning_purpose, COALESCE(narrative, ''), status, version, created_by, created_at, COALESCE(review_started_by, '')`

func scanAssistJob(row pgx.Row) (*domain.ForecastAssistJob, error) {
	var job domain.ForecastAssistJob
	err := row.Scan(&job.JobID, &job.TenantID, &job.LegalEntityID, &job.DomainName, &job.ModelProvider,
		&job.ModelVersion, &job.PlanVersion, &job.PlanningPurpose, &job.Narrative, &job.Status, &job.Version,
		&job.CreatedBy, &job.CreatedAt, &job.ReviewStartedBy)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func loadAssistJob(ctx context.Context, tx pgx.Tx, tenantID, jobID string, forUpdate bool) (*domain.ForecastAssistJob, error) {
	query := `SELECT ` + assistJobColumns + ` FROM forecast_assist_jobs WHERE tenant_id = $1 AND job_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	job, err := scanAssistJob(tx.QueryRow(ctx, query, tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAssistJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load assist job: %w", err)
	}
	return job, nil
}

func requireAssistJobExists(ctx context.Context, tx pgx.Tx, tenantID, jobID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM forecast_assist_jobs WHERE tenant_id = $1 AND job_id = $2)`,
		tenantID, jobID).Scan(&exists); err != nil {
		return fmt.Errorf("check assist job: %w", err)
	}
	if !exists {
		return domain.ErrAssistJobNotFound
	}
	return nil
}

// openOrCreateJob returns the job a suggestion command writes into.
//
//   - jobContext.JobID set: lock the existing job, check If-Match, and require
//     it to still be open for suggestions (Queued/Generated). Its frozen model
//     context is authoritative.
//   - jobContext.JobID empty: require an Approved model release, then create a
//     new job in status Generated.
//
// The returned bool is true when a new job was created.
func openOrCreateJob(ctx context.Context, tx pgx.Tx, cmd Command, jobContext domain.JobContext) (*domain.ForecastAssistJob, bool, error) {
	if !jobContext.StartsNewJob() {
		job, err := loadAssistJob(ctx, tx, cmd.TenantID, jobContext.JobID, true)
		if err != nil {
			return nil, false, err
		}
		if err := checkExpectedVersion(cmd, job.Version); err != nil {
			return nil, false, err
		}
		if !job.Status.IsOpenForSuggestions() {
			return nil, false, domain.NewConflictError("job_not_open",
				fmt.Sprintf("forecast assist job is %s; suggestions can only be added while it is Generated", job.Status))
		}
		return job, false, nil
	}

	if err := requireApprovedModelRelease(ctx, tx, cmd.TenantID, jobContext.DomainName, jobContext.ModelProvider, jobContext.ModelVersion); err != nil {
		return nil, false, err
	}
	jobID := domain.PrefixForecastAssistJob + uuid.New().String()
	job, err := scanAssistJob(tx.QueryRow(ctx, `
		INSERT INTO forecast_assist_jobs (
			job_id, tenant_id, legal_entity_id, domain_name, model_provider, model_version,
			plan_version, planning_purpose, status, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Generated', $9)
		RETURNING `+assistJobColumns,
		jobID, cmd.TenantID, cmd.LegalEntityID, jobContext.DomainName, jobContext.ModelProvider, jobContext.ModelVersion,
		jobContext.PlanVersion, jobContext.PlanningPurpose, cmd.ActorID))
	if err != nil {
		return nil, false, fmt.Errorf("create assist job: %w", err)
	}
	return job, true, nil
}

// bumpJobVersion increments the job version after content was appended to an
// existing job, so a client holding an old ETag sees the change.
func bumpJobVersion(ctx context.Context, tx pgx.Tx, tenantID, jobID string) error {
	_, err := tx.Exec(ctx, `UPDATE forecast_assist_jobs SET version = version + 1 WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID)
	if err != nil {
		return fmt.Errorf("bump job version: %w", err)
	}
	return nil
}

func insertEvidence(ctx context.Context, tx pgx.Tx, tenantID, jobID string, evidence []domain.EvidenceInput) ([]string, error) {
	evidenceIDs := make([]string, 0, len(evidence))
	for _, item := range evidence {
		evidenceID := domain.PrefixEvidenceReference + uuid.New().String()
		if _, err := tx.Exec(ctx, `
			INSERT INTO evidence_references (evidence_id, tenant_id, job_id, source_ref, description)
			VALUES ($1, $2, $3, $4, $5)`,
			evidenceID, tenantID, jobID, item.SourceRef, item.Description); err != nil {
			return nil, fmt.Errorf("insert evidence reference: %w", err)
		}
		evidenceIDs = append(evidenceIDs, evidenceID)
	}
	return evidenceIDs, nil
}

// enqueueSuggestionGenerated writes AI.ForecastSuggestionGenerated.
func enqueueSuggestionGenerated(ctx context.Context, tx pgx.Tx, cmd Command, job *domain.ForecastAssistJob,
	suggestionKind string, suggestionIDs, evidenceIDs []string) error {
	return outbox.Enqueue(ctx, tx, outbox.Record{
		AggregateType:    "forecast_assist_job",
		AggregateID:      job.JobID,
		AggregateVersion: job.Version,
		Params: events.PublishParams{
			EventType:     events.EventForecastSuggestionGenerated,
			SubjectID:     job.JobID,
			TenantID:      job.TenantID,
			LegalEntityID: job.LegalEntityID,
			ActorID:       cmd.ActorID,
			CorrelationID: cmd.CorrelationID,
			Payload: map[string]interface{}{
				"job_id":           job.JobID,
				"suggestion_kind":  suggestionKind,
				"suggestion_ids":   suggestionIDs,
				"evidence_ids":     evidenceIDs,
				"domain_name":      job.DomainName,
				"model_provider":   job.ModelProvider,
				"model_version":    job.ModelVersion,
				"plan_version":     job.PlanVersion,
				"planning_purpose": job.PlanningPurpose,
				"advisory":         true,
			},
		},
	})
}

// suggestionWriter inserts the command-specific suggestion rows and returns
// their ids. It receives the (possibly new) job.
type suggestionWriter func(tx pgx.Tx, job *domain.ForecastAssistJob) ([]string, error)

// runSuggestionCommand holds the steps shared by SuggestDrivers, SuggestRange
// and GenerateNarrative.
func (s *PgStore) runSuggestionCommand(ctx context.Context, cmd Command, jobContext domain.JobContext, suggestionKind string,
	evidence []domain.EvidenceInput, writeSuggestions suggestionWriter) (*domain.ForecastAssistJob, error) {
	var result *domain.ForecastAssistJob
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		// Step 1: replay protection.
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		// Step 2: find or create the job.
		job, created, err := openOrCreateJob(ctx, tx, cmd, jobContext)
		if err != nil {
			return err
		}
		// Step 3: write suggestions and their evidence.
		suggestionIDs, err := writeSuggestions(tx, job)
		if err != nil {
			return err
		}
		evidenceIDs, err := insertEvidence(ctx, tx, cmd.TenantID, job.JobID, evidence)
		if err != nil {
			return err
		}
		if !created {
			if err := bumpJobVersion(ctx, tx, cmd.TenantID, job.JobID); err != nil {
				return err
			}
		}
		result, err = loadAssistJob(ctx, tx, cmd.TenantID, job.JobID, false)
		if err != nil {
			return err
		}
		// Step 4: event + idempotent result.
		if err := enqueueSuggestionGenerated(ctx, tx, cmd, result, suggestionKind, suggestionIDs, evidenceIDs); err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, job.JobID)
	})
	return result, err
}

// SuggestDrivers implements POST /v1/forecast-assist/suggest-drivers.
func (s *PgStore) SuggestDrivers(ctx context.Context, cmd Command, req domain.SuggestDriversRequest) (*domain.ForecastAssistJob, error) {
	return s.runSuggestionCommand(ctx, cmd, req.JobContext, "drivers", req.Evidence,
		func(tx pgx.Tx, job *domain.ForecastAssistJob) ([]string, error) {
			driverIDs := make([]string, 0, len(req.Drivers))
			for _, driver := range req.Drivers {
				driverID := domain.PrefixSuggestedDriver + uuid.New().String()
				if _, err := tx.Exec(ctx, `
					INSERT INTO suggested_drivers (driver_id, tenant_id, job_id, driver_name, suggested_value, rationale)
					VALUES ($1, $2, $3, $4, $5, $6)`,
					driverID, cmd.TenantID, job.JobID, driver.DriverName, driver.SuggestedValue, driver.Rationale); err != nil {
					return nil, fmt.Errorf("insert suggested driver: %w", err)
				}
				driverIDs = append(driverIDs, driverID)
			}
			return driverIDs, nil
		})
}

// SuggestRange implements POST /v1/forecast-assist/suggest-range.
func (s *PgStore) SuggestRange(ctx context.Context, cmd Command, req domain.SuggestRangeRequest) (*domain.ForecastAssistJob, error) {
	return s.runSuggestionCommand(ctx, cmd, req.JobContext, "ranges", req.Evidence,
		func(tx pgx.Tx, job *domain.ForecastAssistJob) ([]string, error) {
			rangeIDs := make([]string, 0, len(req.Ranges))
			for _, suggestedRange := range req.Ranges {
				rangeID := domain.PrefixSuggestedRange + uuid.New().String()
				if _, err := tx.Exec(ctx, `
					INSERT INTO suggested_ranges (range_id, tenant_id, job_id, period_label, low_value, high_value, confidence_note)
					VALUES ($1, $2, $3, $4, $5, $6, $7)`,
					rangeID, cmd.TenantID, job.JobID, suggestedRange.PeriodLabel, suggestedRange.LowValue,
					suggestedRange.HighValue, suggestedRange.ConfidenceNote); err != nil {
					return nil, fmt.Errorf("insert suggested range: %w", err)
				}
				rangeIDs = append(rangeIDs, rangeID)
			}
			return rangeIDs, nil
		})
}

// GenerateNarrative implements POST /v1/forecast-assist/generate-narrative.
// The narrative is write-once per job.
func (s *PgStore) GenerateNarrative(ctx context.Context, cmd Command, req domain.GenerateNarrativeRequest) (*domain.ForecastAssistJob, error) {
	return s.runSuggestionCommand(ctx, cmd, req.JobContext, "narrative", req.Evidence,
		func(tx pgx.Tx, job *domain.ForecastAssistJob) ([]string, error) {
			if job.Narrative != "" {
				return nil, domain.NewConflictError("narrative_already_generated", "this job already has a narrative; it is write-once")
			}
			if _, err := tx.Exec(ctx, `UPDATE forecast_assist_jobs SET narrative = $3 WHERE tenant_id = $1 AND job_id = $2`,
				cmd.TenantID, job.JobID, req.NarrativeText); err != nil {
				return nil, fmt.Errorf("set narrative: %w", err)
			}
			return []string{job.JobID}, nil
		})
}

// StartPlannerReview implements POST /v1/forecast-assist/jobs/{jobID}:start-review:
// Generated -> PlannerReview. From here on the job's content is frozen.
func (s *PgStore) StartPlannerReview(ctx context.Context, cmd Command, jobID string) (*domain.ForecastAssistJob, error) {
	var reviewed *domain.ForecastAssistJob
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		job, err := loadAssistJob(ctx, tx, cmd.TenantID, jobID, true)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(cmd, job.Version); err != nil {
			return err
		}
		if job.Status != domain.AssistJobGenerated {
			return domain.NewInvalidTransitionError("forecast assist job", string(job.Status), string(domain.AssistJobPlannerReview))
		}
		reviewed, err = scanAssistJob(tx.QueryRow(ctx, `
			UPDATE forecast_assist_jobs
			SET status = 'PlannerReview', review_started_by = $3, version = version + 1
			WHERE tenant_id = $1 AND job_id = $2
			RETURNING `+assistJobColumns, cmd.TenantID, jobID, cmd.ActorID))
		if err != nil {
			return fmt.Errorf("start planner review: %w", err)
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, jobID)
	})
	return reviewed, err
}

// targetForecastSnapshot is the provenance recorded when a suggestion is
// accepted for a forecast. The forecast row is only READ.
type targetForecastSnapshot struct {
	forecastID string
	status     string
	version    int
}

func readTargetForecast(ctx context.Context, tx pgx.Tx, tenantID, forecastID string) (*targetForecastSnapshot, error) {
	if forecastID == "" {
		return nil, nil
	}
	snapshot := targetForecastSnapshot{forecastID: forecastID}
	err := tx.QueryRow(ctx, `SELECT status, version FROM forecast_models WHERE tenant_id = $1 AND id = $2`,
		tenantID, forecastID).Scan(&snapshot.status, &snapshot.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &domain.Error{Kind: domain.KindValidation, Code: "target_forecast_not_found", Field: "target_forecast_id",
			Message: "target_forecast_id does not identify a forecast in this tenant"}
	}
	if err != nil {
		return nil, fmt.Errorf("read target forecast: %w", err)
	}
	return &snapshot, nil
}

// decisionInput is what recordDecision writes.
type decisionInput struct {
	decision domain.PlannerDecisionType
	reason   string
	note     string
	target   *targetForecastSnapshot
}

func recordDecision(ctx context.Context, tx pgx.Tx, cmd Command, jobID string, input decisionInput) error {
	decisionID := domain.PrefixPlannerDecision + uuid.New().String()
	var reason, note, targetID, targetStatus *string
	var targetVersion *int
	if input.reason != "" {
		reason = &input.reason
	}
	if input.note != "" {
		note = &input.note
	}
	if input.target != nil {
		targetID = &input.target.forecastID
		targetStatus = &input.target.status
		targetVersion = &input.target.version
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO planner_decisions (
			decision_id, tenant_id, job_id, decision, reason, actor, note,
			target_forecast_id, target_forecast_status, target_forecast_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		decisionID, cmd.TenantID, jobID, string(input.decision), reason, cmd.ActorID, note,
		targetID, targetStatus, targetVersion)
	if err != nil {
		return fmt.Errorf("record planner decision: %w", err)
	}
	return nil
}

// lockJobInPlannerReview performs the checks shared by accept and reject.
func lockJobInPlannerReview(ctx context.Context, tx pgx.Tx, cmd Command, jobID string, target domain.AssistJobStatus) (*domain.ForecastAssistJob, error) {
	job, err := loadAssistJob(ctx, tx, cmd.TenantID, jobID, true)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(cmd, job.Version); err != nil {
		return nil, err
	}
	if job.Status != domain.AssistJobPlannerReview {
		return nil, domain.NewInvalidTransitionError("forecast assist job", string(job.Status), string(target))
	}
	return job, nil
}

// AcceptSuggestion implements POST /v1/forecast-assist/jobs/{jobID}:accept:
// PlannerReview -> Accepted.
//
// Rules:
//   - Segregation of duties: the principal that requested/generated the
//     suggestions (job.created_by) cannot accept them — an AI suggestion may not
//     bypass independent human review.
//   - The accepted suggestion NEVER edits a forecast. If a target forecast is
//     named, its id/status/version are snapshotted on the decision, and
//     AI.ForecastSuggestionAccepted tells the FIN-02 owner to create a normal
//     planner-reviewed candidate (spec N-20).
func (s *PgStore) AcceptSuggestion(ctx context.Context, cmd Command, jobID string, req domain.AcceptSuggestionRequest) (*domain.JobDecisionResult, error) {
	var result *domain.JobDecisionResult
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		job, err := lockJobInPlannerReview(ctx, tx, cmd, jobID, domain.AssistJobAccepted)
		if err != nil {
			return err
		}
		if job.CreatedBy == cmd.ActorID {
			return domain.NewForbiddenError("segregation_of_duties",
				"the principal who requested the AI suggestions cannot accept them; an independent planner must decide")
		}
		target, err := readTargetForecast(ctx, tx, cmd.TenantID, req.TargetForecastID)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE forecast_assist_jobs SET status = 'Accepted', version = version + 1 WHERE tenant_id = $1 AND job_id = $2`,
			cmd.TenantID, jobID); err != nil {
			return fmt.Errorf("accept suggestion: %w", err)
		}
		if err := recordDecision(ctx, tx, cmd, jobID, decisionInput{decision: domain.PlannerDecisionAccepted, note: req.Note, target: target}); err != nil {
			return err
		}

		result, err = loadJobDecisionResult(ctx, tx, cmd.TenantID, jobID)
		if err != nil {
			return err
		}
		if err := enqueueSuggestionAccepted(ctx, tx, cmd, result); err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, jobID)
	})
	return result, err
}

func enqueueSuggestionAccepted(ctx context.Context, tx pgx.Tx, cmd Command, result *domain.JobDecisionResult) error {
	job, decision := result.Job, result.Decision
	return outbox.Enqueue(ctx, tx, outbox.Record{
		AggregateType:    "forecast_assist_job",
		AggregateID:      job.JobID,
		AggregateVersion: job.Version,
		Params: events.PublishParams{
			EventType:     events.EventForecastSuggestionAccepted,
			SubjectID:     job.JobID,
			TenantID:      job.TenantID,
			LegalEntityID: job.LegalEntityID,
			ActorID:       cmd.ActorID,
			CorrelationID: cmd.CorrelationID,
			Payload: map[string]interface{}{
				"job_id":                  job.JobID,
				"decision_id":             decision.DecisionID,
				"plan_version":            job.PlanVersion,
				"model_provider":          job.ModelProvider,
				"model_version":           job.ModelVersion,
				"target_forecast_id":      decision.TargetForecastID,
				"target_forecast_status":  decision.TargetForecastStatus,
				"target_forecast_version": decision.TargetForecastVersion,
				// The consumer (FIN-02) must create a normal forecast candidate;
				// this service applied nothing to any forecast.
				"fin_candidate_required": true,
				"applied_to_forecast":    false,
			},
		},
	})
}

// RejectSuggestion implements POST /v1/forecast-assist/jobs/{jobID}:reject:
// PlannerReview -> Rejected, with a mandatory reason.
func (s *PgStore) RejectSuggestion(ctx context.Context, cmd Command, jobID string, req domain.RejectSuggestionRequest) (*domain.JobDecisionResult, error) {
	var result *domain.JobDecisionResult
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		if _, err := lockJobInPlannerReview(ctx, tx, cmd, jobID, domain.AssistJobRejected); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE forecast_assist_jobs SET status = 'Rejected', version = version + 1 WHERE tenant_id = $1 AND job_id = $2`,
			cmd.TenantID, jobID); err != nil {
			return fmt.Errorf("reject suggestion: %w", err)
		}
		if err := recordDecision(ctx, tx, cmd, jobID, decisionInput{decision: domain.PlannerDecisionRejected, reason: req.Reason}); err != nil {
			return err
		}
		var err error
		result, err = loadJobDecisionResult(ctx, tx, cmd.TenantID, jobID)
		if err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, jobID)
	})
	return result, err
}

// ── reads ───────────────────────────────────────────────────────────────────

func loadPlannerDecision(ctx context.Context, tx pgx.Tx, tenantID, jobID string) (*domain.PlannerDecision, error) {
	var decision domain.PlannerDecision
	var targetVersion *int
	err := tx.QueryRow(ctx, `
		SELECT decision_id, tenant_id, job_id, decision, COALESCE(reason, ''), COALESCE(note, ''), actor,
		       COALESCE(target_forecast_id::text, ''), COALESCE(target_forecast_status, ''), target_forecast_version, decided_at
		FROM planner_decisions WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID).
		Scan(&decision.DecisionID, &decision.TenantID, &decision.JobID, &decision.Decision, &decision.Reason, &decision.Note, &decision.Actor,
			&decision.TargetForecastID, &decision.TargetForecastStatus, &targetVersion, &decision.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPlannerDecisionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load planner decision: %w", err)
	}
	if targetVersion != nil {
		decision.TargetForecastVersion = *targetVersion
	}
	return &decision, nil
}

func loadJobDecisionResult(ctx context.Context, tx pgx.Tx, tenantID, jobID string) (*domain.JobDecisionResult, error) {
	job, err := loadAssistJob(ctx, tx, tenantID, jobID, false)
	if err != nil {
		return nil, err
	}
	decision, err := loadPlannerDecision(ctx, tx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	return &domain.JobDecisionResult{Job: job, Decision: decision}, nil
}

// GetJobDecisionResult returns the job and its decision (used for idempotent
// replays of :accept and :reject).
func (s *PgStore) GetJobDecisionResult(ctx context.Context, tenantID, jobID string) (*domain.JobDecisionResult, error) {
	var result *domain.JobDecisionResult
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadJobDecisionResult(ctx, tx, tenantID, jobID)
		result = loaded
		return err
	})
	return result, err
}

// GetAssistJob implements GET /v1/forecast-assist/jobs/{jobID}.
func (s *PgStore) GetAssistJob(ctx context.Context, tenantID, jobID string) (*domain.ForecastAssistJob, error) {
	var job *domain.ForecastAssistJob
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadAssistJob(ctx, tx, tenantID, jobID, false)
		job = loaded
		return err
	})
	return job, err
}

// ListAssistJobs implements GET /v1/forecast-assist/jobs.
func (s *PgStore) ListAssistJobs(ctx context.Context, tenantID string, filter domain.AssistJobListFilter) ([]domain.ForecastAssistJob, string, error) {
	limit := pageLimit(filter.Page)
	conditions := []string{"tenant_id = $1"}
	args := []interface{}{tenantID}
	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.Page.HasCursorFilter {
		args = append(args, filter.Page.AfterCreatedAt, filter.Page.AfterID)
		conditions = append(conditions, fmt.Sprintf("(created_at, job_id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := `SELECT ` + assistJobColumns + ` FROM forecast_assist_jobs WHERE ` + strings.Join(conditions, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, job_id DESC LIMIT $%d`, len(args))

	jobs := []domain.ForecastAssistJob{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list assist jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			job, err := scanAssistJob(rows)
			if err != nil {
				return fmt.Errorf("scan assist job: %w", err)
			}
			jobs = append(jobs, *job)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		last := jobs[limit-1]
		nextCursor = domain.EncodeCursor(last.CreatedAt, last.JobID)
	}
	return jobs, nextCursor, nil
}

// GetSuggestedDrivers implements GET /v1/forecast-assist/jobs/{jobID}/drivers.
func (s *PgStore) GetSuggestedDrivers(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedDriver, error) {
	drivers := []domain.SuggestedDriver{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireAssistJobExists(ctx, tx, tenantID, jobID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT driver_id, tenant_id, job_id, driver_name, suggested_value, rationale, created_at
			FROM suggested_drivers WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at, driver_id`, tenantID, jobID)
		if err != nil {
			return fmt.Errorf("list suggested drivers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.SuggestedDriver
			if err := rows.Scan(&d.DriverID, &d.TenantID, &d.JobID, &d.DriverName, &d.SuggestedValue, &d.Rationale, &d.CreatedAt); err != nil {
				return fmt.Errorf("scan suggested driver: %w", err)
			}
			drivers = append(drivers, d)
		}
		return rows.Err()
	})
	return drivers, err
}

// GetSuggestedRanges implements GET /v1/forecast-assist/jobs/{jobID}/ranges.
func (s *PgStore) GetSuggestedRanges(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedRange, error) {
	ranges := []domain.SuggestedRange{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireAssistJobExists(ctx, tx, tenantID, jobID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT range_id, tenant_id, job_id, period_label, low_value, high_value, confidence_note, created_at
			FROM suggested_ranges WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at, range_id`, tenantID, jobID)
		if err != nil {
			return fmt.Errorf("list suggested ranges: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.SuggestedRange
			if err := rows.Scan(&r.RangeID, &r.TenantID, &r.JobID, &r.PeriodLabel, &r.LowValue, &r.HighValue, &r.ConfidenceNote, &r.CreatedAt); err != nil {
				return fmt.Errorf("scan suggested range: %w", err)
			}
			ranges = append(ranges, r)
		}
		return rows.Err()
	})
	return ranges, err
}

// GetEvidenceReferences implements GET /v1/forecast-assist/jobs/{jobID}/evidence.
func (s *PgStore) GetEvidenceReferences(ctx context.Context, tenantID, jobID string) ([]domain.EvidenceReference, error) {
	evidence := []domain.EvidenceReference{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireAssistJobExists(ctx, tx, tenantID, jobID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, tenant_id, job_id, source_ref, description, created_at
			FROM evidence_references WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at, evidence_id`, tenantID, jobID)
		if err != nil {
			return fmt.Errorf("list evidence references: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.EvidenceReference
			if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.JobID, &e.SourceRef, &e.Description, &e.CreatedAt); err != nil {
				return fmt.Errorf("scan evidence reference: %w", err)
			}
			evidence = append(evidence, e)
		}
		return rows.Err()
	})
	return evidence, err
}

// GetPlannerDecision implements GET /v1/forecast-assist/jobs/{jobID}/decision.
func (s *PgStore) GetPlannerDecision(ctx context.Context, tenantID, jobID string) (*domain.PlannerDecision, error) {
	var decision *domain.PlannerDecision
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireAssistJobExists(ctx, tx, tenantID, jobID); err != nil {
			return err
		}
		loaded, err := loadPlannerDecision(ctx, tx, tenantID, jobID)
		decision = loaded
		return err
	})
	return decision, err
}
