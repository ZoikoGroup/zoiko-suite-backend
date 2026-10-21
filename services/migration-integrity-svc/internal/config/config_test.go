package config

import "testing"

func TestLoad_IntegrityScoreThreshold(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk")

	t.Run("defaults to 95", func(t *testing.T) {
		t.Setenv("INTEGRITY_SCORE_THRESHOLD", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IntegrityScoreThreshold != 95 {
			t.Fatalf("expected default threshold 95, got %v", cfg.IntegrityScoreThreshold)
		}
	})

	t.Run("reads the configured value", func(t *testing.T) {
		t.Setenv("INTEGRITY_SCORE_THRESHOLD", "99.5")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IntegrityScoreThreshold != 99.5 {
			t.Fatalf("expected configured threshold 99.5, got %v", cfg.IntegrityScoreThreshold)
		}
	})

	t.Run("accepts the 0-100 boundaries", func(t *testing.T) {
		for _, raw := range []string{"0", "100"} {
			t.Setenv("INTEGRITY_SCORE_THRESHOLD", raw)
			if _, err := Load(); err != nil {
				t.Fatalf("INTEGRITY_SCORE_THRESHOLD=%q must be accepted: %v", raw, err)
			}
		}
	})

	t.Run("refuses values outside 0-100 and non-numbers", func(t *testing.T) {
		for _, raw := range []string{"101", "-1", "abc", "95%"} {
			t.Setenv("INTEGRITY_SCORE_THRESHOLD", raw)
			if _, err := Load(); err == nil {
				t.Fatalf("INTEGRITY_SCORE_THRESHOLD=%q must be refused", raw)
			}
		}
	})
}
