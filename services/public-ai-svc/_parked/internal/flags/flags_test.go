package flags

import (
	"context"
	"testing"

	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 10 App. B: the commercial-extension gates exist and stay off in R0.
// Turning one on is refused; recording it as off is allowed.
func TestDoc10GatesAreHardOff(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for _, name := range []string{SecureLeadIntakeEnabled, CRMSyncEnabled, BookingSyncEnabled, ThirdPartyTrackingEnabled, StreamEnabled} {
		if err := Set(ctx, st, name, []byte("true"), "sre-1", "CHG-1", "test"); err == nil {
			t.Errorf("%s=true must be refused in R0", name)
		}
		if err := Set(ctx, st, name, []byte("false"), "sre-1", "CHG-1", "test"); err != nil {
			t.Errorf("%s=false: %v", name, err)
		}
	}
	if err := Set(ctx, st, CRMSyncEnabled, []byte("false"), "", "", "test"); err == nil {
		t.Error("a flag change needs actor and approval")
	}
}

// Doc 09 §22: a kill-switch change is audited with before and after hashes.
func TestFlagChangesAreAudited(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := Set(ctx, st, PublicAIEnabled, []byte("false"), "sre-1", "CHG-3", "test"); err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, st, PublicAIEnabled, []byte("true"), "sre-2", "CHG-4", "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListAudit(ctx, 10)
	if len(got) != 2 || got[0].Actor != "sre-2" || got[0].BeforeHash != store.Hash([]byte("false")) || got[0].AfterHash != store.Hash([]byte("true")) {
		t.Fatalf("flag audit: %+v", got)
	}
}
