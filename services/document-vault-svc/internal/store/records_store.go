package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/outbox"
)

func (s *PgStore) insertRecordOutboxEvent(ctx context.Context, tx pgx.Tx, eventType, recordID, tenantID, legalEntityID, actorID, correlationID string, payload map[string]any) error {
	if correlationID == "" {
		correlationID = recordID
	}
	env, err := outbox.NewVariantAEnvelope(eventType, correlationID, tenantID, legalEntityID, actorID, payload)
	if err != nil {
		return fmt.Errorf("build outbox envelope: %w", err)
	}
	actor := actorID
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "RECORD",
		AggregateID:   recordID,
		EventType:     eventType,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       &actor,
		CorrelationID: correlationID,
		Payload:       env,
	}); err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

const recordColumns = `
	record_id, tenant_id, legal_entity_id, document_id, document_version_id, record_class,
	jurisdiction_scope, business_context, retention_schedule_ref, record_state,
	declared_by_principal_id, declaration_reason, declared_at, correlation_id
`

func scanRecord(row pgx.Row, rec *domain.Record) error {
	return row.Scan(&rec.RecordID, &rec.TenantID, &rec.LegalEntityID, &rec.DocumentID, &rec.DocumentVersionID,
		&rec.RecordClass, &rec.JurisdictionScope, &rec.BusinessContext, &rec.RetentionScheduleRef, &rec.RecordState,
		&rec.DeclaredByPrincipalID, &rec.DeclarationReason, &rec.DeclaredAt, &rec.CorrelationID)
}

// isUniqueViolationOn reports whether err is a unique-constraint
// violation on the named constraint — used to tell "idempotent replay"
// apart from "genuine conflict" without a second round trip.
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// DeclareRecordV2 converts one exact, already-committed document
// version into a governed record. Each document_version_id may be
// declared at most once (migration 000009's UNIQUE constraint); a
// replay of the same correlation_id returns the original record rather
// than erroring or creating a duplicate.
func (s *PgStore) DeclareRecordV2(ctx context.Context, p domain.DeclareRecordV2Params) (*domain.Record, bool, error) {
	if !domain.RecordClass(p.RecordClass).Valid() {
		return nil, false, domain.ErrInvalidRecordClass
	}
	if p.JurisdictionScope == "" {
		return nil, false, domain.ErrJurisdictionScopeRequired
	}

	var out domain.Record
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var versionExists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM document_versions dv JOIN documents d ON d.document_id = dv.document_id
				WHERE dv.document_version_id = $1 AND dv.document_id = $2 AND d.tenant_id::text = $3)`,
			p.DocumentVersionID, p.DocumentID, tenantID,
		).Scan(&versionExists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !versionExists {
			return domain.ErrDocumentVersionNotFoundForRecord
		}

		var retentionRef *string
		if p.RetentionScheduleRef != "" {
			retentionRef = &p.RetentionScheduleRef
		}
		var corrID *string
		if p.CorrelationID != "" {
			corrID = &p.CorrelationID

			// Check for idempotent replay BEFORE attempting the insert.
			// A replay of the same correlation_id typically also carries
			// the same document_version_id, which would violate BOTH
			// unique constraints at once — Postgres only reports
			// whichever one it hits first, so classifying the violation
			// after the fact is unreliable. Checking first avoids the
			// ambiguity entirely.
			existing := tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM records WHERE tenant_id::text = $1 AND correlation_id = $2`,
				tenantID, p.CorrelationID)
			if err := scanRecord(existing, &out); err == nil {
				return nil
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("document store unavailable: %w", mapPgError(err))
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO records (tenant_id, legal_entity_id, document_id, document_version_id, record_class,
				jurisdiction_scope, business_context, retention_schedule_ref, declared_by_principal_id,
				declaration_reason, correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+recordColumns,
			tenantID, p.LegalEntityID, p.DocumentID, p.DocumentVersionID, p.RecordClass,
			p.JurisdictionScope, p.BusinessContext, retentionRef, p.DeclaredByPrincipalID,
			p.DeclarationReason, corrID,
		)
		if err := scanRecord(row, &out); err != nil {
			if isUniqueViolationOn(err, "records_document_version_unique") {
				return domain.ErrDocumentVersionAlreadyDeclared
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		created = true
		return s.insertRecordOutboxEvent(ctx, tx, "record.declared", out.RecordID, tenantID, p.LegalEntityID,
			p.DeclaredByPrincipalID, p.CorrelationID, map[string]any{
				"record_id":           out.RecordID,
				"tenant_id":           tenantID,
				"document_id":         p.DocumentID,
				"document_version_id": p.DocumentVersionID,
				"record_class":        p.RecordClass,
				"jurisdiction_scope":  p.JurisdictionScope,
			})
	})
	if errors.Is(err, domain.ErrDocumentVersionNotFoundForRecord) || errors.Is(err, domain.ErrDocumentVersionAlreadyDeclared) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}
	return &out, created, nil
}

func (s *PgStore) GetRecord(ctx context.Context, recordID string) (*domain.Record, error) {
	var out domain.Record
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM records WHERE record_id = $1 AND tenant_id::text = $2`, recordID, tenantID)
		if err := scanRecord(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRecordNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetRecordByDocumentVersion(ctx context.Context, documentVersionID string) (*domain.Record, error) {
	var out domain.Record
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM records WHERE document_version_id = $1 AND tenant_id::text = $2`,
			documentVersionID, tenantID)
		if err := scanRecord(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRecordNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

const relationshipColumns = `relationship_id, tenant_id, source_record_id, target_record_id, relationship_type, created_by_principal_id, created_at`

func scanRelationship(row pgx.Row, rel *domain.RecordRelationship) error {
	return row.Scan(&rel.RelationshipID, &rel.TenantID, &rel.SourceRecordID, &rel.TargetRecordID,
		&rel.RelationshipType, &rel.CreatedByPrincipalID, &rel.CreatedAt)
}

// CreateRecordRelationship records one evidentiary link between two
// records. For SUPERSEDES, the target record's state moves to
// SUPERSEDED in the same transaction — the only command in this wave
// that changes record_state.
func (s *PgStore) CreateRecordRelationship(ctx context.Context, p domain.CreateRecordRelationshipParams) (*domain.RecordRelationship, error) {
	if !domain.RelationshipType(p.RelationshipType).Valid() {
		return nil, domain.ErrInvalidRelationshipType
	}
	if p.SourceRecordID == p.TargetRecordID {
		return nil, domain.ErrRecordRelationshipSelfReference
	}

	var out domain.RecordRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var sourceLegalEntityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM records WHERE record_id = $1 AND tenant_id::text = $2`,
			p.SourceRecordID, tenantID).Scan(&sourceLegalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRecordNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		var targetExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE record_id = $1 AND tenant_id::text = $2)`,
			p.TargetRecordID, tenantID).Scan(&targetExists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !targetExists {
			return domain.ErrRecordNotFound
		}

		if p.RelationshipType == string(domain.RelationshipSupersedes) {
			tag, err := tx.Exec(ctx, `UPDATE records SET record_state = 'SUPERSEDED' WHERE record_id = $1 AND tenant_id::text = $2 AND record_state = 'DECLARED'`,
				p.TargetRecordID, tenantID)
			if err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrRecordAlreadySuperseded
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO record_relationships (tenant_id, source_record_id, target_record_id, relationship_type, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+relationshipColumns,
			tenantID, p.SourceRecordID, p.TargetRecordID, p.RelationshipType, p.CreatedByPrincipalID,
		)
		if err := scanRelationship(row, &out); err != nil {
			if isUniqueViolationOn(err, "record_relationships_unique") {
				return domain.ErrDuplicateRecordRelationship
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return s.insertRecordOutboxEvent(ctx, tx, "record.relationship_created", p.SourceRecordID, tenantID, sourceLegalEntityID,
			p.CreatedByPrincipalID, "", map[string]any{
				"relationship_id":   out.RelationshipID,
				"tenant_id":         tenantID,
				"source_record_id":  p.SourceRecordID,
				"target_record_id":  p.TargetRecordID,
				"relationship_type": p.RelationshipType,
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) ListRecordRelationships(ctx context.Context, recordID string) ([]domain.RecordRelationship, error) {
	var out []domain.RecordRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, err := tx.Query(ctx, `SELECT `+relationshipColumns+` FROM record_relationships
			WHERE tenant_id::text = $1 AND (source_record_id = $2 OR target_record_id = $2) ORDER BY created_at`,
			tenantID, recordID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rel domain.RecordRelationship
			if err := scanRelationship(rows, &rel); err != nil {
				return err
			}
			out = append(out, rel)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
