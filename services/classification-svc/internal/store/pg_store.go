// Package store is the PgStore persistence layer for classification-svc
// (AI-02, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write.
// A job's candidates and feature snapshot are immutable evidence from
// the moment Classify creates them; only the job's own status and its
// single terminal ClassificationDecision ever move, exactly once.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/classification-svc/internal/domain"
	"zoiko.io/classification-svc/internal/outbox"
)

// Store is the AI-02 persistence contract.
type Store interface {
	RegisterModelRelease(ctx context.Context, tenantID string, req domain.RegisterModelReleaseRequest, actor string) (*domain.ModelRelease, error)
	Classify(ctx context.Context, tenantID string, req domain.ClassifyRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error)
	AcceptSuggestion(ctx context.Context, tenantID, jobID string, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error)
	RejectSuggestion(ctx context.Context, tenantID, jobID string, req domain.RejectSuggestionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error)
	OverrideWithReason(ctx context.Context, tenantID, jobID string, req domain.OverrideWithReasonRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error)

	GetJob(ctx context.Context, tenantID, jobID string) (*domain.ClassificationJob, error)
	GetCandidates(ctx context.Context, tenantID, jobID string) ([]domain.ClassificationCandidate, error)
	GetFeatureSnapshot(ctx context.Context, tenantID, jobID string) (*domain.FeatureSnapshot, error)
	GetDecision(ctx context.Context, tenantID, jobID string) (*domain.ClassificationDecision, error)
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
	return fmt.Errorf("classification-svc: %s", pgErr.Message)
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── ModelRelease ─────────────────────────────────────────────────────────────

const modelReleaseColumns = `model_release_id, tenant_id, taxonomy_id, model_provider, model_version, max_drift_bp, created_at, created_by`

func scanModelRelease(row pgx.Row) (*domain.ModelRelease, error) {
	var m domain.ModelRelease
	if err := row.Scan(&m.ModelReleaseID, &m.TenantID, &m.TaxonomyID, &m.ModelProvider, &m.ModelVersion, &m.MaxDriftBP,
		&m.CreatedAt, &m.CreatedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) RegisterModelRelease(ctx context.Context, tenantID string, req domain.RegisterModelReleaseRequest, actor string) (*domain.ModelRelease, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ModelRelease
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newID(domain.PrefixModelRelease)
		got, err := scanModelRelease(tx.QueryRow(ctx, `
			INSERT INTO model_releases (model_release_id, tenant_id, taxonomy_id, model_provider, model_version, max_drift_bp, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, taxonomy_id, model_provider, model_version) DO UPDATE SET max_drift_bp = EXCLUDED.max_drift_bp
			RETURNING `+modelReleaseColumns,
			id, tenantID, req.TaxonomyID, req.ModelProvider, req.ModelVersion, req.MaxDriftBP, actor))
		if err != nil {
			return fmt.Errorf("register model release: %w", err)
		}
		out = got
		return nil
	})
	return out, err
}

func loadModelRelease(ctx context.Context, tx pgx.Tx, tenantID, taxonomyID, provider, version string) (*domain.ModelRelease, error) {
	m, err := scanModelRelease(tx.QueryRow(ctx, `SELECT `+modelReleaseColumns+` FROM model_releases
		WHERE tenant_id = $1 AND taxonomy_id = $2 AND model_provider = $3 AND model_version = $4`,
		tenantID, taxonomyID, provider, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrModelReleaseNotFound
	}
	return m, err
}

// ── ClassificationJob ────────────────────────────────────────────────────────

const jobColumns = `job_id, tenant_id, object_ref, object_type, taxonomy_id, taxonomy_version, model_provider,
	model_version, protected, status, created_at, created_by`

func scanJob(row pgx.Row) (*domain.ClassificationJob, error) {
	var j domain.ClassificationJob
	if err := row.Scan(&j.JobID, &j.TenantID, &j.ObjectRef, &j.ObjectType, &j.TaxonomyID, &j.TaxonomyVersion, &j.ModelProvider,
		&j.ModelVersion, &j.Protected, &j.Status, &j.CreatedAt, &j.CreatedBy); err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *PgStore) GetJob(ctx context.Context, tenantID, jobID string) (*domain.ClassificationJob, error) {
	var out *domain.ClassificationJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM classification_jobs WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrJobNotFound
		}
		out = j
		return err
	})
	return out, err
}

func loadJobForUpdate(ctx context.Context, tx pgx.Tx, tenantID, jobID string) (*domain.ClassificationJob, error) {
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM classification_jobs WHERE tenant_id = $1 AND job_id = $2 FOR UPDATE`,
		tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrJobNotFound
	}
	return j, err
}

// Classify accepts a caller-supplied, already-scored candidate set
// (with its feature evidence) and governs the review workflow around
// it — it does not perform classification itself (see the package doc
// comment). It refuses outright, before creating anything, if the
// invocation's own observed drift exceeds the registered model
// release's threshold. A protected job always lands ReviewRequired
// regardless of confidence.
func (s *PgStore) Classify(ctx context.Context, tenantID string, req domain.ClassifyRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ClassificationJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		release, err := loadModelRelease(ctx, tx, tenantID, req.TaxonomyID, req.ModelProvider, req.ModelVersion)
		if err != nil {
			return err
		}
		if req.ObservedDriftBP > release.MaxDriftBP {
			return domain.ErrDriftExceedsThreshold
		}

		jobID := newID(domain.PrefixJob)
		claim.ResourceID = jobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if _, err := scanJob(tx.QueryRow(ctx, `
			INSERT INTO classification_jobs (job_id, tenant_id, object_ref, object_type, taxonomy_id, taxonomy_version,
				model_provider, model_version, protected, status, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'Scored', $10)
			RETURNING `+jobColumns,
			jobID, tenantID, req.ObjectRef, req.ObjectType, req.TaxonomyID, req.TaxonomyVersion, req.ModelProvider,
			req.ModelVersion, req.Protected, actor)); err != nil {
			return fmt.Errorf("create classification job: %w", err)
		}

		featuresJSON, err := json.Marshal(req.Features)
		if err != nil {
			return fmt.Errorf("marshal features: %w", err)
		}
		snapID := newID(domain.PrefixSnapshot)
		if _, err := tx.Exec(ctx, `
			INSERT INTO feature_snapshots (snapshot_id, tenant_id, job_id, features, content_hash)
			VALUES ($1, $2, $3, $4, $5)`,
			snapID, tenantID, jobID, featuresJSON, sha256Hex(featuresJSON)); err != nil {
			return fmt.Errorf("record feature snapshot: %w", err)
		}

		var topLabel string
		var topConfidence int32
		for i, c := range req.Candidates {
			rank := int32(i + 1)
			if rank == 1 {
				topLabel, topConfidence = c.Label, c.ConfidenceBP
			}
			candID := newID(domain.PrefixCandidate)
			if _, err := tx.Exec(ctx, `
				INSERT INTO classification_candidates (candidate_id, tenant_id, job_id, rank, label, confidence_bp)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				candID, tenantID, jobID, rank, c.Label, c.ConfidenceBP); err != nil {
				return fmt.Errorf("insert classification candidate: %w", err)
			}
		}

		requiresReview := req.Protected || topConfidence < req.ReviewConfidenceThresholdBP
		var target domain.JobStatus
		if requiresReview {
			target = domain.JobReviewRequired
		} else {
			target = domain.JobAccepted
		}
		updated, err := scanJob(tx.QueryRow(ctx, `UPDATE classification_jobs SET status = $2 WHERE job_id = $1 RETURNING `+jobColumns, jobID, target))
		if err != nil {
			return fmt.Errorf("finalize job status: %w", err)
		}

		if !requiresReview {
			if err := recordDecision(ctx, tx, tenantID, jobID, domain.DecisionAccepted, topLabel, "", actor); err != nil {
				return err
			}
		}

		out = updated
		eventType := "AI.ClassificationSuggested"
		if !requiresReview {
			eventType = "AI.ClassificationAccepted"
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "classification_job", AggregateID: jobID,
			EventType: eventType, TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

func recordDecision(ctx context.Context, tx pgx.Tx, tenantID, jobID string, decision domain.DecisionType, finalLabel, reason, actor string) error {
	decID := newID(domain.PrefixDecision)
	var labelVal, reasonVal *string
	if finalLabel != "" {
		labelVal = &finalLabel
	}
	if reason != "" {
		reasonVal = &reason
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO classification_decisions (decision_id, tenant_id, job_id, decision, final_label, reason, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		decID, tenantID, jobID, decision, labelVal, reasonVal, actor)
	if err != nil {
		return fmt.Errorf("record classification decision: %w", err)
	}
	return nil
}

func topCandidateLabel(ctx context.Context, tx pgx.Tx, jobID string) (string, error) {
	var label string
	err := tx.QueryRow(ctx, `SELECT label FROM classification_candidates WHERE job_id = $1 AND rank = 1`, jobID).Scan(&label)
	return label, err
}

// AcceptSuggestion accepts the job's own top candidate — only legal
// while the job is ReviewRequired.
func (s *PgStore) AcceptSuggestion(ctx context.Context, tenantID, jobID string, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error) {
	var out *domain.ClassificationJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobReviewRequired {
			return domain.ErrJobNotReviewable
		}
		claim.ResourceID = jobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		label, err := topCandidateLabel(ctx, tx, jobID)
		if err != nil {
			return fmt.Errorf("load top candidate: %w", err)
		}
		if err := recordDecision(ctx, tx, tenantID, jobID, domain.DecisionAccepted, label, "", actor); err != nil {
			return err
		}
		updated, err := scanJob(tx.QueryRow(ctx, `UPDATE classification_jobs SET status = 'Accepted' WHERE job_id = $1 RETURNING `+jobColumns, jobID))
		if err != nil {
			return fmt.Errorf("accept suggestion: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "classification_job", AggregateID: jobID,
			EventType: "AI.ClassificationAccepted", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// RejectSuggestion rejects the job's top candidate outright — terminal,
// and (together with idempotent replay of the original Classify claim)
// what makes a rejected suggestion impossible to reapply invisibly:
// retrying the same Classify request returns the existing job/decision
// rather than creating a fresh, unreviewed one.
func (s *PgStore) RejectSuggestion(ctx context.Context, tenantID, jobID string, req domain.RejectSuggestionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ClassificationJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobReviewRequired {
			return domain.ErrJobNotReviewable
		}
		claim.ResourceID = jobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := recordDecision(ctx, tx, tenantID, jobID, domain.DecisionRejected, "", req.Reason, actor); err != nil {
			return err
		}
		updated, err := scanJob(tx.QueryRow(ctx, `UPDATE classification_jobs SET status = 'Rejected' WHERE job_id = $1 RETURNING `+jobColumns, jobID))
		if err != nil {
			return fmt.Errorf("reject suggestion: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "classification_job", AggregateID: jobID,
			EventType: "AI.ClassificationRejected", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// OverrideWithReason accepts a human-chosen label that differs from
// the model's own top suggestion — always evidenced with a mandatory
// reason, never a bare confidence override.
func (s *PgStore) OverrideWithReason(ctx context.Context, tenantID, jobID string, req domain.OverrideWithReasonRequest, actor string, claim domain.IdempotencyClaim) (*domain.ClassificationJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ClassificationJob
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobReviewRequired {
			return domain.ErrJobNotReviewable
		}
		claim.ResourceID = jobID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := recordDecision(ctx, tx, tenantID, jobID, domain.DecisionOverridden, req.FinalLabel, req.Reason, actor); err != nil {
			return err
		}
		updated, err := scanJob(tx.QueryRow(ctx, `UPDATE classification_jobs SET status = 'Accepted' WHERE job_id = $1 RETURNING `+jobColumns, jobID))
		if err != nil {
			return fmt.Errorf("override with reason: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "classification_job", AggregateID: jobID,
			EventType: "AI.ClassificationAccepted", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Read surfaces ────────────────────────────────────────────────────────────

const candidateColumns = `candidate_id, tenant_id, job_id, rank, label, confidence_bp, created_at`

func scanCandidate(row pgx.Row) (*domain.ClassificationCandidate, error) {
	var c domain.ClassificationCandidate
	if err := row.Scan(&c.CandidateID, &c.TenantID, &c.JobID, &c.Rank, &c.Label, &c.ConfidenceBP, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) GetCandidates(ctx context.Context, tenantID, jobID string) ([]domain.ClassificationCandidate, error) {
	var out []domain.ClassificationCandidate
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+candidateColumns+` FROM classification_candidates WHERE tenant_id = $1 AND job_id = $2 ORDER BY rank`,
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

func (s *PgStore) GetFeatureSnapshot(ctx context.Context, tenantID, jobID string) (*domain.FeatureSnapshot, error) {
	var out *domain.FeatureSnapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var fs domain.FeatureSnapshot
		var featuresJSON []byte
		err := tx.QueryRow(ctx, `SELECT snapshot_id, tenant_id, job_id, features, content_hash, created_at
			FROM feature_snapshots WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID).
			Scan(&fs.SnapshotID, &fs.TenantID, &fs.JobID, &featuresJSON, &fs.ContentHash, &fs.CreatedAt)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(featuresJSON, &fs.Features); err != nil {
			return fmt.Errorf("unmarshal features: %w", err)
		}
		out = &fs
		return nil
	})
	return out, err
}

const decisionColumns = `decision_id, tenant_id, job_id, decision, final_label, reason, actor, decided_at`

func scanDecision(row pgx.Row) (*domain.ClassificationDecision, error) {
	var d domain.ClassificationDecision
	var label, reason *string
	if err := row.Scan(&d.DecisionID, &d.TenantID, &d.JobID, &d.Decision, &label, &reason, &d.Actor, &d.DecidedAt); err != nil {
		return nil, err
	}
	if label != nil {
		d.FinalLabel = *label
	}
	if reason != nil {
		d.Reason = *reason
	}
	return &d, nil
}

func (s *PgStore) GetDecision(ctx context.Context, tenantID, jobID string) (*domain.ClassificationDecision, error) {
	var out *domain.ClassificationDecision
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := scanDecision(tx.QueryRow(ctx, `SELECT `+decisionColumns+` FROM classification_decisions WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		out = d
		return err
	})
	return out, err
}
