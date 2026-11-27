package imports

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Governed release from quarantine (ZS-SVC-Z-001 DQC-02, INV-11/INV-12, TC-07/TC-08, NP-16).
//
// An item that was quarantined is not simply retried. Someone states what was corrected in the
// owning domain (RequestRelease); someone ELSE, holding the release authority, decides
// (DecideRelease). A release puts the item back in the queue so the owning domain re-validates
// it, and how that re-validation ends is recorded. Every step is a durable ledger entry that
// survives the item moving on.

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// RequestRelease asks for a quarantined item to be released. correctionRef names the correction
// made in the owning domain that justifies sending the item again: without it a release would be
// the same data re-sent, bypassing the correction INV-12 requires.
func (s *Service) RequestRelease(ctx context.Context, a Actor, batchID string, rowNo int, reason, correctionRef string) error {
	if blank(reason) || blank(correctionRef) {
		return fmt.Errorf("%w: a release request needs a reason and a reference to the correction made in the owning domain", ErrValidation)
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return err
	}
	if err := requireReleasable(b); err != nil {
		return err
	}
	return s.repo.RequestRelease(ctx, a.TenantID, batchID, rowNo, a.PrincipalID, reason, correctionRef, a.CorrelationID, []Event{{
		Type: EventItemReleaseRequested, Payload: map[string]any{"batch_id": batchID, "row_no": rowNo, "reason": reason,
			"correction_ref": correctionRef, "by": a.PrincipalID},
	}})
}

// ApproveRelease releases the item: it goes back in the queue and the owning domain re-validates
// it. The decider must be independent of the requester and of whoever quarantined the item.
func (s *Service) ApproveRelease(ctx context.Context, a Actor, batchID string, rowNo int, reason string) error {
	return s.decideRelease(ctx, a, batchID, rowNo, reason, true, 0)
}

// DecideReleaseOfRequest decides the open release request NAMED by requestLedgerID, and refuses
// (ErrStaleRelease) if that is no longer the item's open request: ZS-SVC-Z-001 section 11 asks for
// "optimistic concurrency + authority check + reason required" on a release decision.
func (s *Service) DecideReleaseOfRequest(ctx context.Context, a Actor, batchID string, rowNo int, reason string, release bool, requestLedgerID int64) error {
	if requestLedgerID < 1 {
		return fmt.Errorf("%w: a decision must name the release request it answers (request_ledger_id)", ErrValidation)
	}
	return s.decideRelease(ctx, a, batchID, rowNo, reason, release, requestLedgerID)
}

// RefuseRelease declines a release request. The item stays quarantined and the request is closed,
// so a better-evidenced one can be made.
func (s *Service) RefuseRelease(ctx context.Context, a Actor, batchID string, rowNo int, reason string) error {
	return s.decideRelease(ctx, a, batchID, rowNo, reason, false, 0)
}

func (s *Service) decideRelease(ctx context.Context, a Actor, batchID string, rowNo int, reason string, release bool, answersRequest int64) error {
	if blank(reason) {
		return fmt.Errorf("%w: a release decision needs a reason", ErrValidation)
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return err
	}
	if release {
		if err := requireReleasable(b); err != nil {
			return err
		}
	}
	evType := EventItemReleaseRefused
	if release {
		evType = EventItemReleased
	}
	if err := s.repo.DecideRelease(ctx, a.TenantID, batchID, rowNo, a.PrincipalID, reason, a.CorrelationID, release, answersRequest, s.now(),
		s.cfg.ItemMaxAttempts*3, []Event{{Type: evType, Payload: map[string]any{"batch_id": batchID, "row_no": rowNo, "reason": reason, "by": a.PrincipalID}}}); err != nil {
		return err
	}
	// A batch that had finished with exceptions is importing again once an item is back in the queue.
	if release && b.Status == StatusCompletedWithExceptions {
		if _, err := s.transition(ctx, a, b, []Status{StatusCompletedWithExceptions}, StatusImporting, "ITEM_RELEASE", reason, Patch{}); err != nil &&
			!errors.Is(err, ErrInvalidState) {
			return err
		}
	}
	return nil
}

// requireReleasable: items are released while the batch is importing or finished with exceptions.
func requireReleasable(b *Batch) error {
	if b.Status != StatusImporting && b.Status != StatusCompletedWithExceptions {
		return fmt.Errorf("%w: items are released while the batch is importing or has exceptions (%s)", ErrInvalidState, b.Status)
	}
	return nil
}

// QuarantineView is a batch's quarantine state and its ledger. rowNo 0 is the whole batch.
type QuarantineView struct {
	Summary QuarantineSummary `json:"summary"`
	Ledger  []LedgerEntry     `json:"ledger"`
}

func (s *Service) QuarantineView(ctx context.Context, tenantID, batchID string, rowNo int) (*QuarantineView, error) {
	b, err := s.repo.GetBatch(ctx, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	quarantined, total, open, err := s.repo.QuarantineCounts(ctx, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	ledger, err := s.repo.QuarantineLedger(ctx, tenantID, batchID, rowNo)
	if err != nil {
		return nil, err
	}
	if ledger == nil {
		ledger = []LedgerEntry{}
	}
	return &QuarantineView{
		Summary: QuarantineSummary{State: DeriveQuarantineState(b.Status, quarantined, total, open), QuarantinedItems: quarantined,
			TotalItems: total, OpenReleaseRequests: open},
		Ledger: ledger,
	}, nil
}

// DeliveryKey is the Idempotency-Key an item is delivered to the owning domain under.
//
// Normally it is the item's own key, fixed for life: a retried delivery of the same attempt (a
// timeout after the domain committed, say) is then recognised and creates nothing twice. A FAILED
// item that is retried keeps that key for exactly that reason: its first attempt may have been
// committed.
//
// An item released from quarantine is different. Quarantine means the owning domain REFUSED it, so
// nothing was created, and a release is a new attempt after a correction (ZS-SVC-Z-001 INV-12). A
// domain that honours Idempotency-Key answers the original key with its original answer,
// refusals included, so re-sending under it would replay the refusal and a release could never
// take effect, whatever was corrected. Each release therefore delivers under a key of its own.
func DeliveryKey(it Item) string {
	if it.ReleaseGeneration <= 0 {
		return it.IdempotencyKey
	}
	return fmt.Sprintf("%s:rel%d", it.IdempotencyKey, it.ReleaseGeneration)
}
