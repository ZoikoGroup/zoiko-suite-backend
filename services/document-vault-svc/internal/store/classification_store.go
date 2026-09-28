// BIZ-02 Record Classification persistence — see internal/domain/classification.go
// for the full lifecycle/immutability doc comment and migration 000008 for
// the schema/trigger this operates against.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// Reclassify inserts a new CANDIDATE classification proposal meant to
// replace a document's existing CONFIRMED/RESTRICTED classification.
// Refuses if no such classification exists yet — ClassifyRecord is the
// command for that case. The new row is a plain CANDIDATE like
// ClassifyRecord's; it only becomes the document's classification of
// record once SupersedeClassification confirms it and supersedes the
// one it replaces.
func (s *PgStore) Reclassify(ctx context.Context, p domain.ReclassifyParams) (*domain.RecordClassification, error) {
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
	var previousClassificationID string
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var legalEntityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`,
			p.DocumentID, tenantID).Scan(&legalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		var current domain.RecordClassification
		row := tx.QueryRow(ctx, `
			SELECT `+classificationColumns+` FROM record_classifications
			WHERE document_id = $1 AND status <> 'SUPERSEDED'
			ORDER BY proposed_at DESC LIMIT 1
		`, p.DocumentID)
		if err := scanClassification(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotConfirmed
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !domain.CanReclassify(&current) {
			return domain.ErrClassificationNotConfirmed
		}
		previousClassificationID = current.ClassificationID

		row = tx.QueryRow(ctx, `
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

		return s.insertDocumentOutboxEvent(ctx, tx, "classification.reclassified", p.DocumentID, tenantID,
			legalEntityID, p.ProposedByPrincipalID, p.CorrelationID, map[string]any{
				"classification_id":          out.ClassificationID,
				"previous_classification_id": previousClassificationID,
				"document_id":                p.DocumentID,
				"tenant_id":                  tenantID,
				"legal_entity_id":            legalEntityID,
				"classification_value":       string(p.ClassificationValue),
				"source":                     string(p.Source),
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SupersedeClassification confirms a Reclassify proposal and, in the same
// transaction, marks the classification it replaces as SUPERSEDED —
// linking forward via superseded_by_classification_id, never overwriting
// the previous row's own facts. Mirrors SupersedeDocument's forward-link
// pattern (pg_store.go) applied to the classification aggregate.
func (s *PgStore) SupersedeClassification(ctx context.Context, p domain.SupersedeClassificationParams) (*domain.RecordClassification, error) {
	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var previous domain.RecordClassification
		row := tx.QueryRow(ctx, `SELECT `+classificationColumns+` FROM record_classifications WHERE classification_id = $1 AND tenant_id = $2::uuid`,
			p.PreviousClassificationID, tenantID)
		if err := scanClassification(row, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		// Checked in this order deliberately: a SUPERSEDED row already has
		// SupersededByClassificationID set, so checking that first reports
		// the precise reason (already superseded) instead of the more
		// generic "not confirmed" a status check alone would give it.
		if previous.SupersededByClassificationID != nil {
			return domain.ErrClassificationAlreadySuperseded
		}
		if previous.Status != domain.ClassificationStatusConfirmed && previous.Status != domain.ClassificationStatusRestricted {
			return domain.ErrClassificationNotConfirmed
		}

		var next domain.RecordClassification
		row = tx.QueryRow(ctx, `SELECT `+classificationColumns+` FROM record_classifications WHERE classification_id = $1 AND tenant_id = $2::uuid`,
			p.NewClassificationID, tenantID)
		if err := scanClassification(row, &next); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if next.DocumentID != previous.DocumentID {
			return domain.ErrClassificationDocumentMismatch
		}
		if next.Source == domain.ClassificationSourceHuman && next.ProposedByPrincipalID == p.ActorPrincipalID {
			return domain.ErrClassificationSelfConfirmation
		}
		if !domain.CanConfirmClassification(&next) {
			return domain.ErrClassificationNotCandidate
		}

		row = tx.QueryRow(ctx, `
			UPDATE record_classifications
			SET status = 'CONFIRMED', confirmed_by_principal_id = $3, confirmed_at = now()
			WHERE classification_id = $1 AND tenant_id = $2::uuid AND status = 'CANDIDATE'
			RETURNING `+classificationColumns,
			p.NewClassificationID, tenantID, p.ActorPrincipalID,
		)
		if err := scanClassification(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClassificationNotCandidate
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE record_classifications
			SET status = 'SUPERSEDED', superseded_by_classification_id = $3
			WHERE classification_id = $1 AND tenant_id = $2::uuid AND superseded_by_classification_id IS NULL
		`, p.PreviousClassificationID, tenantID, out.ClassificationID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrClassificationAlreadySuperseded
		}

		if _, err := tx.Exec(ctx, `UPDATE documents SET classification = $3, updated_at = now() WHERE document_id = $1 AND tenant_id = $2::uuid`,
			out.DocumentID, tenantID, string(out.ClassificationValue)); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}

		correlationID := ""
		if out.CorrelationID != nil {
			correlationID = *out.CorrelationID
		}
		if err := s.insertDocumentOutboxEvent(ctx, tx, "classification.confirmed", out.DocumentID, tenantID,
			out.LegalEntityID, p.ActorPrincipalID, correlationID, map[string]any{
				"classification_id":    out.ClassificationID,
				"document_id":          out.DocumentID,
				"tenant_id":            tenantID,
				"legal_entity_id":      out.LegalEntityID,
				"classification_value": string(out.ClassificationValue),
			}); err != nil {
			return err
		}
		return s.insertDocumentOutboxEvent(ctx, tx, "classification.superseded", out.DocumentID, tenantID,
			out.LegalEntityID, p.ActorPrincipalID, correlationID, map[string]any{
				"classification_id":               previous.ClassificationID,
				"superseded_by_classification_id": out.ClassificationID,
				"document_id":                     out.DocumentID,
				"tenant_id":                       tenantID,
				"legal_entity_id":                 out.LegalEntityID,
			})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAsOfClassification returns whichever classification was CONFIRMED
// and in effect at a given point in time — the row with the latest
// confirmed_at at or before asOf, regardless of its current status.
// A row this returns may since have been SUPERSEDED; that is expected —
// this answers "what governed the document at time asOf", the same
// historical-reconstruction contract GetAsOfDocument already gives for
// document versions, not "what governs it now" (that's GetClassification).
func (s *PgStore) GetAsOfClassification(ctx context.Context, documentID string, asOf time.Time) (*domain.RecordClassification, error) {
	var out domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT `+classificationColumns+` FROM record_classifications
			WHERE document_id = $1 AND confirmed_at IS NOT NULL AND confirmed_at <= $2
			ORDER BY confirmed_at DESC LIMIT 1
		`, documentID, asOf)
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

// ListClassificationHistory returns every classification decision ever
// proposed for a document, oldest first — CANDIDATE, CONFIRMED,
// SUPERSEDED and RESTRICTED rows alike. GetClassification and
// GetAsOfClassification each answer "what governs/governed this
// document"; this answers "everything that was ever proposed for it",
// the full audit trail the doc's own event catalogue implies.
func (s *PgStore) ListClassificationHistory(ctx context.Context, documentID string) ([]domain.RecordClassification, error) {
	var out []domain.RecordClassification
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+classificationColumns+` FROM record_classifications
			WHERE document_id = $1
			ORDER BY proposed_at ASC
		`, documentID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.RecordClassification
			if err := scanClassification(rows, &c); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListUnclassified returns the documents in one legal entity that have
// never had a classification CONFIRMED — every document lands with a
// seeded CANDIDATE at upload (Wave 1), so "unclassified" here means
// "still nothing but candidate proposals", the governance backlog a
// steward works through.
func (s *PgStore) ListUnclassified(ctx context.Context, legalEntityID string, limit, offset int) ([]domain.Document, error) {
	var out []domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, err := tx.Query(ctx, `
			SELECT `+documentColumns+` FROM documents d
			WHERE d.tenant_id = $1::uuid AND d.legal_entity_id = $2::uuid
			AND NOT EXISTS (
				SELECT 1 FROM record_classifications rc
				WHERE rc.document_id = d.document_id AND rc.status IN ('CONFIRMED', 'RESTRICTED')
			)
			ORDER BY d.created_at DESC, d.document_id DESC
			LIMIT $3 OFFSET $4
		`, tenantID, legalEntityID, limit, offset)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.Document
			if err := scanDocument(rows, &d); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExplainPolicyMapping is deliberately honest rather than fabricated: no
// classification->confirmation policy engine exists in this codebase
// (see domain.AIConfidenceAutoConfirmThreshold's own doc comment —
// AI-02/DATA-GOV are the real owners of that policy and neither exists
// yet). This returns the classification's own recorded decision inputs
// (source, confidence, rule/model version, evidence) and states plainly
// that no automatic policy mapping was applied — never a confidence
// threshold or rule name this service was never given.
func (s *PgStore) ExplainPolicyMapping(ctx context.Context, classificationID string) (*domain.PolicyMappingExplanation, error) {
	c, err := s.FindClassificationByID(ctx, classificationID)
	if err != nil {
		return nil, err
	}
	explanation := "This classification requires human confirmation via ConfirmClassification/SupersedeClassification. " +
		"No automatic confidence-threshold or rule-based policy mapping is configured in this service yet."
	if c.Source == domain.ClassificationSourceHuman {
		explanation = "This classification was proposed by a human and carries no confidence score or policy mapping — " +
			"it requires confirmation by a different principal (maker-checker), not a policy threshold."
	}
	return &domain.PolicyMappingExplanation{
		ClassificationID:       c.ClassificationID,
		ClassificationValue:    c.ClassificationValue,
		Source:                 c.Source,
		Confidence:             c.Confidence,
		RuleModelVersion:       c.RuleModelVersion,
		SourceEvidence:         c.SourceEvidence,
		AutoConfirmThreshold:   domain.AIConfidenceAutoConfirmThreshold,
		AutoConfirmPolicyOwner: "AI-02 / DATA-GOV (not yet implemented)",
		AutoConfirmApplied:     false,
		Explanation:            explanation,
	}, nil
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
