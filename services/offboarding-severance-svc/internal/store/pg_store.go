// Package store provides the PostgreSQL implementation of
// offboarding-severance-svc's persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policy is real and correctly
// written. But every method ALSO filters explicitly by tenant_id in its
// own SQL, rather than relying on RLS alone: this pool connects as a
// Postgres superuser (DB_USER=postgres, same as every other service in
// this platform), and Postgres superusers unconditionally bypass
// Row-Level Security regardless of policy.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/middleware"
)

type Store interface {
	CreateTerminationRequest(ctx context.Context, req *domain.TerminationRequest) (created bool, err error)
	GetTerminationRequest(ctx context.Context, id string) (*domain.TerminationRequest, error)
	ListTerminationRequests(ctx context.Context, legalEntityID string) ([]domain.TerminationRequest, error)
	ApproveTerminationRequest(ctx context.Context, id string, approvedBy string) (*domain.TerminationRequest, error)
	FinalizeEmployeeTermination(ctx context.Context, id string) (*domain.TerminationRequest, error)

	CreateOffboardingChecklist(ctx context.Context, chk *domain.OffboardingChecklist) (created bool, err error)
	GetOffboardingChecklist(ctx context.Context, employeeID string) (*domain.OffboardingChecklist, error)
	GetChecklistItemLegalEntity(ctx context.Context, itemID string) (string, error)
	UpdateChecklistItemStatus(ctx context.Context, itemID string, status domain.ChecklistItemStatus, completedBy string) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withRLS begins a transaction, sets the RLS tenant context via
// set_config (SET LOCAL does not accept bind parameters — this must be a
// function call, not a SET statement), and commits on success.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func requireTenant(ctx context.Context) (string, error) {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return "", domain.ErrIdentityMissing
	}
	return tenantID, nil
}

// CreateTerminationRequest inserts a termination request in INITIATED
// status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL request —
// mutating *req in place to reflect it — rather than creating a duplicate.
func (s *PgStore) CreateTerminationRequest(ctx context.Context, req *domain.TerminationRequest) (created bool, err error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if req.TerminationID == "" {
			req.TerminationID = "term-" + uuid.New().String()
		}
		req.TenantID = tenantID
		now := time.Now().UTC()
		req.CreatedAt = now
		req.UpdatedAt = now
		if req.Status == "" {
			req.Status = domain.TerminationStatusInitiated
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO termination_requests (
				termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
				reason_code, reason_details, notice_period_days, last_working_day, effective_from,
				status, initiated_by, severance_amount, currency, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`,
			req.TerminationID, req.TenantID, req.LegalEntityID, req.EmployeeID, string(req.TerminationType),
			req.ReasonCode, req.ReasonDetails, req.NoticePeriodDays, req.LastWorkingDay, req.EffectiveFrom,
			string(req.Status), req.InitiatedBy, req.SeveranceAmount, req.Currency, req.CorrelationID, req.CreatedAt, req.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert termination request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT termination_id, legal_entity_id, employee_id, termination_type, reason_code,
				       COALESCE(reason_details, ''), notice_period_days, last_working_day::text, effective_from::text,
				       effective_to::text, status, initiated_by, approved_by, approved_at, severance_amount, currency,
				       created_at, updated_at
				FROM termination_requests WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, req.CorrelationID)
			var termType, status string
			if err := row.Scan(
				&req.TerminationID, &req.LegalEntityID, &req.EmployeeID, &termType, &req.ReasonCode,
				&req.ReasonDetails, &req.NoticePeriodDays, &req.LastWorkingDay, &req.EffectiveFrom,
				&req.EffectiveTo, &status, &req.InitiatedBy, &req.ApprovedBy, &req.ApprovedAt, &req.SeveranceAmount, &req.Currency,
				&req.CreatedAt, &req.UpdatedAt,
			); err != nil {
				return err
			}
			req.TerminationType = domain.TerminationType(termType)
			req.Status = domain.TerminationStatus(status)
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetTerminationRequest(ctx context.Context, id string) (*domain.TerminationRequest, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var req domain.TerminationRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
			       reason_code, COALESCE(reason_details, ''), notice_period_days, last_working_day::text, effective_from::text,
			       effective_to::text, status, initiated_by, approved_by, approved_at, severance_amount, currency,
			       correlation_id, created_at, updated_at
			FROM termination_requests
			WHERE termination_id = $1 AND tenant_id = $2
		`
		var termType, status string
		err := tx.QueryRow(ctx, query, id, tenantID).Scan(
			&req.TerminationID, &req.TenantID, &req.LegalEntityID, &req.EmployeeID, &termType,
			&req.ReasonCode, &req.ReasonDetails, &req.NoticePeriodDays, &req.LastWorkingDay, &req.EffectiveFrom,
			&req.EffectiveTo, &status, &req.InitiatedBy, &req.ApprovedBy, &req.ApprovedAt, &req.SeveranceAmount, &req.Currency,
			&req.CorrelationID, &req.CreatedAt, &req.UpdatedAt,
		)
		if err != nil {
			return err
		}
		req.TerminationType = domain.TerminationType(termType)
		req.Status = domain.TerminationStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTerminationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *PgStore) ListTerminationRequests(ctx context.Context, legalEntityID string) ([]domain.TerminationRequest, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var out []domain.TerminationRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
			       reason_code, COALESCE(reason_details, ''), notice_period_days, last_working_day::text, effective_from::text,
			       effective_to::text, status, initiated_by, approved_by, approved_at, severance_amount, currency,
			       correlation_id, created_at, updated_at
			FROM termination_requests
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2)
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var req domain.TerminationRequest
			var termType, status string
			if err := rows.Scan(
				&req.TerminationID, &req.TenantID, &req.LegalEntityID, &req.EmployeeID, &termType,
				&req.ReasonCode, &req.ReasonDetails, &req.NoticePeriodDays, &req.LastWorkingDay, &req.EffectiveFrom,
				&req.EffectiveTo, &status, &req.InitiatedBy, &req.ApprovedBy, &req.ApprovedAt, &req.SeveranceAmount, &req.Currency,
				&req.CorrelationID, &req.CreatedAt, &req.UpdatedAt,
			); err != nil {
				return err
			}
			req.TerminationType = domain.TerminationType(termType)
			req.Status = domain.TerminationStatus(status)
			out = append(out, req)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApproveTerminationRequest transitions INITIATED -> APPROVED atomically:
// the fromStatus check, the transition, and the tenant scope are one
// UPDATE statement, no separate read-then-write race window (a prior
// version fetched the row, checked its status in Go, then issued an
// unconditional UPDATE — two concurrent approve calls could both pass the
// Go-side check and both "succeed").
func (s *PgStore) ApproveTerminationRequest(ctx context.Context, id string, approvedBy string) (*domain.TerminationRequest, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			UPDATE termination_requests
			SET status = $1, approved_by = $2, approved_at = $3, updated_at = $4
			WHERE termination_id = $5 AND tenant_id = $6 AND status = $7
		`, string(domain.TerminationStatusApproved), approvedBy, now, now, id, tenantID, string(domain.TerminationStatusInitiated))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM termination_requests WHERE termination_id = $1 AND tenant_id = $2)`, id, tenantID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return domain.ErrTerminationNotFound
			}
			return domain.ErrAlreadyApproved
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetTerminationRequest(ctx, id)
}

// FinalizeEmployeeTermination transitions APPROVED -> TERMINATED
// atomically. A termination request can only be finalized after approval
// — a prior version had no status guard at all, allowing an unapproved or
// already-terminated request to be "finalized" repeatedly.
func (s *PgStore) FinalizeEmployeeTermination(ctx context.Context, id string) (*domain.TerminationRequest, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		todayStr := now.Format("2006-01-02")
		tag, err := tx.Exec(ctx, `
			UPDATE termination_requests
			SET status = $1, effective_to = $2, updated_at = $3
			WHERE termination_id = $4 AND tenant_id = $5 AND status = $6
		`, string(domain.TerminationStatusTerminated), todayStr, now, id, tenantID, string(domain.TerminationStatusApproved))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM termination_requests WHERE termination_id = $1 AND tenant_id = $2`, id, tenantID).Scan(&status)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTerminationNotFound
			}
			if err != nil {
				return err
			}
			if status == string(domain.TerminationStatusTerminated) {
				return domain.ErrAlreadyTerminated
			}
			return domain.ErrNotApproved
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetTerminationRequest(ctx, id)
}

// CreateOffboardingChecklist inserts a checklist and its items.
//
// Idempotent on (tenant_id, correlation_id): see CreateTerminationRequest's
// doc comment for the rationale.
func (s *PgStore) CreateOffboardingChecklist(ctx context.Context, chk *domain.OffboardingChecklist) (created bool, err error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if chk.ChecklistID == "" {
			chk.ChecklistID = "chk-" + uuid.New().String()
		}
		chk.TenantID = tenantID
		now := time.Now().UTC()
		chk.CreatedAt = now
		chk.UpdatedAt = now
		if chk.Status == "" {
			chk.Status = "OPEN"
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO offboarding_checklists (checklist_id, tenant_id, legal_entity_id, employee_id, termination_id, status, correlation_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, chk.ChecklistID, chk.TenantID, chk.LegalEntityID, chk.EmployeeID, chk.TerminationID, chk.Status, chk.CorrelationID, chk.CreatedAt, chk.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT checklist_id, legal_entity_id, employee_id, termination_id, status, created_at, updated_at
				FROM offboarding_checklists WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, chk.CorrelationID)
			if err := row.Scan(&chk.ChecklistID, &chk.LegalEntityID, &chk.EmployeeID, &chk.TerminationID, &chk.Status, &chk.CreatedAt, &chk.UpdatedAt); err != nil {
				return err
			}
			itemRows, err := tx.Query(ctx, `
				SELECT item_id, checklist_id, category, description, status, completed_by, completed_at
				FROM checklist_items WHERE tenant_id = $1 AND checklist_id = $2
			`, tenantID, chk.ChecklistID)
			if err != nil {
				return err
			}
			defer itemRows.Close()
			chk.Items = nil
			for itemRows.Next() {
				var item domain.ChecklistItem
				var status string
				if err := itemRows.Scan(&item.ItemID, &item.ChecklistID, &item.Category, &item.Description, &status, &item.CompletedBy, &item.CompletedAt); err != nil {
					return err
				}
				item.Status = domain.ChecklistItemStatus(status)
				chk.Items = append(chk.Items, item)
			}
			created = false
			return itemRows.Err()
		}

		queryItem := `
			INSERT INTO checklist_items (item_id, checklist_id, tenant_id, category, description, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`
		for i := range chk.Items {
			item := &chk.Items[i]
			if item.ItemID == "" {
				item.ItemID = "item-" + uuid.New().String()
			}
			item.ChecklistID = chk.ChecklistID
			if item.Status == "" {
				item.Status = domain.ChecklistItemStatusPending
			}
			if _, err := tx.Exec(ctx, queryItem, item.ItemID, item.ChecklistID, chk.TenantID, item.Category, item.Description, string(item.Status), now, now); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetOffboardingChecklist(ctx context.Context, employeeID string) (*domain.OffboardingChecklist, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var chk domain.OffboardingChecklist
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		queryChk := `
			SELECT checklist_id, tenant_id, legal_entity_id, employee_id, termination_id, status, correlation_id, created_at, updated_at
			FROM offboarding_checklists
			WHERE employee_id = $1 AND tenant_id = $2
			ORDER BY created_at DESC LIMIT 1
		`
		if err := tx.QueryRow(ctx, queryChk, employeeID, tenantID).Scan(
			&chk.ChecklistID, &chk.TenantID, &chk.LegalEntityID, &chk.EmployeeID, &chk.TerminationID, &chk.Status, &chk.CorrelationID, &chk.CreatedAt, &chk.UpdatedAt,
		); err != nil {
			return err
		}

		queryItems := `
			SELECT item_id, checklist_id, category, description, status, completed_by, completed_at
			FROM checklist_items
			WHERE checklist_id = $1 AND tenant_id = $2
		`
		rows, err := tx.Query(ctx, queryItems, chk.ChecklistID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var item domain.ChecklistItem
			var status string
			if err := rows.Scan(&item.ItemID, &item.ChecklistID, &item.Category, &item.Description, &status, &item.CompletedBy, &item.CompletedAt); err != nil {
				return err
			}
			item.Status = domain.ChecklistItemStatus(status)
			chk.Items = append(chk.Items, item)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrChecklistNotFound
	}
	if err != nil {
		return nil, err
	}
	return &chk, nil
}

// GetChecklistItemLegalEntity resolves the legal entity that owns a
// checklist item, so the handler can authorize the mutation against the
// correct entity before calling UpdateChecklistItemStatus.
func (s *PgStore) GetChecklistItemLegalEntity(ctx context.Context, itemID string) (string, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return "", err
	}

	var legalEntityID string
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT oc.legal_entity_id
			FROM checklist_items ci
			JOIN offboarding_checklists oc ON oc.checklist_id = ci.checklist_id AND oc.tenant_id = ci.tenant_id
			WHERE ci.item_id = $1 AND ci.tenant_id = $2
		`, itemID, tenantID).Scan(&legalEntityID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrItemNotFound
	}
	if err != nil {
		return "", err
	}
	return legalEntityID, nil
}

func (s *PgStore) UpdateChecklistItemStatus(ctx context.Context, itemID string, status domain.ChecklistItemStatus, completedBy string) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return err
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			UPDATE checklist_items
			SET status = $1, completed_by = $2, completed_at = $3, updated_at = $4
			WHERE item_id = $5 AND tenant_id = $6
		`, string(status), completedBy, now, now, itemID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrItemNotFound
		}
		return nil
	})
}
