package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

type Store interface {
	CreateWorkAuth(ctx context.Context, auth *domain.WorkAuthorization) error
	GetWorkAuth(ctx context.Context, employeeID string) (*domain.WorkAuthorization, error)
	VerifyWorkAuth(ctx context.Context, authID string, verifiedBy string) (*domain.WorkAuthorization, error)

	CreateVisaRecord(ctx context.Context, visa *domain.VisaRecord) error
	GetVisaRecord(ctx context.Context, employeeID string) (*domain.VisaRecord, error)
	FlagVisaExpiration(ctx context.Context, visaID string) (*domain.VisaRecord, error)

	LogWorkingHours(ctx context.Context, log *domain.WorkingHourLog) error
	GetWeeklyHours(ctx context.Context, employeeID string, startDate string) (float64, error)

	CreateComplianceAlert(ctx context.Context, alert *domain.ComplianceAlert) error
	ListComplianceAlerts(ctx context.Context, legalEntityID string) ([]domain.ComplianceAlert, error)
	ResolveComplianceAlert(ctx context.Context, alertID string, resolvedBy string) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

func (s *PgStore) CreateWorkAuth(ctx context.Context, auth *domain.WorkAuthorization) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if auth.AuthID == "" {
		auth.AuthID = "auth-" + uuid.New().String()
	}
	auth.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	auth.CreatedAt = now
	auth.UpdatedAt = now
	if auth.Status == "" {
		auth.Status = domain.VerificationStatusPending
	}

	query := `
		INSERT INTO work_authorizations (
			auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
			issue_date, expiry_date, status, effective_from, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`
	_, err = tx.Exec(ctx, query,
		auth.AuthID, auth.TenantID, auth.LegalEntityID, auth.EmployeeID, auth.DocumentType, auth.DocumentNumber,
		auth.IssueDate, auth.ExpiryDate, string(auth.Status), auth.EffectiveFrom, auth.CreatedAt, auth.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert work authorization: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetWorkAuth(ctx context.Context, employeeID string) (*domain.WorkAuthorization, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	query := `
		SELECT auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
		       issue_date, expiry_date, status, verified_by, verified_at, effective_from, effective_to,
		       created_at, updated_at
		FROM work_authorizations
		WHERE employee_id = $1
		ORDER BY created_at DESC LIMIT 1
	`
	var auth domain.WorkAuthorization
	var status string
	err = tx.QueryRow(ctx, query, employeeID).Scan(
		&auth.AuthID, &auth.TenantID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
		&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt, &auth.EffectiveFrom, &auth.EffectiveTo,
		&auth.CreatedAt, &auth.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, err
	}
	auth.Status = domain.VerificationStatus(status)

	_ = tx.Commit(ctx)
	return &auth, nil
}

func (s *PgStore) VerifyWorkAuth(ctx context.Context, authID string, verifiedBy string) (*domain.WorkAuthorization, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	query := `
		UPDATE work_authorizations
		SET status = 'VERIFIED', verified_by = $1, verified_at = $2, updated_at = $3
		WHERE auth_id = $4
		RETURNING auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
		          issue_date, expiry_date, status, verified_by, verified_at, effective_from, effective_to,
		          created_at, updated_at
	`
	var auth domain.WorkAuthorization
	var status string
	err = tx.QueryRow(ctx, query, verifiedBy, now, now, authID).Scan(
		&auth.AuthID, &auth.TenantID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
		&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt, &auth.EffectiveFrom, &auth.EffectiveTo,
		&auth.CreatedAt, &auth.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, err
	}
	auth.Status = domain.VerificationStatus(status)

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &auth, nil
}

func (s *PgStore) CreateVisaRecord(ctx context.Context, visa *domain.VisaRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if visa.VisaID == "" {
		visa.VisaID = "visa-" + uuid.New().String()
	}
	visa.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	visa.CreatedAt = now
	visa.UpdatedAt = now
	if visa.Status == "" {
		visa.Status = domain.VerificationStatusVerified
	}

	query := `
		INSERT INTO visa_records (
			visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
			expiration_date, grace_period_days, status, flagged_for_expiry, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`
	_, err = tx.Exec(ctx, query,
		visa.VisaID, visa.TenantID, visa.LegalEntityID, visa.EmployeeID, visa.VisaType, visa.IssuingCountry,
		visa.ExpirationDate, visa.GracePeriodDays, string(visa.Status), visa.FlaggedForExpiry, visa.CreatedAt, visa.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert visa record: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetVisaRecord(ctx context.Context, employeeID string) (*domain.VisaRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	query := `
		SELECT visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
		       expiration_date, grace_period_days, status, flagged_for_expiry, created_at, updated_at
		FROM visa_records
		WHERE employee_id = $1
		ORDER BY created_at DESC LIMIT 1
	`
	var visa domain.VisaRecord
	var status string
	err = tx.QueryRow(ctx, query, employeeID).Scan(
		&visa.VisaID, &visa.TenantID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
		&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CreatedAt, &visa.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, err
	}
	visa.Status = domain.VerificationStatus(status)

	_ = tx.Commit(ctx)
	return &visa, nil
}

func (s *PgStore) FlagVisaExpiration(ctx context.Context, visaID string) (*domain.VisaRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	query := `
		UPDATE visa_records
		SET flagged_for_expiry = TRUE, updated_at = $1
		WHERE visa_id = $2
		RETURNING visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
		          expiration_date, grace_period_days, status, flagged_for_expiry, created_at, updated_at
	`
	var visa domain.VisaRecord
	var status string
	err = tx.QueryRow(ctx, query, now, visaID).Scan(
		&visa.VisaID, &visa.TenantID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
		&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CreatedAt, &visa.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	visa.Status = domain.VerificationStatus(status)

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &visa, nil
}

func (s *PgStore) LogWorkingHours(ctx context.Context, log *domain.WorkingHourLog) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if log.LogID == "" {
		log.LogID = "log-" + uuid.New().String()
	}
	log.TenantID = middleware.GetTenantID(ctx)
	log.CreatedAt = time.Now().UTC()

	query := `
		INSERT INTO working_hour_logs (
			log_id, tenant_id, legal_entity_id, employee_id, work_date, hours_worked,
			overtime_hours, weekly_accumulated, is_breached, max_allowed_hours, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err = tx.Exec(ctx, query,
		log.LogID, log.TenantID, log.LegalEntityID, log.EmployeeID, log.WorkDate, log.HoursWorked,
		log.OvertimeHours, log.WeeklyAccumulated, log.IsBreached, log.MaxAllowedHours, log.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert working hour log: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetWeeklyHours(ctx context.Context, employeeID string, startDate string) (float64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return 0, err
	}

	query := `
		SELECT COALESCE(SUM(hours_worked), 0)
		FROM working_hour_logs
		WHERE employee_id = $1 AND work_date >= $2
	`
	var total float64
	err = tx.QueryRow(ctx, query, employeeID, startDate).Scan(&total)
	if err != nil {
		return 0, err
	}

	_ = tx.Commit(ctx)
	return total, nil
}

func (s *PgStore) CreateComplianceAlert(ctx context.Context, alert *domain.ComplianceAlert) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if alert.AlertID == "" {
		alert.AlertID = "alt-" + uuid.New().String()
	}
	alert.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	alert.CreatedAt = now
	alert.UpdatedAt = now

	query := `
		INSERT INTO compliance_alerts (
			alert_id, tenant_id, legal_entity_id, employee_id, category, severity,
			message, is_resolved, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	_, err = tx.Exec(ctx, query,
		alert.AlertID, alert.TenantID, alert.LegalEntityID, alert.EmployeeID, alert.Category, string(alert.Severity),
		alert.Message, alert.IsResolved, alert.CreatedAt, alert.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert compliance alert: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) ListComplianceAlerts(ctx context.Context, legalEntityID string) ([]domain.ComplianceAlert, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	query := `
		SELECT alert_id, tenant_id, legal_entity_id, employee_id, category, severity,
		       message, is_resolved, resolved_by, resolved_at, created_at, updated_at
		FROM compliance_alerts
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := tx.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ComplianceAlert
	for rows.Next() {
		var a domain.ComplianceAlert
		var sev string
		err := rows.Scan(
			&a.AlertID, &a.TenantID, &a.LegalEntityID, &a.EmployeeID, &a.Category, &sev,
			&a.Message, &a.IsResolved, &a.ResolvedBy, &a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		a.Severity = domain.AlertSeverity(sev)
		out = append(out, a)
	}

	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) ResolveComplianceAlert(ctx context.Context, alertID string, resolvedBy string) error {
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
		UPDATE compliance_alerts
		SET is_resolved = TRUE, resolved_by = $1, resolved_at = $2, updated_at = $3
		WHERE alert_id = $4
	`
	_, err = tx.Exec(ctx, query, resolvedBy, now, now, alertID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
