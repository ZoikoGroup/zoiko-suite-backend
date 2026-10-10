package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/ai-governance-svc/internal/events"
)

// Hook verifies that the database trigger persisted the event in the same
// transaction as the business write. Kafka delivery belongs to Relay.
type Hook struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewHook(pool *pgxpool.Pool, logger *zap.Logger) *Hook {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Hook{pool: pool, logger: logger}
}

func (h *Hook) Publish(ctx context.Context, params events.PublishParams) error {
	var exists bool
	err := h.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ai_governance_outbox
			WHERE event_type = $1 AND aggregate_id = $2 AND tenant_id = $3
		)
	`, params.EventType, params.EntityID, params.TenantID).Scan(&exists)
	if err != nil {
		wrapped := fmt.Errorf("verify transactional outbox event %s/%s: %w", params.EventType, params.EntityID, err)
		h.logger.Error("database-backed event was not verifiable", zap.Error(wrapped))
		return wrapped
	}
	if !exists {
		err := fmt.Errorf("transactional outbox event %s/%s is missing", params.EventType, params.EntityID)
		h.logger.Error("business mutation committed without its outbox event", zap.Error(err))
		return err
	}
	return nil
}

func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT to_regclass('public.ai_governance_outbox') IS NOT NULL
			AND to_regclass('public.ai_executions') IS NOT NULL
			AND to_regclass('public.ai_incidents') IS NOT NULL
			AND (
				SELECT count(*) = 13
				FROM pg_trigger
				WHERE NOT tgisinternal
					AND tgname::text = ANY(ARRAY[
						'aig_outbox_ai_runs',
						'aig_outbox_action_risk_classifications',
						'aig_outbox_automation_policies',
						'aig_outbox_automation_actions_insert',
						'aig_outbox_automation_actions_update',
						'aig_outbox_model_provider_registrations',
						'aig_outbox_policy_change_approvals_insert',
						'aig_outbox_policy_change_approvals_update',
						'aig_outbox_use_cases_insert',
						'aig_outbox_use_cases_update',
						'aig_outbox_model_releases',
						'aig_outbox_ai_executions',
						'aig_outbox_ai_incidents'
					])
			)
	`).Scan(&exists); err != nil {
		return fmt.Errorf("verify transactional outbox migration: %w", err)
	}
	if !exists {
		return fmt.Errorf("transactional outbox migration 000005 is not installed")
	}
	return nil
}
