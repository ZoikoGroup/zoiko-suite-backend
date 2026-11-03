package config

import (
	"strings"
	"testing"
)

func TestConfig_CursorKeyRequiredInProtectedTiers(t *testing.T) {
	for _, env := range []string{"staging", "production"} {
		if _, err := cursorSigningKey("", env); err == nil || !strings.Contains(err.Error(), "CURSOR_SIGNING_KEY_HEX") {
			t.Fatalf("%s: want CURSOR_SIGNING_KEY_HEX refusal, got %v", env, err)
		}
	}
	if _, err := cursorSigningKey("00ff", "production"); err == nil {
		t.Fatal("a 2-byte key must be refused")
	}
	if _, err := cursorSigningKey("zz", "local"); err == nil {
		t.Fatal("a non-hex key must be refused")
	}
	key, err := cursorSigningKey("", "local")
	if err != nil || len(key) != 32 {
		t.Fatalf("local generates a 32-byte key, got %d bytes, err %v", len(key), err)
	}
}
