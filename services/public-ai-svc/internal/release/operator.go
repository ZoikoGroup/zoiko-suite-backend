package release

import (
	"context"
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

// PromoteAudited moves the pointer for kind (also rollback) and records the
// action in the append-only control audit first; if the audit cannot be
// written, nothing moves.
func PromoteAudited(ctx context.Context, st store.Store, op Operator, kind, releaseID string, events []store.OutboxEvent) error {
	if err := op.valid(); err != nil {
		return err
	}
	before, _ := st.GetPointer(ctx, kind)
	if err := st.AppendAudit(ctx, store.AuditEntry{Action: "release.promote", Target: kind, Actor: op.Actor, Approval: op.Approval,
		Reason: op.Reason, BeforeHash: store.Hash([]byte(before)), AfterHash: store.Hash([]byte(releaseID)), Environment: op.Environment,
		At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("control audit: %w", err)
	}
	return Promote(ctx, st, kind, releaseID, events)
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
