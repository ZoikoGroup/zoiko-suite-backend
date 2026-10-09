package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"zoiko.io/search-indexer-svc/internal/domain"
)

// CreateRetrievalEvaluation records one §10.1 certification run.
//
// Every run is kept, passes and failures alike. A migration that passed on the
// fourth attempt is a different fact from one that passed on the first, and an
// evaluation table holding only the pass would erase the three that did not.
func (s *PgStore) CreateRetrievalEvaluation(ctx context.Context, e domain.RetrievalEvaluation) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO retrieval_evaluations (
				evaluation_id, generation_id, scope_name, pinned_model, k, cases,
				min_recall, recall, passed, cases_digest, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			e.EvaluationID, e.GenerationID, e.ScopeName, e.PinnedModel, e.K, e.Cases,
			e.MinRecall, e.Recall, e.Passed, e.CasesDigest, e.CreatedByPrincipalID)
		return err
	})
}

// LatestRetrievalEvaluation returns a generation's most recent evaluation, or
// domain.ErrNotFound when it has never been evaluated.
//
// The LATEST, not the best. Certification answers "is this generation good
// enough now", and a pass followed by a fail is a generation that stopped being
// good enough — cutting over on the earlier pass would ignore the newer
// evidence.
func (s *PgStore) LatestRetrievalEvaluation(ctx context.Context, generationID string) (*domain.RetrievalEvaluation, error) {
	var e domain.RetrievalEvaluation
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT evaluation_id, generation_id, scope_name, pinned_model, k, cases,
			       min_recall, recall, passed, cases_digest, created_at, created_by_principal_id
			FROM retrieval_evaluations
			WHERE generation_id = $1
			ORDER BY created_at DESC LIMIT 1`, generationID).Scan(
			&e.EvaluationID, &e.GenerationID, &e.ScopeName, &e.PinnedModel, &e.K, &e.Cases,
			&e.MinRecall, &e.Recall, &e.Passed, &e.CasesDigest, &e.CreatedAt, &e.CreatedByPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}
