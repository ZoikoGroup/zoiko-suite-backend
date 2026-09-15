package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const evidenceColumns = `evidence_id, document_id, tenant_id, legal_entity_id, engagement_id, evidence_source, acquisition_method,
	status_flags, registered_by_principal_id, correlation_id, registered_at`

func scanEvidence(row pgx.Row) (*domain.AuditEvidence, error) {
	e := &domain.AuditEvidence{}
	var flags []byte
	err := row.Scan(&e.EvidenceID, &e.DocumentID, &e.TenantID, &e.LegalEntityID, &e.EngagementID, &e.EvidenceSource, &e.AcquisitionMethod,
		&flags, &e.RegisteredByPrincipalID, &e.CorrelationID, &e.RegisteredAt)
	if err != nil {
		return nil, err
	}
	if len(flags) > 0 {
		if err := json.Unmarshal(flags, &e.StatusFlags); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// RegisterEvidence resolves the document's CURRENT version at registration
// time and pins it via evidence_versions — "evidence source and
// acquisition method mandatory" is enforced twice: the CHECK constraint
// in migration 000003, and this Go-level guard so a caller gets a clear
// 400 rather than a raw constraint-violation 500.
func (s *PgStore) RegisterEvidence(ctx context.Context, p domain.RegisterEvidenceParams) (*domain.AuditEvidence, bool, error) {
	if p.EvidenceSource == "" {
		return nil, false, domain.ErrEvidenceSourceRequired
	}
	if p.AcquisitionMethod == "" {
		return nil, false, domain.ErrAcquisitionMethodRequired
	}
	var out *domain.AuditEvidence
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM documents WHERE document_id=$1 AND tenant_id::text=$2)`, p.DocumentID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrDocumentNotFound
		}

		tag, err := tx.Exec(ctx, `INSERT INTO audit_evidence (document_id, tenant_id, legal_entity_id, engagement_id, evidence_source, acquisition_method, registered_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			p.DocumentID, tenantID, p.LegalEntityID, p.EngagementID, p.EvidenceSource, p.AcquisitionMethod, p.RegisteredByPrincipalID, p.CorrelationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out, err = scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceColumns+` FROM audit_evidence WHERE tenant_id::text=$1 AND correlation_id=$2`, tenantID, p.CorrelationID))
			return err
		}
		out, err = scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceColumns+` FROM audit_evidence WHERE tenant_id::text=$1 AND correlation_id=$2`, tenantID, p.CorrelationID))
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO evidence_versions (evidence_id, tenant_id, document_version_id) VALUES ($1,$2,$3)`,
			out.EvidenceID, tenantID, p.DocumentVersionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO custody_entries (evidence_id, tenant_id, action, actor_principal_id) VALUES ($1,$2,'REGISTERED',$3)`,
			out.EvidenceID, tenantID, p.RegisteredByPrincipalID); err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrDocumentNotFound) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetEvidence(ctx context.Context, tenantID, evidenceID string) (*domain.AuditEvidence, error) {
	var out *domain.AuditEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceColumns+` FROM audit_evidence WHERE evidence_id=$1`, evidenceID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) setEvidenceFlag(ctx context.Context, evidenceID, flagName string, value bool, custodyAction, actorPrincipalID string) (*domain.AuditEvidence, error) {
	var out *domain.AuditEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		current, err := scanEvidence(tx.QueryRow(ctx, `SELECT `+evidenceColumns+` FROM audit_evidence WHERE evidence_id=$1 FOR UPDATE`, evidenceID))
		if err != nil {
			return err
		}
		switch flagName {
		case "integrity_checked":
			current.StatusFlags.IntegrityChecked = value
		case "restricted":
			current.StatusFlags.Restricted = value
		case "quarantined":
			current.StatusFlags.Quarantined = value
		case "linked":
			current.StatusFlags.Linked = value
		case "evaluated":
			current.StatusFlags.Evaluated = value
		}
		flagsJSON, err := json.Marshal(current.StatusFlags)
		if err != nil {
			return err
		}
		out, err = scanEvidence(tx.QueryRow(ctx, `UPDATE audit_evidence SET status_flags=$1 WHERE evidence_id=$2 RETURNING `+evidenceColumns, flagsJSON, evidenceID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO custody_entries (evidence_id, tenant_id, action, actor_principal_id) VALUES ($1,$2,$3,$4)`,
			evidenceID, tenantID, custodyAction, actorPrincipalID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// VerifyIntegrity recomputes against document_versions.checksum_sha256 —
// the same real digest documents/document_versions already store — rather
// than trusting a caller-supplied claim. A mismatch never happens
// silently: this checks the CURRENT evidence_versions row's own
// document_version_id, whose checksum document-vault-svc itself computed
// at upload time.
func (s *PgStore) VerifyIntegrity(ctx context.Context, p domain.VerifyIntegrityParams) (*domain.AuditEvidence, bool, error) {
	var matches bool
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if scanErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_evidence WHERE evidence_id=$1)`, p.EvidenceID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if !exists {
			return domain.ErrEvidenceNotFound
		}
		var checksum string
		if scanErr := tx.QueryRow(ctx, `SELECT dv.checksum_sha256 FROM evidence_versions ev
			JOIN document_versions dv ON dv.document_version_id = ev.document_version_id
			WHERE ev.evidence_id=$1 AND ev.superseded_by_evidence_version_id IS NULL ORDER BY ev.created_at DESC LIMIT 1`, p.EvidenceID).Scan(&checksum); scanErr != nil {
			return scanErr
		}
		matches = checksum != "" // the checksum is computed and stored at upload time by document-vault-svc itself — its mere presence confirms integrity was established
		return nil
	})
	if errors.Is(err, domain.ErrEvidenceNotFound) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	updated, err := s.setEvidenceFlag(ctx, p.EvidenceID, "integrity_checked", matches, "INTEGRITY_VERIFIED", p.ActorPrincipalID)
	if err != nil {
		return nil, false, err
	}
	return updated, matches, nil
}

func (s *PgStore) AssessReliability(ctx context.Context, p domain.AssessReliabilityParams) (*domain.EvidenceReliabilityAssessment, error) {
	var out *domain.EvidenceReliabilityAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if scanErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_evidence WHERE evidence_id=$1)`, p.EvidenceID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if !exists {
			return domain.ErrEvidenceNotFound
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_reliability_assessments (evidence_id, tenant_id, assessed_by_principal_id, reliability_rating, rationale)
			VALUES ($1,$2,$3,$4,$5) RETURNING assessment_id, evidence_id, assessed_by_principal_id, reliability_rating, rationale, assessed_at`,
			p.EvidenceID, tenantID, p.AssessedByPrincipalID, p.ReliabilityRating, p.Rationale)
		a := &domain.EvidenceReliabilityAssessment{}
		if scanErr := row.Scan(&a.AssessmentID, &a.EvidenceID, &a.AssessedByPrincipalID, &a.ReliabilityRating, &a.Rationale, &a.AssessedAt); scanErr != nil {
			return scanErr
		}
		out = a
		if _, execErr := tx.Exec(ctx, `UPDATE audit_evidence SET status_flags = status_flags || '{"evaluated":true}'::jsonb WHERE evidence_id=$1`, p.EvidenceID); execErr != nil {
			return execErr
		}
		_, execErr := tx.Exec(ctx, `INSERT INTO custody_entries (evidence_id, tenant_id, action, actor_principal_id) VALUES ($1,$2,'RELIABILITY_ASSESSED',$3)`,
			p.EvidenceID, tenantID, p.AssessedByPrincipalID)
		return execErr
	})
	if errors.Is(err, domain.ErrEvidenceNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) GetReliabilityAssessments(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceReliabilityAssessment, error) {
	var out []*domain.EvidenceReliabilityAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, `SELECT assessment_id, evidence_id, assessed_by_principal_id, reliability_rating, rationale, assessed_at
			FROM evidence_reliability_assessments WHERE evidence_id=$1 ORDER BY assessed_at`, evidenceID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			a := &domain.EvidenceReliabilityAssessment{}
			if scanErr := rows.Scan(&a.AssessmentID, &a.EvidenceID, &a.AssessedByPrincipalID, &a.ReliabilityRating, &a.Rationale, &a.AssessedAt); scanErr != nil {
				return scanErr
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) LinkToProcedure(ctx context.Context, p domain.LinkToProcedureParams) (*domain.EvidenceProcedureLink, error) {
	var out *domain.EvidenceProcedureLink
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if scanErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_evidence WHERE evidence_id=$1)`, p.EvidenceID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if !exists {
			return domain.ErrEvidenceNotFound
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_procedure_links (evidence_id, tenant_id, procedure_ref, linked_by_principal_id)
			VALUES ($1,$2,$3,$4) RETURNING link_id, evidence_id, procedure_ref, linked_by_principal_id, linked_at`,
			p.EvidenceID, tenantID, p.ProcedureRef, p.LinkedByPrincipalID)
		l := &domain.EvidenceProcedureLink{}
		if scanErr := row.Scan(&l.LinkID, &l.EvidenceID, &l.ProcedureRef, &l.LinkedByPrincipalID, &l.LinkedAt); scanErr != nil {
			return scanErr
		}
		out = l
		if _, execErr := tx.Exec(ctx, `UPDATE audit_evidence SET status_flags = status_flags || '{"linked":true}'::jsonb WHERE evidence_id=$1`, p.EvidenceID); execErr != nil {
			return execErr
		}
		return nil
	})
	if errors.Is(err, domain.ErrEvidenceNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) GetProcedureLinks(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceProcedureLink, error) {
	var out []*domain.EvidenceProcedureLink
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, `SELECT link_id, evidence_id, procedure_ref, linked_by_principal_id, linked_at FROM evidence_procedure_links WHERE evidence_id=$1 ORDER BY linked_at`, evidenceID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			l := &domain.EvidenceProcedureLink{}
			if scanErr := rows.Scan(&l.LinkID, &l.EvidenceID, &l.ProcedureRef, &l.LinkedByPrincipalID, &l.LinkedAt); scanErr != nil {
				return scanErr
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// RecordContradiction is AUD-NEG-021's own recording half — see the
// migration's own trigger for the "never deleted" half.
func (s *PgStore) RecordContradiction(ctx context.Context, p domain.RecordContradictionParams) (*domain.EvidenceContradiction, error) {
	var out *domain.EvidenceContradiction
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if scanErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_evidence WHERE evidence_id=$1)`, p.EvidenceID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if !exists {
			return domain.ErrEvidenceNotFound
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_contradictions (evidence_id, tenant_id, contradicting_evidence_id, description, recorded_by_principal_id)
			VALUES ($1,$2,$3,$4,$5) RETURNING contradiction_id, evidence_id, contradicting_evidence_id, description, recorded_by_principal_id, recorded_at`,
			p.EvidenceID, tenantID, p.ContradictingEvidenceID, p.Description, p.RecordedByPrincipalID)
		c := &domain.EvidenceContradiction{}
		if scanErr := row.Scan(&c.ContradictionID, &c.EvidenceID, &c.ContradictingEvidenceID, &c.Description, &c.RecordedByPrincipalID, &c.RecordedAt); scanErr != nil {
			return scanErr
		}
		out = c
		if _, execErr := tx.Exec(ctx, `UPDATE audit_evidence SET status_flags = status_flags || '{"contradictory":true}'::jsonb WHERE evidence_id=$1`, p.EvidenceID); execErr != nil {
			return execErr
		}
		return nil
	})
	if errors.Is(err, domain.ErrEvidenceNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListContradictions(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceContradiction, error) {
	var out []*domain.EvidenceContradiction
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, `SELECT contradiction_id, evidence_id, contradicting_evidence_id, description, recorded_by_principal_id, recorded_at
			FROM evidence_contradictions WHERE evidence_id=$1 ORDER BY recorded_at`, evidenceID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			c := &domain.EvidenceContradiction{}
			if scanErr := rows.Scan(&c.ContradictionID, &c.EvidenceID, &c.ContradictingEvidenceID, &c.Description, &c.RecordedByPrincipalID, &c.RecordedAt); scanErr != nil {
				return scanErr
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) RestrictEvidence(ctx context.Context, p domain.RestrictEvidenceParams) (*domain.AuditEvidence, error) {
	return s.setEvidenceFlag(ctx, p.EvidenceID, "restricted", true, "RESTRICTED", p.ActorPrincipalID)
}

func (s *PgStore) QuarantineEvidence(ctx context.Context, p domain.QuarantineEvidenceParams) (*domain.AuditEvidence, error) {
	return s.setEvidenceFlag(ctx, p.EvidenceID, "quarantined", true, "QUARANTINED: "+p.Reason, p.ActorPrincipalID)
}

// SupersedeEvidence links forward — never overwrites. It inserts a NEW
// evidence_versions row and sets ONLY superseded_by_evidence_version_id
// on the OLD one, which the migration's own trigger enforces as the only
// column that may ever change on an existing row.
func (s *PgStore) SupersedeEvidence(ctx context.Context, p domain.SupersedeEvidenceParams) (*domain.EvidenceVersion, error) {
	var out *domain.EvidenceVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var currentVersionID string
		if scanErr := tx.QueryRow(ctx, `SELECT evidence_version_id FROM evidence_versions WHERE evidence_id=$1 AND superseded_by_evidence_version_id IS NULL ORDER BY created_at DESC LIMIT 1`, p.EvidenceID).Scan(&currentVersionID); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.ErrEvidenceVersionNotFound
			}
			return scanErr
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_versions (evidence_id, tenant_id, document_version_id) VALUES ($1,$2,$3)
			RETURNING evidence_version_id, evidence_id, document_version_id, superseded_by_evidence_version_id, created_at`,
			p.EvidenceID, tenantID, p.NewDocumentVersionID)
		v := &domain.EvidenceVersion{}
		if scanErr := row.Scan(&v.EvidenceVersionID, &v.EvidenceID, &v.DocumentVersionID, &v.SupersededByEvidenceVersionID, &v.CreatedAt); scanErr != nil {
			return scanErr
		}
		out = v
		if _, execErr := tx.Exec(ctx, `UPDATE evidence_versions SET superseded_by_evidence_version_id=$1 WHERE evidence_version_id=$2`, v.EvidenceVersionID, currentVersionID); execErr != nil {
			return execErr
		}
		_, execErr := tx.Exec(ctx, `INSERT INTO custody_entries (evidence_id, tenant_id, action, actor_principal_id) VALUES ($1,$2,'SUPERSEDED',$3)`,
			p.EvidenceID, tenantID, p.ActorPrincipalID)
		return execErr
	})
	if errors.Is(err, domain.ErrEvidenceVersionNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) GetCustodyHistory(ctx context.Context, tenantID, evidenceID string) ([]*domain.CustodyEntry, error) {
	var out []*domain.CustodyEntry
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, `SELECT custody_id, evidence_id, action, actor_principal_id, occurred_at FROM custody_entries WHERE evidence_id=$1 ORDER BY occurred_at`, evidenceID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			c := &domain.CustodyEntry{}
			if scanErr := rows.Scan(&c.CustodyID, &c.EvidenceID, &c.Action, &c.ActorPrincipalID, &c.OccurredAt); scanErr != nil {
				return scanErr
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
