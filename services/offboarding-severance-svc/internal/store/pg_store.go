package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/middleware"
)

type Store interface {
	CreateTerminationRequest(ctx context.Context, req *domain.TerminationRequest) error
	GetTerminationRequest(ctx context.Context, id string) (*domain.TerminationRequest, error)
	ListTerminationRequests(ctx context.Context, legalEntityID string) ([]domain.TerminationRequest, error)
	ApproveTerminationRequest(ctx context.Context, id string, approvedBy string) (*domain.TerminationRequest, error)
	FinalizeEmployeeTermination(ctx context.Context, id string) (*domain.TerminationRequest, error)
	
	CreateOffboardingChecklist(ctx context.Context, chk *domain.OffboardingChecklist) error
	GetOffboardingChecklist(ctx context.Context, employeeID string) (*domain.OffboardingChecklist, error)
	UpdateChecklistItemStatus(ctx context.Context, itemID string, status domain.ChecklistItemStatus, completedBy string) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	return err
}

func (s *PgStore) CreateTerminationRequest(ctx context.Context, req *domain.TerminationRequest) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if req.TerminationID == "" {
		req.TerminationID = "term-" + uuid.New().String()
	}
	req.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	req.CreatedAt = now
	req.UpdatedAt = now
	if req.Status == "" {
		req.Status = domain.TerminationStatusInitiated
	}

	query := `
		INSERT INTO termination_requests (
			termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
			reason_code, reason_details, notice_period_days, last_working_day, effective_from,
			status, initiated_by, severance_amount, currency, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`
	_, err = tx.Exec(ctx, query,
		req.TerminationID, req.TenantID, req.LegalEntityID, req.EmployeeID, string(req.TerminationType),
		req.ReasonCode, req.ReasonDetails, req.NoticePeriodDays, req.LastWorkingDay, req.EffectiveFrom,
		string(req.Status), req.InitiatedBy, req.SeveranceAmount, req.Currency, req.CreatedAt, req.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert termination request: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetTerminationRequest(ctx context.Context, id string) (*domain.TerminationRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	query := `
		SELECT termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
		       reason_code, COALESCE(reason_details, ''), notice_period_days, last_working_day, effective_from,
		       effective_to, status, initiated_by, approved_by, approved_at, severance_amount, currency,
		       created_at, updated_at
		FROM termination_requests
		WHERE termination_id = $1
	`
	var req domain.TerminationRequest
	var termType, status string
	err = tx.QueryRow(ctx, query, id).Scan(
		&req.TerminationID, &req.TenantID, &req.LegalEntityID, &req.EmployeeID, &termType,
		&req.ReasonCode, &req.ReasonDetails, &req.NoticePeriodDays, &req.LastWorkingDay, &req.EffectiveFrom,
		&req.EffectiveTo, &status, &req.InitiatedBy, &req.ApprovedBy, &req.ApprovedAt, &req.SeveranceAmount, &req.Currency,
		&req.CreatedAt, &req.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrTerminationNotFound
		}
		return nil, err
	}
	req.TerminationType = domain.TerminationType(termType)
	req.Status = domain.TerminationStatus(status)

	_ = tx.Commit(ctx)
	return &req, nil
}

func (s *PgStore) ListTerminationRequests(ctx context.Context, legalEntityID string) ([]domain.TerminationRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	query := `
		SELECT termination_id, tenant_id, legal_entity_id, employee_id, termination_type,
		       reason_code, COALESCE(reason_details, ''), notice_period_days, last_working_day, effective_from,
		       effective_to, status, initiated_by, approved_by, approved_at, severance_amount, currency,
		       created_at, updated_at
		FROM termination_requests
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := tx.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TerminationRequest
	for rows.Next() {
		var req domain.TerminationRequest
		var termType, status string
		err := rows.Scan(
			&req.TerminationID, &req.TenantID, &req.LegalEntityID, &req.EmployeeID, &termType,
			&req.ReasonCode, &req.ReasonDetails, &req.NoticePeriodDays, &req.LastWorkingDay, &req.EffectiveFrom,
			&req.EffectiveTo, &status, &req.InitiatedBy, &req.ApprovedBy, &req.ApprovedAt, &req.SeveranceAmount, &req.Currency,
			&req.CreatedAt, &req.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		req.TerminationType = domain.TerminationType(termType)
		req.Status = domain.TerminationStatus(status)
		out = append(out, req)
	}

	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) ApproveTerminationRequest(ctx context.Context, id string, approvedBy string) (*domain.TerminationRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	req, err := s.GetTerminationRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Status != domain.TerminationStatusInitiated {
		return nil, domain.ErrAlreadyApproved
	}

	now := time.Now().UTC()
	req.Status = domain.TerminationStatusApproved
	req.ApprovedBy = &approvedBy
	req.ApprovedAt = &now
	req.UpdatedAt = now

	query := `
		UPDATE termination_requests
		SET status = $1, approved_by = $2, approved_at = $3, updated_at = $4
		WHERE termination_id = $5
	`
	_, err = tx.Exec(ctx, query, string(req.Status), req.ApprovedBy, req.ApprovedAt, req.UpdatedAt, id)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *PgStore) FinalizeEmployeeTermination(ctx context.Context, id string) (*domain.TerminationRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	req, err := s.GetTerminationRequest(ctx, id)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	req.Status = domain.TerminationStatusTerminated
	req.EffectiveTo = &todayStr
	req.UpdatedAt = now

	query := `
		UPDATE termination_requests
		SET status = $1, effective_to = $2, updated_at = $3
		WHERE termination_id = $4
	`
	_, err = tx.Exec(ctx, query, string(req.Status), req.EffectiveTo, req.UpdatedAt, id)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *PgStore) CreateOffboardingChecklist(ctx context.Context, chk *domain.OffboardingChecklist) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if chk.ChecklistID == "" {
		chk.ChecklistID = "chk-" + uuid.New().String()
	}
	chk.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	chk.CreatedAt = now
	chk.UpdatedAt = now
	if chk.Status == "" {
		chk.Status = "OPEN"
	}

	queryChk := `
		INSERT INTO offboarding_checklists (checklist_id, tenant_id, legal_entity_id, employee_id, termination_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`
	_, err = tx.Exec(ctx, queryChk, chk.ChecklistID, chk.TenantID, chk.LegalEntityID, chk.EmployeeID, chk.TerminationID, chk.Status, chk.CreatedAt, chk.UpdatedAt)
	if err != nil {
		return err
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
		_, err = tx.Exec(ctx, queryItem, item.ItemID, item.ChecklistID, chk.TenantID, item.Category, item.Description, string(item.Status), now, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetOffboardingChecklist(ctx context.Context, employeeID string) (*domain.OffboardingChecklist, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	queryChk := `
		SELECT checklist_id, tenant_id, legal_entity_id, employee_id, termination_id, status, created_at, updated_at
		FROM offboarding_checklists
		WHERE employee_id = $1
		ORDER BY created_at DESC LIMIT 1
	`
	var chk domain.OffboardingChecklist
	err = tx.QueryRow(ctx, queryChk, employeeID).Scan(
		&chk.ChecklistID, &chk.TenantID, &chk.LegalEntityID, &chk.EmployeeID, &chk.TerminationID, &chk.Status, &chk.CreatedAt, &chk.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrChecklistNotFound
		}
		return nil, err
	}

	queryItems := `
		SELECT item_id, checklist_id, category, description, status, completed_by, completed_at
		FROM checklist_items
		WHERE checklist_id = $1
	`
	rows, err := tx.Query(ctx, queryItems, chk.ChecklistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var item domain.ChecklistItem
		var status string
		err := rows.Scan(&item.ItemID, &item.ChecklistID, &item.Category, &item.Description, &status, &item.CompletedBy, &item.CompletedAt)
		if err != nil {
			return nil, err
		}
		item.Status = domain.ChecklistItemStatus(status)
		chk.Items = append(chk.Items, item)
	}

	_ = tx.Commit(ctx)
	return &chk, nil
}

func (s *PgStore) UpdateChecklistItemStatus(ctx context.Context, itemID string, status domain.ChecklistItemStatus, completedBy string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	now := time.Now().UTC()
	query := `
		UPDATE checklist_items
		SET status = $1, completed_by = $2, completed_at = $3, updated_at = $4
		WHERE item_id = $5
	`
	_, err = tx.Exec(ctx, query, string(status), completedBy, now, now, itemID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
