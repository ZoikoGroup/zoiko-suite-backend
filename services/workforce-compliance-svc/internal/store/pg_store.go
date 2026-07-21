// Package store provides the PostgreSQL implementation of
// workforce-compliance-svc's persistence layer.
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

	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

type Store interface {
	CreateWorkAuth(ctx context.Context, auth *domain.WorkAuthorization) (created bool, err error)
	GetWorkAuth(ctx context.Context, employeeID string) (*domain.WorkAuthorization, error)
	GetWorkAuthByID(ctx context.Context, authID string) (*domain.WorkAuthorization, error)
	VerifyWorkAuth(ctx context.Context, authID string, verifiedBy string) (*domain.WorkAuthorization, error)

	CreateVisaRecord(ctx context.Context, visa *domain.VisaRecord) (created bool, err error)
	GetVisaRecord(ctx context.Context, employeeID string) (*domain.VisaRecord, error)
	FlagVisaExpiration(ctx context.Context, visaID string) (*domain.VisaRecord, error)

	LogWorkingHours(ctx context.Context, log *domain.WorkingHourLog) (created bool, err error)
	GetWeeklyHours(ctx context.Context, employeeID string, startDate string) (float64, error)

	CreateComplianceAlert(ctx context.Context, alert *domain.ComplianceAlert) error
	GetComplianceAlert(ctx context.Context, alertID string) (*domain.ComplianceAlert, error)
	ListComplianceAlerts(ctx context.Context, legalEntityID string) ([]domain.ComplianceAlert, error)
	ResolveComplianceAlert(ctx context.Context, alertID string, resolvedBy string) error
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

// CreateWorkAuth inserts a work authorization record in PENDING status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL record —
// mutating *auth in place to reflect it — rather than creating a
// duplicate.
func (s *PgStore) CreateWorkAuth(ctx context.Context, auth *domain.WorkAuthorization) (created bool, err error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if auth.AuthID == "" {
			auth.AuthID = "auth-" + uuid.New().String()
		}
		auth.TenantID = tenantID
		now := time.Now().UTC()
		auth.CreatedAt = now
		auth.UpdatedAt = now
		if auth.Status == "" {
			auth.Status = domain.VerificationStatusPending
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO work_authorizations (
				auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
				issue_date, expiry_date, status, effective_from, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`,
			auth.AuthID, auth.TenantID, auth.LegalEntityID, auth.EmployeeID, auth.DocumentType, auth.DocumentNumber,
			auth.IssueDate, auth.ExpiryDate, string(auth.Status), auth.EffectiveFrom, auth.CorrelationID, auth.CreatedAt, auth.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert work authorization: %w", err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT auth_id, legal_entity_id, employee_id, document_type, document_number,
				       issue_date::text, expiry_date::text, status, verified_by, verified_at,
				       effective_from::text, effective_to::text, created_at, updated_at
				FROM work_authorizations WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, auth.CorrelationID)
			var status string
			if err := row.Scan(
				&auth.AuthID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
				&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt,
				&auth.EffectiveFrom, &auth.EffectiveTo, &auth.CreatedAt, &auth.UpdatedAt,
			); err != nil {
				return err
			}
			auth.Status = domain.VerificationStatus(status)
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetWorkAuth(ctx context.Context, employeeID string) (*domain.WorkAuthorization, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var auth domain.WorkAuthorization
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
			       issue_date::text, expiry_date::text, status, verified_by, verified_at,
			       effective_from::text, effective_to::text, correlation_id, created_at, updated_at
			FROM work_authorizations
			WHERE employee_id = $1 AND tenant_id = $2
			ORDER BY created_at DESC LIMIT 1
		`
		var status string
		err := tx.QueryRow(ctx, query, employeeID, tenantID).Scan(
			&auth.AuthID, &auth.TenantID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
			&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt, &auth.EffectiveFrom, &auth.EffectiveTo,
			&auth.CorrelationID, &auth.CreatedAt, &auth.UpdatedAt,
		)
		if err != nil {
			return err
		}
		auth.Status = domain.VerificationStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return &auth, nil
}

// GetWorkAuthByID resolves a work authorization by its primary key — used
// by the handler to authorize a verify action against the record's real
// legal entity before mutating it.
func (s *PgStore) GetWorkAuthByID(ctx context.Context, authID string) (*domain.WorkAuthorization, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var auth domain.WorkAuthorization
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
			       issue_date::text, expiry_date::text, status, verified_by, verified_at,
			       effective_from::text, effective_to::text, correlation_id, created_at, updated_at
			FROM work_authorizations
			WHERE auth_id = $1 AND tenant_id = $2
		`
		var status string
		err := tx.QueryRow(ctx, query, authID, tenantID).Scan(
			&auth.AuthID, &auth.TenantID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
			&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt, &auth.EffectiveFrom, &auth.EffectiveTo,
			&auth.CorrelationID, &auth.CreatedAt, &auth.UpdatedAt,
		)
		if err != nil {
			return err
		}
		auth.Status = domain.VerificationStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return &auth, nil
}

// VerifyWorkAuth transitions PENDING -> VERIFIED atomically: the
// fromStatus check, the transition, and the tenant scope are one UPDATE
// statement, no separate read-then-write race window.
func (s *PgStore) VerifyWorkAuth(ctx context.Context, authID string, verifiedBy string) (*domain.WorkAuthorization, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var auth domain.WorkAuthorization
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		query := `
			UPDATE work_authorizations
			SET status = 'VERIFIED', verified_by = $1, verified_at = $2, updated_at = $3
			WHERE auth_id = $4 AND tenant_id = $5 AND status = $6
			RETURNING auth_id, tenant_id, legal_entity_id, employee_id, document_type, document_number,
			          issue_date::text, expiry_date::text, status, verified_by, verified_at,
			          effective_from::text, effective_to::text, created_at, updated_at
		`
		var status string
		err := tx.QueryRow(ctx, query, verifiedBy, now, now, authID, tenantID, string(domain.VerificationStatusPending)).Scan(
			&auth.AuthID, &auth.TenantID, &auth.LegalEntityID, &auth.EmployeeID, &auth.DocumentType, &auth.DocumentNumber,
			&auth.IssueDate, &auth.ExpiryDate, &status, &auth.VerifiedBy, &auth.VerifiedAt, &auth.EffectiveFrom, &auth.EffectiveTo,
			&auth.CreatedAt, &auth.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_authorizations WHERE auth_id = $1 AND tenant_id = $2)`, authID, tenantID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return domain.ErrRecordNotFound
			}
			return fmt.Errorf("work authorization is not in PENDING status")
		}
		if err != nil {
			return err
		}
		auth.Status = domain.VerificationStatus(status)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &auth, nil
}

// CreateVisaRecord inserts a visa record.
//
// Idempotent on (tenant_id, correlation_id): see CreateWorkAuth's doc
// comment for the rationale.
func (s *PgStore) CreateVisaRecord(ctx context.Context, visa *domain.VisaRecord) (created bool, err error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if visa.VisaID == "" {
			visa.VisaID = "visa-" + uuid.New().String()
		}
		visa.TenantID = tenantID
		now := time.Now().UTC()
		visa.CreatedAt = now
		visa.UpdatedAt = now
		if visa.Status == "" {
			visa.Status = domain.VerificationStatusVerified
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO visa_records (
				visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
				expiration_date, grace_period_days, status, flagged_for_expiry, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`,
			visa.VisaID, visa.TenantID, visa.LegalEntityID, visa.EmployeeID, visa.VisaType, visa.IssuingCountry,
			visa.ExpirationDate, visa.GracePeriodDays, string(visa.Status), visa.FlaggedForExpiry, visa.CorrelationID, visa.CreatedAt, visa.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert visa record: %w", err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT visa_id, legal_entity_id, employee_id, visa_type, issuing_country,
				       expiration_date::text, grace_period_days, status, flagged_for_expiry, created_at, updated_at
				FROM visa_records WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, visa.CorrelationID)
			var status string
			if err := row.Scan(
				&visa.VisaID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
				&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CreatedAt, &visa.UpdatedAt,
			); err != nil {
				return err
			}
			visa.Status = domain.VerificationStatus(status)
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetVisaRecord(ctx context.Context, employeeID string) (*domain.VisaRecord, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var visa domain.VisaRecord
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
			       expiration_date::text, grace_period_days, status, flagged_for_expiry, correlation_id, created_at, updated_at
			FROM visa_records
			WHERE employee_id = $1 AND tenant_id = $2
			ORDER BY created_at DESC LIMIT 1
		`
		var status string
		err := tx.QueryRow(ctx, query, employeeID, tenantID).Scan(
			&visa.VisaID, &visa.TenantID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
			&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CorrelationID, &visa.CreatedAt, &visa.UpdatedAt,
		)
		if err != nil {
			return err
		}
		visa.Status = domain.VerificationStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return &visa, nil
}

// FlagVisaExpiration marks a visa as flagged, atomically guarded so a
// retried call doesn't raise a second duplicate compliance alert for the
// same visa (the handler creates one alert per successful flag).
func (s *PgStore) FlagVisaExpiration(ctx context.Context, visaID string) (*domain.VisaRecord, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var visa domain.VisaRecord
	var alreadyFlagged bool
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		query := `
			UPDATE visa_records
			SET flagged_for_expiry = TRUE, updated_at = $1
			WHERE visa_id = $2 AND tenant_id = $3 AND flagged_for_expiry = FALSE
			RETURNING visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
			          expiration_date::text, grace_period_days, status, flagged_for_expiry, created_at, updated_at
		`
		var status string
		err := tx.QueryRow(ctx, query, now, visaID, tenantID).Scan(
			&visa.VisaID, &visa.TenantID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
			&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CreatedAt, &visa.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			// Either not found, or already flagged — fetch to distinguish
			// and return the current row either way (flagging is
			// idempotent from the caller's perspective; only the
			// downstream alert-raising step must not be duplicated).
			row := tx.QueryRow(ctx, `
				SELECT visa_id, tenant_id, legal_entity_id, employee_id, visa_type, issuing_country,
				       expiration_date::text, grace_period_days, status, flagged_for_expiry, created_at, updated_at
				FROM visa_records WHERE visa_id = $1 AND tenant_id = $2
			`, visaID, tenantID)
			if err := row.Scan(
				&visa.VisaID, &visa.TenantID, &visa.LegalEntityID, &visa.EmployeeID, &visa.VisaType, &visa.IssuingCountry,
				&visa.ExpirationDate, &visa.GracePeriodDays, &status, &visa.FlaggedForExpiry, &visa.CreatedAt, &visa.UpdatedAt,
			); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.ErrRecordNotFound
				}
				return err
			}
			alreadyFlagged = true
			visa.Status = domain.VerificationStatus(status)
			return nil
		}
		if err != nil {
			return err
		}
		visa.Status = domain.VerificationStatus(status)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if alreadyFlagged {
		return &visa, domain.ErrAlreadyFlagged
	}
	return &visa, nil
}

// LogWorkingHours inserts a working-hour log entry.
//
// Idempotent on (tenant_id, correlation_id): without this, a retried call
// would double-count an employee's weekly accumulated hours, which feeds
// directly into the statutory breach determination below it.
func (s *PgStore) LogWorkingHours(ctx context.Context, log *domain.WorkingHourLog) (created bool, err error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if log.LogID == "" {
			log.LogID = "log-" + uuid.New().String()
		}
		log.TenantID = tenantID
		log.CreatedAt = time.Now().UTC()

		tag, err := tx.Exec(ctx, `
			INSERT INTO working_hour_logs (
				log_id, tenant_id, legal_entity_id, employee_id, work_date, hours_worked,
				overtime_hours, weekly_accumulated, is_breached, max_allowed_hours, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`,
			log.LogID, log.TenantID, log.LegalEntityID, log.EmployeeID, log.WorkDate, log.HoursWorked,
			log.OvertimeHours, log.WeeklyAccumulated, log.IsBreached, log.MaxAllowedHours, log.CorrelationID, log.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert working hour log: %w", err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT log_id, legal_entity_id, employee_id, work_date::text, hours_worked,
				       overtime_hours, weekly_accumulated, is_breached, max_allowed_hours, created_at
				FROM working_hour_logs WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, log.CorrelationID)
			if err := row.Scan(
				&log.LogID, &log.LegalEntityID, &log.EmployeeID, &log.WorkDate, &log.HoursWorked,
				&log.OvertimeHours, &log.WeeklyAccumulated, &log.IsBreached, &log.MaxAllowedHours, &log.CreatedAt,
			); err != nil {
				return err
			}
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetWeeklyHours(ctx context.Context, employeeID string, startDate string) (float64, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return 0, err
	}

	var total float64
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT COALESCE(SUM(hours_worked), 0)
			FROM working_hour_logs
			WHERE employee_id = $1 AND work_date >= $2 AND tenant_id = $3
		`
		return tx.QueryRow(ctx, query, employeeID, startDate, tenantID).Scan(&total)
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func (s *PgStore) CreateComplianceAlert(ctx context.Context, alert *domain.ComplianceAlert) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return err
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if alert.AlertID == "" {
			alert.AlertID = "alt-" + uuid.New().String()
		}
		alert.TenantID = tenantID
		now := time.Now().UTC()
		alert.CreatedAt = now
		alert.UpdatedAt = now

		query := `
			INSERT INTO compliance_alerts (
				alert_id, tenant_id, legal_entity_id, employee_id, category, severity,
				message, is_resolved, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`
		_, err := tx.Exec(ctx, query,
			alert.AlertID, alert.TenantID, alert.LegalEntityID, alert.EmployeeID, alert.Category, string(alert.Severity),
			alert.Message, alert.IsResolved, alert.CreatedAt, alert.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert compliance alert: %w", err)
		}
		return nil
	})
}

// GetComplianceAlert resolves an alert by its primary key — used by the
// handler to authorize a resolve action against the alert's real legal
// entity, instead of listing every alert in the tenant and scanning for
// a match in Go.
func (s *PgStore) GetComplianceAlert(ctx context.Context, alertID string) (*domain.ComplianceAlert, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var a domain.ComplianceAlert
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var sev string
		err := tx.QueryRow(ctx, `
			SELECT alert_id, tenant_id, legal_entity_id, employee_id, category, severity,
			       message, is_resolved, resolved_by, resolved_at, created_at, updated_at
			FROM compliance_alerts WHERE alert_id = $1 AND tenant_id = $2
		`, alertID, tenantID).Scan(
			&a.AlertID, &a.TenantID, &a.LegalEntityID, &a.EmployeeID, &a.Category, &sev,
			&a.Message, &a.IsResolved, &a.ResolvedBy, &a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt,
		)
		if err != nil {
			return err
		}
		a.Severity = domain.AlertSeverity(sev)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAlertNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) ListComplianceAlerts(ctx context.Context, legalEntityID string) ([]domain.ComplianceAlert, error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}

	var out []domain.ComplianceAlert
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT alert_id, tenant_id, legal_entity_id, employee_id, category, severity,
			       message, is_resolved, resolved_by, resolved_at, created_at, updated_at
			FROM compliance_alerts
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2)
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var a domain.ComplianceAlert
			var sev string
			if err := rows.Scan(
				&a.AlertID, &a.TenantID, &a.LegalEntityID, &a.EmployeeID, &a.Category, &sev,
				&a.Message, &a.IsResolved, &a.ResolvedBy, &a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt,
			); err != nil {
				return err
			}
			a.Severity = domain.AlertSeverity(sev)
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ResolveComplianceAlert(ctx context.Context, alertID string, resolvedBy string) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return err
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			UPDATE compliance_alerts
			SET is_resolved = TRUE, resolved_by = $1, resolved_at = $2, updated_at = $3
			WHERE alert_id = $4 AND tenant_id = $5
		`, resolvedBy, now, now, alertID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrAlertNotFound
		}
		return nil
	})
}
