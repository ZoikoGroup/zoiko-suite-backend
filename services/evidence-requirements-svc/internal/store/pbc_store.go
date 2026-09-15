package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

const evidenceRequestColumns = `request_id, tenant_id, legal_entity_id, requirement_id, title, description, assigned_to_principal_id,
	due_at, status, created_by_principal_id, correlation_id, created_at`

func scanEvidenceRequest(row pgx.Row) (*domain.EvidenceRequest, error) {
	r := &domain.EvidenceRequest{}
	err := row.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.RequirementID, &r.Title, &r.Description, &r.AssignedToPrincipalID,
		&r.DueAt, &r.Status, &r.CreatedByPrincipalID, &r.CorrelationID, &r.CreatedAt)
	if err == nil {
		r.IsOverdue = r.DueAt.Before(time.Now().UTC()) && r.Status != domain.EvidenceRequestSatisfied && r.Status != domain.EvidenceRequestClosed
	}
	return r, err
}

func (s *PgStore) CreateEvidenceRequest(ctx context.Context, requestID string, p domain.CreateEvidenceRequestParams) (*domain.EvidenceRequest, bool, error) {
	var out *domain.EvidenceRequest
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO evidence_requests (request_id, tenant_id, legal_entity_id, requirement_id, title, description, assigned_to_principal_id, due_at, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			requestID, p.TenantID, p.LegalEntityID, p.RequirementID, p.Title, p.Description, p.AssignedToPrincipalID, p.DueAt, p.CreatedByPrincipalID, p.CorrelationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			out, err = scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE request_id=$1`, requestID))
			created = true
			return err
		}
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetEvidenceRequest(ctx context.Context, tenantID, requestID string) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2`, requestID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListOpenEvidenceRequests(ctx context.Context, tenantID, legalEntityID string) ([]*domain.EvidenceRequest, error) {
	var out []*domain.EvidenceRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE tenant_id=$1 AND legal_entity_id=$2 AND status NOT IN ('SATISFIED','CLOSED') ORDER BY due_at`,
			tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanEvidenceRequest(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func transitionEvidenceRequest(ctx context.Context, tx pgx.Tx, requestID, tenantID string, from []domain.EvidenceRequestStatus, to domain.EvidenceRequestStatus) (*domain.EvidenceRequest, error) {
	fromStrs := make([]string, len(from))
	for i, f := range from {
		fromStrs[i] = string(f)
	}
	row := tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1 WHERE request_id=$2 AND tenant_id=$3 AND status = ANY($4) RETURNING `+evidenceRequestColumns,
		string(to), requestID, tenantID, fromStrs)
	out, err := scanEvidenceRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2)`, requestID, tenantID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrEvidenceRequestNotFound
		}
		return nil, domain.ErrEvidenceRequestInvalidState
	}
	return out, err
}

func (s *PgStore) SendEvidenceRequest(ctx context.Context, p domain.SendEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = transitionEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, []domain.EvidenceRequestStatus{domain.EvidenceRequestDraft}, domain.EvidenceRequestSent)
		return err
	})
	if errors.Is(err, domain.ErrEvidenceRequestNotFound) || errors.Is(err, domain.ErrEvidenceRequestInvalidState) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ReassignEvidenceRequest(ctx context.Context, p domain.ReassignEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET assigned_to_principal_id=$1 WHERE request_id=$2 AND tenant_id=$3 RETURNING `+evidenceRequestColumns,
			p.NewAssigneePrincipalID, p.RequestID, p.TenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ExtendDueDate(ctx context.Context, p domain.ExtendDueDateParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET due_at=$1 WHERE request_id=$2 AND tenant_id=$3 RETURNING `+evidenceRequestColumns,
			p.NewDueAt, p.RequestID, p.TenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ViewEvidenceRequest(ctx context.Context, p domain.ViewEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2 FOR UPDATE`, p.RequestID, p.TenantID))
		if err != nil {
			return err
		}
		if current.Status != domain.EvidenceRequestSent {
			out = current
			return nil // idempotent — already viewed or past it
		}
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1 WHERE request_id=$2 RETURNING `+evidenceRequestColumns, domain.EvidenceRequestViewed, p.RequestID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const responseColumns = `response_id, request_id, version, submitted_by_principal_id, artifact_document_id, malware_scan_status, created_at`

func scanResponse(row pgx.Row) (*domain.EvidenceRequestResponse, error) {
	r := &domain.EvidenceRequestResponse{}
	err := row.Scan(&r.ResponseID, &r.RequestID, &r.Version, &r.SubmittedByPrincipalID, &r.ArtifactDocumentID, &r.MalwareScanStatus, &r.CreatedAt)
	return r, err
}

// SubmitResponse always inserts the NEXT version — the real, DB-enforced
// mechanism behind AUD-NEG-017 "new PBC response overwrites prior
// version": there is no UPDATE path for an existing response's content at
// all (see the reject_evidence_response_mutation trigger).
func (s *PgStore) SubmitResponse(ctx context.Context, p domain.SubmitResponseParams) (*domain.EvidenceRequestResponse, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.EvidenceRequestResponse
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorResponseID string
		err := tx.QueryRow(ctx, `SELECT response_id FROM evidence_request_responses WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorResponseID)
		if err == nil {
			out, err = scanResponse(tx.QueryRow(ctx, `SELECT `+responseColumns+` FROM evidence_request_responses WHERE response_id=$1`, priorResponseID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2)`, p.RequestID, p.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrEvidenceRequestNotFound
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM evidence_request_responses WHERE request_id=$1`, p.RequestID).Scan(&nextVersion); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_request_responses (request_id, tenant_id, version, submitted_by_principal_id, artifact_document_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+responseColumns,
			p.RequestID, p.TenantID, nextVersion, p.SubmittedByPrincipalID, p.ArtifactDocumentID, p.CorrelationID)
		out, err = scanResponse(row)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE evidence_requests SET status=$1 WHERE request_id=$2 AND status NOT IN ($3,$4)`,
			domain.EvidenceRequestResponseReceived, p.RequestID, domain.EvidenceRequestSatisfied, domain.EvidenceRequestClosed); err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrEvidenceRequestNotFound) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) RecordScanResult(ctx context.Context, p domain.ScanResultParams) (*domain.EvidenceRequestResponse, error) {
	var out *domain.EvidenceRequestResponse
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE evidence_request_responses SET malware_scan_status=$1 WHERE response_id=$2 AND tenant_id=$3 AND malware_scan_status=$4 RETURNING `+responseColumns,
			p.Result, p.ResponseID, p.TenantID, domain.MalwareScanPending)
		var err error
		out, err = scanResponse(row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrResponseNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) AcknowledgeReceipt(ctx context.Context, p domain.AcknowledgeReceiptParams) (*domain.EvidenceRequestReceipt, error) {
	var out *domain.EvidenceRequestReceipt
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_request_responses WHERE response_id=$1 AND tenant_id=$2)`, p.ResponseID, p.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrResponseNotFound
		}
		row := tx.QueryRow(ctx, `INSERT INTO evidence_request_receipts (response_id, tenant_id, acknowledged_by_principal_id)
			VALUES ($1,$2,$3) RETURNING receipt_id, response_id, acknowledged_by_principal_id, acknowledged_at`,
			p.ResponseID, p.TenantID, p.AcknowledgedByPrincipalID)
		rec := &domain.EvidenceRequestReceipt{}
		if err := row.Scan(&rec.ReceiptID, &rec.ResponseID, &rec.AcknowledgedByPrincipalID, &rec.AcknowledgedAt); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if errors.Is(err, domain.ErrResponseNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) AddRequestNote(ctx context.Context, p domain.AddRequestNoteParams) (*domain.EvidenceRequestNote, error) {
	var out *domain.EvidenceRequestNote
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO evidence_request_notes (request_id, tenant_id, author_principal_id, body, visibility)
			VALUES ($1,$2,$3,$4,$5) RETURNING note_id, request_id, author_principal_id, body, visibility, created_at`,
			p.RequestID, p.TenantID, p.AuthorPrincipalID, p.Body, p.Visibility)
		n := &domain.EvidenceRequestNote{}
		if err := row.Scan(&n.NoteID, &n.RequestID, &n.AuthorPrincipalID, &n.Body, &n.Visibility, &n.CreatedAt); err != nil {
			return err
		}
		out = n
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) RequestClarification(ctx context.Context, p domain.RequestClarificationParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1 WHERE request_id=$2 AND tenant_id=$3 AND status=$4 RETURNING `+evidenceRequestColumns,
			domain.EvidenceRequestClarificationRequired, p.RequestID, p.TenantID, domain.EvidenceRequestResponseReceived))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvidenceRequestInvalidState
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// MarkSatisfied is the real, DB-enforced form of AUD-NEG-016 "PBC
// submitter marks own file sufficient evidence": the CAS predicate below
// refuses if the latest response's own submitter is the actor, and
// separately refuses unless that response's artifact has been scanned
// CLEAN.
func (s *PgStore) MarkSatisfied(ctx context.Context, p domain.MarkSatisfiedParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2)`, p.RequestID, p.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrEvidenceRequestNotFound
		}

		latest, err := scanResponse(tx.QueryRow(ctx, `SELECT `+responseColumns+` FROM evidence_request_responses WHERE request_id=$1 ORDER BY version DESC LIMIT 1`, p.RequestID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrEvidenceRequestInvalidState
		}
		if err != nil {
			return err
		}
		if latest.SubmittedByPrincipalID == p.ActorPrincipalID {
			return domain.ErrSelfEvaluationNotAllowed
		}
		if latest.MalwareScanStatus != domain.MalwareScanClean {
			return domain.ErrArtifactNotScanned
		}

		row := tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1
			WHERE request_id=$2 AND tenant_id=$3 AND status IN ($4,$5)
			AND NOT EXISTS (SELECT 1 FROM evidence_request_responses r WHERE r.request_id=$2 AND r.version=(SELECT MAX(version) FROM evidence_request_responses WHERE request_id=$2) AND r.submitted_by_principal_id=$6)
			RETURNING `+evidenceRequestColumns,
			domain.EvidenceRequestSatisfied, p.RequestID, p.TenantID, domain.EvidenceRequestResponseReceived, domain.EvidenceRequestUnderEvaluation, p.ActorPrincipalID)
		out, err = scanEvidenceRequest(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrEvidenceRequestInvalidState
		}
		return err
	})
	if errors.Is(err, domain.ErrEvidenceRequestNotFound) || errors.Is(err, domain.ErrEvidenceRequestInvalidState) ||
		errors.Is(err, domain.ErrSelfEvaluationNotAllowed) || errors.Is(err, domain.ErrArtifactNotScanned) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) CloseEvidenceRequest(ctx context.Context, p domain.CloseEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1 WHERE request_id=$2 AND tenant_id=$3 AND status <> $1 RETURNING `+evidenceRequestColumns,
			domain.EvidenceRequestClosed, p.RequestID, p.TenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		checkErr := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2)`, p.RequestID, p.TenantID).Scan(&exists)
		})
		if checkErr == nil && exists {
			return s.GetEvidenceRequest(ctx, p.TenantID, p.RequestID) // already closed — idempotent
		}
		return nil, domain.ErrEvidenceRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
