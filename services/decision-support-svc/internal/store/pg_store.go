// Package store persists decision-support-svc's data in Postgres.
//
// Every tenant-facing method runs inside one transaction that first sets
// app.tenant_id for row-level security, AND filters by tenant_id in SQL —
// two independent locks on the same door. A row belonging to another
// tenant is therefore indistinguishable from a row that does not exist.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/decision-support-svc/internal/domain"
)

// PgStore implements service.Store and the outbox relay's store.
type PgStore struct {
	pool *pgxpool.Pool
}

// New builds a PgStore on pool.
func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenantTx runs fn in a transaction scoped to tenantID by RLS, and
// commits only if fn succeeds.
func (s *PgStore) withTenantTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if tenantID == "" {
		return domain.ErrTenantMissing
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// ── recommendations ──────────────────────────────────────────────────────────

const recommendationColumns = `
	recommendation_id, tenant_id, legal_entity_id, subject_type, subject_reference,
	action_type, recommended_action, confidence_score, rationale, prior_decisions_sampled,
	outcome_distribution, history_status, evidence_refs,
	recommendation_source, model_id, model_version, prompt_version, assurance_state,
	oversight_class, status, version, expires_at,
	decided_by_principal_id, decided_at, decision_reason, downstream_action_ref, approval_reference,
	superseded_by_recommendation_id, supersedes_recommendation_id,
	requested_by_principal_id, correlation_id, created_at, updated_at`

// scanRecommendation reads one row selected with recommendationColumns.
func scanRecommendation(row pgx.Row) (*domain.Recommendation, error) {
	var rec domain.Recommendation
	var recommendedAction, historyStatus, source, oversightClass, status string
	var distributionJSON, evidenceJSON []byte

	err := row.Scan(
		&rec.RecommendationID, &rec.TenantID, &rec.LegalEntityID, &rec.SubjectType, &rec.SubjectReference,
		&rec.ActionType, &recommendedAction, &rec.ConfidenceScore, &rec.Rationale, &rec.PriorDecisionsSampled,
		&distributionJSON, &historyStatus, &evidenceJSON,
		&source, &rec.ModelID, &rec.ModelVersion, &rec.PromptVersion, &rec.AssuranceState,
		&oversightClass, &status, &rec.Version, &rec.ExpiresAt,
		&rec.DecidedByPrincipalID, &rec.DecidedAt, &rec.DecisionReason, &rec.DownstreamActionRef, &rec.ApprovalReference,
		&rec.SupersededByRecommendationID, &rec.SupersedesRecommendationID,
		&rec.RequestedByPrincipalID, &rec.CorrelationID, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	rec.RecommendedAction = domain.RecommendedAction(recommendedAction)
	rec.HistoryStatus = domain.HistoryStatus(historyStatus)
	rec.Source = domain.RecommendationSource(source)
	rec.OversightClass = domain.OversightClass(oversightClass)
	rec.RequiresIndependentReviewer = rec.OversightClass.RequiresIndependentReviewer()
	rec.Status = domain.Status(status)
	rec.DownstreamActionExecuted = false

	if err := json.Unmarshal(distributionJSON, &rec.OutcomeDistribution); err != nil {
		return nil, fmt.Errorf("decode outcome_distribution: %w", err)
	}
	if err := json.Unmarshal(evidenceJSON, &rec.EvidenceRefs); err != nil {
		return nil, fmt.Errorf("decode evidence_refs: %w", err)
	}
	return &rec, nil
}

// InsertRecommendation persists a new recommendation, its creation history
// entry, its outbox messages and its idempotency record — all or nothing.
func (s *PgStore) InsertRecommendation(ctx context.Context, write domain.NewRecommendationWrite) error {
	rec := write.Recommendation
	return s.withTenantTx(ctx, rec.TenantID, func(tx pgx.Tx) error {
		if err := insertIdempotencyRecord(ctx, tx, write.Idempotency); err != nil {
			return err
		}
		if err := insertRecommendationRow(ctx, tx, rec); err != nil {
			return err
		}
		if err := insertDisposition(ctx, tx, write.Disposition); err != nil {
			return err
		}
		return insertOutboxMessages(ctx, tx, write.Outbox)
	})
}

func insertRecommendationRow(ctx context.Context, tx pgx.Tx, rec domain.Recommendation) error {
	distributionJSON, err := json.Marshal(nonNilMap(rec.OutcomeDistribution))
	if err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(nonNilSlice(rec.EvidenceRefs))
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO recommendations (
			recommendation_id, tenant_id, legal_entity_id, subject_type, subject_reference,
			action_type, recommended_action, confidence_score, rationale, prior_decisions_sampled,
			outcome_distribution, history_status, evidence_refs,
			recommendation_source, model_id, model_version, prompt_version, assurance_state,
			oversight_class, status, version, expires_at,
			requested_by_principal_id, correlation_id, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13,
			$14, $15, $16, $17, $18,
			$19, $20, $21, $22,
			$23, $24, $25, $26
		)`,
		rec.RecommendationID, rec.TenantID, rec.LegalEntityID, rec.SubjectType, rec.SubjectReference,
		rec.ActionType, string(rec.RecommendedAction), rec.ConfidenceScore, rec.Rationale, rec.PriorDecisionsSampled,
		distributionJSON, string(rec.HistoryStatus), evidenceJSON,
		string(rec.Source), rec.ModelID, rec.ModelVersion, rec.PromptVersion, rec.AssuranceState,
		string(rec.OversightClass), string(rec.Status), rec.Version, rec.ExpiresAt,
		rec.RequestedByPrincipalID, rec.CorrelationID, rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert recommendation: %w", err)
	}
	return nil
}

// GetRecommendation returns one recommendation of tenantID, or
// domain.ErrRecommendationNotFound.
func (s *PgStore) GetRecommendation(ctx context.Context, tenantID, recommendationID string) (*domain.Recommendation, error) {
	var rec *domain.Recommendation
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			"SELECT "+recommendationColumns+" FROM recommendations WHERE tenant_id = $1 AND recommendation_id = $2",
			tenantID, recommendationID)
		var scanErr error
		rec, scanErr = scanRecommendation(row)
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRecommendationNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ListRecommendations returns up to filter.Limit recommendations of
// tenantID, newest first, after filter.After.
func (s *PgStore) ListRecommendations(ctx context.Context, tenantID string, filter domain.ListFilter) ([]domain.Recommendation, error) {
	query, args := buildListQuery(tenantID, filter)

	var out []domain.Recommendation
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			rec, err := scanRecommendation(rows)
			if err != nil {
				return err
			}
			out = append(out, *rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// buildListQuery adds one numbered placeholder per supplied filter.
func buildListQuery(tenantID string, filter domain.ListFilter) (string, []any) {
	query := "SELECT " + recommendationColumns + " FROM recommendations WHERE tenant_id = $1"
	args := []any{tenantID}

	addCondition := func(sqlTemplate string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(sqlTemplate, len(args))
	}

	if filter.LegalEntityID != "" {
		addCondition(" AND legal_entity_id = $%d", filter.LegalEntityID)
	}
	if filter.SubjectType != "" {
		addCondition(" AND subject_type = $%d", filter.SubjectType)
	}
	if filter.SubjectReference != "" {
		addCondition(" AND subject_reference = $%d", filter.SubjectReference)
	}
	if filter.ActionType != "" {
		addCondition(" AND action_type = $%d", filter.ActionType)
	}
	if filter.Status != "" {
		addCondition(" AND status = $%d", string(filter.Status))
	}
	if filter.After != nil {
		args = append(args, filter.After.CreatedAt, filter.After.RecommendationID)
		query += fmt.Sprintf(" AND (created_at, recommendation_id) < ($%d, $%d::uuid)", len(args)-1, len(args))
	}

	args = append(args, filter.Limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC, recommendation_id DESC LIMIT $%d", len(args))
	return query, args
}

// ApplyTransition persists a status change, its history entry, its outbox
// messages and its idempotency record — all or nothing. The UPDATE only
// matches if the row still has the expected version and status, so a
// concurrent decision makes this one fail with domain.ErrVersionMismatch.
func (s *PgStore) ApplyTransition(ctx context.Context, write domain.TransitionWrite) error {
	rec := write.Updated
	return s.withTenantTx(ctx, rec.TenantID, func(tx pgx.Tx) error {
		if err := insertIdempotencyRecord(ctx, tx, write.Idempotency); err != nil {
			return err
		}
		if err := updateRecommendationStatus(ctx, tx, write); err != nil {
			return err
		}
		if write.ReplacementID != nil {
			if err := linkReplacement(ctx, tx, rec, *write.ReplacementID); err != nil {
				return err
			}
		}
		if err := insertDisposition(ctx, tx, write.Disposition); err != nil {
			return err
		}
		return insertOutboxMessages(ctx, tx, write.Outbox)
	})
}

func updateRecommendationStatus(ctx context.Context, tx pgx.Tx, write domain.TransitionWrite) error {
	rec := write.Updated
	tag, err := tx.Exec(ctx, `
		UPDATE recommendations SET
			status = $1, version = $2, updated_at = $3,
			decided_by_principal_id = $4, decided_at = $5, decision_reason = $6,
			downstream_action_ref = $7, approval_reference = $8, superseded_by_recommendation_id = $9
		WHERE tenant_id = $10 AND recommendation_id = $11 AND version = $12 AND status = $13`,
		string(rec.Status), rec.Version, rec.UpdatedAt,
		rec.DecidedByPrincipalID, rec.DecidedAt, rec.DecisionReason,
		rec.DownstreamActionRef, rec.ApprovalReference, rec.SupersededByRecommendationID,
		rec.TenantID, rec.RecommendationID, write.ExpectedVersion, string(write.FromStatus),
	)
	if err != nil {
		return fmt.Errorf("update recommendation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrVersionMismatch
	}
	return nil
}

// linkReplacement records on the replacement which recommendation it
// supersedes. It must still be awaiting review.
func linkReplacement(ctx context.Context, tx pgx.Tx, superseded domain.Recommendation, replacementID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE recommendations SET
			supersedes_recommendation_id = $1, version = version + 1, updated_at = $2
		WHERE tenant_id = $3 AND recommendation_id = $4 AND status = 'REVIEW_REQUIRED'`,
		superseded.RecommendationID, superseded.UpdatedAt, superseded.TenantID, replacementID,
	)
	if err != nil {
		return fmt.Errorf("link replacement recommendation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrReplacementNotPending
	}
	return nil
}

// ── dispositions ─────────────────────────────────────────────────────────────

func insertDisposition(ctx context.Context, tx pgx.Tx, d domain.Disposition) error {
	var fromStatus *string
	if d.FromStatus != nil {
		value := string(*d.FromStatus)
		fromStatus = &value
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO recommendation_dispositions (
			disposition_id, tenant_id, recommendation_id, from_status, to_status,
			decided_by_principal_id, reason, downstream_action_ref, approval_reference,
			replacement_recommendation_id, resulting_version, correlation_id, request_id, decided_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		d.DispositionID, d.TenantID, d.RecommendationID, fromStatus, string(d.ToStatus),
		d.DecidedByPrincipalID, d.Reason, d.DownstreamActionRef, d.ApprovalReference,
		d.ReplacementRecommendationID, d.ResultingVersion, d.CorrelationID, d.RequestID, d.DecidedAt,
	)
	if err != nil {
		return fmt.Errorf("insert disposition: %w", err)
	}
	return nil
}

// ListDispositions returns a recommendation's history, oldest first.
func (s *PgStore) ListDispositions(ctx context.Context, tenantID, recommendationID string) ([]domain.Disposition, error) {
	var out []domain.Disposition
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT disposition_id, tenant_id, recommendation_id, from_status, to_status,
			       decided_by_principal_id, reason, downstream_action_ref, approval_reference,
			       replacement_recommendation_id, resulting_version, correlation_id, request_id, decided_at
			FROM recommendation_dispositions
			WHERE tenant_id = $1 AND recommendation_id = $2
			ORDER BY decided_at ASC, resulting_version ASC`,
			tenantID, recommendationID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var d domain.Disposition
			var fromStatus *string
			var toStatus string
			if err := rows.Scan(
				&d.DispositionID, &d.TenantID, &d.RecommendationID, &fromStatus, &toStatus,
				&d.DecidedByPrincipalID, &d.Reason, &d.DownstreamActionRef, &d.ApprovalReference,
				&d.ReplacementRecommendationID, &d.ResultingVersion, &d.CorrelationID, &d.RequestID, &d.DecidedAt,
			); err != nil {
				return err
			}
			if fromStatus != nil {
				status := domain.Status(*fromStatus)
				d.FromStatus = &status
			}
			d.ToStatus = domain.Status(toStatus)
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── idempotency ──────────────────────────────────────────────────────────────

// FindIdempotencyRecord returns the stored record for key, or nil.
func (s *PgStore) FindIdempotencyRecord(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error) {
	var record *domain.IdempotencyRecord
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var r domain.IdempotencyRecord
		err := tx.QueryRow(ctx, `
			SELECT tenant_id, idempotency_key, operation, request_hash, resource_id, response_body
			FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, key,
		).Scan(&r.TenantID, &r.Key, &r.Operation, &r.RequestHash, &r.ResourceID, &r.ResponseBody)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		record = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// insertIdempotencyRecord claims the key inside the write's transaction. If
// another transaction holds the same key, Postgres waits for it; if that one
// committed, nothing is inserted and the caller replays its response.
func insertIdempotencyRecord(ctx context.Context, tx pgx.Tx, r domain.IdempotencyRecord) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, idempotency_key, operation, request_hash, resource_id, response_body)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		r.TenantID, r.Key, r.Operation, r.RequestHash, r.ResourceID, []byte(r.ResponseBody),
	)
	if err != nil {
		return fmt.Errorf("insert idempotency record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrIdempotencyRace
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func nonNilMap(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
