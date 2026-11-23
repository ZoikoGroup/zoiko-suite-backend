package flags

import (
	"context"
	"errors"
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

// Doc 09 ADM-T046 / Doc 08 §04: re-enabling the kill switch or model
// generation after a disable needs a second actor.
func TestReEnableNeedsDifferentActor(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{PublicAIEnabled, ModelGenerationEnabled} {
		st := store.NewMemory()
		if err := Set(ctx, st, name, []byte("true"), "sre-1", "CHG-0", "test"); err != nil {
			t.Fatalf("%s: enabling a never-disabled flag: %v", name, err)
		}
		if err := Set(ctx, st, name, []byte("false"), "sre-1", "INC-1", "test"); err != nil {
			t.Fatal(err)
		}
		err := Set(ctx, st, name, []byte(" true "), "sre-1", "INC-1", "test")
		if !errors.Is(err, ErrSameActor) {
			t.Fatalf("%s: same actor re-enable = %v, want ErrSameActor", name, err)
		}
		if err := Set(ctx, st, name, []byte("false"), "sre-1", "INC-1", "test"); err != nil {
			t.Fatalf("%s: disabling again is always allowed: %v", name, err)
		}
		if err := Set(ctx, st, name, []byte("true"), "sre-2", "INC-1", "test"); err != nil {
			t.Fatalf("%s: second actor re-enable: %v", name, err)
		}
		fs, _ := st.Flags(ctx)
		for _, f := range fs {
			if f.Name == name && string(f.Value) != "true" {
				t.Fatalf("%s = %s after re-enable", name, f.Value)
			}
		}
	}
	// Other flags are not dual-controlled.
	st := store.NewMemory()
	for _, v := range []string{"false", "true"} {
		if err := Set(ctx, st, TrialCTAEnabled, []byte(v), "sre-1", "CHG-1", "test"); err != nil {
			t.Fatal(err)
		}
	}
}

// Doc 10 App. B: commercial_routes_enabled defaults on, validates as a bool
// and fails closed with the rest of the flags.
func TestCommercialRoutesFlag(t *testing.T) {
	ctx := context.Background()
	if !Defaults().CommercialRoutes {
		t.Fatal("commercial_routes_enabled must default to true")
	}
	st := store.NewMemory()
	if err := Set(ctx, st, CommercialRoutesEnabled, []byte(`"yes"`), "sre-1", "CHG-1", "test"); err == nil {
		t.Fatal("non-boolean commercial_routes_enabled must be refused")
	}
	if err := Set(ctx, st, CommercialRoutesEnabled, []byte("false"), "sre-1", "CHG-1", "test"); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(st)
	if err := l.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if l.Values().CommercialRoutes {
		t.Fatal("commercial_routes_enabled=false not applied")
	}
}

// SEC-P0-09: outside dev/test the global concurrency cap cannot be removed.
func TestMaxConcurrentTurnsCapRequiredOutsideDev(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for _, env := range []string{"staging", "production"} {
		if err := Set(ctx, st, MaxConcurrentTurns, []byte("0"), "sre-1", "CHG-1", env); err == nil {
			t.Errorf("max_concurrent_turns=0 in %s must be refused", env)
		}
		if err := Set(ctx, st, MaxConcurrentTurns, []byte("50"), "sre-1", "CHG-1", env); err != nil {
			t.Errorf("max_concurrent_turns=50 in %s: %v", env, err)
		}
	}
	if err := Set(ctx, st, MaxConcurrentTurns, []byte("0"), "sre-1", "CHG-1", "dev"); err != nil {
		t.Errorf("dev may run uncapped: %v", err)
	}
}
