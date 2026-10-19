package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
)

// Receipt outcomes (provider_callback_receipts.outcome).
const (
	OutcomeApplied           = "APPLIED"
	OutcomeDuplicate         = "DUPLICATE"
	OutcomeRegressionBlocked = "REGRESSION_BLOCKED"
	OutcomeStaleIgnored      = "STALE_IGNORED"
	OutcomeUnknownPayment    = "UNKNOWN_PAYMENT"
	OutcomeAmbiguousPayment  = "AMBIGUOUS_PAYMENT"

	// QuarantineOwner is the queue that owns a provider callback no durable payment could be found for (INT-CTRL-034). The
	// quarantine event names it, so an alert routes somewhere instead of nowhere.
	QuarantineOwner = "payment-operations"
)

// CallbackInput is one AUTHENTICATED provider callback (its signature has already been verified).
type CallbackInput struct {
	Provider       string
	Scheme         string
	EventRef       string
	PaymentRef     string
	RawStatus      string
	Mapped         domain.ExecutionStatus
	EventType      string // the legacy event type for the mapped status
	EventTime      *time.Time
	BodyHash       string
	MappingVersion string
	CorrelationID  string
}

// CallbackResult says what was done.
type CallbackResult struct {
	Outcome string
	Detail  string
	Payment *domain.PaymentExecutionState
}

// Receipt is a row of provider_callback_receipts.
type Receipt struct {
	ReceiptID      string     `json:"receipt_id"`
	Provider       string     `json:"provider_key"`
	EventRef       string     `json:"event_ref"`
	PaymentRef     string     `json:"payment_ref"`
	PaymentID      *string    `json:"payment_id,omitempty"`
	LegalEntityID  *string    `json:"legal_entity_id,omitempty"`
	Scheme         string     `json:"scheme"`
	ReportedStatus string     `json:"reported_status"`
	MappedStatus   string     `json:"mapped_status"`
	EventTime      *time.Time `json:"event_time,omitempty"`
	BodyHash       string     `json:"body_hash"`
	Outcome        string     `json:"outcome"`
	Detail         string     `json:"detail,omitempty"`
	ReceivedAt     time.Time  `json:"received_at"`
}

// CallbackStore is what the callback processor needs.
type CallbackStore interface {
	ApplyProviderCallback(ctx context.Context, in CallbackInput) (*CallbackResult, error)
	ListReceipts(ctx context.Context, legalEntityID string, includeUnmatched bool, paymentID string, limit int) ([]Receipt, error)
}

var _ CallbackStore = (*PgStore)(nil)

// statusRank orders the states a provider can move a payment through. A callback may only move a payment
// forward (or stay): an older or lower-ranked callback never regresses it.
func statusRank(s domain.ExecutionStatus) int {
	switch s {
	case domain.StatusPrepared, domain.StatusSubmitted:
		return 0
	case domain.StatusAccepted:
		return 1
	case domain.StatusPending:
		return 2
	case domain.StatusSettled, domain.StatusRejected:
		return 3
	}
	return 4 // RETURNED, CANCELLED and anything else are terminal
}

func isFinal(s domain.ExecutionStatus) bool { return statusRank(s) >= 3 }

// ApplyProviderCallback applies one authenticated callback inside the caller's tenant scope: the payment is
// found, the row locked (so concurrent callbacks for one payment are serialised), duplicates are recognised,
// stale and regressing callbacks are refused, and exactly one receipt records what happened. The state change,
// its history row, its outbox event and the receipt commit together.
func (s *PgStore) ApplyProviderCallback(ctx context.Context, in CallbackInput) (*CallbackResult, error) {
	res := &CallbackResult{}
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenant := tenantFrom(ctx)
		receipt := func(outcome, detail string, paymentID, entity *string) error {
			res.Outcome, res.Detail = outcome, detail
			_, err := tx.Exec(ctx, "SAVEPOINT receipt")
			if err != nil {
				return err
			}
			// an event that correlates to no durable payment is quarantined for a named owner, once per event
			quarantined := outcome == OutcomeUnknownPayment || outcome == OutcomeAmbiguousPayment
			var priorQuarantine int
			if quarantined {
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_callback_receipts WHERE tenant_id = $1 AND provider_key = $2 AND event_ref = $3
					AND outcome IN ('UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT')`, tenant, in.Provider, in.EventRef).Scan(&priorQuarantine); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO provider_callback_receipts (tenant_id, provider_key, event_ref, payment_ref, payment_id, legal_entity_id, scheme,
					reported_status, mapped_status, event_time, body_hash, outcome, detail)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				tenant, in.Provider, in.EventRef, in.PaymentRef, paymentID, entity, in.Scheme,
				in.RawStatus, string(in.Mapped), in.EventTime, in.BodyHash, outcome, detail)
			if err != nil && isUniqueViolation(err) {
				// a concurrent copy decided this event first
				if _, rb := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT receipt"); rb != nil {
					return rb
				}
				res.Outcome, res.Detail = OutcomeDuplicate, "decided by a concurrent copy"
				_, err = tx.Exec(ctx, `
					INSERT INTO provider_callback_receipts (tenant_id, provider_key, event_ref, payment_ref, payment_id, legal_entity_id, scheme,
						reported_status, mapped_status, event_time, body_hash, outcome, detail)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'DUPLICATE', $12)`,
					tenant, in.Provider, in.EventRef, in.PaymentRef, paymentID, entity, in.Scheme,
					in.RawStatus, string(in.Mapped), in.EventTime, in.BodyHash, res.Detail)
			}
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT receipt"); err != nil {
				return err
			}
			if quarantined && priorQuarantine == 0 && res.Outcome == outcome {
				return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.provider-callback.quarantined", "PROVIDER_CALLBACK_QUARANTINED", "provider-callback",
					in.Provider+":"+in.EventRef, tenant, "", "provider-callback", in.CorrelationID,
					map[string]any{"provider": in.Provider, "event_ref": in.EventRef, "outcome": outcome, "owner": QuarantineOwner, "body_hash": in.BodyHash})
			}
			return nil
		}

		// find the payment, by our id or by the provider's reference, in this tenant only
		rows, err := tx.Query(ctx, `
			SELECT payment_id::text, legal_entity_id::text FROM payment_execution_states
			WHERE (provider_request_id = $1 OR payment_id::text = $1) ORDER BY created_at LIMIT 2 FOR UPDATE`, in.PaymentRef)
		if err != nil {
			return err
		}
		type pr struct{ id, entity string }
		var found []pr
		for rows.Next() {
			var p pr
			if err := rows.Scan(&p.id, &p.entity); err != nil {
				rows.Close()
				return err
			}
			found = append(found, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		switch len(found) {
		case 0:
			return receipt(OutcomeUnknownPayment, "no payment in this tenant has that reference", nil, nil)
		case 1:
		default:
			return receipt(OutcomeAmbiguousPayment, "more than one payment has that reference", nil, nil)
		}
		pid, entity := found[0].id, found[0].entity

		current, err := scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, pid))
		if err != nil {
			return err
		}
		res.Payment = current

		var already int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM provider_callback_receipts
			WHERE tenant_id = $1 AND provider_key = $2 AND event_ref = $3
			  AND outcome NOT IN ('DUPLICATE', 'UNKNOWN_PAYMENT', 'AMBIGUOUS_PAYMENT')`, tenant, in.Provider, in.EventRef).Scan(&already); err != nil {
			return err
		}
		if already > 0 {
			return receipt(OutcomeDuplicate, "this provider event was already decided", &pid, &entity)
		}

		var lastAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT last_provider_event_at FROM payment_execution_states WHERE payment_id = $1`, pid).Scan(&lastAt); err != nil {
			return err
		}
		switch {
		case current.Status == in.Mapped:
			return receipt(OutcomeStaleIgnored, "the payment is already in that state", &pid, &entity)
		case isFinal(current.Status) || statusRank(current.Status) > 3:
			return receipt(OutcomeRegressionBlocked, "the payment is final; a callback cannot change it", &pid, &entity)
		case in.EventTime != nil && lastAt != nil && in.EventTime.Before(*lastAt):
			return receipt(OutcomeStaleIgnored, "the callback is older than one already applied", &pid, &entity)
		case statusRank(in.Mapped) < statusRank(current.Status):
			return receipt(OutcomeStaleIgnored, "the callback reports an earlier stage than the payment has reached", &pid, &entity)
		}

		_, err = tx.Exec(ctx, "SAVEPOINT apply_provider_callback")
		if err != nil {
			return err
		}
		updated, uerr := scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = $2, finality_source = 'PROVIDER_CALLBACK', mapping_version = $3, provider_key = $4,
				last_provider_event_at = GREATEST(COALESCE($5, now()), COALESCE(last_provider_event_at, 'epoch'::timestamptz)), updated_at = now()
			WHERE payment_id = $1 RETURNING `+stateColumns, pid, string(in.Mapped), in.MappingVersion, in.Provider, in.EventTime))
		if isRegressionBlocked(uerr) {
			// the trigger is the backstop for what the checks above should already have caught
			if _, rb := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_provider_callback"); rb != nil {
				return rb
			}
			return receipt(OutcomeRegressionBlocked, "refused by the payment's state guard", &pid, &entity)
		}
		if uerr != nil {
			return uerr
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT apply_provider_callback"); err != nil {
			return err
		}
		res.Payment = updated
		var tenantPtr *string
		if updated.TenantID != nil {
			tenantPtr = updated.TenantID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO status_events (event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, detail, actor_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'provider-callback')`,
			uuid.New().String(), tenantPtr, pid, in.EventType, string(current.Status), string(in.Mapped), in.EventRef, "via "+in.Provider); err != nil {
			return err
		}
		if err := s.enqueueEvent(ctx, tx, canonicalForLegacy(in.EventType), in.EventType, "payment-execution-state", pid,
			tenant, updated.LegalEntityID, "provider-callback", in.CorrelationID, updated); err != nil {
			return err
		}
		return receipt(OutcomeApplied, "", &pid, &entity)
	})
	if err != nil {
		s.log.Error("pg ApplyProviderCallback failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return res, nil
}

func (s *PgStore) ListReceipts(ctx context.Context, legalEntityID string, includeUnmatched bool, paymentID string, limit int) ([]Receipt, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Receipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT receipt_id::text, provider_key, event_ref, payment_ref, payment_id::text, legal_entity_id::text, scheme, reported_status,
				mapped_status, event_time, body_hash, outcome, detail, received_at
			FROM provider_callback_receipts
			WHERE (legal_entity_id::text = $1 OR ($2 AND legal_entity_id IS NULL))
			  AND ($3 = '' OR payment_id::text = $3)
			ORDER BY received_at DESC, receipt_id LIMIT $4`, legalEntityID, includeUnmatched, paymentID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Receipt
			if err := rows.Scan(&r.ReceiptID, &r.Provider, &r.EventRef, &r.PaymentRef, &r.PaymentID, &r.LegalEntityID, &r.Scheme, &r.ReportedStatus,
				&r.MappedStatus, &r.EventTime, &r.BodyHash, &r.Outcome, &r.Detail, &r.ReceivedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg ListReceipts failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func tenantFrom(ctx context.Context) string { return middleware.TenantFromContext(ctx) }
