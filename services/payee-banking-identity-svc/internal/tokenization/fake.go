package tokenization

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
)

// fakePrefix marks a Fake token so one can never be mistaken for a real one.
const fakePrefix = "faketok_"

// Fake is a deterministic, in-process Tokenizer for tests and local
// development, so neither needs a running key-management-svc.
//
// NOT FOR PRODUCTION: a Fake token is the tenant and value base64-encoded, so
// anyone holding it can recover the value. It exists to exercise the
// contract — tokens differ from values, the same value tokenizes the same way
// for the same tenant, and another tenant's token is refused on
// detokenization (SEC-T29) — not to protect anything. cmd/server never
// constructs one.
type Fake struct {
	// Err, when set, is returned by every call: set it to
	// ErrTokenizationServiceUnavailable to simulate an outage.
	Err error

	mu    sync.Mutex
	calls int
}

var _ Tokenizer = (*Fake)(nil)

// Calls is how many Tokenize/Detokenize calls the Fake has received.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *Fake) record() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.Err
}

func (f *Fake) Tokenize(_ context.Context, scope Scope, classification, value string) (string, error) {
	if err := f.record(); err != nil {
		return "", err
	}
	if scope.TenantID == "" || value == "" || classification == "" {
		return "", ErrInvalidRequest
	}
	return fakePrefix + base64.RawURLEncoding.EncodeToString([]byte(scope.TenantID+"\x00"+value)), nil
}

func (f *Fake) Detokenize(_ context.Context, scope Scope, token string) (string, error) {
	if err := f.record(); err != nil {
		return "", err
	}
	if scope.TenantID == "" || scope.PrincipalID == "" || token == "" {
		return "", ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, fakePrefix))
	if err != nil || !strings.HasPrefix(token, fakePrefix) {
		return "", ErrDetokenizationDenied
	}
	tenant, value, ok := strings.Cut(string(raw), "\x00")
	if !ok || tenant != scope.TenantID {
		return "", ErrDetokenizationDenied // SEC-T29: not this tenant's token
	}
	return value, nil
}
