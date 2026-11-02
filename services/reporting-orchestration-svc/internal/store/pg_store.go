// Package store is reporting-orchestration-svc's Postgres persistence.
//
// Every request-scoped method runs inside inTenantTx, which sets
// app.tenant_id for the row-level-security policies (migrations 000001-3)
// AND every query also filters tenant_id explicitly — two independent
// tenant-isolation mechanisms. State changes and their outbox events are
// written in the same transaction.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// PgStore implements every store interface the handlers depend on.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Page is a bounded window of a list query. The handler validates the bounds.
type Page struct {
	Limit  int
	Offset int
}

// inTenantTx runs fn inside a transaction scoped to tenantID for RLS, and
// commits only when fn returns nil.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// insertOutboxEvent writes event to outbox_events inside tx. When the event
// has no payload, defaultPayload (normally the resulting entity) is used.
func insertOutboxEvent(ctx context.Context, tx pgx.Tx, event domain.OutboxEvent, defaultPayload interface{}) error {
	if event.EventType == "" {
		return nil // caller chose not to emit an event
	}
	payload := event.Payload
	if payload == nil {
		payload = defaultPayload
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events
		  (aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, actor_id, correlation_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.AggregateType, event.AggregateID, event.EventType, event.TenantID,
		event.LegalEntityID, event.ActorID, event.CorrelationID, payloadJSON,
	)
	if err != nil {
		return fmt.Errorf("insert outbox event %s: %w", event.EventType, err)
	}
	return nil
}

// ─── Report definitions ──────────────────────────────────────────────────────

const definitionColumns = `id::text, tenant_id, legal_entity_id, report_name, report_type, output_format,
	data_sources, COALESCE(schedule_cron, ''), is_scheduled, status, version, created_by, created_at, updated_at`

func scanDefinition(row pgx.Row) (*domain.ReportDefinition, error) {
	definition := &domain.ReportDefinition{}
	err := row.Scan(
		&definition.ID, &definition.TenantID, &definition.LegalEntityID, &definition.ReportName,
		&definition.ReportType, &definition.OutputFormat, &definition.DataSources, &definition.ScheduleCron,
		&definition.IsScheduled, &definition.Status, &definition.Version, &definition.CreatedBy,
		&definition.CreatedAt, &definition.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if definition.DataSources == nil {
		definition.DataSources = []string{}
	}
	return definition, nil
}

// CreateDefinition inserts a new DRAFT definition plus its outbox event.
func (s *PgStore) CreateDefinition(ctx context.Context, definition *domain.ReportDefinition, event domain.OutboxEvent) (*domain.ReportDefinition, error) {
	var created *domain.ReportDefinition
	err := s.inTenantTx(ctx, definition.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO report_definitions
			  (id, tenant_id, legal_entity_id, report_name, report_type, output_format,
			   data_sources, schedule_cron, is_scheduled, status, version, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,1,$11)
			RETURNING `+definitionColumns,
			uuid.New().String(), definition.TenantID, definition.LegalEntityID, definition.ReportName,
			string(definition.ReportType), string(definition.OutputFormat), definition.DataSources,
			definition.ScheduleCron, definition.IsScheduled, string(domain.DefStatusDraft), definition.CreatedBy,
		)
		var err error
		created, err = scanDefinition(row)
		if err != nil {
			return fmt.Errorf("insert report_definition: %w", err)
		}
		event.AggregateID = created.ID
		return insertOutboxEvent(ctx, tx, event, created)
	})
	return created, err
}

// GetDefinition returns the tenant's definition, or ErrDefinitionNotFound.
func (s *PgStore) GetDefinition(ctx context.Context, tenantID, id string) (*domain.ReportDefinition, error) {
	var found *domain.ReportDefinition
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		found, err = scanDefinition(tx.QueryRow(ctx,
			`SELECT `+definitionColumns+` FROM report_definitions WHERE id = $1 AND tenant_id = $2`, id, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDefinitionNotFound
		}
		return err
	})
	return found, err
}

// DefinitionFilter narrows ListDefinitions; empty fields are ignored.
type DefinitionFilter struct {
	LegalEntityID string
	ReportType    string
	Status        string
}

// ListDefinitions returns one page of the tenant's definitions, newest first.
func (s *PgStore) ListDefinitions(ctx context.Context, tenantID string, filter DefinitionFilter, page Page) ([]domain.ReportDefinition, error) {
	definitions := []domain.ReportDefinition{}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+definitionColumns+`
			FROM report_definitions
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id = $2)
			  AND ($3 = '' OR report_type = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC, id
			LIMIT $5 OFFSET $6`,
			tenantID, filter.LegalEntityID, filter.ReportType, filter.Status, page.Limit, page.Offset)
		if err != nil {
			return fmt.Errorf("list report_definitions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			definition, err := scanDefinition(rows)
			if err != nil {
				return fmt.Errorf("scan report_definition: %w", err)
			}
			definitions = append(definitions, *definition)
		}
		return rows.Err()
	})
	return definitions, err
}

// TransitionDefinitionParams describes one lifecycle change.
type TransitionDefinitionParams struct {
	TenantID        string
	DefinitionID    string
	ToStatus        domain.DefinitionStatus
	ExpectedVersion int // If-Match; 0 = no precondition
	Event           domain.OutboxEvent
}

// DefinitionTransition is the outcome of TransitionDefinitionStatus.
type DefinitionTransition struct {
	FromStatus domain.DefinitionStatus
	Definition *domain.ReportDefinition
}

// TransitionDefinitionStatus moves a definition along its lifecycle.
//
// Steps, all in one transaction:
//  1. lock the row (FOR UPDATE) — 404 if missing or another tenant's;
//  2. check If-Match against version — 412 on mismatch;
//  3. check the lifecycle table — 409 when the transition is illegal;
//  4. update status, bump version;
//  5. write the outbox event.
func (s *PgStore) TransitionDefinitionStatus(ctx context.Context, p TransitionDefinitionParams) (*DefinitionTransition, error) {
	var result *DefinitionTransition
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanDefinition(tx.QueryRow(ctx,
			`SELECT `+definitionColumns+` FROM report_definitions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			p.DefinitionID, p.TenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDefinitionNotFound
		}
		if err != nil {
			return err
		}

		if p.ExpectedVersion != 0 && p.ExpectedVersion != current.Version {
			return domain.ErrVersionMismatch
		}
		if !domain.CanTransitionDefinition(current.Status, p.ToStatus) {
			return domain.ErrIllegalTransition
		}

		updated, err := scanDefinition(tx.QueryRow(ctx, `
			UPDATE report_definitions
			SET status = $1, version = version + 1, updated_at = now()
			WHERE id = $2 AND tenant_id = $3
			RETURNING `+definitionColumns,
			string(p.ToStatus), p.DefinitionID, p.TenantID))
		if err != nil {
			return fmt.Errorf("update report_definition status: %w", err)
		}

		result = &DefinitionTransition{FromStatus: current.Status, Definition: updated}
		return insertOutboxEvent(ctx, tx, p.Event, map[string]interface{}{
			"definition":  updated,
			"from_status": current.Status,
			"to_status":   updated.Status,
		})
	})
	return result, err
}

// ─── Report runs ─────────────────────────────────────────────────────────────

// DATE columns are read as ::text (pgx cannot scan DATE into string).
const runColumns = `id::text, tenant_id, definition_id::text, triggered_by, requested_by,
	COALESCE(period_start::text, ''), COALESCE(period_end::text, ''), status, row_count,
	COALESCE(output_location, ''), COALESCE(error_message, ''), started_at, completed_at, created_at`

func scanRun(row pgx.Row) (*domain.ReportRun, error) {
	run := &domain.ReportRun{}
	err := row.Scan(
		&run.ID, &run.TenantID, &run.DefinitionID, &run.TriggeredBy, &run.RequestedBy,
		&run.PeriodStart, &run.PeriodEnd, &run.Status, &run.RowCount,
		&run.OutputLocation, &run.ErrorMessage, &run.StartedAt, &run.CompletedAt, &run.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// CreateRun records a run against an ACTIVE definition.
//
// The definition row is locked FOR SHARE in the same transaction, so a
// concurrent PAUSE/DEPRECATE cannot slip in between the status check and
// the insert.
func (s *PgStore) CreateRun(ctx context.Context, run *domain.ReportRun, event domain.OutboxEvent) (*domain.ReportRun, error) {
	var created *domain.ReportRun
	err := s.inTenantTx(ctx, run.TenantID, func(tx pgx.Tx) error {
		var definitionStatus domain.DefinitionStatus
		err := tx.QueryRow(ctx,
			`SELECT status FROM report_definitions WHERE id = $1 AND tenant_id = $2 FOR SHARE`,
			run.DefinitionID, run.TenantID).Scan(&definitionStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDefinitionNotFound
		}
		if err != nil {
			return err
		}
		if definitionStatus != domain.DefStatusActive {
			return domain.ErrDefinitionNotActive
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO report_runs
			  (id, tenant_id, definition_id, triggered_by, requested_by, period_start, period_end,
			   status, row_count, output_location, error_message, started_at, completed_at, created_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::date,NULLIF($7,'')::date,$8,$9,NULLIF($10,''),NULLIF($11,''),$12,$13,$14)
			RETURNING `+runColumns,
			uuid.New().String(), run.TenantID, run.DefinitionID, string(run.TriggeredBy), run.RequestedBy,
			run.PeriodStart, run.PeriodEnd, string(run.Status), run.RowCount, run.OutputLocation,
			run.ErrorMessage, run.StartedAt, run.CompletedAt, run.CreatedAt,
		)
		created, err = scanRun(row)
		if err != nil {
			return fmt.Errorf("insert report_run: %w", err)
		}
		event.AggregateID = created.ID
		return insertOutboxEvent(ctx, tx, event, created)
	})
	return created, err
}

// GetRun returns the tenant's run, or ErrRunNotFound.
func (s *PgStore) GetRun(ctx context.Context, tenantID, id string) (*domain.ReportRun, error) {
	var found *domain.ReportRun
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		found, err = scanRun(tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM report_runs WHERE id = $1 AND tenant_id = $2`, id, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		return err
	})
	return found, err
}

// RunFilter narrows ListRuns; empty fields are ignored.
type RunFilter struct {
	DefinitionID string
	Status       string
}

// ListRuns returns one page of the tenant's runs, newest first.
func (s *PgStore) ListRuns(ctx context.Context, tenantID string, filter RunFilter, page Page) ([]domain.ReportRun, error) {
	runs := []domain.ReportRun{}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+runColumns+`
			FROM report_runs
			WHERE tenant_id = $1
			  AND ($2 = '' OR definition_id::text = $2)
			  AND ($3 = '' OR status = $3)
			ORDER BY created_at DESC, id
			LIMIT $4 OFFSET $5`,
			tenantID, filter.DefinitionID, filter.Status, page.Limit, page.Offset)
		if err != nil {
			return fmt.Errorf("list report_runs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return fmt.Errorf("scan report_run: %w", err)
			}
			runs = append(runs, *run)
		}
		return rows.Err()
	})
	return runs, err
}
