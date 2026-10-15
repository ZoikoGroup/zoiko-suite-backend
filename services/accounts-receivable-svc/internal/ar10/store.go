package ar10

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// querier is satisfied by pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func newID() string { return uuid.NewString() }

// ── refunds ──────────────────────────────────────────────────────────────────

const refundCols = `refund_id::text, legal_entity_id::text, COALESCE(customer_party_id::text,''), receipt_id::text, COALESCE(related_open_item_id::text,''),
	currency, amount, destination_ref, destination_fingerprint, destination_verification, payer_account_ref, reason_code, evidence_id, state, binding_hash,
	COALESCE(approved_hash,''), requested_by, COALESCE(approved_by,''), approved_at, COALESCE(submitted_by,''), submitted_at, COALESCE(attempt_idempotency_key,''),
	COALESCE(external_attempt_id,''), COALESCE(provider_request_id,''), COALESCE(payment_status_id,''), external_state, payment_state, COALESCE(failure_reason,''),
	settled_at, COALESCE(accounting_state,''), COALESCE(journal_id,''), COALESCE(accounting_error,''), COALESCE(control_exception,''), COALESCE(cancel_reason,''),
	version, created_at, updated_at`

func scanRefund(row pgx.Row) (*Refund, error) {
	var r Refund
	var ver []byte
	if err := row.Scan(&r.RefundID, &r.LegalEntityID, &r.CustomerPartyID, &r.ReceiptID, &r.RelatedOpenItemID, &r.Currency, &r.Amount, &r.DestinationRef,
		&r.DestinationFingerprint, &ver, &r.PayerAccountRef, &r.ReasonCode, &r.EvidenceID, &r.State, &r.BindingHash, &r.ApprovedHash, &r.RequestedBy, &r.ApprovedBy,
		&r.ApprovedAt, &r.SubmittedBy, &r.SubmittedAt, &r.AttemptIdempotencyKey, &r.ExternalAttemptID, &r.ProviderRequestID, &r.PaymentStatusID, &r.ExternalState,
		&r.PaymentState, &r.FailureReason, &r.SettledAt, &r.AccountingState, &r.JournalID, &r.AccountingError, &r.ControlException, &r.CancelReason, &r.Version,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(ver, &r.DestinationVerification)
	return &r, nil
}

func getRefund(ctx context.Context, q querier, tenantID, id string, lock bool) (*Refund, error) {
	if err := mustUUID("refund_id", id); err != nil {
		return nil, err
	}
	sql := `SELECT ` + refundCols + ` FROM ar10_refunds WHERE tenant_id = $1 AND refund_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanRefund(q.QueryRow(ctx, sql, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("refund")
	}
	return r, err
}

// ── write-offs ───────────────────────────────────────────────────────────────

const writeOffCols = `writeoff_id::text, legal_entity_id::text, open_item_id::text, customer_party_id::text, currency, amount, residual_at_request, reason_category,
	reason_code, evidence_id, tax_treatment, tax_relief_amount, COALESCE(tax_reviewer,''), COALESCE(tax_evidence_id,''), tax_reviewed_at, required_tier, requester_tier,
	state, binding_hash, COALESCE(approved_hash,''), requested_by, COALESCE(approved_by,''), COALESCE(approved_tier,0), approved_at, COALESCE(committed_by,''),
	committed_at, COALESCE(fiscal_period,''), COALESCE(accounting_state,''), COALESCE(journal_id,''), COALESCE(accounting_error,''), version, created_at, updated_at`

func scanWriteOff(row pgx.Row) (*WriteOff, error) {
	var w WriteOff
	if err := row.Scan(&w.WriteOffID, &w.LegalEntityID, &w.OpenItemID, &w.CustomerPartyID, &w.Currency, &w.Amount, &w.ResidualAtRequest, &w.ReasonCategory,
		&w.ReasonCode, &w.EvidenceID, &w.TaxTreatment, &w.TaxReliefAmount, &w.TaxReviewer, &w.TaxEvidenceID, &w.TaxReviewedAt, &w.RequiredTier, &w.RequesterTier,
		&w.State, &w.BindingHash, &w.ApprovedHash, &w.RequestedBy, &w.ApprovedBy, &w.ApprovedTier, &w.ApprovedAt, &w.CommittedBy, &w.CommittedAt, &w.FiscalPeriod,
		&w.AccountingState, &w.JournalID, &w.AccountingError, &w.Version, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

func getWriteOff(ctx context.Context, q querier, tenantID, id string, lock bool) (*WriteOff, error) {
	if err := mustUUID("write_off_id", id); err != nil {
		return nil, err
	}
	sql := `SELECT ` + writeOffCols + ` FROM ar10_writeoffs WHERE tenant_id = $1 AND writeoff_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	w, err := scanWriteOff(q.QueryRow(ctx, sql, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("write-off")
	}
	return w, err
}

// ── history / evidence ───────────────────────────────────────────────────────

func addHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, aggType, id, command, from, to string, detail map[string]any) error {
	var d []byte
	if detail != nil {
		d, _ = json.Marshal(detail)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar10_history (history_id, tenant_id, legal_entity_id, aggregate_type, aggregate_id, command, from_state, to_state, actor_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, newID(), c.TenantID, c.LegalEntityID, aggType, id, command, from, to, c.PrincipalID, d)
	return err
}

func audit(ctx context.Context, tx pgx.Tx, c kit.Caller, aggType, aggID, command, reason, evidence string, before, after any, detail map[string]any) error {
	return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: aggType, AggregateID: aggID, Command: command, ReasonCode: reason, EvidenceID: evidence,
		Before: before, After: after, Detail: detail})
}

// collectionHoldOwner is the principal who last placed a collection hold on the open item ("" when unknown), read from the shared
// append-only audit trail AR-07 writes for PlaceCollectionHold.
func collectionHoldOwner(ctx context.Context, q querier, tenantID, openItemID string) (string, error) {
	var actor string
	err := q.QueryRow(ctx, `SELECT actor_id FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_type = 'AR_OPEN_ITEM' AND aggregate_id = $2
		AND command = 'PlaceCollectionHold' ORDER BY created_at DESC LIMIT 1`, tenantID, openItemID).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return actor, err
}

// postingStatus reads the ACC-04 queue row of a source fact (found=false = never queued).
func postingStatus(ctx context.Context, q querier, tenantID, sourceEventID string) (status string, found bool, err error) {
	e := q.QueryRow(ctx, `SELECT status FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, tenantID, sourceEventID).Scan(&status)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", false, nil
	}
	return status, e == nil, e
}
