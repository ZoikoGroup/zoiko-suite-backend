// Package bankingrefresh runs the WFP-07 Banking outcome refresh: it asks Banking what
// became of instructions that were handed over and records the answer as append-only
// observations. It changes no instruction and reopens nothing; a returned, failed or
// unresolved payment surfaces through reconciliation and the close gate, and reopening is an
// explicit, authorised command. Pending/Unknown is never reported as failed or paid.
package bankingrefresh

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/paymentstatus"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Attempts reads the adapter's status of an attempt.
type Attempts interface {
	AttemptStatus(ctx context.Context, tenantID, principalID, legalEntityID, attemptID string) (string, error)
}

// Payments reads Banking's execution state of a payment.
type Payments interface {
	Snapshot(ctx context.Context, tenantID, legalEntityID, paymentID string) (*paymentstatus.Snapshot, error)
}

// Source is the store surface the refresher needs.
type Source interface {
	RefreshBankingOutcomes(ctx context.Context, limit int, observe store.ObserveFunc, mk store.ReconEventBuilder) (int, error)
}

type Refresher struct {
	store     Source
	attempts  Attempts
	payments  Payments
	principal string
	interval  time.Duration
	batch     int
	log       *zap.Logger
}

// New builds a refresher. Each claimed row stays locked while Banking is asked, so the batch is small.
func New(st Source, attempts Attempts, payments Payments, principal string, interval time.Duration, batch int, log *zap.Logger) *Refresher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batch <= 0 {
		batch = 10
	}
	if principal == "" {
		principal = "payroll-run-svc"
	}
	return &Refresher{store: st, attempts: attempts, payments: payments, principal: principal, interval: interval, batch: batch, log: log}
}

func (r *Refresher) Start(ctx context.Context) {
	r.log.Info("banking outcome refresh started", zap.Duration("interval", r.interval), zap.Int("batch", r.batch))
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("banking outcome refresh stopped")
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce refreshes one batch and returns how many new observations were recorded.
func (r *Refresher) RunOnce(ctx context.Context) int {
	n, err := r.store.RefreshBankingOutcomes(ctx, r.batch, r.Observe, systemEvents())
	if err != nil && ctx.Err() == nil {
		r.log.Error("banking outcome refresh failed", zap.Error(err))
	}
	return n
}

// Observe turns what Banking says about one instruction into a single observation, the most
// authoritative one available: a linked payment's status beats the attempt's. It returns nil
// when Banking cannot be asked, so nothing is invented.
func (r *Refresher) Observe(ctx context.Context, in domain.PaymentInstruction) *domain.BankingObservation {
	var obs *domain.BankingObservation
	switch {
	case in.AttemptID == nil || *in.AttemptID == "":
		detail := "the instruction was committed to Banking but no attempt id is recorded; resubmit to reuse the same idempotency key"
		obs = &domain.BankingObservation{Source: domain.ObsSourceHandoff, RawStatus: "NO_ATTEMPT", Outcome: domain.OutcomeUnresolved, Detail: &detail}
	default:
		status, err := r.attempts.AttemptStatus(ctx, in.TenantID, r.principal, in.LegalEntityID, *in.AttemptID)
		if err != nil {
			r.log.Warn("banking attempt status unavailable", zap.String("instruction_id", in.InstructionID), zap.Error(err))
			return nil
		}
		obs = &domain.BankingObservation{Source: domain.ObsSourceAttempt, RawStatus: status, Outcome: domain.OutcomeOfAttempt(status), AttemptID: in.AttemptID}
	}
	if in.PaymentID != nil && r.payments != nil {
		snap, err := r.payments.Snapshot(ctx, in.TenantID, in.LegalEntityID, *in.PaymentID)
		if err != nil {
			r.log.Warn("banking payment status unavailable", zap.String("instruction_id", in.InstructionID), zap.Error(err))
			return nil
		}
		out := domain.OutcomeOfPayment(snap.Status)
		var detail *string
		switch {
		case snap.LegalEntityID != in.LegalEntityID || snap.SourceReference != in.InstructionID:
			out = domain.OutcomeUnresolved
			d := "the linked payment does not belong to this instruction"
			detail = &d
		case snap.HasOpenConflict:
			out = domain.OutcomeUnresolved
			d := "Banking holds an open status conflict for this payment"
			detail = &d
		}
		obs = &domain.BankingObservation{Source: domain.ObsSourcePayment, RawStatus: snap.Status, Outcome: out, AttemptID: in.AttemptID, PaymentID: in.PaymentID, Detail: detail}
	}
	return obs
}

// systemEvents builds the events the refresher writes; the actor is the service itself.
func systemEvents() store.ReconEventBuilder {
	return func(ch store.ReconChange) ([]domain.OutboxEvent, error) {
		cid := ""
		if ch.Instruction != nil {
			cid = ch.Instruction.CorrelationID
		}
		return events.BuildReconEvents(ch.Kind, ch.Run, ch.Exception, ch.Instruction, ch.Observation, "payroll-run-svc", cid, "")
	}
}
