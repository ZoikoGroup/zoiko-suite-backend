// Package store is the PgStore persistence layer for
// document-extraction-svc (AI-01, ZS-SVC-N-001 §4). Every method runs
// inside one transaction that first declares app.tenant_id for RLS,
// then performs the write. A candidate's evidentiary columns and its
// evidence span are immutable from the moment ExtractDocument creates
// them; only the accept/reject decision ever moves, exactly once.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/document-extraction-svc/internal/domain"
	"zoiko.io/document-extraction-svc/internal/outbox"
)

// Store is the AI-01 persistence contract.
type Store interface {
	ExtractDocument(ctx context.Context, tenantID string, req domain.ExtractDocumentRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionJob, error)
	AcceptCandidate(ctx context.Context, tenantID, candidateID string, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionCandidate, error)
	RejectCandidate(ctx context.Context, tenantID, candidateID string, req domain.RejectCandidateRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionCandidate, error)
	ReprocessWithVersion(ctx context.Context, tenantID, jobID string, req domain.ReprocessWithVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionJob, error)

	GetJob(ctx context.Context, tenantID, jobID string) (*domain.ExtractionJob, error)
	GetCandidates(ctx context.Context, tenantID, jobID string) ([]domain.ExtractionCandidate, error)
	GetEvidenceSpan(ctx context.Context, tenantID, candidateID string) (*domain.EvidenceSpan, error)
	GetModelInvocation(ctx context.Context, tenantID, jobID string) (*domain.ModelInvocation, error)
	GetDecisions(ctx context.Context, tenantID, candidateID string) ([]domain.ExtractionDecision, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("document-extraction-svc: %s", pgErr.Message)
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

// ── ExtractionJob ────────────────────────────────────────────────────────────

const jobColumns = `job_id, tenant_id, document_ref, document_hash, extraction_schema_id, extraction_schema_version,
	review_confidence_threshold_bp, classification, residency_region, status, reprocessed_from_job_id, created_at, created_by`

func scanJob(row pgx.Row) (*domain.ExtractionJob, error) {
	var j domain.ExtractionJob
	if err := row.Scan(&j.JobID, &j.TenantID, &j.DocumentRef, &j.DocumentHash, &j.ExtractionSchemaID, &j.ExtractionSchemaVersion,
		&j.ReviewConfidenceThresholdBP, &j.Classification, &j.ResidencyRegion, &j.Status, &j.ReprocessedFromJobID, &j.CreatedAt,
		&j.CreatedBy); err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *PgStore) GetJob(ctx context.Context, tenantID, jobID string) (*domain.ExtractionJob, error) {
	var out *domain.ExtractionJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM extraction_jobs WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrJobNotFound
		}
		out = j
		return err
	})
	return out, err
}

// ExtractDocument accepts a caller-supplied candidate set (with model
// invocation provenance and per-field evidence spans) and governs the
// review workflow around it — it does not perform extraction itself
// (see the package doc comment). Protected fields below the job's own
// review-confidence threshold land Pending; everything else is
// auto-accepted immediately.
func (s *PgStore) ExtractDocument(ctx context.Context, tenantID string, req domain.ExtractDocumentRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ExtractionJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		jobID := newID(domain.PrefixJob)
		claim.ResourceID = jobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		job, err := createJob(ctx, tx, tenantID, jobID, nil, req.DocumentRef, req.DocumentHash, req.ExtractionSchemaID,
			req.ExtractionSchemaVersion, req.ReviewConfidenceThresholdBP, req.Classification, req.ResidencyRegion, actor,
			req.ModelProvider, req.ModelVersion, req.PromptVersion, req.RequestContentHash, req.ResponseContentHash, req.Candidates)
		if err != nil {
			return err
		}
		out = job
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "extraction_job", AggregateID: job.JobID,
			EventType: "AI.ExtractionCompleted", TenantID: &tenantID, Payload: job})
	})
	return out, err
}

// ReprocessWithVersion always creates a brand new job — it never edits
// the job (or any of its candidates/decisions) being reprocessed. That
// is what keeps a prompt/model upgrade from silently changing an
// already-accepted extraction's workflow: the old evidence trail is
// untouched, and reprocessed_from_job_id is the only link between them.
func (s *PgStore) ReprocessWithVersion(ctx context.Context, tenantID, jobID string, req domain.ReprocessWithVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ExtractionJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		oldJob, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM extraction_jobs WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrJobNotFound
		}
		if err != nil {
			return err
		}

		newJobID := newID(domain.PrefixJob)
		claim.ResourceID = newJobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		newJob, err := createJob(ctx, tx, tenantID, newJobID, &oldJob.JobID, oldJob.DocumentRef, oldJob.DocumentHash,
			oldJob.ExtractionSchemaID, oldJob.ExtractionSchemaVersion, oldJob.ReviewConfidenceThresholdBP, oldJob.Classification,
			oldJob.ResidencyRegion, actor, req.ModelProvider, req.ModelVersion, req.PromptVersion, req.RequestContentHash,
			req.ResponseContentHash, req.Candidates)
		if err != nil {
			return err
		}
		out = newJob
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "extraction_job", AggregateID: newJob.JobID,
			EventType: "AI.ExtractionCompleted", TenantID: &tenantID, Payload: newJob})
	})
	return out, err
}

func createJob(ctx context.Context, tx pgx.Tx, tenantID, jobID string, reprocessedFrom *string, documentRef, documentHash,
	schemaID string, schemaVersion int64, thresholdBP int32, classification, residency, actor, modelProvider, modelVersion,
	promptVersion, requestHash, responseHash string, candidates []domain.CandidateInput) (*domain.ExtractionJob, error) {

	if _, err := scanJob(tx.QueryRow(ctx, `
		INSERT INTO extraction_jobs (job_id, tenant_id, document_ref, document_hash, extraction_schema_id,
			extraction_schema_version, review_confidence_threshold_bp, classification, residency_region, status,
			reprocessed_from_job_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'Running', $10, $11)
		RETURNING `+jobColumns,
		jobID, tenantID, documentRef, documentHash, schemaID, schemaVersion, thresholdBP, classification, residency,
		reprocessedFrom, actor)); err != nil {
		return nil, fmt.Errorf("create extraction job: %w", err)
	}

	invID := newID(domain.PrefixInvocation)
	if _, err := tx.Exec(ctx, `
		INSERT INTO model_invocations (invocation_id, tenant_id, job_id, model_provider, model_version, prompt_version,
			request_content_hash, response_content_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		invID, tenantID, jobID, modelProvider, modelVersion, promptVersion, requestHash, responseHash); err != nil {
		return nil, fmt.Errorf("record model invocation: %w", err)
	}

	pendingCount := 0
	now := time.Now().UTC()
	for _, c := range candidates {
		candID := newID(domain.PrefixCandidate)
		requiresReview := c.Protected && c.ConfidenceBP < thresholdBP
		status := domain.CandidateAccepted
		var decidedAt *time.Time
		var decidedBy *string
		if requiresReview {
			status = domain.CandidatePending
			pendingCount++
		} else {
			decidedAt = &now
			decidedBy = &actor
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO extraction_candidates (candidate_id, tenant_id, job_id, field_name, extracted_value, confidence_bp,
				protected, status, decided_at, decided_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			candID, tenantID, jobID, c.FieldName, c.ExtractedValue, c.ConfidenceBP, c.Protected, status, decidedAt, decidedBy); err != nil {
			return nil, fmt.Errorf("insert extraction candidate: %w", err)
		}

		spanID := newID(domain.PrefixSpan)
		if _, err := tx.Exec(ctx, `
			INSERT INTO evidence_spans (span_id, tenant_id, candidate_id, page_number, start_offset, end_offset, snippet_text)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			spanID, tenantID, candID, c.Span.PageNumber, c.Span.StartOffset, c.Span.EndOffset, c.Span.SnippetText); err != nil {
			return nil, fmt.Errorf("insert evidence span: %w", err)
		}
	}

	target := domain.JobAccepted
	if pendingCount > 0 {
		target = domain.JobReviewRequired
	}
	updated, err := scanJob(tx.QueryRow(ctx, `UPDATE extraction_jobs SET status = $2 WHERE job_id = $1 RETURNING `+jobColumns, jobID, target))
	if err != nil {
		return nil, fmt.Errorf("finalize job status: %w", err)
	}
	return updated, nil
}

// ── ExtractionCandidate decisions ───────────────────────────────────────────

const candidateColumns = `candidate_id, tenant_id, job_id, field_name, extracted_value, confidence_bp, protected, status,
	created_at, decided_at, decided_by`

func scanCandidate(row pgx.Row) (*domain.ExtractionCandidate, error) {
	var c domain.ExtractionCandidate
	var decidedBy *string
	if err := row.Scan(&c.CandidateID, &c.TenantID, &c.JobID, &c.FieldName, &c.ExtractedValue, &c.ConfidenceBP, &c.Protected,
		&c.Status, &c.CreatedAt, &c.DecidedAt, &decidedBy); err != nil {
		return nil, err
	}
	if decidedBy != nil {
		c.DecidedBy = *decidedBy
	}
	return &c, nil
}

func (s *PgStore) GetCandidates(ctx context.Context, tenantID, jobID string) ([]domain.ExtractionCandidate, error) {
	var out []domain.ExtractionCandidate
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+candidateColumns+` FROM extraction_candidates WHERE tenant_id = $1 AND job_id = $2 ORDER BY field_name`,
			tenantID, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCandidate(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

func loadCandidateForUpdate(ctx context.Context, tx pgx.Tx, tenantID, candidateID string) (*domain.ExtractionCandidate, error) {
	c, err := scanCandidate(tx.QueryRow(ctx, `SELECT `+candidateColumns+` FROM extraction_candidates WHERE tenant_id = $1 AND candidate_id = $2 FOR UPDATE`,
		tenantID, candidateID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCandidateNotFound
	}
	return c, err
}

func decideCandidate(ctx context.Context, tx pgx.Tx, tenantID, candidateID string, decision domain.CandidateStatus, reason, actor string) (*domain.ExtractionCandidate, error) {
	cand, err := loadCandidateForUpdate(ctx, tx, tenantID, candidateID)
	if err != nil {
		return nil, err
	}
	if cand.Status != domain.CandidatePending {
		return nil, domain.ErrCandidateNotPending
	}

	updated, err := scanCandidate(tx.QueryRow(ctx, `
		UPDATE extraction_candidates SET status = $2, decided_at = NOW(), decided_by = $3
		WHERE candidate_id = $1 RETURNING `+candidateColumns,
		candidateID, decision, actor))
	if err != nil {
		return nil, fmt.Errorf("decide candidate: %w", err)
	}

	decID := newID(domain.PrefixDecision)
	var reasonVal *string
	if reason != "" {
		reasonVal = &reason
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO extraction_decisions (decision_id, tenant_id, candidate_id, decision, reason, actor)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		decID, tenantID, candidateID, decision, reasonVal, actor); err != nil {
		return nil, fmt.Errorf("record extraction decision: %w", err)
	}

	var pendingCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM extraction_candidates WHERE job_id = $1 AND status = 'Pending'`, cand.JobID).Scan(&pendingCount); err != nil {
		return nil, err
	}
	if pendingCount == 0 {
		if _, err := tx.Exec(ctx, `UPDATE extraction_jobs SET status = 'Accepted' WHERE job_id = $1 AND status = 'ReviewRequired'`, cand.JobID); err != nil {
			return nil, fmt.Errorf("finalize job after last decision: %w", err)
		}
	}
	return updated, nil
}

func (s *PgStore) AcceptCandidate(ctx context.Context, tenantID, candidateID string, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionCandidate, error) {
	var out *domain.ExtractionCandidate
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		claim.ResourceID = candidateID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := decideCandidate(ctx, tx, tenantID, candidateID, domain.CandidateAccepted, "", actor)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "extraction_candidate", AggregateID: got.CandidateID,
			EventType: "AI.CandidateAccepted", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) RejectCandidate(ctx context.Context, tenantID, candidateID string, req domain.RejectCandidateRequest, actor string, claim domain.IdempotencyClaim) (*domain.ExtractionCandidate, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ExtractionCandidate
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		claim.ResourceID = candidateID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := decideCandidate(ctx, tx, tenantID, candidateID, domain.CandidateRejected, req.Reason, actor)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "extraction_candidate", AggregateID: got.CandidateID,
			EventType: "AI.CandidateRejected", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── Read surfaces ────────────────────────────────────────────────────────────

const spanColumns = `span_id, tenant_id, candidate_id, page_number, start_offset, end_offset, snippet_text, created_at`

func scanSpan(row pgx.Row) (*domain.EvidenceSpan, error) {
	var sp domain.EvidenceSpan
	if err := row.Scan(&sp.SpanID, &sp.TenantID, &sp.CandidateID, &sp.PageNumber, &sp.StartOffset, &sp.EndOffset,
		&sp.SnippetText, &sp.CreatedAt); err != nil {
		return nil, err
	}
	return &sp, nil
}

func (s *PgStore) GetEvidenceSpan(ctx context.Context, tenantID, candidateID string) (*domain.EvidenceSpan, error) {
	var out *domain.EvidenceSpan
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sp, err := scanSpan(tx.QueryRow(ctx, `SELECT `+spanColumns+` FROM evidence_spans WHERE tenant_id = $1 AND candidate_id = $2`, tenantID, candidateID))
		out = sp
		return err
	})
	return out, err
}

const invocationColumns = `invocation_id, tenant_id, job_id, model_provider, model_version, prompt_version,
	request_content_hash, response_content_hash, invoked_at`

func scanInvocation(row pgx.Row) (*domain.ModelInvocation, error) {
	var m domain.ModelInvocation
	if err := row.Scan(&m.InvocationID, &m.TenantID, &m.JobID, &m.ModelProvider, &m.ModelVersion, &m.PromptVersion,
		&m.RequestContentHash, &m.ResponseContentHash, &m.InvokedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) GetModelInvocation(ctx context.Context, tenantID, jobID string) (*domain.ModelInvocation, error) {
	var out *domain.ModelInvocation
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		m, err := scanInvocation(tx.QueryRow(ctx, `SELECT `+invocationColumns+` FROM model_invocations WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		out = m
		return err
	})
	return out, err
}

const decisionColumns = `decision_id, tenant_id, candidate_id, decision, reason, actor, decided_at`

func scanDecision(row pgx.Row) (*domain.ExtractionDecision, error) {
	var d domain.ExtractionDecision
	var reason *string
	if err := row.Scan(&d.DecisionID, &d.TenantID, &d.CandidateID, &d.Decision, &reason, &d.Actor, &d.DecidedAt); err != nil {
		return nil, err
	}
	if reason != nil {
		d.Reason = *reason
	}
	return &d, nil
}

func (s *PgStore) GetDecisions(ctx context.Context, tenantID, candidateID string) ([]domain.ExtractionDecision, error) {
	var out []domain.ExtractionDecision
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+decisionColumns+` FROM extraction_decisions WHERE tenant_id = $1 AND candidate_id = $2 ORDER BY decided_at`,
			tenantID, candidateID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDecision(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}
