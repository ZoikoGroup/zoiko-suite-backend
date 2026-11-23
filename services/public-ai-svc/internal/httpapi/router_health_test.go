package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
)

type stubConfig struct{}

func (stubConfig) PublicConfig() contract.ConfigResponse {
	return contract.ConfigResponse{AssistantDisplayName: "x"}
}

type stubFlags struct{ v flags.Values }

func (s stubFlags) Values() flags.Values { return s.v }

// Doc 08 §10: /health/startup is 503 until the first release and flag load
// has completed.
func TestStartupWaitsForFirstLoad(t *testing.T) {
	done := false
	h := New(testConfig(), nil, Deps{Started: func() bool { return done }}).Router()
	if w := do(t, h, http.MethodGet, "/health/startup", "", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("startup before first load = %d, want 503", w.Code)
	}
	done = true
	if w := do(t, h, http.MethodGet, "/health/startup", "", "", ""); w.Code != http.StatusOK {
		t.Fatalf("startup after first load = %d, want 200", w.Code)
	}
}

// OPR-011: the kill switch is not a release failure. With releases fresh
// and public_ai_enabled=false the service stays ready, public_ai_release_ready
// stays 1 and /config says chat_enabled=false.
func TestKillSwitchIsNotAReleaseFailure(t *testing.T) {
	fv := flags.Defaults()
	fv.PublicAIEnabled = false
	h := New(testConfig(), nil, Deps{
		Ready:        func() bool { return true },
		ReleaseReady: func() bool { return true },
		Config:       stubConfig{},
		Flags:        stubFlags{v: fv},
	}).Router()
	if w := do(t, h, http.MethodGet, "/health/ready", "", "", ""); w.Code != http.StatusOK {
		t.Fatalf("ready with kill switch on = %d, want 200", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/metrics", "", "", ""); !strings.Contains(w.Body.String(), "public_ai_release_ready 1") {
		t.Fatal("kill switch must not drop public_ai_release_ready")
	}
	w := do(t, h, http.MethodGet, "/public-ai/v1/config", testOrigin, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("config = %d %s", w.Code, w.Body.String())
	}
	var cfg contract.ConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Capabilities.ChatEnabled == nil || *cfg.Capabilities.ChatEnabled {
		t.Fatalf("chat_enabled = %v, want false", cfg.Capabilities.ChatEnabled)
	}
}

// The release_ready metric follows the release runtime only, not draining
// or other readiness inputs.
func TestReleaseReadyMetricFollowsReleases(t *testing.T) {
	h := New(testConfig(), nil, Deps{Ready: func() bool { return true }, ReleaseReady: func() bool { return false }}).Router()
	if w := do(t, h, http.MethodGet, "/metrics", "", "", ""); !strings.Contains(w.Body.String(), "public_ai_release_ready 0") {
		t.Fatal("public_ai_release_ready should be 0 when releases are not ready")
	}
}

func TestConfigChatEnabledTrue(t *testing.T) {
	h := New(testConfig(), nil, Deps{Config: stubConfig{}, ChatEnabled: func() bool { return true }}).Router()
	w := do(t, h, http.MethodGet, "/public-ai/v1/config", testOrigin, "", "")
	if !strings.Contains(w.Body.String(), `"chat_enabled":true`) {
		t.Fatalf("config lacks chat_enabled=true: %s", w.Body.String())
	}
}

// Doc 06 §12: with PUBLIC_AI_ADMIN_ADDR set, /metrics and /health/* move to
// the admin listener and the public listener no longer serves them.
func TestAdminListenerSeparatesInternalEndpoints(t *testing.T) {
	cfg := testConfig()
	cfg.AdminAddr = "127.0.0.1:9101"
	srv := New(cfg, nil, Deps{Ready: func() bool { return true }})
	pub, admin := srv.Router(), srv.AdminRouter()
	for _, p := range []string{"/metrics", "/health/live", "/health/ready", "/health/startup"} {
		if w := do(t, pub, http.MethodGet, p, "", "", ""); w.Code != http.StatusNotFound {
			t.Errorf("public listener serves %s: %d", p, w.Code)
		}
		if w := do(t, admin, http.MethodGet, p, "", "", ""); w.Code != http.StatusOK {
			t.Errorf("admin listener %s: %d", p, w.Code)
		}
	}
	if w := do(t, admin, http.MethodGet, "/public-ai/v1/config", testOrigin, "", ""); w.Code != http.StatusNotFound {
		t.Errorf("admin listener must not serve the public API: %d", w.Code)
	}
	// Unset: unchanged behaviour, probes on the public listener.
	if w := do(t, New(testConfig(), nil, Deps{}).Router(), http.MethodGet, "/health/live", "", "", ""); w.Code != http.StatusOK {
		t.Errorf("without an admin addr /health/live must stay on the public listener: %d", w.Code)
	}
}
