// Package paymentinit consumes payment-initiation-adapter-svc's (BNK-06)
// outbox events so payment-status-svc (BNK-07) can reach the two states its
// own state model names but could never previously set:
// SUBMITTED and PENDING_UNKNOWN (SPEC_DEVIATIONS A4).
//
// Producers, by the event types actually published (grep the producer, not
// the spec): payment-initiation-adapter-svc, topic KAFKA_EVENTS_TOPIC
// (default zoiko.payment-initiation.events):
//
//	PAYMENT_SUBMITTED            BNK-06 recorded the attempt as sent to the
//	                             provider -> this service's SUBMITTED.
//	PAYMENT_SUBMISSION_AMBIGUOUS BNK-06 could not determine the outcome (a
//	                             timeout or an ambiguous provider response)
//	                             -> this service's PENDING_UNKNOWN.
//
// Every payload names a source_reference: the caller's own instruction
// reference, the same natural key RecordPaymentStatus's own idempotent-dedup
// uses (A5) to find the execution state this event is about. There is no
// other field in common between the two services' internal ids.
//
// This is a real asynchronous consumer, not a synchronous call: BNK-06's own
// commit must never be coupled to BNK-07 being reachable, and delivery is
// at-least-once (ZS-EVENT-001 §7), so every apply here is idempotent against
// a redelivered or out-of-order event.
package paymentinit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-status-svc/internal/middleware"
)

// Store is the slice of store.Store the handler needs.
type Store interface {
	ApplySubmissionEvent(ctx context.Context, tenantID, sourceReference string, toStatus domain.ExecutionStatus, providerRef, actorPrincipalID, correlationID string) (state *domain.PaymentExecutionState, applied bool, err error)
}

// Outcome labels for Handler.Handle, exported for the metric.
const (
	OutcomeSubmitted      = "submitted"
	OutcomePendingUnknown = "pending_unknown"
	// OutcomeNoop: the row was already at or past the target status — a
	// redelivery, or real provider evidence already superseded BNK-06's own
	// observation. Not an error.
	OutcomeNoop      = "noop"
	OutcomeIgnored   = "ignored"
	OutcomeMalformed = "malformed"
)

const (
	eventTypeSubmitted = "PAYMENT_SUBMITTED"
	eventTypeAmbiguous = "PAYMENT_SUBMISSION_AMBIGUOUS"
	// actorPrincipalID stamped on the status_event and outbox event this
	// consumer writes: there is no human or upstream principal to attribute
	// it to, so it names the consumer itself, the same way a system-driven
	// transition is attributed elsewhere in this codebase.
	actorPrincipalID = "payment-status-svc.bnk06-event-consumer"
)

// Handler decides what one BNK-06 event means.
type Handler struct {
	store Store
	// transient reports a failure worth retrying (an outage, or the row not
	// existing yet) as opposed to one redelivery will never fix.
	transient func(error) bool
	log       *zap.Logger
}

func NewHandler(store Store, transient func(error) bool, log *zap.Logger) *Handler {
	return &Handler{store: store, transient: transient, log: log}
}

// IsTransient reports whether err is worth retrying: the store is
// unreachable, or no unresolved row exists yet for this source_reference
// (RecordPaymentStatus for this instruction may simply not have committed
// yet). Every other outcome — a malformed payload, a found-but-already-
// resolved row — is handled inside Handle and never reaches this check as an
// error at all.
func IsTransient(err error) bool {
	return errors.Is(err, domain.ErrStoreUnavailable) || errors.Is(err, domain.ErrUnresolvedPaymentNotFound)
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	Payload       json.RawMessage `json:"payload"`
}

type payload struct {
	SourceReference string `json:"source_reference"`
	ProviderRef     string `json:"provider_ref"`
}

// Handle processes one message. A non-nil error is transient and the message
// should be retried; every other outcome is final and returned as the
// outcome label.
func (h *Handler) Handle(ctx context.Context, value []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(value, &env); err != nil {
		return OutcomeMalformed, nil
	}
	var toStatus domain.ExecutionStatus
	switch env.EventType {
	case eventTypeSubmitted:
		toStatus = domain.StatusSubmitted
	case eventTypeAmbiguous:
		toStatus = domain.StatusPendingUnknown
	default:
		return OutcomeIgnored, nil
	}
	var p payload
	if err := json.Unmarshal(env.Payload, &p); err != nil || env.TenantID == "" || p.SourceReference == "" {
		return OutcomeMalformed, nil
	}
	ctx = svcmiddleware.WithTenant(ctx, env.TenantID)

	_, applied, err := h.store.ApplySubmissionEvent(ctx, env.TenantID, p.SourceReference, toStatus, p.ProviderRef, actorPrincipalID, "bnk06-event-"+env.EventID)
	switch {
	case err != nil && h.transient(err):
		return "", err
	case err != nil:
		// domain.ErrInvalidTransition only, from a defensive guard this
		// switch's own cases never trigger — a malformed event is still not
		// worth retrying.
		h.log.Warn("BNK-06 event refused", zap.String("event_id", env.EventID), zap.String("source_reference", p.SourceReference), zap.Error(err))
		return OutcomeMalformed, nil
	case !applied:
		return OutcomeNoop, nil
	case toStatus == domain.StatusPendingUnknown:
		return OutcomePendingUnknown, nil
	default:
		return OutcomeSubmitted, nil
	}
}

// Reader is the slice of kafka.Reader the loop uses.
type Reader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Run consumes until ctx ends. A transient failure is retried with capped
// backoff for up to maxAttempts, then the message is committed and counted as
// dropped, so one bad partition head (or one row that never arrives) cannot
// stall every later event forever. observe records each outcome.
func Run(ctx context.Context, r Reader, h *Handler, log *zap.Logger, observe func(outcome string)) {
	const maxAttempts = 8
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("BNK-06 event fetch failed", zap.Error(err))
			sleep(ctx, 2*time.Second)
			continue
		}
		outcome := "dropped"
		backoff := 500 * time.Millisecond
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			o, err := h.Handle(ctx, m.Value)
			if err == nil {
				outcome = o
				break
			}
			if ctx.Err() != nil {
				return
			}
			log.Warn("BNK-06 event handling failed; retrying", zap.Int("attempt", attempt), zap.String("topic", m.Topic), zap.Int64("offset", m.Offset), zap.Error(err))
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		if outcome == "dropped" {
			log.Error("BNK-06 event dropped after retries; its SUBMITTED/PENDING_UNKNOWN transition was not applied", zap.String("topic", m.Topic), zap.Int64("offset", m.Offset))
		}
		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			log.Warn("BNK-06 event commit failed", zap.Error(err))
		}
		// observe fires after the commit, not before: a caller watching the
		// metric should only ever see an outcome once it is durable, never
		// one whose commit might still fail or not yet have happened.
		observe(outcome)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
