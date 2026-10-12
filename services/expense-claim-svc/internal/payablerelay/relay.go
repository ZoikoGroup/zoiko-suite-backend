// Package payablerelay is the reliable AP-08 hand-off. ApproveExpenseClaim
// only writes a durable payable_requests row (same transaction as the
// approval); this relay turns each row into an AP-08 payable, idempotently
// (source_reference = claim id), and records the payable id on the claim.
//
// Before any payable is requested it re-verifies, live, that the claimant is
// still an ACTIVE employee and that the claimant's reimbursement payee is an
// ACTIVE controlled destination in payee-banking-identity-svc (ORG-10). If
// there is no such payee the claim is NOT payable: the request is marked
// BLOCKED with a stable reason and re-checked on a schedule, so it heals
// once a payee is onboarded. No fallback identity is ever used.
//
// The relay also observes settlement: a REIMBURSABLE claim whose AP-08
// payable reports SETTLED is closed (REIMBURSABLE to CLOSED).
package payablerelay

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	"zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/payableopenitem"
	"zoiko.io/expense-claim-svc/internal/payeeidentity"
	"zoiko.io/expense-claim-svc/internal/store"
)

// SystemActor is the principal recorded on relay-driven transitions.
const SystemActor = "system:payable-relay"

type Relay struct {
	store    store.Store
	employee employeemaster.Client
	payee    payeeidentity.Client
	payable  payableopenitem.Client
	log      *zap.Logger

	interval       time.Duration
	blockedRecheck time.Duration
	batch          int
	now            func() time.Time
}

func New(st store.Store, emp employeemaster.Client, payee payeeidentity.Client, payable payableopenitem.Client, log *zap.Logger) *Relay {
	return &Relay{
		store: st, employee: emp, payee: payee, payable: payable, log: log,
		interval: 5 * time.Second, blockedRecheck: 5 * time.Minute, batch: 25, now: time.Now,
	}
}

// Start runs the relay until ctx is cancelled.
func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
			tick++
			if tick%6 == 0 { // settlement observation is less urgent than the hand-off
				r.ReconcileSettlements(ctx)
			}
		}
	}
}

// RunOnce processes every due payable request.
func (r *Relay) RunOnce(ctx context.Context) {
	reqs, err := r.store.ListDuePayableRequests(ctx, r.batch)
	if err != nil {
		r.log.Error("payable relay: list due requests failed", zap.Error(err))
		return
	}
	for _, req := range reqs {
		r.Process(ctx, req)
	}
}

// ProcessClaim attempts the hand-off for one claim right away (best effort;
// the scheduled relay is the guarantee). ctx must carry the claim's tenant.
func (r *Relay) ProcessClaim(ctx context.Context, claimID string) {
	req, err := r.store.FindPayableRequest(ctx, claimID)
	if err != nil || req == nil || req.State == domain.PayableCreated {
		return
	}
	r.Process(ctx, *req)
}

func (r *Relay) backoff(attempts int) time.Time {
	d := 15 * time.Second
	for i := 0; i < attempts && d < 15*time.Minute; i++ {
		d *= 2
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return r.now().Add(d)
}

// Process drives one payable request one step.
func (r *Relay) Process(ctx context.Context, req domain.PayableRequest) {
	ctx = middleware.WithTenant(ctx, req.TenantID)
	fail := func(err error) {
		r.log.Warn("payable relay: attempt failed, will retry", zap.String("claim_id", req.ClaimID), zap.Error(err))
		if e := r.store.RecordPayableFailure(ctx, req, err.Error(), r.backoff(req.Attempts)); e != nil {
			r.log.Error("payable relay: could not record failure", zap.Error(e))
		}
	}
	block := func(reason string) {
		if e := r.store.BlockPayableRequest(ctx, req, reason, r.now().Add(r.blockedRecheck)); e != nil {
			r.log.Error("payable relay: could not record block", zap.Error(e))
		}
	}

	// Claimant status ambiguity blocks payment (spec section 10 failure semantics).
	if err := r.employee.VerifyActiveClaimant(ctx, req.RequestedBy, req.LegalEntityID, req.ClaimantPrincipalID); err != nil {
		if errors.Is(err, domain.ErrClaimantNotEligible) {
			block(domain.BlockedClaimantNotActive)
			return
		}
		fail(err)
		return
	}

	// The reimbursement payee must be a controlled ORG-10 destination. The
	// party reference is the claim's payment-preference reference, or the
	// claimant's own party reference when none was given. In either case it
	// is only a lookup key: nothing is paid unless ORG-10 has an ACTIVE
	// destination for it.
	partyRef := req.PaymentPreferenceRef
	if partyRef == "" {
		partyRef = req.ClaimantPrincipalID
	}
	dest, err := r.payee.GetActiveDestination(ctx, req.TenantID, req.RequestedBy, req.LegalEntityID, partyRef)
	if err != nil {
		if errors.Is(err, domain.ErrNoControlledPayee) {
			block(domain.BlockedNoControlledPayee)
			return
		}
		fail(err)
		return
	}

	p, err := r.payable.CreatePayableFromApprovedSource(ctx, req.TenantID, req.RequestedBy, payableopenitem.CreatePayableRequest{
		LegalEntityID: req.LegalEntityID, SourceType: payableopenitem.SourceExpenseClaim, SourceReference: req.ClaimID,
		PayeeRef: partyRef, OriginalAmount: req.Amount, Currency: req.Currency, DueDate: req.DueDate,
	})
	if err != nil {
		fail(err)
		return
	}
	if err := r.store.CompletePayableRequest(ctx, req, p.PayableID, partyRef, dest.DestinationID); err != nil {
		r.log.Error("payable relay: payable created in AP-08 but could not be recorded; the idempotent retry will record it",
			zap.String("claim_id", req.ClaimID), zap.String("payable_id", p.PayableID), zap.Error(err))
	}
}

// ReconcileSettlements closes REIMBURSABLE claims whose payable AP-08
// reports SETTLED.
func (r *Relay) ReconcileSettlements(ctx context.Context) {
	cands, err := r.store.ListSettlementCandidates(ctx, r.batch)
	if err != nil {
		r.log.Error("payable relay: list settlement candidates failed", zap.Error(err))
		return
	}
	for _, c := range cands {
		cctx := middleware.WithTenant(ctx, c.TenantID)
		p, err := r.payable.GetPayable(cctx, c.TenantID, c.PrincipalID, c.PayableID)
		if err != nil {
			r.log.Warn("payable relay: settlement lookup failed", zap.String("claim_id", c.ClaimID), zap.Error(err))
			continue
		}
		_ = r.store.MarkSettlementChecked(cctx, c.TenantID, c.ClaimID)
		if p.Status != payableopenitem.StatusSettled {
			continue
		}
		if _, err := r.store.CloseClaim(cctx, domain.CommandParams{
			ClaimID: c.ClaimID, PrincipalID: SystemActor, Reason: "AP-08 payable " + c.PayableID + " settled",
		}); err != nil && !errors.Is(err, domain.ErrInvalidTransition) {
			r.log.Error("payable relay: close failed", zap.String("claim_id", c.ClaimID), zap.Error(err))
		}
	}
}
