package main

import (
	"context"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 08 OPR-012: shutdown always outlasts a turn's lease (2×model timeout
// + 5 s, httpapi), so an in-flight turn finishes with a stored outcome.
func TestShutdownOutlastsTurnLease(t *testing.T) {
	for _, mt := range []time.Duration{6 * time.Second, 15 * time.Second, 30 * time.Second} {
		lease := 2*mt + 5*time.Second
		if got := shutdownTimeout(config.Config{ModelTimeout: mt}); got <= lease {
			t.Errorf("model timeout %s: shutdown %s does not outlast the %s lease", mt, got, lease)
		}
	}
}

// Doc 08 §11: with the largest model timeout config accepts, drain plus the
// shutdown wait still fits the compose stop grace period.
func TestShutdownFitsStopGrace(t *testing.T) {
	if got := drainGrace + shutdownTimeout(config.Config{ModelTimeout: config.MaxModelTimeout}); got > config.StopGracePeriod {
		t.Fatalf("drain + shutdown = %s exceeds the %s stop grace", got, config.StopGracePeriod)
	}
}

// OPR-011: readiness covers releases, flags and draining; the kill switch
// (PUBLIC_AI_ENABLED=false or public_ai_enabled=false) is served as the
// KILL_SWITCH outcome, never as not-ready.
func TestReadinessExcludesKillSwitch(t *testing.T) {
	ready := readiness(func() bool { return true }, func() bool { return true })
	if !ready() {
		t.Fatal("fresh releases and readable flags must be ready")
	}
	if readiness(func() bool { return false }, func() bool { return true })() {
		t.Fatal("stale releases must not be ready")
	}
	if readiness(func() bool { return true }, func() bool { return false })() {
		t.Fatal("unreadable flags must not be ready")
	}
	draining.Store(true)
	defer draining.Store(false)
	if ready() {
		t.Fatal("draining must not be ready")
	}
}

func TestEnvKillSwitchFoldsIntoFlag(t *testing.T) {
	fl := flags.NewLoader(store.NewMemory())
	if err := fl.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !(envKillSwitch{fl: fl, enabled: true}).Values().PublicAIEnabled {
		t.Fatal("PUBLIC_AI_ENABLED=true keeps the flag value")
	}
	if (envKillSwitch{fl: fl, enabled: false}).Values().PublicAIEnabled {
		t.Fatal("PUBLIC_AI_ENABLED=false must read as the kill switch")
	}
}
