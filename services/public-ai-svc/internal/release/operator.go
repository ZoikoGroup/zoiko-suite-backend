package release

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// Operator is who performs a control action and on what authority (Doc 06
// §21, Doc 09 §05/§22): every promotion, rollback and revocation names an
// actor, an approval reference and a reason.
type Operator struct {
	Actor       string
	Approval    string
	Reason      string
	Environment string
}

func (op Operator) valid() error {
	if op.Actor == "" || op.Approval == "" {
		return fmt.Errorf("an operator action needs an actor and an approval reference")
	}
	return nil
}

// Control audit actions for promotion. A promote entry followed by a
// promote-failed entry for the same target and release means the pointer
// never moved.
const (
	AuditPromote       = "release.promote"
	AuditPromoteFailed = "release.promote.failed"
)

// PromoteAudited moves the pointer for kind (also rollback) and records the
// action in the append-only control audit (Doc 08 OPR-033).
//
// The store has no transaction spanning the audit ledger and the pointer, so
// the order is chosen to fail safe: the audit is written first, and if it
// cannot be, nothing moves (a pointer never moves unaudited). If the move
// then fails, a compensating promote-failed entry is appended so the ledger
// does not claim a promotion that did not happen; if even that fails, both
// errors are returned for the operator to reconcile by hand.
func PromoteAudited(ctx context.Context, st store.Store, op Operator, kind, releaseID string, events []store.OutboxEvent) error {
	if err := op.valid(); err != nil {
		return err
	}
	before, _ := st.GetPointer(ctx, kind)
	entry := store.AuditEntry{Action: AuditPromote, Target: kind, Actor: op.Actor, Approval: op.Approval,
		Reason: op.Reason, BeforeHash: store.Hash([]byte(before)), AfterHash: store.Hash([]byte(releaseID)), Environment: op.Environment,
		At: time.Now().UTC()}
	if err := st.AppendAudit(ctx, entry); err != nil {
		return fmt.Errorf("control audit: %w", err)
	}
	if err := Promote(ctx, st, kind, releaseID, events); err != nil {
		comp := entry
		comp.Action, comp.Reason, comp.At = AuditPromoteFailed, "pointer did not move: "+err.Error(), time.Now().UTC()
		if aerr := st.AppendAudit(context.WithoutCancel(ctx), comp); aerr != nil {
			return errors.Join(fmt.Errorf("promote: %w", err), fmt.Errorf("compensating control audit: %w", aerr))
		}
		return fmt.Errorf("promote: %w", err)
	}
	return nil
}

// RevokeAudited appends to the revocation ledger after recording the action
// in the control audit.
func RevokeAudited(ctx context.Context, st store.Store, op Operator, targetKind, targetID, reason string, events []store.OutboxEvent) (int64, error) {
	if err := op.valid(); err != nil {
		return 0, err
	}
	if err := st.AppendAudit(ctx, store.AuditEntry{Action: "revocation.add", Target: targetKind + ":" + targetID, Actor: op.Actor,
		Approval: op.Approval, Reason: reason, AfterHash: store.Hash([]byte(targetKind + ":" + targetID)), Environment: op.Environment,
		At: time.Now().UTC()}); err != nil {
		return 0, fmt.Errorf("control audit: %w", err)
	}
	return Revoke(ctx, st, targetKind, targetID, reason, events)
}
