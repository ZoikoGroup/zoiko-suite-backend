package release

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// SignoffRoles are the Doc 11 §15 gate approvers a release manifest names.
var SignoffRoles = []string{"product", "cto", "security", "dpo", "qa", "sre", "founder"}

// RecordManifest stores a signed release manifest's fingerprint in the
// hash-chained control audit (Doc 08 §06, OPR-048): the record is immutable
// and any later edit to the file no longer matches it.
func RecordManifest(ctx context.Context, st store.Store, op Operator, releaseID string, body []byte) error {
	return st.AppendAudit(ctx, store.AuditEntry{Action: "manifest.recorded", Target: releaseID, Actor: op.Actor, Approval: op.Approval,
		Reason: op.Reason, AfterHash: store.Hash(body), Environment: op.Environment, At: time.Now().UTC()})
}

// SignOff records one named approver's sign-off on a recorded manifest
// (Doc 08 §33, Doc 11 §24): who, in which role, under which approval, when.
func SignOff(ctx context.Context, st store.Store, op Operator, role, releaseID string, body []byte) error {
	if !contains(SignoffRoles, role) {
		return fmt.Errorf("unknown sign-off role %q (want one of %v)", role, SignoffRoles)
	}
	if _, err := recordedManifest(ctx, st, releaseID, body); err != nil {
		return err
	}
	return st.AppendAudit(ctx, store.AuditEntry{Action: "manifest.signoff", Target: releaseID, Actor: op.Actor, ActorRole: role,
		Approval: op.Approval, Reason: op.Reason, AfterHash: store.Hash(body), Environment: op.Environment, At: time.Now().UTC()})
}

// SignoffRecord is one recorded sign-off.
type SignoffRecord struct {
	Role, Actor, Approval string
	At                    time.Time
}

// ErrManifestNotRecorded: the manifest was never recorded, or was changed
// after it was recorded.
var ErrManifestNotRecorded = errors.New("release manifest is not recorded in the control audit (or was changed after recording)")

// VerifyDeployManifest is the deploy-time check (Doc 08 §06): the manifest
// signature verifies, its exact bytes were recorded, and it reports the
// sign-offs recorded so far and the roles still missing.
func VerifyDeployManifest(ctx context.Context, st store.Store, pubKey, releaseID string, body, sig []byte) ([]SignoffRecord, []string, error) {
	if err := signing.Verify(pubKey, body, sig); err != nil {
		return nil, nil, fmt.Errorf("manifest signature: %w", err)
	}
	entries, err := recordedManifest(ctx, st, releaseID, body)
	if err != nil {
		return nil, nil, err
	}
	var signed []SignoffRecord
	have := map[string]bool{}
	for _, e := range entries {
		if e.Action == "manifest.signoff" && e.AfterHash == store.Hash(body) {
			signed = append(signed, SignoffRecord{Role: e.ActorRole, Actor: e.Actor, Approval: e.Approval, At: e.At})
			have[e.ActorRole] = true
		}
	}
	var missing []string
	for _, r := range SignoffRoles {
		if !have[r] {
			missing = append(missing, r)
		}
	}
	sort.Slice(signed, func(i, j int) bool { return signed[i].Role < signed[j].Role })
	return signed, missing, nil
}

// recordedManifest returns the audit entries for releaseID once the
// manifest with these exact bytes is found recorded.
func recordedManifest(ctx context.Context, st store.Store, releaseID string, body []byte) ([]store.AuditEntry, error) {
	all, err := st.ListAudit(ctx, 100000)
	if err != nil {
		return nil, err
	}
	var mine []store.AuditEntry
	recorded := false
	for _, e := range all {
		if e.Target != releaseID {
			continue
		}
		mine = append(mine, e)
		if e.Action == "manifest.recorded" && e.AfterHash == store.Hash(body) {
			recorded = true
		}
	}
	if !recorded {
		return nil, ErrManifestNotRecorded
	}
	return mine, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
