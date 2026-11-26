package config

import (
	"os"
	"testing"
)

// The package's other tests call Load and are not about the event region, so it is set for
// them here; TestLoad_RequiresEventResidencyRegion clears it to prove it is required.
func TestMain(m *testing.M) {
	_ = os.Setenv("EVENT_RESIDENCY_REGION", "local")
	os.Exit(m.Run())
}

func TestLoad_RequiresEventResidencyRegion(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a missing EVENT_RESIDENCY_REGION")
	}
	t.Setenv("EVENT_RESIDENCY_REGION", " uk ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EventResidencyRegion != "uk" {
		t.Fatalf("region = %q, want uk", cfg.EventResidencyRegion)
	}
}
