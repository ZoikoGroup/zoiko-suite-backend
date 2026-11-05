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
	due_at, status, created_by_principal_id, correlation_id, created_at, record_version`

// storeWrap preserves the domain errors callers are allowed to branch on and
// reclassifies raw driver failures as a store outage. In particular 22P02
// (malformed UUID) becomes ErrInvalidIdentifier instead of a 503.
func storeWrap(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{
		domain.ErrStoreUnavailable,
		domain.ErrInvalidIdentifier,
		domain.ErrIdempotencyMismatch,
		domain.ErrEvidenceRequestNotFound,
		domain.ErrEvidenceRequestInvalidState,
		domain.ErrResponseNotFound,
		domain.ErrSelfEvaluationNotAllowed,
		domain.ErrArtifactNotScanned,
		domain.ErrVersionMismatch,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%w: %w", domain.ErrStoreUnavailable, mapPgError(err))
}

func optionalUUIDEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func scanEvidenceRequest(row pgx.Row) (*domain.EvidenceRequest, error) {
	r := &domain.EvidenceRequest{}
	err := row.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.RequirementID, &r.Title, &r.Description, &r.AssignedToPrincipalID,
		&r.DueAt, &r.Status, &r.CreatedByPrincipalID, &r.CorrelationID, &r.CreatedAt, &r.RecordVersion)
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
		if err != nil {
			return err
		}
		// The same correlation id must describe the same request. Returning
		// the original for a different title/entity/assignee would report a
		// write that never happened (API standard: IDEMPOTENCY_MISMATCH).
		if out.LegalEntityID != p.LegalEntityID ||
			!optionalUUIDEqual(out.RequirementID, p.RequirementID) ||
			out.Title != p.Title ||
			out.Description != p.Description ||
			out.AssignedToPrincipalID != p.AssignedToPrincipalID ||
			// Postgres timestamptz has microsecond precision; a retried body
			// carrying nanoseconds (Go's RFC3339Nano) is the same request,
			// not a mismatch.
			!out.DueAt.UTC().Truncate(time.Microsecond).Equal(p.DueAt.UTC().Truncate(time.Microsecond)) ||
			out.CreatedByPrincipalID != p.CreatedByPrincipalID {
			return domain.ErrIdempotencyMismatch
		}
		return nil
	})
	if err != nil {
		return nil, false, storeWrap(err)
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
		return nil, storeWrap(err)
	}
	return out, nil
}

// GetResponseRequest resolves a response to its parent request. The scan
// callback needs the request's legal entity to authorize the write; without
// this lookup the handler could only check tenant scope, which is too broad.
func (s *PgStore) GetResponseRequest(ctx context.Context, tenantID, responseID string) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests
			WHERE request_id = (SELECT request_id FROM evidence_request_responses WHERE response_id=$1 AND tenant_id=$2)
			  AND tenant_id=$2`, responseID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrResponseNotFound
	}
	if err != nil {
		return nil, storeWrap(err)
	}
	return out, nil
}

// mutateEvidenceRequest is the one conditional UPDATE behind every manager
// command on a request (API-3). set is a SQL fragment whose placeholders start
// at $5 (setArgs); cond, when not empty, is an extra predicate that may use
// the same placeholders. The command applies only while the request is in one
// of from (any status when from is empty) and, unless expectedVersion is 0
// ("If-Match: *"), still at that record_version; record_version is bumped in
// the same statement. When no row changes it says why, in the same
// transaction: not found, a stale version (412), or a state that does not
// permit the command (422). Stale is reported first: either way the caller's
// read is out of date, and reading again is what they need to do.
func mutateEvidenceRequest(ctx context.Context, tx pgx.Tx, requestID, tenantID string, expectedVersion int64,
	from []domain.EvidenceRequestStatus, set, cond string, setArgs ...any) (*domain.EvidenceRequest, error) {
	fromStrs := make([]string, len(from))
	for i, f := range from {
		fromStrs[i] = string(f)
	}
	where := `request_id = $1 AND tenant_id = $2
		  AND ($3::bigint = 0 OR record_version = $3::bigint)
		  AND (cardinality($4::text[]) = 0 OR status = ANY($4::text[]))`
	if cond != "" {
		where += " AND " + cond
	}
	args := append([]any{requestID, tenantID, expectedVersion, fromStrs}, setArgs...)
	out, err := scanEvidenceRequest(tx.QueryRow(ctx,
		`UPDATE evidence_requests SET `+set+`, record_version = record_version + 1 WHERE `+where+` RETURNING `+evidenceRequestColumns,
		args...))
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var current int64
	var status string
	switch err := tx.QueryRow(ctx, `SELECT record_version, status FROM evidence_requests WHERE request_id = $1 AND tenant_id = $2`,
		requestID, tenantID).Scan(&current, &status); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, domain.ErrEvidenceRequestNotFound
	case err != nil:
		return nil, err
	}
	if expectedVersion != 0 && current != expectedVersion {
		return nil, &domain.StaleVersionError{Current: current}
	}
	return nil, domain.ErrEvidenceRequestInvalidState
}

// pbcResult maps a request command's error: the domain answers pass through,
// pgx.ErrNoRows is a missing request, anything else is a store failure.
func pbcResult(out *domain.EvidenceRequest, err error) (*domain.EvidenceRequest, error) {
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, domain.ErrEvidenceRequestNotFound
	default:
		return nil, storeWrap(err)
	}
}

func (s *PgStore) SendEvidenceRequest(ctx context.Context, p domain.SendEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			[]domain.EvidenceRequestStatus{domain.EvidenceRequestDraft}, "status = $5", "", string(domain.EvidenceRequestSent))
		return err
	})
	return pbcResult(out, err)
}

func (s *PgStore) ReassignEvidenceRequest(ctx context.Context, p domain.ReassignEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			nil, "assigned_to_principal_id = $5", "", p.NewAssigneePrincipalID)
		return err
	})
	return pbcResult(out, err)
}

func (s *PgStore) ExtendDueDate(ctx context.Context, p domain.ExtendDueDateParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			nil, "due_at = $5", "", p.NewDueAt)
		return err
	})
	return pbcResult(out, err)
}

// ViewEvidenceRequest records that the assignee opened a SENT request. It is
// the contributor's action, so it takes no If-Match; it moves the status, so
// it bumps record_version.
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
		out, err = scanEvidenceRequest(tx.QueryRow(ctx, `UPDATE evidence_requests SET status=$1, record_version = record_version + 1
			WHERE request_id=$2 AND tenant_id=$3 RETURNING `+evidenceRequestColumns, domain.EvidenceRequestViewed, p.RequestID, p.TenantID))
		return err
	})
	return pbcResult(out, err)
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
		// Lock the parent request so two concurrent submissions cannot both
		// read MAX(version) and race to insert the same version.
		var lockedRequestID string
		if err := tx.QueryRow(ctx,
			`SELECT request_id FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2 FOR UPDATE`,
			p.RequestID, p.TenantID).Scan(&lockedRequestID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrEvidenceRequestNotFound
			}
			return err
		}

		var priorResponseID string
		err := tx.QueryRow(ctx, `SELECT response_id FROM evidence_request_responses WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorResponseID)
		if err == nil {
			out, err = scanResponse(tx.QueryRow(ctx, `SELECT `+responseColumns+` FROM evidence_request_responses WHERE response_id=$1`, priorResponseID))
			if err != nil {
				return err
			}
			if out.RequestID != p.RequestID ||
				out.SubmittedByPrincipalID != p.SubmittedByPrincipalID ||
				!optionalUUIDEqual(out.ArtifactDocumentID, p.ArtifactDocumentID) {
				return domain.ErrIdempotencyMismatch
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM evidence_request_responses WHERE request_id=$1`, p.RequestID).Scan(&nextVersion); err != nil {
			return err
		}
		out, err = scanResponse(tx.QueryRow(ctx, `INSERT INTO evidence_request_responses (request_id, tenant_id, version, submitted_by_principal_id, artifact_document_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
			RETURNING `+responseColumns,
			p.RequestID, p.TenantID, nextVersion, p.SubmittedByPrincipalID, p.ArtifactDocumentID, p.CorrelationID))
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent request using the same correlation id inserted
			// first. Return that row, but only if it describes the same
			// submission.
			out, err = scanResponse(tx.QueryRow(ctx, `SELECT `+responseColumns+` FROM evidence_request_responses WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
			if err != nil {
				return err
			}
			if out.RequestID != p.RequestID ||
				out.SubmittedByPrincipalID != p.SubmittedByPrincipalID ||
				!optionalUUIDEqual(out.ArtifactDocumentID, p.ArtifactDocumentID) {
				return domain.ErrIdempotencyMismatch
			}
			return nil
		}
		if err != nil {
			return err
		}

		// A new response moves the request to RESPONSE_RECEIVED and is a change
		// to the aggregate, so it bumps record_version: a manager still holding
		// the ETag from before the response is refused (412) and reads again.
		if _, err := tx.Exec(ctx, `UPDATE evidence_requests SET status=$1, record_version = record_version + 1
			WHERE request_id=$2 AND tenant_id=$3 AND status NOT IN ($4,$5)`,
			domain.EvidenceRequestResponseReceived, p.RequestID, p.TenantID, domain.EvidenceRequestSatisfied, domain.EvidenceRequestClosed); err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrEvidenceRequestNotFound) || errors.Is(err, domain.ErrIdempotencyMismatch) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, storeWrap(err)
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
		return nil, storeWrap(err)
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
		return nil, storeWrap(err)
	}
	return out, nil
}

func (s *PgStore) AddRequestNote(ctx context.Context, p domain.AddRequestNoteParams) (*domain.EvidenceRequestNote, error) {
	var out *domain.EvidenceRequestNote
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2)`, p.RequestID, p.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrEvidenceRequestNotFound
		}
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
		return nil, storeWrap(err)
	}
	return out, nil
}

func (s *PgStore) RequestClarification(ctx context.Context, p domain.RequestClarificationParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			[]domain.EvidenceRequestStatus{domain.EvidenceRequestResponseReceived}, "status = $5", "",
			string(domain.EvidenceRequestClarificationRequired))
		return err
	})
	return pbcResult(out, err)
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

		// The self-evaluation refusal is repeated inside the CAS so that a
		// response submitted between the read above and this write cannot
		// slip past it.
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			[]domain.EvidenceRequestStatus{domain.EvidenceRequestResponseReceived, domain.EvidenceRequestUnderEvaluation},
			"status = $5",
			`NOT EXISTS (SELECT 1 FROM evidence_request_responses r WHERE r.request_id=$1
			   AND r.version=(SELECT MAX(version) FROM evidence_request_responses WHERE request_id=$1)
			   AND r.submitted_by_principal_id=$6)`,
			string(domain.EvidenceRequestSatisfied), p.ActorPrincipalID)
		return err
	})
	return pbcResult(out, err)
}

// CloseEvidenceRequest closes a request from any open status. Closing one
// that is already closed, with a current If-Match, returns it unchanged
// (idempotent); with a stale one it is 412 like any other command.
func (s *PgStore) CloseEvidenceRequest(ctx context.Context, p domain.CloseEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	var out *domain.EvidenceRequest
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = mutateEvidenceRequest(ctx, tx, p.RequestID, p.TenantID, p.ExpectedVersion,
			[]domain.EvidenceRequestStatus{domain.EvidenceRequestDraft, domain.EvidenceRequestSent, domain.EvidenceRequestViewed,
				domain.EvidenceRequestResponseReceived, domain.EvidenceRequestUnderEvaluation, domain.EvidenceRequestSatisfied,
				domain.EvidenceRequestClarificationRequired},
			"status = $5", "", string(domain.EvidenceRequestClosed))
		if errors.Is(err, domain.ErrEvidenceRequestInvalidState) {
			out, err = scanEvidenceRequest(tx.QueryRow(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests WHERE request_id=$1 AND tenant_id=$2`, p.RequestID, p.TenantID))
		}
		return err
	})
	return pbcResult(out, err)
}
