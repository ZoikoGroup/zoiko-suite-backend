package release

import (
	"errors"
	"path/filepath"
	"testing"

	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 06 §12 (RUNBOOK RB-11): rotating the release signing key. Releases are
// immutable, so rotation re-signs the current content as new release ids
// with the new key, promotes them, then restarts the service with the new
// public key. Afterwards a release signed with the old key is refused, and
// the old key cannot sign anything the service will load.
func TestSigningKeyRotation(t *testing.T) {
	e := setup(t) // running on the old key
	newPub, newKey, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// 1. Re-sign the same content under new release ids with the new key.
	kb, kbSig, _, err := BuildKB(filepath.Join(fixtures, "kb"), "kb-rotated-1", now, newKey)
	if err != nil {
		t.Fatal(err)
	}
	cl, clSig, err := BuildClaims(filepath.Join(fixtures, "claims.json"), "claims-rotated-1", now, newKey)
	if err != nil {
		t.Fatal(err)
	}
	// 2. Publishing verifies with the key the operator names: the new one.
	if err := Publish(ctx, e.st, newPub, "kb-rotated-1", "kb", kb, kbSig); err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, e.st, newPub, "claims-rotated-1", "claims", cl, clSig); err != nil {
		t.Fatal(err)
	}
	// The still-running service (old key) must not load the new releases
	// early: the switch happens with the restart, not before.
	if err := Promote(ctx, e.st, "kb", "kb-rotated-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := Promote(ctx, e.st, "claims", "claims-rotated-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Refresh(ctx); err == nil {
		t.Fatal("a runtime on the old key must refuse releases signed with the new key")
	}
	// 3. Restart with the new public key: the rotated releases load.
	rotated := New(e.st, newPub)
	if err := rotated.Refresh(ctx); err != nil {
		t.Fatalf("rotated runtime: %v", err)
	}
	// 4. Rolling back to a release signed with the retired key fails closed.
	if err := Promote(ctx, e.st, "kb", "kb-test-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := rotated.Refresh(ctx); err == nil {
		t.Fatal("a release signed with the retired key must be refused after rotation")
	}
	// 5. The retired key cannot publish anything the new key accepts.
	stale, staleSig, _, _ := BuildKB(filepath.Join(fixtures, "kb"), "kb-stale-1", now, e.key)
	if err := Publish(ctx, e.st, newPub, "kb-stale-1", "kb", stale, staleSig); err == nil {
		t.Fatal("a release signed with the retired key must not publish under the new key")
	}
}

// Doc 08 §13, OPR-021/029: while promotion_frozen is on, promotion is
// refused and rollback still works.
func TestPromotionFreeze(t *testing.T) {
	e := setup(t)
	op := Operator{Actor: "sre-1", Approval: "CHG-1", Environment: "test"}
	if err := e.st.SetFlag(ctx, store.Flag{Name: flags.PromotionFrozen, Value: []byte("true"), ActorRef: "oncall", ApprovalRef: "INC-7", UpdatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	if err := PromotionGateAudited(ctx, e.st, "kb", "kb-test-1", nil, op, nil, false); !errors.Is(err, ErrPromotionFrozen) {
		t.Fatalf("promotion during a freeze: %v", err)
	}
	if err := PromotionGateAudited(ctx, e.st, "kb", "kb-test-1", nil, op, nil, true); errors.Is(err, ErrPromotionFrozen) {
		t.Fatal("rollback must stay available during a freeze")
	}
}
