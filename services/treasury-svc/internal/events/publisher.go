package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.CashBalance has no
// legal_entity_id field of its own — the bank account's legal entity is
// resolved at the handler call site and threaded through explicitly.
// domain.EffectiveCashResponse already carries real TenantID/
// LegalEntityID. Neither domain object has an actor field, so actor_id
// is threaded through explicitly from each handler's already-verified
// principalID. No jurisdiction field exists anywhere in this service.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishCashPositionUpdated emits the cash.position.updated event.
func (p *Publisher) PublishCashPositionUpdated(ctx context.Context, correlationID, legalEntityID, actorID string, balance domain.CashBalance) {
	p.emit(ctx, "cash.position.updated", correlationID, balance.TenantID, legalEntityID, actorID, map[string]any{
		"tenant_id":         balance.TenantID,
		"bank_account_id":   balance.BankAccountID,
		"ledger_balance":    balance.LedgerBalance,
		"available_balance": balance.AvailableBalance,
		"as_of_timestamp":   balance.AsOfTimestamp,
	})
}

// PublishEffectiveCashUpdated emits the effective.cash.position.updated event.
func (p *Publisher) PublishEffectiveCashUpdated(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse) {
	p.emit(ctx, "effective.cash.position.updated", correlationID, resp.TenantID, resp.LegalEntityID, actorID, map[string]any{
		"tenant_id":                resp.TenantID,
		"legal_entity_id":          resp.LegalEntityID,
		"currency_code":            resp.CurrencyCode,
		"effective_available_cash": resp.EffectiveAvailableCash,
		"as_of_timestamp":          resp.AsOfTimestamp,
	})
}

// PublishLiquidityThresholdBreached emits the liquidity.threshold.breached event.
func (p *Publisher) PublishLiquidityThresholdBreached(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse) {
	escalationEmail := ""
	minRequired := 0.0
	if resp.ThresholdDetails != nil {
		minRequired = resp.ThresholdDetails.MinimumRequiredBalance
	}
	p.emit(ctx, "liquidity.threshold.breached", correlationID, resp.TenantID, resp.LegalEntityID, actorID, map[string]any{
		"tenant_id":                resp.TenantID,
		"legal_entity_id":          resp.LegalEntityID,
		"currency_code":            resp.CurrencyCode,
		"minimum_required_balance": minRequired,
		"effective_available_cash": resp.EffectiveAvailableCash,
		"escalation_email":         escalationEmail,
	})
}

// ── BNK-01 domain events ─────────────────────────────────────────────────────
//
// Previously this service published nothing at all for the bank-account
// lifecycle — the Publisher interface only had Cash* methods. Every
// bnk01_handler.go command now calls one of these.

func bankAccountPayload(acct domain.BankAccount) map[string]any {
	return map[string]any{
		"bank_account_id": acct.BankAccountID,
		"tenant_id":       acct.TenantID,
		"legal_entity_id": acct.LegalEntityID,
		"account_status":  acct.AccountStatus,
	}
}

func (p *Publisher) PublishBankAccountCreated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	p.emit(ctx, "bank_account.created", correlationID, acct.TenantID, acct.LegalEntityID, actorID, bankAccountPayload(acct))
}

func (p *Publisher) PublishBankAccountOwnershipVerified(ctx context.Context, correlationID, actorID string, acct domain.BankAccount, evidence domain.OwnershipEvidence) {
	payload := bankAccountPayload(acct)
	payload["evidence_id"] = evidence.EvidenceID
	payload["verification_method"] = evidence.VerificationMethod
	p.emit(ctx, "bank_account.ownership_verified", correlationID, acct.TenantID, acct.LegalEntityID, actorID, payload)
}

func (p *Publisher) PublishBankAccountMetadataAmended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	p.emit(ctx, "bank_account.metadata_amended", correlationID, acct.TenantID, acct.LegalEntityID, actorID, bankAccountPayload(acct))
}

func (p *Publisher) PublishBankAccountOperationalUseChanged(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	payload := bankAccountPayload(acct)
	payload["requested_operational_use"] = acct.RequestedOperationalUse
	p.emit(ctx, "bank_account.operational_use_changed", correlationID, acct.TenantID, acct.LegalEntityID, actorID, payload)
}

func (p *Publisher) PublishBankAccountSuspended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	p.emit(ctx, "bank_account.suspended", correlationID, acct.TenantID, acct.LegalEntityID, actorID, bankAccountPayload(acct))
}

func (p *Publisher) PublishBankAccountReactivated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	p.emit(ctx, "bank_account.reactivated", correlationID, acct.TenantID, acct.LegalEntityID, actorID, bankAccountPayload(acct))
}

func (p *Publisher) PublishBankAccountClosed(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	p.emit(ctx, "bank_account.closed", correlationID, acct.TenantID, acct.LegalEntityID, actorID, bankAccountPayload(acct))
}

func (p *Publisher) PublishBankAccountTokenRotated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	payload := bankAccountPayload(acct)
	payload["token_version"] = acct.TokenVersion
	p.emit(ctx, "bank_account.token_rotated", correlationID, acct.TenantID, acct.LegalEntityID, actorID, payload)
}

// treasuryTransferPayload is the shared field set for BNK-09's Wave 12
// events — deliberately excludes protected_field_hash (internal only).
func treasuryTransferPayload(t domain.TreasuryTransfer) map[string]any {
	return map[string]any{
		"transfer_id":            t.TransferID,
		"source_bank_account_id": t.SourceBankAccountID,
		"target_bank_account_id": t.TargetBankAccountID,
		"amount":                 t.Amount,
		"currency_code":          t.CurrencyCode,
		"is_cross_entity":        t.IsCrossEntity,
		"status":                 t.Status,
		"maker_principal_id":     t.MakerPrincipalID,
	}
}

// PublishTreasuryTransferReturned corresponds to the doc's own
// TreasuryTransferReturned event — a bank return of an already-submitted
// transfer.
func (p *Publisher) PublishTreasuryTransferReturned(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer) {
	payload := treasuryTransferPayload(t)
	payload["return_reason"] = t.ReturnReason
	p.emit(ctx, "treasury_transfer.returned", correlationID, t.TenantID, "", actorID, payload)
}

// PublishTreasuryTransferCancelled has no literal counterpart in the
// doc's own canonical event catalogue (Cancelled is a named state and
// CancelBeforeSubmission a named command, but no TreasuryTransferCancelled
// event is listed anywhere) — a real inconsistency in the spec, resolved
// here by following this codebase's own established rule that every
// state transition publishes something (see Wave 8's BNK-01 events).
func (p *Publisher) PublishTreasuryTransferCancelled(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer) {
	payload := treasuryTransferPayload(t)
	payload["cancel_reason"] = t.CancelReason
	p.emit(ctx, "treasury_transfer.cancelled", correlationID, t.TenantID, "", actorID, payload)
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "treasury-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(tenantID), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
