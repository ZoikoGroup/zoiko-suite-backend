package webhook

import (
	"testing"
	"time"
)

// S7-B3: a secret rotated in the vault reaches a running verifier.
func TestVerifier_SetSecretsRotatesWithoutARestart(t *testing.T) {
	oldS, newS := "old-secret-0123456789", "new-secret-0123456789"
	v := NewVerifier(map[string][]string{"ses": {oldS}}, time.Minute)
	now := time.Now()
	body := []byte(`{"event":"delivered"}`)

	if err := v.Verify("ses", Sign([]byte(oldS), now, body), body); err != nil {
		t.Fatalf("the old secret verifies before rotation: %v", err)
	}
	v.SetSecrets(map[string][]string{"ses": {newS}})
	if err := v.Verify("ses", Sign([]byte(newS), now, body), body); err != nil {
		t.Fatalf("the new secret verifies after rotation: %v", err)
	}
	if err := v.Verify("ses", Sign([]byte(oldS), now, body), body); err == nil {
		t.Fatal("the retired secret must stop verifying")
	}
	// Too-short secrets are dropped, and a provider with none refuses every callback.
	v.SetSecrets(map[string][]string{"ses": {"short"}})
	if err := v.Verify("ses", Sign([]byte("short"), now, body), body); err != ErrNoSecret {
		t.Fatalf("a short secret gives no usable secret: %v", err)
	}
	if got := v.Providers(); len(got) != 0 {
		t.Fatalf("providers: %v", got)
	}
}
