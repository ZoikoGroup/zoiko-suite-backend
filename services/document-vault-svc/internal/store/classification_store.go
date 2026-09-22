// BIZ-02 Record Classification persistence — see internal/domain/classification.go
// for the full lifecycle/immutability doc comment and migration 000008 for
// the schema/trigger this operates against.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const classificationColumns = `
	classification_id, document_id, tenant_id, legal_entity_id, classification_value, status, source,
	confidence, rule_model_version, source_evidence, proposed_by_principal_id, proposed_at,
	confirmed_by_principal_id, confirmed_at, superseded_by_classification_id, effective_at, correlation_id
`

func scanClassification(row pgx.Row, c *domain.RecordClassification) error {
	return row.Scan(&c.ClassificationID, &c.DocumentID, &c.TenantID, &c.LegalEntityID, &c.ClassificationValue, &c.Status, &c.Source,
		&c.Confidence, &c.RuleModelVersion, &c.SourceEvidence, &c.ProposedByPrincipalID, &c.ProposedAt,
		&c.ConfirmedByPrincipalID, &c.ConfirmedAt, &c.SupersededByClassificationID, &c.EffectiveAt, &c.CorrelationID)
}

// ClassifyRecord inserts a new CANDIDATE classification proposal for a
// document. Refuses if the document already has a CONFIRMED/RESTRICTED
// classification — Reclassify (a later wave) is the command for that
// case, not a second ClassifyRecord.
func (s *PgStore) ClassifyRecord(ctx context.Context, p domain.ClassifyRecordParams) (*domain.RecordClassification, error) {
	if p.Source != domain.ClassificationSourceHuman && p.Source != domain.ClassificationSourceAI {
		return nil, domain.ErrInvalidClassificationSource
	}
	if p.Source == domain.ClassificationSourceAI && p.Confidence == nil {
		return nil, domain.ErrAIConfidenceRequired
	}
	if p.Source == domain.ClassificationSourceHuman && p.Confidence != nil {
		return nil, domain.ErrHumanConfidenceNotAllowed
	}

	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var legalEntityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`,
			p.DocumentID, tenantID).Scan(&legalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		var existingStatus *string
		if err := tx.QueryRow(ctx, `
			SELECT status FROM record_classifications
			WHERE document_id = $1 AND status IN ('CONFIRMED', 'RESTRICTED')
			ORDER BY proposed_at DESC LIMIT 1
		`, p.DocumentID).Scan(&existingStatus); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if existingStatus != nil {
			return domain.ErrClassificationNotCandidate
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO record_classifications (
				document_id, tenant_id, legal_entity_id, classification_value, source,
				confidence, rule_model_version, source_evidence, proposed_by_principal_id, correlation_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+classificationColumns,
			p.DocumentID, tenantID, legalEntityID, string(p.ClassificationValue), string(p.Source),
			p.Confidence, nullableString(p.RuleModelVersion), nullableString(p.SourceEvidence), p.ProposedByPrincipalID, nullableString(p.CorrelationID),
		)
		if err := scanClassification(row, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}

		return s.insertDocumentOutboxEvent(ctx, tx, "classification.proposed", p.DocumentID, tenantID,
			legalEntityID, p.ProposedByPrincipalID, p.CorrelationID, map[string]any{
				"classification_id":    out.ClassificationID,
				"document_id":          p.DocumentID,
				"tenant_id":            tenantID,
				"legal_entity_id":      legalEntityID,
				"classification_value": string(p.ClassificationValue),
				"source":               string(p.Source),
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FindClassificationByID is a read-only lookup used to authorize
// ConfirmClassification BEFORE any mutation runs — see the handler's own
// doc comment on why fetch must happen first.
func (s *PgStore) FindClassificationByID(ctx context.Context, classificationID string) (*domain.RecordClassification, error) {
	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+classificationColumns+` FROM record_classifications WHERE classification_id = $1 AND tenant_id = $2::uuid`,
			classificationID, tenantID)
		return scanClassification(row, &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrClassificationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfirmClassification moves a CANDIDATE classification to CONFIRMED,
// and refreshes documents.classification to match — the one place that
// cached snapshot is ever written after document creation. Self-check
// (maker-checker) applies only to HUMAN-sourced proposals; see
// migration 000008's own CHECK constraint, mirrored here so the error
// is a real domain sentinel rather than a raw constraint violation.
func (s *PgStore) ConfirmClassification(ctx context.Context, p domain.ConfirmClassificationParams) (*domain.RecordClassification, error) {
	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var existing domain.RecordClassification
		row := tx.QueryRow(ctx, `SELECT `+classificationColumns+` FROM record_classifications WHERE classification_id = $1 AND tenant_id = $2::uuid`,
			p.ClassificationID, tenantID)
		if err := scanClassification(row, &existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if existing.Source == domain.ClassificationSourceHuman && existing.ProposedByPrincipalID == p.ConfirmedByPrincipalID {
			return domain.ErrClassificationSelfConfirmation
		}
		if !domain.CanConfirmClassification(&existing) {
			return domain.ErrClassificationNotCandidate
		}

		row = tx.QueryRow(ctx, `
			UPDATE record_classifications
			SET status = 'CONFIRMED', confirmed_by_principal_id = $3, confirmed_at = now()
			WHERE classification_id = $1 AND tenant_id = $2::uuid AND status = 'CANDIDATE'
			RETURNING `+classificationColumns,
			p.ClassificationID, tenantID, p.ConfirmedByPrincipalID,
		)
		if err := scanClassification(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotCandidate
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE documents SET classification = $3, updated_at = now() WHERE document_id = $1 AND tenant_id = $2::uuid`,
			out.DocumentID, tenantID, string(out.ClassificationValue)); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}

		correlationID := ""
		if out.CorrelationID != nil {
			correlationID = *out.CorrelationID
		}
		return s.insertDocumentOutboxEvent(ctx, tx, "classification.confirmed", out.DocumentID, tenantID,
			out.LegalEntityID, p.ConfirmedByPrincipalID, correlationID, map[string]any{
				"classification_id":    out.ClassificationID,
				"document_id":          out.DocumentID,
				"tenant_id":            tenantID,
				"legal_entity_id":      out.LegalEntityID,
				"classification_value": string(out.ClassificationValue),
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetClassification returns the current classification for a document —
// the most recent row that has not itself been superseded.
func (s *PgStore) GetClassification(ctx context.Context, documentID string) (*domain.RecordClassification, error) {
	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT `+classificationColumns+` FROM record_classifications
			WHERE document_id = $1 AND status <> 'SUPERSEDED'
			ORDER BY proposed_at DESC LIMIT 1
		`, documentID)
		return scanClassification(row, &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrClassificationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
