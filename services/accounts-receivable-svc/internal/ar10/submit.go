package ar10

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func attemptKey(refundID string) string { return "ar10-refund-" + refundID + "-1" }

func (m *Module) prepareInput(rf *Refund, now time.Time) PrepareInput {
	return PrepareInput{LegalEntityID: rf.LegalEntityID, SourceReference: rf.RefundID, IdempotencyKey: rf.AttemptIdempotencyKey, PayerAccountRef: rf.PayerAccountRef,
		PayeeRef: rf.DestinationRef, Amount: rf.Amount, Currency: rf.Currency, PaymentReference: "REFUND-" + rf.RefundID, ExecutionDate: now}
}

func (m *Module) gatewayErr(err error) error {
	var rej *GatewayRejected
	if errors.As(err, &rej) {
		return kit.Unprocessable(kit.CodeValidationFailed, "the payment path refused the instruction (nothing was sent): "+rej.Msg)
	}
	return kit.Unavailable("the payment path is unavailable: no money moved; the refund can be submitted again")
}

// ── SubmitRefundForPayment ───────────────────────────────────────────────────

// submitRefundForPayment releases an APPROVED refund to the payment path, exactly once. The state flips to SUBMITTED under the row
// lock BEFORE any network call (so concurrent submits cannot both call out), the durable external attempt id is stored BEFORE the
// provider is asked, and an ambiguous answer is UNKNOWN (REFUND_STATE_UNKNOWN), never failed. A repeat submit is a no-op; an UNKNOWN
// refund is never resubmitted — it is reconciled.
func (m *Module) submitRefundForPayment(w http.ResponseWriter, r *http.Request) {
	var req refundBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareRefund(w, r, req, ActionRefundSubmit)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	switch cur.State {
	case RfSubmitted, RfPending, RfSettled:
		kit.WriteJSON(w, http.StatusOK, refundOut{Refund: cur}) // idempotent: already released
		return
	case RfUnknown:
		m.fail(w, errStateUnknown("the external state is unknown: reconcile with the payment path; the refund is never resubmitted blindly"))
		return
	case RfApproved:
	default:
		m.fail(w, errTransition("only an APPROVED refund can be submitted for payment (it is "+cur.State+")"))
		return
	}
	if m.gateway == nil || m.dest == nil || m.receipts == nil {
		m.fail(w, kit.Unavailable("the payment path / destination verification / AR-08 receipts are not wired: nothing was submitted"))
		return
	}
	if c.PrincipalID == cur.RequestedBy {
		m.fail(w, m.denied(ctx, c, AggregateRefund, cur.RefundID, "submitRefundForPayment", errSoD("a refund cannot be released for payment by the principal who requested it"), nil))
		return
	}
	// The destination must still be what the approval covered.
	info, err := m.dest.Verify(ctx, c, cur.DestinationRef)
	if err != nil {
		m.fail(w, err)
		return
	}
	if !info.Active() || info.Fingerprint != cur.DestinationFingerprint {
		if _, ierr := m.invalidateApproval(ctx, c, cur.RefundID, "the verified destination changed after approval (status "+info.Status+")"); ierr != nil {
			m.fail(w, ierr)
			return
		}
		m.fail(w, errDestinationChanged("the destination changed after approval: the approval is invalidated and the refund must be re-approved"))
		return
	}
	now := m.deps.Now()
	proceed := false
	cur2, err := m.lockRefund(ctx, c, cur.RefundID, func(tx pgx.Tx, p *Refund) error {
		if p.State != RfApproved {
			return nil // somebody else already moved it: answer with the current state, call nothing
		}
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.ApprovedHash == "" || p.Binding() != p.ApprovedHash {
			return errDestinationChanged("the refund no longer matches the content that was approved")
		}
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `state = 'SUBMITTED', submitted_by = $3, submitted_at = $4, attempt_idempotency_key = $5, external_state = 'PREPARING'`,
			c.PrincipalID, now, attemptKey(p.RefundID)); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", p.RefundID, "submitRefundForPayment", p.State, RfSubmitted, map[string]any{"approved_hash": p.ApprovedHash}); err != nil {
			return err
		}
		proceed = true
		return audit(ctx, tx, c, AggregateRefund, p.RefundID, "submitRefundForPayment", "", "", map[string]any{"state": p.State},
			map[string]any{"state": RfSubmitted, "submitted_by": c.PrincipalID}, map[string]any{"attempt_idempotency_key": attemptKey(p.RefundID)})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if !proceed {
		kit.WriteJSON(w, http.StatusOK, refundOut{Refund: cur2})
		return
	}
	// Durable external attempt BEFORE any submission.
	attempt, err := m.gateway.PrepareAttempt(ctx, c, m.prepareInput(cur2, now))
	if err != nil {
		// Nothing was sent (prepare creates no money movement and is idempotent on the key): back to APPROVED.
		if aerr := m.abortSubmit(ctx, c, cur.RefundID, err); aerr != nil {
			m.deps.Log.Error("ar10 could not abort a failed prepare", zap.Error(aerr))
		}
		m.fail(w, m.gatewayErr(err))
		return
	}
	stored, err := m.storeAttempt(ctx, c, cur.RefundID, attempt)
	if err != nil {
		m.fail(w, err) // attempt id not stored: reconciliation re-prepares idempotently
		return
	}
	out, err := m.applyAttempt(ctx, c, stored, attempt, true)
	if err != nil {
		m.fail(w, err)
		return
	}
	m.respondRefund(w, out)
}

func (m *Module) respondRefund(w http.ResponseWriter, out *Refund) {
	if out.State == RfUnknown {
		kit.WriteJSON(w, http.StatusAccepted, refundOut{Refund: out, Code: kit.CodeRefundStateUnknown,
			Detail: "the external payment state is unknown; reconcile with payment-status-svc. The refund is not failed and will not be resubmitted."})
		return
	}
	kit.WriteJSON(w, http.StatusOK, refundOut{Refund: out})
}

// abortSubmit puts a SUBMITTED refund that never obtained an external attempt back to APPROVED.
func (m *Module) abortSubmit(ctx context.Context, c kit.Caller, id string, cause error) error {
	_, err := m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if p.State != RfSubmitted || p.ExternalAttemptID != "" {
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'APPROVED', submitted_by = NULL, submitted_at = NULL, external_state = ''`); err != nil {
			return err
		}
		return addHistory(ctx, tx, c, "REFUND", id, "submitAborted", RfSubmitted, RfApproved, map[string]any{"cause": clip(cause.Error(), 300)})
	})
	return err
}

// storeAttempt records the durable external attempt id (once) and its state.
func (m *Module) storeAttempt(ctx context.Context, c kit.Caller, id string, a *Attempt) (*Refund, error) {
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if p.ExternalAttemptID != "" {
			if p.ExternalAttemptID != a.AttemptID {
				return kit.Conflict(kit.CodeInvalidTransition, "the refund already has a different external attempt")
			}
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `external_attempt_id = $3, external_state = $4`, a.AttemptID, a.Status); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar10_refund_attempt") {
				return kit.Conflict(kit.CodeInvalidTransition, "the external attempt belongs to another refund")
			}
			return err
		}
		return audit(ctx, tx, c, AggregateRefund, id, "externalAttemptRecorded", "", "", nil, map[string]any{"external_attempt_id": a.AttemptID, "attempt_status": a.Status}, nil)
	})
}

// applyAttempt moves the refund according to the adapter's attempt status. afterSubmit says this call just asked the adapter to submit
// (a PREPARED answer then means the adapter could not record the outcome: ambiguous). Without it a PREPARED attempt is left alone,
// except that a stale one (older than SubmitSettle) is submitted once by reconciliation.
func (m *Module) applyAttempt(ctx context.Context, c kit.Caller, rf *Refund, a *Attempt, afterSubmit bool) (*Refund, error) {
	if a.Status == AttemptPrepared && !afterSubmit {
		if rf.SubmittedAt == nil || m.deps.Now().Sub(*rf.SubmittedAt) < SubmitSettle {
			return rf, nil // the first submit may still be in flight
		}
		sub, err := m.gateway.SubmitAttempt(ctx, c, a.AttemptID)
		if err != nil {
			return m.markUnknown(ctx, c, rf.RefundID, "the provider did not answer the submission: "+err.Error(), "")
		}
		return m.applyAttempt(ctx, c, rf, sub, true)
	}
	if a.Status == AttemptPrepared && afterSubmit {
		sub, err := m.gateway.SubmitAttempt(ctx, c, a.AttemptID)
		if err != nil {
			return m.markUnknown(ctx, c, rf.RefundID, "the provider did not answer the submission: "+err.Error(), "")
		}
		if sub.Status == AttemptPrepared {
			return m.markUnknown(ctx, c, rf.RefundID, "the adapter could not record the submission outcome", "")
		}
		return m.applyAttempt(ctx, c, rf, sub, true)
	}
	switch a.Status {
	case AttemptSubmitted:
		out, err := m.markSubmitted(ctx, c, rf.RefundID, a)
		if err != nil {
			return nil, err
		}
		return m.trackPayment(ctx, c, out), nil
	case AttemptRejectedPre, AttemptCancelled:
		return m.markFailed(ctx, c, rf.RefundID, "the payment path did not send the instruction: "+a.Status+" "+a.RejectionReason)
	case AttemptQuarantined:
		return m.markUnknown(ctx, c, rf.RefundID, "the attempt is quarantined at the payment path", "ATTEMPT_QUARANTINED")
	default: // PENDING_UNKNOWN or anything unrecognised: ambiguous, never failed
		return m.markUnknown(ctx, c, rf.RefundID, "the provider outcome is ambiguous ("+a.Status+")", "")
	}
}

func inFlight(state string) bool {
	return state == RfSubmitted || state == RfPending || state == RfUnknown
}

func (m *Module) markSubmitted(ctx context.Context, c kit.Caller, id string, a *Attempt) (*Refund, error) {
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if !inFlight(p.State) || (p.State == RfSubmitted && p.ExternalState == AttemptSubmitted && p.ProviderRequestID == a.ProviderRequestID) {
			return nil
		}
		firstTime := p.ExternalState != AttemptSubmitted
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'SUBMITTED', external_state = 'SUBMITTED', provider_request_id = COALESCE(NULLIF($3,''), provider_request_id), control_exception = NULL`,
			a.ProviderRequestID); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", id, "providerAccepted", p.State, RfSubmitted, map[string]any{"provider_request_id": a.ProviderRequestID}); err != nil {
			return err
		}
		if firstTime {
			np := *p
			np.State, np.Version, np.ProviderRequestID = RfSubmitted, p.Version+1, a.ProviderRequestID
			return emitRefund(ctx, tx, c, &np, EvRefundSubmitted, map[string]any{"provider_request_id": a.ProviderRequestID})
		}
		return nil
	})
}

// markUnknown records the ambiguous external state. NEVER failed: the instruction may be live at the provider.
func (m *Module) markUnknown(ctx context.Context, c kit.Caller, id, reason, control string) (*Refund, error) {
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if !inFlight(p.State) || (p.State == RfUnknown && control == "") {
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'UNKNOWN', external_state = 'PENDING_UNKNOWN', control_exception = NULLIF($3,'')`, control); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", id, "externalStateUnknown", p.State, RfUnknown, map[string]any{"reason": clip(reason, 500)}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, id, "externalStateUnknown", "", "", map[string]any{"state": p.State}, map[string]any{"state": RfUnknown},
			map[string]any{"reason": clip(reason, 500)}); err != nil {
			return err
		}
		np := *p
		np.State, np.Version = RfUnknown, p.Version+1
		return emitRefund(ctx, tx, c, &np, EvRefundStateUnknown, map[string]any{"code": kit.CodeRefundStateUnknown, "reason": clip(reason, 500)})
	})
}

// markFailed finalises a refund the payment path definitively did not execute and gives the reserved cash back to the receipt.
func (m *Module) markFailed(ctx context.Context, c kit.Caller, id, reason string) (*Refund, error) {
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if !inFlight(p.State) {
			return nil
		}
		if m.receipts == nil {
			return kit.Unavailable("AR-08 receipts not wired: the reservation cannot be released, nothing changed")
		}
		if err := m.receipts.ReleaseUnapplied(ctx, tx, c, "REFUND:"+p.RefundID); err != nil {
			return err
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'FAILED', failure_reason = $3`, clip(reason, 1000)); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", id, "refundFailed", p.State, RfFailed, map[string]any{"reason": clip(reason, 500)}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, id, "refundFailed", "", "", map[string]any{"state": p.State}, map[string]any{"state": RfFailed},
			map[string]any{"reason": clip(reason, 500)}); err != nil {
			return err
		}
		np := *p
		np.State, np.Version = RfFailed, p.Version+1
		return emitRefund(ctx, tx, c, &np, EvRefundFailed, map[string]any{"failure_reason": clip(reason, 500)})
	})
}

// trackPayment registers the BNK-07 tracking record for a submitted attempt, once. A failure leaves the refund as it is:
// reconciliation registers it later.
func (m *Module) trackPayment(ctx context.Context, c kit.Caller, rf *Refund) *Refund {
	if rf.PaymentStatusID != "" || rf.ProviderRequestID == "" || !inFlight(rf.State) {
		return rf
	}
	pid, err := m.gateway.RegisterPayment(ctx, c, rf.LegalEntityID, rf.ProviderRequestID, rf.RefundID)
	if err != nil {
		m.deps.Log.Warn("ar10 payment tracking not registered yet", zap.String("refund_id", rf.RefundID), zap.Error(err))
		return rf
	}
	out, err := m.lockRefund(ctx, c, rf.RefundID, func(tx pgx.Tx, p *Refund) error {
		if p.PaymentStatusID != "" {
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `payment_status_id = $3`, pid); err != nil {
			return err
		}
		return audit(ctx, tx, c, AggregateRefund, p.RefundID, "paymentTrackingRegistered", "", "", nil, map[string]any{"payment_status_id": pid}, nil)
	})
	if err != nil {
		m.deps.Log.Warn("ar10 could not store the payment tracking id", zap.String("refund_id", rf.RefundID), zap.Error(err))
		return rf
	}
	return out
}

// invalidateApproval sends an APPROVED refund back to PENDING_APPROVAL with the approval cleared (committed on its own).
func (m *Module) invalidateApproval(ctx context.Context, c kit.Caller, id, why string) (*Refund, error) {
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if p.State != RfApproved {
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'PENDING_APPROVAL', approved_hash = NULL, approved_by = NULL, approved_at = NULL`); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", id, "approvalInvalidated", p.State, RfPendingApproval, map[string]any{"reason": why}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, id, "submitRefundForPayment.denied", "", "", map[string]any{"state": p.State, "approved_hash": p.ApprovedHash},
			map[string]any{"state": RfPendingApproval}, map[string]any{"code": kit.CodeRefundDestinationChanged, "reason": why}); err != nil {
			return err
		}
		np := *p
		np.State, np.Version = RfPendingApproval, p.Version+1
		return emitRefund(ctx, tx, c, &np, EvRefundInvalidated, map[string]any{"reason": why})
	})
}

// ── ReconcileRefundState ─────────────────────────────────────────────────────

// reconcileRefundState resolves an in-flight refund against the payment path. SETTLED comes ONLY from payment-status-svc's
// execution state: AR never decides settlement. A definitive rejection fails the refund and releases the reservation; anything
// still ambiguous stays UNKNOWN. Nothing is ever resubmitted from here except a stale attempt the adapter still holds as PREPARED
// (never submitted), and never an attempt the provider may already have seen.
func (m *Module) reconcileRefundState(w http.ResponseWriter, r *http.Request) {
	var req refundBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareRefund(w, r, req, ActionRefundReconcile)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if m.gateway == nil || m.receipts == nil {
		m.fail(w, kit.Unavailable("the payment path / AR-08 receipts are not wired: nothing was reconciled"))
		return
	}
	if !inFlight(cur.State) && cur.State != RfSettled {
		m.fail(w, errTransition("a "+cur.State+" refund has nothing to reconcile"))
		return
	}
	if cur.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	out, err := m.reconcile(ctx, c, cur)
	if err != nil {
		m.fail(w, err)
		return
	}
	m.respondRefund(w, out)
}

func (m *Module) reconcile(ctx context.Context, c kit.Caller, rf *Refund) (*Refund, error) {
	// A SETTLED refund is only re-checked for a later return.
	if rf.State == RfSettled {
		if rf.PaymentStatusID == "" {
			return rf, nil
		}
		ps, err := m.gateway.GetPayment(ctx, c, rf.PaymentStatusID)
		if err != nil {
			return nil, kit.Unavailable("payment-status-svc cannot answer: nothing changed")
		}
		if ps.Status != PayReturned || rf.ControlException == controlReturned {
			return rf, nil
		}
		return m.lockRefund(ctx, c, rf.RefundID, func(tx pgx.Tx, p *Refund) error {
			if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `control_exception = $3, payment_state = $4`, controlReturned, ps.Status); err != nil {
				return err
			}
			return audit(ctx, tx, c, AggregateRefund, p.RefundID, "refundReturnedAfterSettlement", "", "", nil, map[string]any{"payment_state": ps.Status},
				map[string]any{"control_exception": controlReturned})
		})
	}
	attemptID := rf.ExternalAttemptID
	if attemptID == "" { // crashed between "submitted" and "attempt recorded": re-prepare (idempotent on the key) to recover the id
		if rf.AttemptIdempotencyKey == "" {
			return nil, errTransition("the refund has no attempt key to recover")
		}
		a, err := m.gateway.PrepareAttempt(ctx, c, m.prepareInput(rf, m.deps.Now()))
		if err != nil {
			return nil, m.gatewayErr(err)
		}
		if rf, err = m.storeAttempt(ctx, c, rf.RefundID, a); err != nil {
			return nil, err
		}
		attemptID = a.AttemptID
	}
	a, err := m.gateway.GetAttempt(ctx, c, attemptID)
	if err != nil {
		return nil, kit.Unavailable("the payment path cannot answer: nothing changed")
	}
	rf2, err := m.applyAttempt(ctx, c, rf, a, false)
	if err != nil {
		return nil, err
	}
	if rf2.State == RfFailed || rf2.State == RfSettled || a.Status != AttemptSubmitted {
		return rf2, nil
	}
	// The attempt reached the provider: BNK-07 owns the execution state.
	if rf2.PaymentStatusID == "" {
		rf2 = m.trackPayment(ctx, c, rf2)
		if rf2.PaymentStatusID == "" {
			return nil, kit.Unavailable("the payment tracking record could not be registered with payment-status-svc: nothing changed")
		}
	}
	ps, err := m.gateway.GetPayment(ctx, c, rf2.PaymentStatusID)
	if err != nil {
		return nil, kit.Unavailable("payment-status-svc cannot answer: the refund state is unchanged")
	}
	next, final := RefundMapPayment(ps.Status)
	switch {
	case final && next == RfSettled:
		return m.settleRefund(ctx, c, rf2.RefundID, ps)
	case final:
		return m.markFailed(ctx, c, rf2.RefundID, "payment-status-svc reports the payment "+ps.Status)
	}
	return m.lockRefund(ctx, c, rf2.RefundID, func(tx pgx.Tx, p *Refund) error {
		if !inFlight(p.State) || (p.State == RfPending && p.PaymentState == ps.Status) {
			return nil
		}
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `state = 'PENDING', payment_state = $3, external_state = 'SUBMITTED'`, ps.Status); err != nil {
			return err
		}
		return addHistory(ctx, tx, c, "REFUND", p.RefundID, "reconcileRefundState", p.State, RfPending, map[string]any{"payment_state": ps.Status})
	})
}

// settleRefund is the only place a refund becomes SETTLED, driven by payment-status-svc's SETTLED. In ONE transaction: the reserved
// cash is consumed, the state flips, the ACC-04 posting is queued (Dr AR_UNAPPLIED_CASH / Cr AR_CASH_RECEIPT) and the event is
// written. A refund relates to unapplied cash, not to an open item residual, so no AR-07 adjustment is owed; the AR consequence is
// the consumed reservation, and its absence is a visible control exception (#32).
func (m *Module) settleRefund(ctx context.Context, c kit.Caller, id string, ps *PaymentState) (*Refund, error) {
	now := m.deps.Now()
	pk := m.settlePeriod(ctx, c, now)
	return m.lockRefund(ctx, c, id, func(tx pgx.Tx, p *Refund) error {
		if p.State == RfSettled {
			return nil
		}
		if !inFlight(p.State) {
			return errTransition("a " + p.State + " refund cannot settle")
		}
		if err := m.receipts.ConsumeUnapplied(ctx, tx, c, "REFUND:"+p.RefundID); err != nil {
			return err
		}
		if err := setRefund(ctx, tx, c.TenantID, id, `state = 'SETTLED', payment_state = $3, external_state = 'SUBMITTED', settled_at = $4, accounting_state = 'PENDING', control_exception = NULL`,
			ps.Status, now); err != nil {
			return err
		}
		if err := kit.QueuePosting(ctx, tx, c, kit.SourceRefund, p.RefundID, kit.PostingEvent{FiscalPeriod: pk, Description: "Customer refund " + p.RefundID,
			SourceEventID: p.RefundID, TransactionCurrency: p.Currency, DocumentDate: day(now), PostingDate: day(now), Lines: BuildRefundPosting(p.Amount)}); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", id, "reconcileRefundState", p.State, RfSettled, map[string]any{"payment_state": ps.Status, "payment_status_id": ps.PaymentID}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, id, "refundSettled", "", "", map[string]any{"state": p.State}, map[string]any{"state": RfSettled},
			map[string]any{"payment_status_id": ps.PaymentID, "fiscal_period": pk, "finality_source": "payment-status-svc"}); err != nil {
			return err
		}
		np := *p
		np.State, np.Version = RfSettled, p.Version+1
		return emitRefund(ctx, tx, c, &np, EvRefundSettled, map[string]any{"payment_status_id": ps.PaymentID, "fiscal_period": pk})
	})
}
