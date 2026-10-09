package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// ── duplicate detection reads ────────────────────────────────────────────────

// FindExactDuplicate returns the id of an invoice with the same raw invoice
// number for this supplier, or "".
func (s *PgStore) FindExactDuplicate(ctx context.Context, tenantID, vendorID, invoiceNumber string) (string, error) {
	var id string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT invoice_id::text FROM vendor_invoices WHERE tenant_id = $1 AND vendor_id = $2 AND invoice_number = $3`,
			tenantID, vendorID, invoiceNumber).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// FindDuplicateCandidates is the broad SQL pre-filter for near-duplicate
// scoring: same supplier, and either the same normalised number or the same
// currency+amount within the date window. Scoring itself is the pure
// domain.AssessDuplicates.
func (s *PgStore) FindDuplicateCandidates(ctx context.Context, tenantID, vendorID, excludeInvoiceID, normalized, currency string, amount float64, invoiceDate time.Time, cfg domain.DuplicateConfig) ([]domain.DuplicateCandidate, error) {
	var out []domain.DuplicateCandidate
	tol := float64(cfg.AmountToleranceCents) / 100
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT invoice_id::text, invoice_number, invoice_number_normalized, amount, currency_code, invoice_date, intake_state, document_type
			FROM vendor_invoices
			WHERE tenant_id = $1 AND vendor_id = $2 AND ($3 = '' OR invoice_id::text <> $3) AND intake_state <> 'REJECTED'
			  AND (invoice_number_normalized = $4
			       OR (currency_code = $5 AND amount BETWEEN $6::numeric - $7::numeric AND $6::numeric + $7::numeric
			           AND invoice_date BETWEEN $8::date - $9::int AND $8::date + $9::int))`,
			tenantID, vendorID, excludeInvoiceID, normalized, currency, amount, tol, invoiceDate, cfg.DateWindowDays)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.DuplicateCandidate
			var intake string
			if err := rows.Scan(&c.InvoiceID, &c.InvoiceNumber, &c.NumberNormalized, &c.Amount, &c.CurrencyCode, &c.InvoiceDate, &intake, &c.DocumentType); err != nil {
				return err
			}
			c.IntakeState = domain.IntakeState(intake)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) LatestAssessment(ctx context.Context, tenantID, invoiceID string) (*domain.DuplicateAssessment, error) {
	var a domain.DuplicateAssessment
	var inputs, cfg, ids, matches []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT assessment_id::text, tenant_id::text, invoice_id::text, verdict, score::float8, exact_key, near_key,
			       inputs, config, matched_invoice_ids, matches, trigger_command, assessed_at
			FROM invoice_duplicate_assessments WHERE tenant_id = $1 AND invoice_id = $2
			ORDER BY assessed_at DESC, assessment_id DESC LIMIT 1`, tenantID, invoiceID).
			Scan(&a.AssessmentID, &a.TenantID, &a.InvoiceID, &a.Verdict, &a.Score, &a.ExactKey, &a.NearKey,
				&inputs, &cfg, &ids, &matches, &a.TriggerCommand, &a.AssessedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	_ = json.Unmarshal(inputs, &a.Inputs)
	_ = json.Unmarshal(cfg, &a.Config)
	_ = json.Unmarshal(ids, &a.MatchedInvoiceIDs)
	_ = json.Unmarshal(matches, &a.Matches)
	return &a, nil
}

// GetInvoiceSource returns the stored canonical source payload.
func (s *PgStore) GetInvoiceSource(ctx context.Context, tenantID, invoiceID string) (json.RawMessage, error) {
	var payload []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source_payload FROM vendor_invoices WHERE tenant_id = $1 AND invoice_id = $2`, tenantID, invoiceID).Scan(&payload)
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, domain.ErrInvoiceNotFound
	}
	return payload, mapPgError(err)
}

func (s *PgStore) ListHistory(ctx context.Context, tenantID, invoiceID string) ([]domain.HistoryEntry, error) {
	out := []domain.HistoryEntry{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT history_id, invoice_id::text, version, command, actor_id, COALESCE(reason, ''), from_state, to_state, detail,
			       COALESCE(correlation_id, ''), occurred_at
			FROM invoice_history WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY history_id`, tenantID, invoiceID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var h domain.HistoryEntry
			var from, to, detail []byte
			if err := rows.Scan(&h.HistoryID, &h.InvoiceID, &h.Version, &h.Command, &h.ActorID, &h.Reason, &from, &to, &detail, &h.CorrelationID, &h.OccurredAt); err != nil {
				return err
			}
			h.FromState, h.ToState, h.Detail = from, to, detail
			out = append(out, h)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return []domain.HistoryEntry{}, nil
	}
	return out, err
}

func (s *PgStore) ListCorrectionLinks(ctx context.Context, tenantID, invoiceID string) ([]domain.CorrectionLink, error) {
	out := []domain.CorrectionLink{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT link_id::text, original_invoice_id::text, linked_invoice_id::text, link_kind, reason, created_by, created_at
			FROM invoice_correction_links WHERE tenant_id = $1 AND (original_invoice_id::text = $2 OR linked_invoice_id::text = $2)
			ORDER BY created_at`, tenantID, invoiceID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.CorrectionLink
			if err := rows.Scan(&l.LinkID, &l.OriginalInvoiceID, &l.LinkedInvoiceID, &l.LinkKind, &l.Reason, &l.CreatedBy, &l.CreatedAt); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// CreditTotal sums the gross amount of non-rejected CREDIT_NOTE documents linked
// to the original invoice.
func (s *PgStore) CreditTotal(ctx context.Context, tenantID, originalID string) (float64, error) {
	var total float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(v.amount), 0)::float8 FROM invoice_correction_links l
			JOIN vendor_invoices v ON v.invoice_id = l.linked_invoice_id
			WHERE l.tenant_id = $1 AND l.original_invoice_id::text = $2 AND l.link_kind = 'CREDIT'
			  AND v.document_type = 'CREDIT_NOTE' AND v.intake_state <> 'REJECTED'`, tenantID, originalID).Scan(&total)
	})
	return total, mapPgError(err)
}

// ── command idempotency ──────────────────────────────────────────────────────

// IdemClaim claims (tenant, key). claimed=true means the caller now owns the
// execution. Otherwise rec holds the existing record: the caller compares
// request_hash and either replays the stored response, refuses a reuse with a
// different body, or reports "in progress". A claim that never completed and is
// older than staleAfter is taken over.
func (s *PgStore) IdemClaim(ctx context.Context, tenantID, key, operation, requestHash string, staleAfter time.Duration) (rec *domain.IdemRecord, claimed bool, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ap_command_idempotency (tenant_id, idempotency_key, operation, request_hash)
			VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`, tenantID, key, operation, requestHash)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 1 {
			claimed = true
			return nil
		}
		var r domain.IdemRecord
		var code *int32
		if err := tx.QueryRow(ctx, `
			SELECT operation, request_hash, status_code, response, created_at FROM ap_command_idempotency
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key).Scan(&r.Operation, &r.RequestHash, &code, &r.Response, &r.CreatedAt); err != nil {
			return err
		}
		if code != nil {
			c := int(*code)
			r.StatusCode = &c
		}
		if r.StatusCode == nil && r.RequestHash == requestHash && time.Since(r.CreatedAt) > staleAfter {
			t, err := tx.Exec(ctx, `UPDATE ap_command_idempotency SET created_at = now()
				WHERE tenant_id = $1 AND idempotency_key = $2 AND status_code IS NULL AND created_at = $3`, tenantID, key, r.CreatedAt)
			if err != nil {
				return err
			}
			if t.RowsAffected() == 1 {
				claimed = true
				return nil
			}
		}
		rec = &r
		return nil
	})
	return rec, claimed, err
}

func (s *PgStore) IdemComplete(ctx context.Context, tenantID, key string, status int, response []byte) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ap_command_idempotency SET status_code = $3, response = $4, completed_at = now()
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key, status, response)
		return err
	})
}

// IdemRelease drops an unfinished claim (the command failed), so a retry runs.
func (s *PgStore) IdemRelease(ctx context.Context, tenantID, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM ap_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2 AND status_code IS NULL`, tenantID, key)
		return err
	})
}

// ── AP-08 payable-creation queue ─────────────────────────────────────────────

const payableCols = `request_id::text, tenant_id::text, legal_entity_id::text, invoice_id::text, source_reference, payload, principal_id,
	correlation_id, status, attempts, next_attempt_at, last_error, payable_id, created_at, completed_at`

func scanPayable(row pgx.Row) (*domain.PayableRequest, error) {
	var p domain.PayableRequest
	var payload []byte
	if err := row.Scan(&p.RequestID, &p.TenantID, &p.LegalEntityID, &p.InvoiceID, &p.SourceReference, &payload, &p.PrincipalID,
		&p.CorrelationID, &p.Status, &p.Attempts, &p.NextAttemptAt, &p.LastError, &p.PayableID, &p.CreatedAt, &p.CompletedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(payload, &p.Payload)
	return &p, nil
}

func (s *PgStore) GetPayableRequest(ctx context.Context, tenantID, invoiceID string) (*domain.PayableRequest, error) {
	p, err := scanPayable(s.pool.QueryRow(ctx, `SELECT `+payableCols+` FROM payable_creation_requests WHERE tenant_id::text = $1 AND invoice_id::text = $2`, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, mapPgError(err)
}

// ClaimPayableRequests leases due rows (FOR UPDATE SKIP LOCKED) and counts the attempt.
func (s *PgStore) ClaimPayableRequests(ctx context.Context, limit int, lease time.Duration) ([]domain.PayableRequest, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE payable_creation_requests SET status = 'IN_PROGRESS', attempts = attempts + 1, locked_until = now() + $2::interval
		WHERE request_id IN (
			SELECT request_id FROM payable_creation_requests
			WHERE status IN ('PENDING','IN_PROGRESS') AND next_attempt_at <= now() AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+payableCols, limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PayableRequest
	for rows.Next() {
		p, err := scanPayable(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PgStore) FinishPayableRequest(ctx context.Context, requestID, status, payableID, lastError string, retryAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE payable_creation_requests SET status = $2, payable_id = COALESCE(NULLIF($3, ''), payable_id), last_error = NULLIF($4, ''),
			next_attempt_at = $5, locked_until = NULL,
			completed_at = CASE WHEN $2 IN ('CREATED','BLOCKED','DEAD') THEN now() ELSE NULL END
		WHERE request_id::text = $1`, requestID, status, payableID, lastError, retryAt)
	return err
}

// ── ACC-04 posting queue ─────────────────────────────────────────────────────

const postingCols = `request_id::text, tenant_id::text, legal_entity_id::text, invoice_id::text, source_event_id, request_payload, principal_id,
	correlation_id, status, attempts, last_error, posting_execution_id, created_at, completed_at`

func scanPosting(row pgx.Row) (*domain.AccountingPostingRequest, error) {
	var p domain.AccountingPostingRequest
	var payload []byte
	if err := row.Scan(&p.RequestID, &p.TenantID, &p.LegalEntityID, &p.InvoiceID, &p.SourceEventID, &payload, &p.PrincipalID,
		&p.CorrelationID, &p.Status, &p.Attempts, &p.LastError, &p.PostingExecutionID, &p.CreatedAt, &p.CompletedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(payload, &p.Payload)
	return &p, nil
}

func (s *PgStore) GetPostingRequest(ctx context.Context, tenantID, invoiceID string) (*domain.AccountingPostingRequest, error) {
	p, err := scanPosting(s.pool.QueryRow(ctx, `SELECT `+postingCols+` FROM accounting_posting_requests WHERE tenant_id::text = $1 AND invoice_id::text = $2`, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, mapPgError(err)
}

func (s *PgStore) ClaimPostingRequests(ctx context.Context, limit int, lease time.Duration) ([]domain.AccountingPostingRequest, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE accounting_posting_requests SET status = 'IN_PROGRESS', attempts = attempts + 1, locked_until = now() + $2::interval, updated_at = now()
		WHERE request_id IN (
			SELECT request_id FROM accounting_posting_requests
			WHERE status IN ('PENDING','IN_PROGRESS') AND next_attempt_at <= now() AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+postingCols, limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AccountingPostingRequest
	for rows.Next() {
		p, err := scanPosting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PgStore) FinishPostingRequest(ctx context.Context, requestID, status, executionID, lastError string, retryAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE accounting_posting_requests SET status = $2, posting_execution_id = COALESCE(NULLIF($3, ''), posting_execution_id),
			last_error = NULLIF($4, ''), next_attempt_at = $5, locked_until = NULL, updated_at = now(),
			completed_at = CASE WHEN $2 IN ('POSTED','FAILED','QUARANTINED') THEN now() ELSE NULL END
		WHERE request_id::text = $1`, requestID, status, executionID, lastError, retryAt)
	return err
}
