package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// ListEvidenceRequestTransitions returns a request's history, oldest first.
func (s *PgStore) ListEvidenceRequestTransitions(ctx context.Context, tenantID, requestID string) ([]*domain.EvidenceRequestTransition, error) {
	var out []*domain.EvidenceRequestTransition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT transition_id::text, request_id::text, from_status, to_status, command, actor, changed_fields,
			assigned_to_principal_id, due_at, record_version, correlation_id, recorded_at
			FROM evidence_request_transitions WHERE tenant_id = $1 AND request_id = $2 ORDER BY recorded_at, record_version, transition_id`,
			tenantID, requestID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.EvidenceRequestTransition
			if err := rows.Scan(&t.TransitionID, &t.RequestID, &t.FromStatus, &t.ToStatus, &t.Command, &t.Actor, &t.ChangedFields,
				&t.AssignedToPrincipalID, &t.DueAt, &t.RecordVersion, &t.CorrelationID, &t.RecordedAt); err != nil {
				return err
			}
			out = append(out, &t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, storeWrap(err)
	}
	if out == nil {
		out = []*domain.EvidenceRequestTransition{}
	}
	return out, nil
}
