package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/store"
)

// EvalEvidence is the part of an eval report the promotion gate reads.
type EvalEvidence struct {
	Verdict   string `json:"verdict"`
	ReleaseID string `json:"knowledge_release_id"`
}

// PromotionGate applies Doc 03 §10-11 before a pointer moves:
//   - the release carries at least two distinct approvers (two-person control);
//   - the operator promoting it is not one of them (KB-016: independent signer);
//   - a PASS evaluation exists, and for a KB release it ran against that
//     release (KB-027). A rollback to a release that was already promoted
//     skips the evaluation check but not the approver checks.
func PromotionGate(kind, releaseID string, manifest []byte, op Operator, ev *EvalEvidence, rollback bool) error {
	approvers, err := Approvers(kind, manifest)
	if err != nil {
		return err
	}
	return gate(approvers, kind, releaseID, op, ev, rollback)
}

// Approvers returns the approvers recorded in a signed release manifest.
func Approvers(kind string, manifest []byte) ([]string, error) {
	var approvers []string
	switch kind {
	case knowledge.KindKB:
		var m knowledge.Manifest
		if err := json.Unmarshal(manifest, &m); err != nil {
			return nil, fmt.Errorf("promotion gate: %w", err)
		}
		approvers = m.ApprovedBy
	case claims.KindClaims:
		var m claims.Manifest
		if err := json.Unmarshal(manifest, &m); err != nil {
			return nil, fmt.Errorf("promotion gate: %w", err)
		}
		approvers = m.ApprovedBy
	default:
		return nil, fmt.Errorf("promotion gate: unknown kind %q", kind)
	}
	return approvers, nil
}

func gate(approvers []string, kind, releaseID string, op Operator, ev *EvalEvidence, rollback bool) error {
	seen := map[string]bool{}
	for _, a := range approvers {
		if a = strings.TrimSpace(a); a != "" {
			seen[strings.ToLower(a)] = true
		}
	}
	if len(seen) < 2 {
		return fmt.Errorf("promotion denied: release %s needs two distinct approvers", releaseID)
	}
	if seen[strings.ToLower(strings.TrimSpace(op.Actor))] {
		return fmt.Errorf("promotion denied: %s approved release %s and cannot also promote it", op.Actor, releaseID)
	}
	if rollback {
		return nil
	}
	if ev == nil || ev.Verdict != "PASS" {
		return fmt.Errorf("promotion denied: a PASS evaluation report is required")
	}
	if kind == knowledge.KindKB && ev.ReleaseID != releaseID {
		return fmt.Errorf("promotion denied: evaluation ran against %q, not %s", ev.ReleaseID, releaseID)
	}
	return nil
}

// auditScanLimit bounds how far back rollback verification reads the
// control audit (newest first).
const auditScanLimit = 100000

// ErrNoPriorPromotion means a rollback names a release the control audit
// never saw promoted for that kind.
var ErrNoPriorPromotion = errors.New("rollback denied: no completed promotion of this release in the control audit")

// VerifyRollback checks that releaseID was really promoted for kind before
// (Doc 03 §11): -rollback is never taken on trust. A promotion followed by
// its compensating promote-failed entry does not count.
func VerifyRollback(ctx context.Context, st store.Store, kind, releaseID string) error {
	entries, err := st.ListAudit(ctx, auditScanLimit)
	if err != nil {
		return fmt.Errorf("rollback denied: control audit unavailable: %w", err)
	}
	want := store.Hash([]byte(releaseID))
	failed := 0
	for _, e := range entries { // newest first: a failure is seen before its promote
		if e.Target != kind || e.AfterHash != want {
			continue
		}
		switch e.Action {
		case AuditPromoteFailed:
			failed++
		case AuditPromote:
			if failed > 0 {
				failed--
				continue
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s %s", ErrNoPriorPromotion, kind, releaseID)
}

// PromotionGateAudited is PromotionGate plus, for a rollback, proof from the
// control audit that the release was promoted before.
func PromotionGateAudited(ctx context.Context, st store.Store, kind, releaseID string, manifest []byte, op Operator, ev *EvalEvidence, rollback bool) error {
	// Doc 08 §13, OPR-021/029: no promotion while the freeze is on; rollback
	// stays available because it is the recovery. Checked first: a frozen
	// promotion is refused whatever else is wrong with it.
	if !rollback {
		fs, err := st.Flags(ctx)
		if err != nil {
			return fmt.Errorf("promotion freeze check: %w", err)
		}
		for _, f := range fs {
			if f.Name == flags.PromotionFrozen && strings.TrimSpace(string(f.Value)) == "true" {
				return ErrPromotionFrozen
			}
		}
	}
	if err := PromotionGate(kind, releaseID, manifest, op, ev, rollback); err != nil {
		return err
	}
	if rollback {
		return VerifyRollback(ctx, st, kind, releaseID)
	}
	return nil
}

// ErrPromotionFrozen: promotion_frozen is on (an error-budget burn or P0
// incident is open). Roll back instead, or clear the freeze with approval.
var ErrPromotionFrozen = errors.New("promotion is frozen (promotion_frozen=true): an error-budget burn or P0 incident is open; rollback is still allowed")
