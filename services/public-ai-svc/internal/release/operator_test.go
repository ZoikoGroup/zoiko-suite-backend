package release

import (
	"context"
	"testing"

	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 06 §21, Doc 09 §22: promotion, rollback and revocation are audited
// before they act, and refused without an actor and approval.
func TestOperatorActionsAreAudited(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.PutRelease(ctx, store.Release{ReleaseID: "kb-2", Kind: "kb", Manifest: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := PromoteAudited(ctx, st, Operator{Actor: "sre-1"}, "kb", "kb-2", nil); err == nil {
		t.Fatal("promotion without an approval reference must be refused")
	}
	if p, _ := st.GetPointer(ctx, "kb"); p == "kb-2" {
		t.Fatal("refused promotion moved the pointer")
	}
	op := Operator{Actor: "sre-1", Approval: "CHG-7", Reason: "weekly release", Environment: "staging"}
	if err := PromoteAudited(ctx, st, op, "kb", "kb-2", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RevokeAudited(ctx, st, op, "source", "src-pricing", "WITHDRAWN", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListAudit(ctx, 10)
	if len(got) != 2 || got[0].Action != "revocation.add" || got[1].Action != "release.promote" ||
		got[1].Actor != "sre-1" || got[1].Approval != "CHG-7" || got[1].AfterHash != store.Hash([]byte("kb-2")) {
		t.Fatalf("audit: %+v", got)
	}
}
