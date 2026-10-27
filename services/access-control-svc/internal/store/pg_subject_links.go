package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
)

// Subject links (migration 000015): the administered employee-to-principal
// mapping the HR-event consumer acts on. Tenant-owned, under withRLS.

const subjectLinkColumns = `tenant_id, employee_id, principal_id, legal_entity_id,
	COALESCE(last_manager_employee_id, ''), COALESCE(last_status, ''),
	linked_by_principal_id, correlation_id, created_at, updated_at`

func scanSubjectLink(row pgx.Row, l *domain.SubjectLink) error {
	return row.Scan(&l.TenantID, &l.EmployeeID, &l.PrincipalID, &l.LegalEntityID,
		&l.LastManagerEmployeeID, &l.LastStatus, &l.LinkedByPrincipalID, &l.CorrelationID, &l.CreatedAt, &l.UpdatedAt)
}

// UpsertSubjectLink records "employee E is principal P". Re-linking an
// employee to another principal replaces the link; what was last observed
// about the employee (manager, status) is kept.
func (s *PgStore) UpsertSubjectLink(ctx context.Context, l *domain.SubjectLink) error {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return err
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanSubjectLink(tx.QueryRow(ctx, `
			INSERT INTO iam_subject_links (tenant_id, employee_id, principal_id, legal_entity_id, linked_by_principal_id, correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, employee_id) DO UPDATE
			   SET principal_id = EXCLUDED.principal_id, legal_entity_id = EXCLUDED.legal_entity_id,
			       linked_by_principal_id = EXCLUDED.linked_by_principal_id, correlation_id = EXCLUDED.correlation_id,
			       updated_at = now()
			RETURNING `+subjectLinkColumns,
			tenantID, l.EmployeeID, l.PrincipalID, l.LegalEntityID, l.LinkedByPrincipalID, l.CorrelationID), l)
	})
}

func (s *PgStore) ListSubjectLinks(ctx context.Context) ([]domain.SubjectLink, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.SubjectLink{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+subjectLinkColumns+" FROM iam_subject_links WHERE tenant_id = $1 ORDER BY employee_id", tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.SubjectLink
			if err := scanSubjectLink(rows, &l); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// FindSubjectLink returns (nil, nil) for an unlinked employee.
func (s *PgStore) FindSubjectLink(ctx context.Context, employeeID string) (*domain.SubjectLink, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var l domain.SubjectLink
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanSubjectLink(tx.QueryRow(ctx, "SELECT "+subjectLinkColumns+" FROM iam_subject_links WHERE tenant_id = $1 AND employee_id = $2", tenantID, employeeID), &l)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ObserveSubject records what an HR event said about a linked employee and
// returns what was recorded before, in one transaction, so two deliveries of
// the same change cannot both see the old value. An empty manager or status
// leaves that field as it was. Returns (nil, nil) for an unlinked employee.
func (s *PgStore) ObserveSubject(ctx context.Context, employeeID, managerEmployeeID, status string) (*domain.SubjectLink, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var before domain.SubjectLink
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanSubjectLink(tx.QueryRow(ctx, "SELECT "+subjectLinkColumns+" FROM iam_subject_links WHERE tenant_id = $1 AND employee_id = $2 FOR UPDATE", tenantID, employeeID), &before); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE iam_subject_links
			   SET last_manager_employee_id = COALESCE(NULLIF($1, ''), last_manager_employee_id),
			       last_status = COALESCE(NULLIF($2, ''), last_status), updated_at = now()
			 WHERE tenant_id = $3 AND employee_id = $4`,
			managerEmployeeID, status, tenantID, employeeID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &before, nil
}

// RecordEmploymentEnded enqueues employment.changed for a linked employee's
// exit (000016), for authorization-svc to project as the principal's status.
func (s *PgStore) RecordEmploymentEnded(ctx context.Context, l domain.SubjectLink, newStatus, oldStatus, workerType, sourceEventID string) error {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return err
	}
	ev, err := events.EmploymentChanged(l, newStatus, oldStatus, workerType, sourceEventID,
		requestCorrelation(ctx, "hr-"+sourceEventID), time.Now())
	if err != nil {
		return err
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error { return enqueue(ctx, tx, tenantID, ev) })
}
