// Package events holds treasury-svc's cross-service event consumers.
// BankStatementConsumer is this service's first one — the fix for the
// Banking & Treasury audit's single most severe finding: treasury-svc
// never had a real bank balance, because its CreateCashBalance call at
// account registration wrote exactly one "0.00" placeholder, with its own
// error discarded, and nothing ever updated it. BNK-08's own
// composeBankBalances/CalculateCashPosition engine was always real; it
// just had no real data to read.
//
// Follows the exact Kafka reader + shared inbox pattern
// bank-reconciliation-svc already uses for its own first cross-service
// consumer (internal/events/consumer.go there, ConflictConsumer) — same
// at-least-once delivery, same exactly-once-in-effect guarantee via one
// inbox transaction per delivery, same "a missing broker must not stop the
// service" posture.
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/eventing/inbox"
	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// bankStatementAcceptedType is banking-connector-svc's own canonical event
// type (internal/domain/bnk0304.go's AcceptStatement, enqueueEvent's
// canonicalType argument there). legacyBankStatementAcceptedType is the
// same event's pre-standard event_type — matched too, since
// envelope.Parse / inbox.Parse fall back to whichever name is present.
const (
	bankStatementAcceptedType       = "com.zoikosuite.banking.bank-statement.accepted"
	legacyBankStatementAcceptedType = "banking.statement.accepted"
)

// ConsumerName is the stable inbox consumer name — renaming it forgets
// every receipt, so a redelivery after a rename would be reprocessed.
const ConsumerName = "treasury-svc.bank-statement-accepted"

// systemActorID is this consumer's own actor identity for the writes it
// makes (cash_balances, cash_position_snapshots) — there is no human or
// HTTP caller behind an event delivery.
const systemActorID = "system:treasury-svc:bank-statement-accepted-consumer"

// CashBalanceWriter is the one durable, in-transaction effect this
// consumer's inbox delivery must commit atomically with its own receipt:
// the real cash_balances row that replaces the dead 0.00 placeholder.
type CashBalanceWriter interface {
	CreateCashBalanceInTx(ctx context.Context, tx pgx.Tx, bal *domain.CashBalance) error
}

// Recalculator is the best-effort, outside-the-inbox-transaction step:
// BNK-08's own "recalculation on material bank/payment events" NFR,
// applied in-process (handler.Handler.RecalculateAndPublishCashPosition)
// rather than over HTTP. Its failure never fails the delivery — the real
// balance is already durably on file by the time this runs.
type Recalculator interface {
	RecalculateAndPublishCashPosition(ctx context.Context, tenantID, legalEntityID, currencyCode, correlationID, actorID string) (*domain.CashPositionSnapshot, error)
}

// Processor is the shared inbox, scoped down to just what this consumer
// needs — mirrors bank-reconciliation-svc's own Processor interface.
type Processor interface {
	Process(ctx context.Context, msg inbox.Message, fn inbox.Handler) (inbox.Result, error)
}

// BankStatementConsumer bridges banking-connector-svc's BankStatementAccepted
// event into a real treasury-svc bank balance.
type BankStatementConsumer struct {
	log        *zap.Logger
	balances   CashBalanceWriter
	recalc     Recalculator
	proc       Processor
	retryDelay time.Duration
}

func NewBankStatementConsumer(log *zap.Logger, balances CashBalanceWriter, recalc Recalculator, proc Processor) *BankStatementConsumer {
	return &BankStatementConsumer{log: log, balances: balances, recalc: recalc, proc: proc, retryDelay: time.Second}
}

// SetRetryDelay sets the pause between in-process attempts of one delivery.
func (c *BankStatementConsumer) SetRetryDelay(d time.Duration) { c.retryDelay = d }

// envelopeWire is the subset of the canonical ZS-EVENT-001 envelope
// (zoiko.io/eventing/envelope.Envelope) this consumer reads — only the
// fields it acts on, so a field banking-connector-svc's envelope package
// adds later doesn't break decoding here. "type" (canonical) and
// "event_type" (legacy) are both read; whichever is present is used.
type envelopeWire struct {
	ID            string          `json:"id"`
	LegacyID      string          `json:"event_id"`
	Type          string          `json:"type"`
	LegacyType    string          `json:"event_type"`
	TenantID      string          `json:"tenantid"`
	LegacyTenant  string          `json:"tenant_id"`
	PayloadHash   string          `json:"payloadhash"`
	Region        string          `json:"residencyregion"`
	Classification string         `json:"classification"`
	CorrelationID string          `json:"correlationid"`
	LegacyCorr    string          `json:"correlation_id"`
	Data          json.RawMessage `json:"data"`
}

// bankStatementAcceptedPayload mirrors banking-connector-svc's
// domain.BankStatementAcceptedEvent exactly (internal/domain/bnk0304.go
// there) — the event's own data payload.
type bankStatementAcceptedPayload struct {
	StatementID    string    `json:"statement_id"`
	ConnectionID   string    `json:"connection_id"`
	TenantID       string    `json:"tenant_id"`
	LegalEntityID  string    `json:"legal_entity_id"`
	BankAccountID  string    `json:"bank_account_id"`
	ClosingBalance float64   `json:"closing_balance"`
	OpeningBalance float64   `json:"opening_balance"`
	Currency       string    `json:"currency"`
	StatementDate  time.Time `json:"statement_date"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

// Run consumes until ctx is cancelled. A broker that is absent or
// unreachable must not crash the service — see
// bank-reconciliation-svc's ConflictConsumer.Run, copied verbatim here.
func (c *BankStatementConsumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("bank-statement consumer: kafka reader close failed", zap.Error(err))
		}
	}()

	c.log.Info("bank-statement-accepted consumer started",
		zap.Strings("brokers", reader.Config().Brokers),
		zap.String("topic", reader.Config().Topic),
		zap.String("group_id", reader.Config().GroupID),
	)

	consecutiveFailures := 0
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				c.log.Info("bank-statement-accepted consumer stopped")
				return
			}
			consecutiveFailures++
			if consecutiveFailures == 1 {
				c.log.Warn("bank-statement-accepted consumer: kafka read failed — retrying until it recovers", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		consecutiveFailures = 0

		for {
			err := c.Handle(ctx, msg.Value)
			if err == nil {
				break
			}
			c.log.Warn("bank-statement-accepted consumer: could not record the delivery — offset left uncommitted, retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.log.Warn("bank-statement-accepted consumer: offset commit failed — the delivery may be seen again and will be deduplicated", zap.Error(err))
		}
	}
}

const maxInProcessAttempts = 20

// Handle decodes and dispatches one event. Exported so it can be driven
// directly in tests without a broker — same shape as
// bank-reconciliation-svc's ConflictConsumer.Handle.
func (c *BankStatementConsumer) Handle(ctx context.Context, raw []byte) error {
	var env envelopeWire
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("bank-statement-accepted consumer: undecodable event — dropped", zap.Error(err))
		return nil
	}
	eventType := env.Type
	if eventType == "" {
		eventType = env.LegacyType
	}
	if eventType != bankStatementAcceptedType && eventType != legacyBankStatementAcceptedType {
		return nil
	}
	// Built directly from the envelope rather than inbox.Parse: banking-
	// connector-svc's enqueueEvent (internal/store/pg_store.go there) sets
	// AggregateID (the statement id) but never AggregateVersion — it has no
	// per-aggregate ordering concept for a terminal "accepted" fact — and
	// inbox.Parse's own validation refuses an aggregate id with no version.
	// This consumer doesn't need aggregate-version ordering either (there is
	// exactly one real transition per statement, not a stream of revisions
	// to order), so it dedupes on event id alone and leaves AggregateID
	// unset.
	payloadHash := env.PayloadHash
	if payloadHash == "" {
		sum := sha256.Sum256(env.Data)
		payloadHash = "sha256:" + hex.EncodeToString(sum[:])
	}
	msg := inbox.Message{
		EventID:        first(env.ID, env.LegacyID),
		EventType:      eventType,
		TenantID:       first(env.TenantID, env.LegacyTenant),
		PayloadHash:    payloadHash,
		Region:         env.Region,
		Classification: env.Classification,
	}
	if msg.EventID == "" || msg.TenantID == "" {
		c.log.Error("bank-statement-accepted consumer: event lacks event_id/tenant — dropped, cannot be deduplicated",
			zap.String("event_id", msg.EventID), zap.String("tenant_id", msg.TenantID))
		return nil
	}

	var payload bankStatementAcceptedPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		c.log.Error("bank-statement-accepted consumer: undecodable payload — dropped", zap.Error(err))
		return nil
	}
	correlationID := first(env.CorrelationID, env.LegacyCorr)

	for attempt := 1; attempt <= maxInProcessAttempts; attempt++ {
		res, err := c.proc.Process(ctx, msg, func(hctx context.Context, tx pgx.Tx, _ inbox.Receipt) (string, error) {
			return c.apply(hctx, tx, payload)
		})
		switch {
		case err == nil:
			c.logOutcome(msg, res)
			if res.Outcome == inbox.Processed {
				// Best-effort recalculation: outside the inbox transaction
				// (it is not itself an inbox-safe effect — it calls out to
				// accounts-payable-svc/obligations-svc), and its failure
				// does not fail this delivery. The durable fact — the real
				// closing balance — already committed above.
				c.recalculate(ctx, payload, correlationID)
			}
			return nil
		case res.FailureState == inbox.StateQuarantine:
			c.log.Error("bank-statement-accepted consumer: event quarantined — needs an approved repair and replay",
				zap.String("event_id", msg.EventID), zap.Int("attempts", res.Attempt), zap.Error(err))
			return nil
		case res.FailureState == inbox.StateFailed:
			c.log.Warn("bank-statement-accepted consumer: attempt failed, retrying",
				zap.String("event_id", msg.EventID), zap.Int("attempt", res.Attempt), zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.retryDelay):
			}
		default:
			return err
		}
	}
	return fmt.Errorf("event %s still failing after %d attempts", msg.EventID, maxInProcessAttempts)
}

func (c *BankStatementConsumer) logOutcome(msg inbox.Message, res inbox.Result) {
	switch res.Outcome {
	case inbox.Processed:
		c.log.Info("real bank balance recorded from BankStatementAccepted", zap.String("event_id", msg.EventID), zap.String("balance_id", res.ResultRef))
	case inbox.Duplicate:
		c.log.Debug("bank-statement-accepted consumer: duplicate event — skipped", zap.String("event_id", msg.EventID))
	case inbox.Stale:
		c.log.Info("bank-statement-accepted consumer: superseded event — skipped", zap.String("event_id", msg.EventID))
	case inbox.Held:
		c.log.Error("bank-statement-accepted consumer: event is quarantined and awaiting repair — not processed", zap.String("event_id", msg.EventID))
	}
}

// apply is the durable effect, run inside the inbox transaction: write the
// real cash_balances row. AsOfTimestamp is the statement's own
// StatementDate — never time.Now() — because freshness
// (domain.CashPositionStale) must be judged against when the SOURCE data
// is from, not when this consumer happened to process it.
func (c *BankStatementConsumer) apply(ctx context.Context, tx pgx.Tx, p bankStatementAcceptedPayload) (string, error) {
	if p.BankAccountID == "" || p.TenantID == "" {
		return "", inbox.Poison(errors.New("event names no bank_account_id/tenant_id"))
	}
	bal := newCashBalance(p)
	if err := c.balances.CreateCashBalanceInTx(ctx, tx, bal); err != nil {
		return "", fmt.Errorf("write real bank balance: %w", err)
	}
	return bal.BalanceID, nil
}

// recalculate is BNK-08's "recalculation on material bank/payment events"
// NFR, best-effort. A failure here (most commonly: accounts-payable-svc or
// obligations-svc unreachable) is logged, not retried and not fatal to the
// delivery — an explicit CalculateCashPosition/RefreshCashPosition call,
// or the next accepted statement, catches the snapshot up.
func (c *BankStatementConsumer) recalculate(ctx context.Context, p bankStatementAcceptedPayload, correlationID string) {
	if c.recalc == nil {
		return
	}
	// composeBankBalancesCtx/ListBankAccounts/GetLatestCashBalances all read
	// the tenant from context via svcmiddleware.TenantFromContext (set by
	// TenantContext() on the HTTP path) — an event delivery has no HTTP
	// request to carry that, so it must be set explicitly here from the
	// event's own tenant_id, the same way the inbox transaction's RLS
	// setting was scoped to it in apply().
	ctx = svcmiddleware.WithTenant(ctx, p.TenantID)
	if _, err := c.recalc.RecalculateAndPublishCashPosition(ctx, p.TenantID, p.LegalEntityID, p.Currency, correlationID, systemActorID); err != nil {
		c.log.Warn("bank-statement-accepted consumer: cash position recalculation deferred — real balance is already durably on file",
			zap.String("tenant_id", p.TenantID), zap.String("legal_entity_id", p.LegalEntityID), zap.String("currency", p.Currency), zap.Error(err))
	}
}

// newCashBalance builds the real cash_balances row this event earns.
// LedgerBalance and AvailableBalance are both the statement's
// ClosingBalance: BNK-03 statements carry one closing balance, not a
// separate available-vs-ledger split, so there is no second real number
// to put in AvailableBalance instead of repeating it.
func newCashBalance(p bankStatementAcceptedPayload) *domain.CashBalance {
	closing := domain.Decimal(fmt.Sprintf("%.4f", p.ClosingBalance))
	return &domain.CashBalance{
		BalanceID:        uuid.New().String(),
		TenantID:         p.TenantID,
		BankAccountID:    p.BankAccountID,
		LedgerBalance:    closing,
		AvailableBalance: closing,
		AsOfTimestamp:    p.StatementDate,
		CorrelationID:    p.StatementID,
	}
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
