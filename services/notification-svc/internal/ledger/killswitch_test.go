package ledger_test

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/notification-svc/internal/ledger"
)

func TestKillSwitch_GlobalScope(t *testing.T) {
	ctx := context.Background()
	ks := ledger.NewKillSwitchManager(zap.NewNop())

	blocked, _ := ks.Check(ctx, "t-1", "ZS-IA-001")
	if blocked {
		t.Fatal("expected unblocked initially")
	}

	ks.EngageGlobal(ledger.KillSwitchReasonSecurityIncident, "security-admin")

	blocked, reason := ks.Check(ctx, "t-1", "ZS-IA-001")
	if !blocked {
		t.Fatal("expected global kill switch to block delivery")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}

	ks.DisengageGlobal("security-admin")

	blocked, _ = ks.Check(ctx, "t-1", "ZS-IA-001")
	if blocked {
		t.Fatal("expected unblocked after disengage")
	}
}

func TestKillSwitch_TenantScope(t *testing.T) {
	ctx := context.Background()
	ks := ledger.NewKillSwitchManager(zap.NewNop())

	ks.EngageTenant("tenant-evil", ledger.KillSwitchReasonTenantLock, "compliance-officer")

	// Target tenant must be blocked
	blocked, _ := ks.Check(ctx, "tenant-evil", "ZS-IA-001")
	if !blocked {
		t.Fatal("expected tenant-evil to be blocked")
	}

	// Other tenants must NOT be blocked
	blocked, _ = ks.Check(ctx, "tenant-good", "ZS-IA-001")
	if blocked {
		t.Fatal("expected tenant-good to remain unblocked")
	}

	ks.DisengageTenant("tenant-evil", "compliance-officer")
	blocked, _ = ks.Check(ctx, "tenant-evil", "ZS-IA-001")
	if blocked {
		t.Fatal("expected tenant-evil to be unblocked after release")
	}
}

func TestKillSwitch_TemplateScope(t *testing.T) {
	ctx := context.Background()
	ks := ledger.NewKillSwitchManager(zap.NewNop())

	ks.EngageTemplate("ZS-IA-005", ledger.KillSwitchReasonTemplateDefect, "release-engineer")

	// ZS-IA-005 is blocked
	blocked, _ := ks.Check(ctx, "t-1", "ZS-IA-005")
	if !blocked {
		t.Fatal("expected ZS-IA-005 to be blocked")
	}

	// ZS-IA-001 is unaffected
	blocked, _ = ks.Check(ctx, "t-1", "ZS-IA-001")
	if blocked {
		t.Fatal("expected ZS-IA-001 to remain unblocked")
	}
}

func TestKillSwitch_TemplateFamilyWildcardScope(t *testing.T) {
	ctx := context.Background()
	ks := ledger.NewKillSwitchManager(zap.NewNop())

	// Block all Family A templates
	ks.EngageTemplate("ZS-IA-*", ledger.KillSwitchReasonSecurityIncident, "secops")

	blocked, _ := ks.Check(ctx, "t-1", "ZS-IA-001")
	if !blocked {
		t.Fatal("expected ZS-IA-001 to be blocked by wildcard ZS-IA-*")
	}
	blocked, _ = ks.Check(ctx, "t-1", "ZS-IA-004")
	if !blocked {
		t.Fatal("expected ZS-IA-004 to be blocked by wildcard ZS-IA-*")
	}

	// Family B is unaffected
	blocked, _ = ks.Check(ctx, "t-1", "ZS-OW-017")
	if blocked {
		t.Fatal("expected ZS-OW-017 to remain unblocked")
	}
}
