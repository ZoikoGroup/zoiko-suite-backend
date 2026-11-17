package config

import "testing"

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
