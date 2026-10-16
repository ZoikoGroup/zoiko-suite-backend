package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// ManualChange is what a committed manual-input command changed; the builder turns it into an outbox event
// inside the same transaction.
type ManualChange struct {
	Kind  string // PROPOSE | APPROVE | REJECT | WITHDRAW
	Input *domain.ManualInput
}

type ManualEventBuilder func(ManualChange) (domain.OutboxEvent, error)

const manualCols = `manual_id, tenant_id, legal_entity_id, payroll_period_id, worker_id::text, input_code, category, amount::text, currency,
	effective_date::text, reason, evidence_ref, state, proposed_by, decided_by, decided_at, decision_note, correlation_id, created_at`

func scanManual(row pgx.Row) (*domain.ManualInput, error) {
	var m domain.ManualInput
	err := row.Scan(&m.ManualID, &m.TenantID, &m.LegalEntityID, &m.PayrollPeriodID, &m.WorkerID, &m.InputCode, &m.Category, &m.Amount,
		&m.Currency, &m.EffectiveDate, &m.Reason, &m.EvidenceRef, &m.State, &m.ProposedBy, &m.DecidedBy, &m.DecidedAt, &m.DecisionNote,
		&m.CorrelationID, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManualInputNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func enqueueManual(ctx context.Context, tx pgx.Tx, mk ManualEventBuilder, ch ManualChange) error {
	if mk == nil {
		return nil
	}
	ev, err := mk(ch)
	if err != nil {
		return fmt.Errorf("build event: %w", err)
	}
	if !domain.EventCatalogue[ev.EventType] {
		return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
		ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
		return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
	}
	return nil
}

// ProposeManualInput records a PROPOSED manual input. The proposer states the category as intent; the batch
// still classifies by the governed mapping, and a disagreement quarantines the line.
func (s *PgStore) ProposeManualInput(ctx context.Context, m *domain.ManualInput, mk ManualEventBuilder) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		stored, err := scanManual(tx.QueryRow(ctx, `
			INSERT INTO wfp_manual_inputs (manual_id, tenant_id, legal_entity_id, payroll_period_id, worker_id, input_code, category,
				amount, currency, effective_date, reason, evidence_ref, proposed_by, correlation_id)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8::numeric, $9, $10::date, $11, $12, $13, $14) RETURNING `+manualCols,
			m.ManualID, tenantID, m.LegalEntityID, m.PayrollPeriodID, m.WorkerID, m.InputCode, m.Category,
			m.Amount, m.Currency, m.EffectiveDate, m.Reason, m.EvidenceRef, m.ProposedBy, m.CorrelationID))
		if err != nil {
			return err
		}
		if err := enqueueManual(ctx, tx, mk, ManualChange{Kind: "PROPOSE", Input: stored}); err != nil {
			return err
		}
		*m = *stored
		return nil
	})
}

// GetManualInput loads one manual input.
func (s *PgStore) GetManualInput(ctx context.Context, id string) (*domain.ManualInput, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ManualInput
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		out, err = scanManual(tx.QueryRow(ctx, `SELECT `+manualCols+` FROM wfp_manual_inputs WHERE tenant_id = $1 AND manual_id = $2::uuid`, tenantID, id))
		return err
	})
	return out, err
}

// DecideManualInput moves a PROPOSED input to APPROVED, REJECTED or WITHDRAWN under a row lock. Approving or
// rejecting by the principal who proposed it is refused here and again by the database.
func (s *PgStore) DecideManualInput(ctx context.Context, id, decision, actor, note string, mk ManualEventBuilder) (*domain.ManualInput, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ManualInput
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanManual(tx.QueryRow(ctx, `SELECT `+manualCols+` FROM wfp_manual_inputs WHERE tenant_id = $1 AND manual_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.State != domain.ManualProposed {
			return domain.ErrManualInputNotPending
		}
		kind := map[string]string{domain.ManualApproved: "APPROVE", domain.ManualRejected: "REJECT", domain.ManualWithdrawn: "WITHDRAW"}[decision]
		switch decision {
		case domain.ManualApproved, domain.ManualRejected:
			if cur.ProposedBy == actor {
				return domain.ErrManualFourEyes
			}
		case domain.ManualWithdrawn:
			if cur.ProposedBy != actor {
				return domain.ErrManualOnlyMakerWithdraws
			}
		default:
			return fmt.Errorf("unknown decision %q", decision)
		}
		var n *string
		if note != "" {
			n = &note
		}
		stored, err := scanManual(tx.QueryRow(ctx, `UPDATE wfp_manual_inputs SET state = $3, decided_by = $4, decided_at = now(), decision_note = $5
			WHERE tenant_id = $1 AND manual_id = $2::uuid RETURNING `+manualCols, tenantID, id, decision, actor, n))
		if err != nil {
			return err
		}
		if err := enqueueManual(ctx, tx, mk, ManualChange{Kind: kind, Input: stored}); err != nil {
			return err
		}
		out = stored
		return nil
	})
	return out, err
}

// ListManualInputs lists a legal entity's manual inputs, optionally for one period and state.
func (s *PgStore) ListManualInputs(ctx context.Context, legalEntityID, periodID, state string) ([]domain.ManualInput, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.ManualInput{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+manualCols+` FROM wfp_manual_inputs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR payroll_period_id = $3) AND ($4 = '' OR state = $4)
			ORDER BY created_at, manual_id LIMIT 1000`, tenantID, legalEntityID, periodID, state)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanManual(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// ListApprovedManualInputs is what a batch for this entity and period pulls in: APPROVED inputs only.
func (s *PgStore) ListApprovedManualInputs(ctx context.Context, legalEntityID, periodID string) ([]domain.ManualInput, error) {
	return s.ListManualInputs(ctx, legalEntityID, periodID, domain.ManualApproved)
}
