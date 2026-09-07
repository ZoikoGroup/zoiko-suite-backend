package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/leave-absence-svc/internal/domain"
	svcmiddleware "zoiko.io/leave-absence-svc/internal/middleware"
)

const (
	statusActive   = "ACTIVE"
	statusInactive = "INACTIVE"
)

const holidayColumns = `
	holiday_id, tenant_id, legal_entity_id, name, holiday_date::text,
	holiday_type, is_recurring, description, status, created_at, updated_at`

func scanHoliday(row pgx.Row, h *domain.Holiday) error {
	return row.Scan(
		&h.HolidayID, &h.TenantID, &h.LegalEntityID, &h.Name, &h.Date,
		&h.HolidayType, &h.IsRecurring, &h.Description, &h.Status, &h.CreatedAt, &h.UpdatedAt,
	)
}

func (s *PgStore) CreateHoliday(ctx context.Context, h *domain.Holiday) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO holidays (
				holiday_id, tenant_id, legal_entity_id, name, holiday_date,
				holiday_type, is_recurring, description, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, h.HolidayID, tenantID, h.LegalEntityID, h.Name, h.Date,
			h.HolidayType, h.IsRecurring, h.Description, h.Status, h.CreatedAt, h.UpdatedAt)
		return err
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrHolidayDateExists
	}
	return err
}

// ListHolidays returns the calendar for a legal entity, oldest first. Inactive
// entries are excluded unless the filter asks for them: a caller computing
// working days must never see a retired holiday.
func (s *PgStore) ListHolidays(ctx context.Context, f domain.HolidayFilter) ([]domain.Holiday, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Holiday
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + holidayColumns + `
			FROM holidays
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if !f.IncludeInactive {
			args = append(args, statusActive)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if f.From != "" {
			args = append(args, f.From)
			query += fmt.Sprintf(" AND holiday_date >= $%d::date", len(args))
		}
		if f.To != "" {
			args = append(args, f.To)
			query += fmt.Sprintf(" AND holiday_date <= $%d::date", len(args))
		}
		query += " ORDER BY holiday_date ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var h domain.Holiday
			if err := scanHoliday(rows, &h); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetHoliday(ctx context.Context, holidayID string) (*domain.Holiday, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var h domain.Holiday
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanHoliday(tx.QueryRow(ctx, `
			SELECT `+holidayColumns+`
			FROM holidays
			WHERE tenant_id = $1 AND holiday_id = $2
		`, tenantID, holidayID), &h)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrHolidayNotFound
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// DeactivateHoliday retires a calendar entry. Holidays are never hard-deleted:
// a leave request approved against last year's calendar must still be
// explicable, so the row stays and only its status moves.
func (s *PgStore) DeactivateHoliday(ctx context.Context, holidayID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE holidays
			SET status = $1, updated_at = NOW()
			WHERE tenant_id = $2 AND holiday_id = $3 AND status = $4
		`, statusInactive, tenantID, holidayID, statusActive)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrHolidayNotFound
		}
		return nil
	})
}

// CountHolidaysInRange counts active holidays between from and to inclusive.
// Used to report how much of a leave span falls on non-working days.
func (s *PgStore) CountHolidaysInRange(ctx context.Context, legalEntityID, from, to string) (int, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}

	var count int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM holidays
			WHERE tenant_id = $1
			  AND legal_entity_id = $2
			  AND status = $3
			  AND holiday_date >= $4::date
			  AND holiday_date <= $5::date
		`, tenantID, legalEntityID, statusActive, from, to).Scan(&count)
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
