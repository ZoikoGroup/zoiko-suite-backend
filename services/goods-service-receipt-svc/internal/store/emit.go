package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/events"
	"zoiko.io/goods-service-receipt-svc/internal/outbox"
)

const (
	sourceService = "goods-service-receipt-svc"
	aggregateType = "goods_service_receipt"
)

// receiptPayload is the business payload of a receipt event: the receipt itself
// (embedded, so existing consumers still find its fields at the top level of
// payload) plus, for a reversal, the reversal record.
type receiptPayload struct {
	*domain.GoodsServiceReceipt
	Reversal *domain.ReceiptReversal `json:"reversal,omitempty"`
}

// emit writes one outbox row per event type, inside the caller's transaction,
// each wrapped in the platform event envelope with the actor and correlation of
// the request that caused the change.
func emit(ctx context.Context, tx pgx.Tx, r *domain.GoodsServiceReceipt, cmd domain.Command, reversal *domain.ReceiptReversal, eventTypes ...string) error {
	for _, t := range eventTypes {
		if err := insertEnvelope(ctx, tx, r, cmd, t, "", receiptPayload{GoodsServiceReceipt: r, Reversal: reversal}); err != nil {
			return err
		}
	}
	return nil
}

// insertEnvelope wraps payload in the event envelope and inserts it as an
// outbox row. A dedupe collision is not an error: the fact already exists.
func insertEnvelope(ctx context.Context, tx pgx.Tx, r *domain.GoodsServiceReceipt, cmd domain.Command, eventType, dedupeKey string, payload any) error {
	id := uuid.NewString()
	env := events.Event{
		EventID:       "evt-" + id,
		EventType:     eventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: sourceService,
		EntityID:      r.ReceiptID,
		LegalEntityID: r.LegalEntityID,
		SourceVersion: r.Version,
		TenantID:      r.TenantID,
		ActorID:       cmd.PrincipalID,
		CorrelationID: cmd.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}
	if _, err := outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: id,
		AggregateType: aggregateType,
		AggregateID:   r.ReceiptID,
		EventType:     eventType,
		TenantID:      r.TenantID,
		LegalEntityID: r.LegalEntityID,
		ActorID:       cmd.PrincipalID,
		CorrelationID: cmd.CorrelationID,
		DedupeKey:     dedupeKey,
		Payload:       env,
	}); err != nil {
		return fmt.Errorf("emit %s: %w", eventType, err)
	}
	return nil
}

// ── GRNI posting requests ────────────────────────────────────────────────────

// AccountingConfig names the ACC-02 mapping keys the GRNI accrual posts against
// (the ledger resolves them to accounts; AP never names GL accounts) and the
// version stamped on every request as the posting policy it was built under.
type AccountingConfig struct {
	DebitMappingKey      string // received-not-invoiced expense / inventory side
	CreditMappingKey     string // GRNI accrual liability
	PostingPolicyVersion string
}

// DefaultAccountingConfig is used when none is supplied.
func DefaultAccountingConfig() AccountingConfig {
	return AccountingConfig{DebitMappingKey: "AP_GRNI_EXPENSE", CreditMappingKey: "AP_GRNI_ACCRUAL", PostingPolicyVersion: "grni-v1"}
}

// WithAccounting sets the mapping keys and policy version.
func (s *PgStore) WithAccounting(c AccountingConfig) *PgStore {
	if c.DebitMappingKey == "" {
		c.DebitMappingKey = DefaultAccountingConfig().DebitMappingKey
	}
	if c.CreditMappingKey == "" {
		c.CreditMappingKey = DefaultAccountingConfig().CreditMappingKey
	}
	if c.PostingPolicyVersion == "" {
		c.PostingPolicyVersion = DefaultAccountingConfig().PostingPolicyVersion
	}
	s.accounting = c
	return s
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

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// requestAccounting records the GRNI consequence of a confirmation (ACCRUE) or a
// reversal (REVERSE) as a durable accounting_posting_requests row in the
// caller's transaction. The receipt never writes the ledger itself (invariant
// #20): a dispatcher submits the row to ACC-04, which is idempotent on
// source_event_id.
//
// sourceEventID is the receipt id for the accrual and
// "<receipt id>:reversal:<reversal id>" for a reversal, and the table is unique
// on (tenant_id, source_event_id): a replayed confirmation or reversal can never
// queue -- and so never post -- the same consequence twice. It returns the new
// request, or nil when this source event was already queued.
func (s *PgStore) requestAccounting(ctx context.Context, tx pgx.Tx, r *domain.GoodsServiceReceipt, cmd domain.Command,
	direction, sourceEventID string, amount float64, documentDate time.Time) (*domain.ReceiptAccountingEvent, error) {

	debit, credit := s.accounting.DebitMappingKey, s.accounting.CreditMappingKey
	if direction == domain.DirectionReverse {
		debit, credit = credit, debit
	}
	amount = float64(cents(amount)) / 100
	corr := cmd.CorrelationID
	if corr == "" {
		corr = "ap04:" + sourceEventID
	}
	verb := "accrual"
	if direction == domain.DirectionReverse {
		verb = "accrual reversal"
	}
	payload, err := json.Marshal(postingRequest{
		LegalEntityID: r.LegalEntityID, FiscalPeriod: documentDate.UTC().Format("2006-01"),
		Description:   fmt.Sprintf("GRNI %s for goods/service receipt %s", verb, r.ReceiptID),
		SourceEventID: sourceEventID, CorrelationID: corr,
		TransactionCurrency: r.CurrencyCode, DocumentDate: documentDate.UTC().Format("2006-01-02"),
		Lines: []postingLine{
			{MappingKey: debit, DebitAmount: amount, Description: "GRNI " + verb},
			{MappingKey: credit, CreditAmount: amount, Description: "GRNI " + verb},
		},
	})
	if err != nil {
		return nil, err
	}
	var ev domain.ReceiptAccountingEvent
	var status string
	err = tx.QueryRow(ctx, `
		INSERT INTO accounting_posting_requests
			(request_id, tenant_id, legal_entity_id, receipt_id, source_event_id, direction, posting_policy_version,
			 request_payload, correlation_id, actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, source_event_id) DO NOTHING
		RETURNING request_id, tenant_id, receipt_id, source_event_id, direction, status, attempts, created_at`,
		uuid.NewString(), r.TenantID, r.LegalEntityID, r.ReceiptID, sourceEventID, direction, s.accounting.PostingPolicyVersion,
		payload, corr, cmd.PrincipalID,
	).Scan(&ev.EventID, &ev.TenantID, &ev.ReceiptID, &ev.SourceEventID, &ev.Direction, &status, &ev.Attempts, &ev.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil // already queued: a replay requests nothing new
	}
	if err != nil {
		return nil, fmt.Errorf("queue accounting posting request: %w", err)
	}
	ev.Status = domain.AccountingEventStatus(status)
	ev.PostingPolicyVersion = s.accounting.PostingPolicyVersion
	return &ev, nil
}

// queueProgressPush records, for a LINE receipt only, the received-quantity
// delta AP-03 must be told about. The row commits with the state change; the
// progress worker delivers it. Idempotent on (tenant, kind, source_ref) so a
// replay cannot queue a second delta.
func queueProgressPush(ctx context.Context, tx pgx.Tx, r *domain.GoodsServiceReceipt, cmd domain.Command, sourceRef string, quantity, amount float64, sign int) error {
	if r.POLineID == nil || quantity <= 0 || amount <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO po_progress_pushes (tenant_id, legal_entity_id, receipt_id, purchase_order_id, po_line_id,
			kind, quantity, amount, delta_sign, source_ref, correlation_id)
		VALUES ($1, $2, $3, $4, $5, 'RECEIVED', $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, kind, source_ref) DO NOTHING`,
		r.TenantID, r.LegalEntityID, r.ReceiptID, r.PurchaseOrderID, *r.POLineID,
		quantity, amount, sign, sourceRef, nullIfEmpty(cmd.CorrelationID))
	if err != nil {
		return fmt.Errorf("queue po progress push: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
