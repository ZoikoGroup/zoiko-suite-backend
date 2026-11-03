package release

import (
	"encoding/json"
	"fmt"
	"strings"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/knowledge"
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
