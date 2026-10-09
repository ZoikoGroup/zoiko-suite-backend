package config_test

import (
	"strings"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/config"
)

func TestLoad_PeriodGateDefaults(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk") // required since the shared eventing outbox (main)
	t.Setenv("PERIOD_GATE_MODE", "")
	t.Setenv("PERIOD_GATE_SHADOW_TIMEOUT", "")
	t.Setenv("ACCOUNTING_PERIOD_URL", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PeriodGateMode != "off" || c.PeriodGateShadowTimeout != 300*time.Millisecond ||
		c.AccountingPeriodURL != "http://accounting-period-svc:8174" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoad_ShadowAndOffAccepted(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk") // required since the shared eventing outbox (main)
	for _, m := range []string{"off", "shadow", " Shadow ", "OFF"} {
		t.Setenv("PERIOD_GATE_MODE", m)
		c, err := config.Load()
		if err != nil {
			t.Fatalf("%q: %v", m, err)
		}
		if c.PeriodGateMode != "off" && c.PeriodGateMode != "shadow" {
			t.Fatalf("%q -> %q", m, c.PeriodGateMode)
		}
	}
}

func TestLoad_EnforceAndGarbageRejectedAtStartup(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk") // required since the shared eventing outbox (main)
	for _, m := range []string{"enforce", "ENFORCE", "on", "true", "shadow,enforce", "garbage"} {
		t.Setenv("PERIOD_GATE_MODE", m)
		c, err := config.Load()
		if err == nil || c != nil {
			t.Fatalf("PERIOD_GATE_MODE=%q must fail startup, got cfg=%v err=%v", m, c, err)
		}
		if !strings.Contains(err.Error(), "enforce is not implemented") && !strings.Contains(err.Error(), "not implemented in this phase") {
			t.Errorf("%q: error should say enforce is not implemented in this phase, got: %v", m, err)
		}
	}
}

func TestLoad_BadShadowTimeoutRejected(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk") // required since the shared eventing outbox (main)
	for _, v := range []string{"abc", "0", "-5ms", "300"} {
		t.Setenv("PERIOD_GATE_MODE", "shadow")
		t.Setenv("PERIOD_GATE_SHADOW_TIMEOUT", v)
		if _, err := config.Load(); err == nil {
			t.Errorf("timeout %q should be rejected", v)
		}
	}
	t.Setenv("PERIOD_GATE_SHADOW_TIMEOUT", "750ms")
	c, err := config.Load()
	if err != nil || c.PeriodGateShadowTimeout != 750*time.Millisecond {
		t.Fatalf("got %v %v", c, err)
	}
}
