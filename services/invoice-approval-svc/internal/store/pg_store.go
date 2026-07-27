package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/invoice-approval-svc/internal/domain"
	svcmiddleware "zoiko.io/invoice-approval-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

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

func (s *PgStore) CreateRequest(ctx context.Context, req *domain.InvoiceApprovalRequest) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO invoice_approval_requests (
				approval_request_id, tenant_id, legal_entity_id, invoice_id,
				workflow_instance_id, invoice_amount, currency_code, status,
				current_step, total_steps, created_by_principal_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, req.ApprovalRequestID, tenantID, req.LegalEntityID, req.InvoiceID,
			req.WorkflowInstanceID, req.InvoiceAmount, req.CurrencyCode, req.Status,
			req.CurrentStep, req.TotalSteps, req.CreatedByPrincipalID, req.CreatedAt, req.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetRequest(ctx context.Context, id string) (*domain.InvoiceApprovalRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var req domain.InvoiceApprovalRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT approval_request_id, tenant_id, legal_entity_id, invoice_id,
			       workflow_instance_id, invoice_amount, currency_code, status,
			       current_step, total_steps, created_by_principal_id, created_at, updated_at
			FROM invoice_approval_requests
			WHERE approval_request_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&req.ApprovalRequestID, &req.TenantID, &req.LegalEntityID, &req.InvoiceID,
			&req.WorkflowInstanceID, &req.InvoiceAmount, &req.CurrencyCode, &req.Status,
			&req.CurrentStep, &req.TotalSteps, &req.CreatedByPrincipalID, &req.CreatedAt, &req.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *PgStore) ListRequests(ctx context.Context, legalEntityID, invoiceID, status string) ([]domain.InvoiceApprovalRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.InvoiceApprovalRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT approval_request_id, tenant_id, legal_entity_id, invoice_id,
			       workflow_instance_id, invoice_amount, currency_code, status,
			       current_step, total_steps, created_by_principal_id, created_at, updated_at
			FROM invoice_approval_requests
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if invoiceID != "" {
			args = append(args, invoiceID)
			query += fmt.Sprintf(" AND invoice_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var req domain.InvoiceApprovalRequest
			if err := rows.Scan(
				&req.ApprovalRequestID, &req.TenantID, &req.LegalEntityID, &req.InvoiceID,
				&req.WorkflowInstanceID, &req.InvoiceAmount, &req.CurrencyCode, &req.Status,
				&req.CurrentStep, &req.TotalSteps, &req.CreatedByPrincipalID, &req.CreatedAt, &req.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, req)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) AddDecisionAndUpdateStatus(ctx context.Context, decision *domain.ApprovalDecision, newStatus string, newStep int) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Insert Decision
		_, err := tx.Exec(ctx, `
			INSERT INTO approval_decisions (
				approval_decision_id, tenant_id, approval_request_id, step_number,
				decided_by_principal_id, decision, decision_reason, decided_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, decision.ApprovalDecisionID, tenantID, decision.ApprovalRequestID, decision.StepNumber,
			decision.DecidedByPrincipalID, decision.Decision, decision.DecisionReason, decision.DecidedAt)
		if err != nil {
			return err
		}

		// 2. Update Request Status & Current Step
		res, err := tx.Exec(ctx, `
			UPDATE invoice_approval_requests
			SET status = $1, current_step = $2, updated_at = $3
			WHERE approval_request_id = $4 AND tenant_id = $5
		`, newStatus, newStep, time.Now().UTC(), decision.ApprovalRequestID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrRequestNotFound
		}
		return nil
	})
}

func (s *PgStore) GetDecisionsByRequest(ctx context.Context, requestID string) ([]domain.ApprovalDecision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ApprovalDecision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT approval_decision_id, tenant_id, approval_request_id, step_number,
			       decided_by_principal_id, decision, COALESCE(decision_reason, ''), decided_at
			FROM approval_decisions
			WHERE approval_request_id = $1 AND tenant_id = $2
			ORDER BY step_number ASC, decided_at ASC
		`, requestID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var dec domain.ApprovalDecision
			if err := rows.Scan(
				&dec.ApprovalDecisionID, &dec.TenantID, &dec.ApprovalRequestID, &dec.StepNumber,
				&dec.DecidedByPrincipalID, &dec.Decision, &dec.DecisionReason, &dec.DecidedAt,
			); err != nil {
				return err
			}
			out = append(out, dec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}