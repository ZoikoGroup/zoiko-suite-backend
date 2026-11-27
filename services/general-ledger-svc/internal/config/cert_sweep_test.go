package config_test

import (
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/config"
)

func TestParseCertSweepInterval(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      15 * time.Minute, // the default
		"  ":    15 * time.Minute,
		"off":   0,
		"OFF":   0,
		" Off ": 0,
		"0":     0,
		"0s":    0,
		"1m":    time.Minute, // the bounds are inclusive
		"15m":   15 * time.Minute,
		"90s":   90 * time.Second,
		"2h":    2 * time.Hour,
		"24h":   24 * time.Hour,
	} {
		got, err := config.ParseCertSweepInterval(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	// A sweep calls other services for every watched certification: tighter than a minute is refused, and so is anything that is not a
	// duration, is negative, or leaves a revocation unfound for more than a day.
	for _, raw := range []string{"59s", "1s", "1ms", "-5m", "25h", "100h", "soon", "15", "15 minutes", "1m30"} {
		if got, err := config.ParseCertSweepInterval(raw); err == nil {
			t.Fatalf("%q was accepted as %v", raw, got)
		}
	}
}

func TestLoad_CertSweepSettings(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk")
	t.Setenv("CERT_SWEEP_PRINCIPAL_ID", "")
	t.Setenv("CERT_SWEEP_INTERVAL", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Defaults: a 15 minute interval and NO principal, so the scheduler does not start until one is named.
	if c.CertSweepInterval != 15*time.Minute || c.CertSweepPrincipalID != "" {
		t.Fatalf("defaults: %v %q", c.CertSweepInterval, c.CertSweepPrincipalID)
	}
	t.Setenv("CERT_SWEEP_PRINCIPAL_ID", "  svc-cert-sweeper ")
	t.Setenv("CERT_SWEEP_INTERVAL", "5m")
	if c, err = config.Load(); err != nil || c.CertSweepPrincipalID != "svc-cert-sweeper" || c.CertSweepInterval != 5*time.Minute {
		t.Fatalf("%+v %v", c, err)
	}
	// A bad interval stops the service at startup rather than running a schedule nobody asked for.
	t.Setenv("CERT_SWEEP_INTERVAL", "10s")
	if _, err := config.Load(); err == nil {
		t.Fatal("an interval below the minimum was accepted at startup")
	}
}
