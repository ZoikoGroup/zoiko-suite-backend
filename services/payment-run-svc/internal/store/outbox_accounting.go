package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/outbox"
)

// AccountingConfig names the ACC-02 mapping keys used for a payment's
// posting. The ledger resolves them to accounts; AP never knows account codes.
type AccountingConfig struct {
	PayableControlKey     string // debited for the liability cleared (net + withholding)
	PaymentClearingKey    string // credited for the net amount paid to the payee
	WithholdingPayableKey string // credited for the amount withheld for the tax authority
}

// DefaultAccountingConfig is used when none is supplied.
func DefaultAccountingConfig() AccountingConfig {
	return AccountingConfig{
		PayableControlKey: "AP_PAYABLE_CONTROL", PaymentClearingKey: "AP_PAYMENT_CLEARING",
		WithholdingPayableKey: "AP_WITHHOLDING_PAYABLE",
	}
}

// WithAccounting sets the mapping keys used for settlement postings.
func (s *PgStore) WithAccounting(c AccountingConfig) *PgStore {
	s.accounting = c
	return s
}

// MaxPostingAttempts is how many transient failures a posting request
// survives before it is shown as FAILED (and waits for an operator requeue).
const MaxPostingAttempts = 10

// QueueWorkerAccounting is the app.queue_worker value migration 000008 accepts
// for cross-tenant dispatch of accounting_posting_requests.
const QueueWorkerAccounting = "accounting-dispatcher"

// enqueue writes a domain event to the transactional outbox, in the caller's
// transaction.
func (s *PgStore) enqueue(ctx context.Context, tx pgx.Tx, eventType, aggregateID, legalEntityID string, tenantID *string, entity any, actor string) error {
	corr := middleware.CorrelationIDFromContext(ctx)
	var corrPtr *string
	if corr != "" {
		corrPtr = &corr
	}
	id := uuid.NewString()
	env := outbox.NewVariantBEnvelope(id, eventType, aggregateID, tenantID, &actor, corrPtr, entity)
	return outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: id, AggregateType: "payment_run", AggregateID: aggregateID, EventType: eventType,
		TenantID: tenantID, LegalEntityID: legalEntityID, ActorID: &actor, CorrelationID: corrPtr, Payload: env,
	})
}

// recordAndEnqueue writes the run's evidence row and its outbox event
// together, so a published event always has matching evidence.
func (s *PgStore) recordAndEnqueue(ctx context.Context, tx pgx.Tx, r *domain.PaymentRun, eventType, detail, actor string) error {
	if err := s.recordEvent(ctx, tx, r.TenantID, r.RunID, eventType, detail, actor); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, eventType, r.RunID, r.LegalEntityID, r.TenantID, r, actor)
}

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// afterInstructionStatus runs inside ReconcileInstruction's transaction, once
// a new status has been applied: it publishes the instruction event and, for
// SETTLED, records the payment-clearing posting request. Both commit or roll
// back with the status change.
func (s *PgStore) afterInstructionStatus(ctx context.Context, tx pgx.Tx, i *domain.RunInstruction, reason, actor string) error {
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM payment_runs WHERE run_id = $1`, i.RunID))
	if err != nil {
		return err
	}
	if err := s.enqueue(ctx, tx, domain.InstructionEventType(i.Status), i.InstructionID, run.LegalEntityID, i.TenantID, i, actor); err != nil {
		return err
	}
	if i.Status != domain.InstructionSettled {
		return nil
	}
	return s.requestSettlementPosting(ctx, tx, run, i, actor)
}

// postingLine / postingRequest mirror general-ledger-svc's
// PostAccountingEventRequest (ACC-04), lines addressed by mapping key.
type postingLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

type postingRequest struct {
	LegalEntityID       string        `json:"legal_entity_id"`
	FiscalPeriod        string        `json:"fiscal_period"`
	Description         string        `json:"description"`
	SourceEventID       string        `json:"source_event_id"`
	CorrelationID       string        `json:"correlation_id"`
	TransactionCurrency string        `json:"transaction_currency"`
	DocumentDate        string        `json:"document_date"`
	Lines               []postingLine `json:"lines"`
}

// requestSettlementPosting records the accounting consequence of a
// bank-confirmed payment: the payable liability (net + withheld) is cleared
// against payment clearing (net) and withholding payable (withheld). It is
// keyed by the instruction, so a replayed settlement requests nothing twice.
// AP never posts to the ledger itself; the dispatcher submits this to ACC-04.
func (s *PgStore) requestSettlementPosting(ctx context.Context, tx pgx.Tx, run *domain.PaymentRun, i *domain.RunInstruction, actor string) error {
	var withholding float64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(withholding_amount), 0) FROM run_instruction_payables WHERE instruction_id = $1`, i.InstructionID).Scan(&withholding); err != nil {
		return err
	}
	net := float64(cents(i.NetAmount)) / 100
	wht := float64(cents(withholding)) / 100
	now := time.Now().UTC()
	corr := middleware.CorrelationIDFromContext(ctx)
	if corr == "" {
		corr = "ap11:" + i.InstructionID
	}
	req := postingRequest{
		LegalEntityID: run.LegalEntityID, FiscalPeriod: now.Format("2006-01"),
		Description:   fmt.Sprintf("Supplier payment settled: run %s instruction %s", run.RunID, i.InstructionID),
		SourceEventID: "ap11:settle:" + i.InstructionID, CorrelationID: corr,
		TransactionCurrency: i.Currency, DocumentDate: now.Format("2006-01-02"),
		Lines: []postingLine{
			{MappingKey: s.accounting.PayableControlKey, DebitAmount: net + wht, Description: "payable cleared"},
			{MappingKey: s.accounting.PaymentClearingKey, CreditAmount: net, Description: "payment clearing"},
		},
	}
	if wht > 0 {
		req.Lines = append(req.Lines, postingLine{MappingKey: s.accounting.WithholdingPayableKey, CreditAmount: wht, Description: "withholding payable"})
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO accounting_posting_requests (request_id, tenant_id, legal_entity_id, run_id, instruction_id, source_event_id, request_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		uuid.NewString(), i.TenantID, run.LegalEntityID, run.RunID, i.InstructionID, req.SourceEventID, payload)
	return err
}

// ── dispatch ─────────────────────────────────────────────────────────────────

// PostingResult is what the ledger answered for a request.
type PostingResult struct {
	ExecutionID string
	// Final means the answer is definitive: Posted=true is POSTED,
	// Posted=false with Final=true is QUARANTINED (the ledger refused it in a
	// way a retry will not fix). Final=false is a transient failure.
	Final  bool
	Posted bool
	Err    string
}

// PostingRequest is one row handed to the dispatcher.
type PostingRequest struct {
	RequestID     string
	TenantID      string
	LegalEntityID string
	SourceEventID string
	Payload       json.RawMessage
	Attempts      int
}

// DispatchPending claims up to limit PENDING requests (FOR UPDATE SKIP LOCKED,
// so replicas never post the same one) and submits each through post. A
// POSTED/QUARANTINED answer is final; a transient failure counts an attempt
// and shows as FAILED after MaxPostingAttempts. Returns how many were POSTED.
func (s *PgStore) DispatchPending(ctx context.Context, limit int, post func(context.Context, PostingRequest) PostingResult) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The dispatcher is a cross-tenant worker with no request tenant. Migration
	// 000008 lets this transaction-local marker (and only it) see and update the
	// queue across tenants; every tenant-facing query still runs under
	// app.tenant_id and sees only its own rows.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.queue_worker', $1, true)", QueueWorkerAccounting); err != nil {
		return 0, fmt.Errorf("set_config app.queue_worker: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT request_id::text, COALESCE(tenant_id::text, ''), legal_entity_id::text, source_event_id, request_payload, attempts
		FROM accounting_posting_requests WHERE status = 'PENDING'
		ORDER BY created_at ASC LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var batch []PostingRequest
	for rows.Next() {
		var r PostingRequest
		if err := rows.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.SourceEventID, &r.Payload, &r.Attempts); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	posted := 0
	for _, r := range batch {
		res := post(ctx, r)
		switch {
		case res.Final && res.Posted:
			_, err = tx.Exec(ctx, `UPDATE accounting_posting_requests SET status = 'POSTED', posting_execution_id = $2, attempts = attempts + 1, last_error = '', updated_at = NOW() WHERE request_id = $1`, r.RequestID, res.ExecutionID)
			posted++
		case res.Final:
			_, err = tx.Exec(ctx, `UPDATE accounting_posting_requests SET status = 'QUARANTINED', attempts = attempts + 1, last_error = $2, updated_at = NOW() WHERE request_id = $1`, r.RequestID, res.Err)
		default:
			status := "PENDING"
			if r.Attempts+1 >= MaxPostingAttempts {
				status = "FAILED"
			}
			_, err = tx.Exec(ctx, `UPDATE accounting_posting_requests SET status = $2, attempts = attempts + 1, last_error = $3, updated_at = NOW() WHERE request_id = $1`, r.RequestID, status, res.Err)
		}
		if err != nil {
			return posted, err
		}
	}
	return posted, tx.Commit(ctx)
}

// AccountingRequest is a posting request as exposed to queries.
type AccountingRequest struct {
	RequestID          string
	InstructionID      string
	SourceEventID      string
	Status             string
	Attempts           int
	LastError          string
	PostingExecutionID string
}

func (s *PgStore) ListAccountingRequests(ctx context.Context, runID string) ([]AccountingRequest, error) {
	var out []AccountingRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT request_id::text, instruction_id::text, source_event_id, status, attempts, last_error, posting_execution_id
			FROM accounting_posting_requests
			WHERE run_id = $1 AND (tenant_id IS NULL OR tenant_id::text = $2)
			ORDER BY created_at ASC`, runID, middleware.TenantFromContext(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r AccountingRequest
			if err := rows.Scan(&r.RequestID, &r.InstructionID, &r.SourceEventID, &r.Status, &r.Attempts, &r.LastError, &r.PostingExecutionID); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListAccountingRequests failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// RequeueAccountingRequests puts a run's FAILED/QUARANTINED requests back to
// PENDING (after an operator fixed the cause, e.g. a missing ACC-02 mapping).
// A POSTED request is never touched. Returns how many were requeued.
func (s *PgStore) RequeueAccountingRequests(ctx context.Context, runID string) (int64, error) {
	var n int64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accounting_posting_requests SET status = 'PENDING', attempts = 0, updated_at = NOW()
			WHERE run_id = $1 AND status IN ('FAILED', 'QUARANTINED')
			  AND (tenant_id IS NULL OR tenant_id::text = $2)`, runID, middleware.TenantFromContext(ctx))
		n = tag.RowsAffected()
		return err
	})
	if isInvalidUUID(err) {
		return 0, domain.ErrRunNotFound
	}
	if err != nil {
		s.log.Error("pg RequeueAccountingRequests failed", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return n, nil
}
