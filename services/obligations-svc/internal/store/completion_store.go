// Group 2 completion pass (21 Oct 2026): attribute updates, the filing
// lifecycle, and the definition / entity-mapping catalogue Doc 03 §8.5 says
// this service owns.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

// isUniqueViolation reports a 23505 on the named constraint or index.
func isUniqueViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (name == "" || pgErr.ConstraintName == name)
}

// ── obligation attribute update ─────────────────────────────────────────────

// UpdateObligation changes an obligation's mutable attributes (due date,
// severity, responsible function) under If-Match, and writes obligation.updated
// naming the changed fields in the same transaction. Identity, entity,
// jurisdiction and source are not mutable: an obligation that changes those
// is a different obligation. A CLOSED obligation is final.
func (s *PgStore) UpdateObligation(ctx context.Context, p domain.UpdateObligationParams) (*domain.Obligation, bool, error) {
	var updated *domain.Obligation
	changed := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		current, err := scanObligation(tx.QueryRow(ctx,
			`SELECT `+obligationColumns+` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2 FOR UPDATE;`,
			p.ObligationID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrObligationNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if current.Version != p.ExpectedVersion {
			return domain.ErrVersionMismatch
		}
		if current.ObligationStatus == "CLOSED" {
			return domain.ErrObligationClosed
		}

		dueDate, severity, function := current.DueDate, current.SeverityLevel, current.ResponsibleFunction
		var fields []string
		if p.DueDate != nil && !p.DueDate.Equal(current.DueDate) {
			dueDate = *p.DueDate
			fields = append(fields, "due_date")
		}
		if p.SeverityLevel != nil && *p.SeverityLevel != current.SeverityLevel {
			severity = *p.SeverityLevel
			fields = append(fields, "severity_level")
		}
		if p.ResponsibleFunction != nil && *p.ResponsibleFunction != current.ResponsibleFunction {
			function = *p.ResponsibleFunction
			fields = append(fields, "responsible_function")
		}
		if len(fields) == 0 {
			updated = current
			return nil
		}

		updated, err = scanObligation(tx.QueryRow(ctx, `
			UPDATE obligations
			SET due_date = $1, severity_level = $2, responsible_function = $3,
			    updated_at = NOW(), version = version + 1
			WHERE obligation_id = $4 AND tenant_id = $5
			RETURNING `+obligationColumns+`;`,
			dueDate, severity, function, p.ObligationID, tenantID))
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		changed = true
		return insertOutboxEventTx(ctx, tx, tenantID, "obligation.updated", p.CorrelationID,
			updated.LegalEntityID, updated.JurisdictionID, p.ActorPrincipalID, map[string]any{
				"obligation_id":        updated.ObligationID,
				"obligation_status":    updated.ObligationStatus,
				"changed_fields":       fields,
				"due_date":             updated.DueDate,
				"severity_level":       updated.SeverityLevel,
				"responsible_function": updated.ResponsibleFunction,
				"version":              updated.Version,
				"updated_at":           updated.UpdatedAt,
			})
	})
	if err != nil {
		return nil, false, err
	}
	return updated, changed, nil
}

// ── filing lifecycle ────────────────────────────────────────────────────────

// FindFilingRequirement reads one filing requirement under its obligation.
func (s *PgStore) FindFilingRequirement(ctx context.Context, obligationID, filingRequirementID string) (*domain.FilingRequirement, error) {
	var f *domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		f, err = scanFilingRequirement(tx.QueryRow(ctx, `
			SELECT `+filingRequirementColumns+` FROM filing_requirements
			WHERE filing_requirement_id = $1 AND obligation_id = $2 AND tenant_id = $3;`,
			filingRequirementID, obligationID, tenantID))
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrFilingRequirementNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return f, nil
}

// UpdateFilingStatus moves a filing requirement along domain.FilingStatuses
// under a row lock, appends the step to filing_requirement_transitions, and
// writes obligation.updated (the filing is part of the obligation aggregate)
// in the same transaction. Requesting the current status is a no-op.
func (s *PgStore) UpdateFilingStatus(ctx context.Context, p domain.UpdateFilingStatusParams) (*domain.FilingRequirement, bool, error) {
	var updated *domain.FilingRequirement
	moved := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		parent, err := scanObligation(tx.QueryRow(ctx,
			`SELECT `+obligationColumns+` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2;`,
			p.ObligationID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrObligationNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		current, err := scanFilingRequirement(tx.QueryRow(ctx, `
			SELECT `+filingRequirementColumns+` FROM filing_requirements
			WHERE filing_requirement_id = $1 AND obligation_id = $2 AND tenant_id = $3
			FOR UPDATE;`, p.FilingRequirementID, p.ObligationID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrFilingRequirementNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if p.ExpectedVersion > 0 && current.RowVersion != p.ExpectedVersion {
			return domain.ErrVersionMismatch
		}
		if current.FilingStatus == p.NewStatus {
			updated = current
			return nil
		}
		legal := false
		for _, next := range domain.FilingStatuses[current.FilingStatus] {
			if next == p.NewStatus {
				legal = true
				break
			}
		}
		if !legal {
			return domain.ErrInvalidTransition
		}
		updated, err = scanFilingRequirement(tx.QueryRow(ctx, `
			UPDATE filing_requirements
			SET filing_status = $1, updated_at = NOW(), row_version = row_version + 1
			WHERE filing_requirement_id = $2 AND tenant_id = $3
			RETURNING `+filingRequirementColumns+`;`, p.NewStatus, p.FilingRequirementID, tenantID))
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO filing_requirement_transitions
				(tenant_id, filing_requirement_id, from_status, to_status, reason, actor_principal_id, correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7);`,
			tenantID, p.FilingRequirementID, current.FilingStatus, p.NewStatus, nullableStr(p.Reason),
			p.ActorPrincipalID, p.CorrelationID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		moved = true
		return insertOutboxEventTx(ctx, tx, tenantID, "obligation.updated", p.CorrelationID,
			parent.LegalEntityID, parent.JurisdictionID, p.ActorPrincipalID, map[string]any{
				"obligation_id":          parent.ObligationID,
				"obligation_status":      parent.ObligationStatus,
				"changed_fields":         []string{"filing_requirements.filing_status"},
				"filing_requirement_id":  updated.FilingRequirementID,
				"filing_status":          updated.FilingStatus,
				"previous_filing_status": current.FilingStatus,
				"updated_at":             updated.UpdatedAt,
			})
	})
	if err != nil {
		return nil, false, err
	}
	return updated, moved, nil
}

// ListFilingTransitions returns a filing requirement's history, oldest first.
func (s *PgStore) ListFilingTransitions(ctx context.Context, obligationID, filingRequirementID string) ([]*domain.FilingTransition, error) {
	if _, err := s.FindFilingRequirement(ctx, obligationID, filingRequirementID); err != nil {
		return nil, err
	}
	var out []*domain.FilingTransition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, err := tx.Query(ctx, `
			SELECT transition_id, filing_requirement_id, from_status, to_status, reason,
			       actor_principal_id, correlation_id, created_at
			FROM filing_requirement_transitions
			WHERE filing_requirement_id = $1 AND tenant_id = $2
			ORDER BY created_at ASC, transition_id ASC;`, filingRequirementID, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		for rows.Next() {
			t := &domain.FilingTransition{}
			if err := rows.Scan(&t.TransitionID, &t.FilingRequirementID, &t.FromStatus, &t.ToStatus, &t.Reason,
				&t.ActorPrincipalID, &t.CorrelationID, &t.CreatedAt); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ── definitions ─────────────────────────────────────────────────────────────

const definitionColumns = `
	d.obligation_definition_id, d.definition_code, d.current_version, d.definition_status,
	d.created_at, d.created_by_principal_id, d.updated_at, d.retired_at,
	v.version, v.jurisdiction_id, v.obligation_type, v.obligation_source_type, v.obligation_source_id,
	v.source_reference, v.severity_level, v.responsible_function, v.description, v.change_reason,
	v.created_at, v.created_by_principal_id`

const definitionFrom = `
	FROM obligation_definitions d
	JOIN obligation_definition_versions v
	  ON v.obligation_definition_id = d.obligation_definition_id AND v.version = d.current_version`

func scanDefinition(row pgx.Row) (*domain.ObligationDefinition, error) {
	d := &domain.ObligationDefinition{Current: &domain.ObligationDefinitionVersion{}}
	v := d.Current
	err := row.Scan(&d.ObligationDefinitionID, &d.DefinitionCode, &d.CurrentVersion, &d.DefinitionStatus,
		&d.CreatedAt, &d.CreatedByPrincipalID, &d.UpdatedAt, &d.RetiredAt,
		&v.Version, &v.JurisdictionID, &v.ObligationType, &v.ObligationSourceType, &v.ObligationSourceID,
		&v.SourceReference, &v.SeverityLevel, &v.ResponsibleFunction, &v.Description, &v.ChangeReason,
		&v.CreatedAt, &v.CreatedByPrincipalID)
	v.ObligationDefinitionID = d.ObligationDefinitionID
	return d, err
}

func insertDefinitionVersionTx(ctx context.Context, tx pgx.Tx, tenantID, definitionID string, version int, c domain.DefinitionContent, reason, actor string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO obligation_definition_versions (
			obligation_definition_id, version, tenant_id, jurisdiction_id, obligation_type,
			obligation_source_type, obligation_source_id, source_reference, severity_level,
			responsible_function, description, change_reason, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);`,
		definitionID, version, tenantID, c.JurisdictionID, c.ObligationType, c.ObligationSourceType,
		c.ObligationSourceID, c.SourceReference, c.SeverityLevel, c.ResponsibleFunction, c.Description, reason, actor)
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return domain.ErrInvalidIdentifier
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func definitionEvent(ctx context.Context, tx pgx.Tx, tenantID, change, actor, correlationID string, d *domain.ObligationDefinition) error {
	return insertOutboxEventTx(ctx, tx, tenantID, "obligation.definition.changed", correlationID, "", d.Current.JurisdictionID, actor,
		map[string]any{
			"obligation_definition_id": d.ObligationDefinitionID,
			"definition_code":          d.DefinitionCode,
			"change":                   change,
			"version":                  d.CurrentVersion,
			"definition_status":        d.DefinitionStatus,
		})
}

func sameContent(v *domain.ObligationDefinitionVersion, c domain.DefinitionContent) bool {
	return v.JurisdictionID == c.JurisdictionID && v.ObligationType == c.ObligationType &&
		v.ObligationSourceType == c.ObligationSourceType && v.ObligationSourceID == c.ObligationSourceID &&
		v.SourceReference == c.SourceReference && v.SeverityLevel == c.SeverityLevel &&
		v.ResponsibleFunction == c.ResponsibleFunction && v.Description == c.Description
}

func (s *PgStore) getDefinitionTx(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*domain.ObligationDefinition, error) {
	q := `SELECT ` + definitionColumns + definitionFrom + ` WHERE d.obligation_definition_id = $1 AND d.tenant_id = $2`
	if lock {
		q += ` FOR UPDATE OF d`
	}
	d, err := scanDefinition(tx.QueryRow(ctx, q, id, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrDefinitionNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// CreateDefinition creates a definition at version 1. Idempotent on
// (tenant, definition_code): the same content returns the existing definition
// (created=false); different content is ErrDefinitionCodeConflict.
func (s *PgStore) CreateDefinition(ctx context.Context, p domain.CreateDefinitionParams) (*domain.ObligationDefinition, bool, error) {
	var out *domain.ObligationDefinition
	created := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		id := uuid.New().String()
		tag, err := tx.Exec(ctx, `
			INSERT INTO obligation_definitions (obligation_definition_id, tenant_id, definition_code, created_by_principal_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, definition_code) DO NOTHING;`, id, tenantID, p.DefinitionCode, p.CreatedByPrincipalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if tag.RowsAffected() == 0 {
			existing, err := scanDefinition(tx.QueryRow(ctx,
				`SELECT `+definitionColumns+definitionFrom+` WHERE d.definition_code = $1 AND d.tenant_id = $2`,
				p.DefinitionCode, tenantID))
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			if !sameContent(existing.Current, p.Content) {
				return domain.ErrDefinitionCodeConflict
			}
			out = existing
			return nil
		}
		if err := insertDefinitionVersionTx(ctx, tx, tenantID, id, 1, p.Content, "created", p.CreatedByPrincipalID); err != nil {
			return err
		}
		if out, err = s.getDefinitionTx(ctx, tx, tenantID, id, false); err != nil {
			return err
		}
		created = true
		return definitionEvent(ctx, tx, tenantID, "created", p.CreatedByPrincipalID, p.CorrelationID, out)
	})
	return out, created, err
}

// GetDefinition reads a definition with its current version.
func (s *PgStore) GetDefinition(ctx context.Context, id string) (*domain.ObligationDefinition, error) {
	var out *domain.ObligationDefinition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.getDefinitionTx(ctx, tx, tenantID, id, false)
		return err
	})
	return out, err
}

// ListDefinitions lists the tenant's definitions, code order, bounded.
func (s *PgStore) ListDefinitions(ctx context.Context, status string, limit int, afterCode string) ([]*domain.ObligationDefinition, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []*domain.ObligationDefinition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, err := tx.Query(ctx, `SELECT `+definitionColumns+definitionFrom+`
			WHERE d.tenant_id = $1 AND ($2 = '' OR d.definition_status = $2) AND d.definition_code > $3
			ORDER BY d.definition_code ASC LIMIT $4;`, tenantID, status, afterCode, limit)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDefinition(rows)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// ListDefinitionVersions returns every version, newest first.
func (s *PgStore) ListDefinitionVersions(ctx context.Context, id string) ([]*domain.ObligationDefinitionVersion, error) {
	var out []*domain.ObligationDefinitionVersion
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.getDefinitionTx(ctx, tx, tenantID, id, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT obligation_definition_id, version, jurisdiction_id, obligation_type, obligation_source_type,
			       obligation_source_id, source_reference, severity_level, responsible_function, description,
			       change_reason, created_at, created_by_principal_id
			FROM obligation_definition_versions
			WHERE obligation_definition_id = $1 AND tenant_id = $2
			ORDER BY version DESC;`, id, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		for rows.Next() {
			v := &domain.ObligationDefinitionVersion{}
			if err := rows.Scan(&v.ObligationDefinitionID, &v.Version, &v.JurisdictionID, &v.ObligationType,
				&v.ObligationSourceType, &v.ObligationSourceID, &v.SourceReference, &v.SeverityLevel,
				&v.ResponsibleFunction, &v.Description, &v.ChangeReason, &v.CreatedAt, &v.CreatedByPrincipalID); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// NewDefinitionVersion supersedes the current version under If-Match. The old
// version stays readable and every obligation raised from it stays pinned to
// it.
func (s *PgStore) NewDefinitionVersion(ctx context.Context, p domain.NewDefinitionVersionParams) (*domain.ObligationDefinition, error) {
	var out *domain.ObligationDefinition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		d, err := s.getDefinitionTx(ctx, tx, tenantID, p.ObligationDefinitionID, true)
		if err != nil {
			return err
		}
		if d.DefinitionStatus != "ACTIVE" {
			return domain.ErrDefinitionRetired
		}
		if d.CurrentVersion != p.ExpectedVersion {
			return domain.ErrVersionMismatch
		}
		next := d.CurrentVersion + 1
		if err := insertDefinitionVersionTx(ctx, tx, tenantID, d.ObligationDefinitionID, next, p.Content, p.ChangeReason, p.ActorPrincipalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE obligation_definitions SET current_version = $1, updated_at = NOW()
			WHERE obligation_definition_id = $2 AND tenant_id = $3;`, next, d.ObligationDefinitionID, tenantID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if out, err = s.getDefinitionTx(ctx, tx, tenantID, d.ObligationDefinitionID, false); err != nil {
			return err
		}
		return definitionEvent(ctx, tx, tenantID, "versioned", p.ActorPrincipalID, p.CorrelationID, out)
	})
	return out, err
}

// RetireDefinition stops new obligations being raised from a definition.
// Existing obligations are untouched. Retiring a retired definition is a no-op.
func (s *PgStore) RetireDefinition(ctx context.Context, id string, expectedVersion int, actor, correlationID string) (*domain.ObligationDefinition, error) {
	var out *domain.ObligationDefinition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		d, err := s.getDefinitionTx(ctx, tx, tenantID, id, true)
		if err != nil {
			return err
		}
		if d.DefinitionStatus == "RETIRED" {
			out = d
			return nil
		}
		if expectedVersion > 0 && d.CurrentVersion != expectedVersion {
			return domain.ErrVersionMismatch
		}
		if _, err := tx.Exec(ctx, `UPDATE obligation_definitions SET definition_status = 'RETIRED', retired_at = NOW(), updated_at = NOW()
			WHERE obligation_definition_id = $1 AND tenant_id = $2;`, id, tenantID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if out, err = s.getDefinitionTx(ctx, tx, tenantID, id, false); err != nil {
			return err
		}
		return definitionEvent(ctx, tx, tenantID, "retired", actor, correlationID, out)
	})
	return out, err
}

// ── entity mappings ─────────────────────────────────────────────────────────

const mappingColumns = `mapping_id, obligation_definition_id, legal_entity_id, effective_from, effective_to,
	created_at, created_by_principal_id, ended_by_principal_id`

func scanMapping(row pgx.Row) (*domain.ObligationEntityMapping, error) {
	m := &domain.ObligationEntityMapping{}
	err := row.Scan(&m.MappingID, &m.ObligationDefinitionID, &m.LegalEntityID, &m.EffectiveFrom, &m.EffectiveTo,
		&m.CreatedAt, &m.CreatedByPrincipalID, &m.EndedByPrincipalID)
	return m, err
}

func mappingEvent(ctx context.Context, tx pgx.Tx, tenantID, change, actor, correlationID string, m *domain.ObligationEntityMapping) error {
	return insertOutboxEventTx(ctx, tx, tenantID, "obligation.entity_mapping.changed", correlationID, m.LegalEntityID, "", actor,
		map[string]any{
			"mapping_id":               m.MappingID,
			"obligation_definition_id": m.ObligationDefinitionID,
			"legal_entity_id":          m.LegalEntityID,
			"change":                   change,
			"effective_from":           m.EffectiveFrom,
			"effective_to":             m.EffectiveTo,
		})
}

// CreateMapping binds an ACTIVE definition to a legal entity from
// effective_from. A second open mapping for the same pair is ErrMappingExists.
func (s *PgStore) CreateMapping(ctx context.Context, p domain.CreateMappingParams) (*domain.ObligationEntityMapping, error) {
	var out *domain.ObligationEntityMapping
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		d, err := s.getDefinitionTx(ctx, tx, tenantID, p.ObligationDefinitionID, false)
		if err != nil {
			return err
		}
		if d.DefinitionStatus != "ACTIVE" {
			return domain.ErrDefinitionRetired
		}
		out, err = scanMapping(tx.QueryRow(ctx, `
			INSERT INTO obligation_entity_mappings
				(tenant_id, obligation_definition_id, legal_entity_id, effective_from, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+mappingColumns+`;`, tenantID, p.ObligationDefinitionID, p.LegalEntityID, p.EffectiveFrom, p.ActorPrincipalID))
		if err != nil {
			if isUniqueViolation(err, "uq_obligation_entity_mappings_open") {
				return domain.ErrMappingExists
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return mappingEvent(ctx, tx, tenantID, "created", p.ActorPrincipalID, p.CorrelationID, out)
	})
	return out, err
}

// ListMappings lists a definition's mappings (open and ended), newest first.
func (s *PgStore) ListMappings(ctx context.Context, definitionID string) ([]*domain.ObligationEntityMapping, error) {
	var out []*domain.ObligationEntityMapping
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.getDefinitionTx(ctx, tx, tenantID, definitionID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+mappingColumns+` FROM obligation_entity_mappings
			WHERE obligation_definition_id = $1 AND tenant_id = $2
			ORDER BY effective_from DESC, mapping_id DESC;`, definitionID, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMapping(rows)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// FindMapping reads one mapping under its definition.
func (s *PgStore) FindMapping(ctx context.Context, definitionID, mappingID string) (*domain.ObligationEntityMapping, error) {
	var out *domain.ObligationEntityMapping
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = scanMapping(tx.QueryRow(ctx, `SELECT `+mappingColumns+` FROM obligation_entity_mappings
			WHERE mapping_id = $1 AND obligation_definition_id = $2 AND tenant_id = $3;`, mappingID, definitionID, tenantID))
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrMappingNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// EndMapping closes an open mapping at effectiveTo. Ending an ended mapping is
// a no-op that returns it unchanged.
func (s *PgStore) EndMapping(ctx context.Context, definitionID, mappingID string, effectiveTo time.Time, actor, correlationID string) (*domain.ObligationEntityMapping, error) {
	var out *domain.ObligationEntityMapping
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		m, err := scanMapping(tx.QueryRow(ctx, `SELECT `+mappingColumns+` FROM obligation_entity_mappings
			WHERE mapping_id = $1 AND obligation_definition_id = $2 AND tenant_id = $3 FOR UPDATE;`,
			mappingID, definitionID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrMappingNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if m.EffectiveTo != nil {
			out = m
			return nil
		}
		if !effectiveTo.After(m.EffectiveFrom) {
			return domain.ErrInvalidField
		}
		out, err = scanMapping(tx.QueryRow(ctx, `UPDATE obligation_entity_mappings
			SET effective_to = $1, ended_by_principal_id = $2
			WHERE mapping_id = $3 AND tenant_id = $4
			RETURNING `+mappingColumns+`;`, effectiveTo, actor, mappingID, tenantID))
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return mappingEvent(ctx, tx, tenantID, "ended", actor, correlationID, out)
	})
	return out, err
}

// pinDefinitionTx returns the current version of an ACTIVE definition for an
// obligation being raised for legalEntityID with the given due date, and
// refuses an entity the definition does not bind at that date.
func (s *PgStore) pinDefinitionTx(ctx context.Context, tx pgx.Tx, tenantID, definitionID, legalEntityID string, at time.Time) (int, error) {
	d, err := s.getDefinitionTx(ctx, tx, tenantID, definitionID, false)
	if err != nil {
		return 0, err
	}
	if d.DefinitionStatus != "ACTIVE" {
		return 0, domain.ErrDefinitionRetired
	}
	var mapped bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM obligation_entity_mappings
			WHERE tenant_id = $1 AND obligation_definition_id = $2 AND legal_entity_id = $3
			  AND effective_from <= $4 AND (effective_to IS NULL OR effective_to > $4));`,
		tenantID, definitionID, legalEntityID, at).Scan(&mapped); err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return 0, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg pinDefinition mapping check failed", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !mapped {
		return 0, domain.ErrEntityNotMapped
	}
	return d.CurrentVersion, nil
}
