package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func writeToken(t *testing.T, dir, service, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, service), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, service, "token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const tokA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const tokB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestReadBootstrapTokens_OnePerService(t *testing.T) {
	dir := t.TempDir()
	writeToken(t, dir, "gateway-auth-svc", tokA)
	writeToken(t, dir, "authorization-svc", tokB)
	got, err := readBootstrapTokens(dir, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if got[tokA] != "gateway-auth-svc" || got[tokB] != "authorization-svc" || len(got) != 2 {
		t.Fatalf("tokens = %v", got)
	}
}

func TestReadBootstrapTokens_Refusals(t *testing.T) {
	cases := map[string]func(dir string){
		"shared token": func(dir string) {
			writeToken(t, dir, "gateway-auth-svc", tokA)
			writeToken(t, dir, "authorization-svc", tokA)
		},
		"short token":  func(dir string) { writeToken(t, dir, "gateway-auth-svc", "short") },
		"bad name":     func(dir string) { writeToken(t, dir, "Gateway_Auth", tokA) },
		"missing file": func(dir string) { _ = os.MkdirAll(filepath.Join(dir, "gateway-auth-svc"), 0o700) },
	}
	for name, setup := range cases {
		dir := t.TempDir()
		setup(dir)
		if _, err := readBootstrapTokens(dir, zap.NewNop()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadBootstrapTokens_LegacySharedTokenRefused(t *testing.T) {
	t.Setenv("MTLS_BOOTSTRAP_TOKEN_PATH", "/bootstrap/token")
	_, err := loadBootstrapTokens(zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "M-01") {
		t.Fatalf("legacy shared token: err = %v", err)
	}
}

func TestLoadBootstrapTokens_UnsetDisables(t *testing.T) {
	t.Setenv("MTLS_BOOTSTRAP_TOKEN_PATH", "")
	t.Setenv("MTLS_BOOTSTRAP_TOKENS_DIR", "")
	got, err := loadBootstrapTokens(zap.NewNop())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
