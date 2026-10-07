package webhook_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/webhook"
)

var (
	secretA = []byte("secret-A-0123456789")
	secretB = []byte("secret-B-0123456789")
	body    = []byte(`{"event_id":"e1","event_type":"DELIVERED"}`)
)

func verifierAt(now time.Time, secrets map[string][]string) *webhook.Verifier {
	v := webhook.NewVerifier(secrets, 5*time.Minute)
	webhook.SetNowForTest(v, func() time.Time { return now })
	return v
}

func TestVerify_AcceptsAValidSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := verifierAt(now, map[string][]string{"ses": {string(secretA)}})
	if err := v.Verify("ses", webhook.Sign(secretA, now, body), body); err != nil {
		t.Fatalf("valid signature refused: %v", err)
	}
	if err := v.Verify("SES", webhook.Sign(secretA, now, body), body); err != nil {
		t.Errorf("provider names are matched case-insensitively: %v", err)
	}
}

func TestVerify_RefusesEveryFailureClosed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := verifierAt(now, map[string][]string{"ses": {string(secretA)}})
	good := webhook.Sign(secretA, now, body)

	cases := []struct {
		name     string
		provider string
		header   string
		body     []byte
		want     error
	}{
		{"unknown provider has no secret", "sendgrid", good, body, webhook.ErrNoSecret},
		{"missing header", "ses", "", body, webhook.ErrMissingSignature},
		{"wrong secret", "ses", webhook.Sign(secretB, now, body), body, webhook.ErrBadSignature},
		{"tampered body", "ses", good, []byte(`{"event_id":"e1","event_type":"HARD_BOUNCE"}`), webhook.ErrBadSignature},
		{"stale timestamp", "ses", webhook.Sign(secretA, now.Add(-6*time.Minute), body), body, webhook.ErrStaleTimestamp},
		{"future timestamp", "ses", webhook.Sign(secretA, now.Add(6*time.Minute), body), body, webhook.ErrStaleTimestamp},
		{"not a header", "ses", "garbage", body, webhook.ErrMalformedHeader},
		{"no v1", "ses", "t=1800000000", body, webhook.ErrMalformedHeader},
		{"no t", "ses", "v1=" + strings.Repeat("a", 64), body, webhook.ErrMalformedHeader},
		{"short v1", "ses", "t=1800000000,v1=abcd", body, webhook.ErrMalformedHeader},
		{"non-hex v1", "ses", "t=1800000000,v1=" + strings.Repeat("z", 64), body, webhook.ErrMalformedHeader},
		{"duplicate t", "ses", "t=1800000000,t=1800000001,v1=" + strings.Repeat("a", 64), body, webhook.ErrMalformedHeader},
		{"non-numeric t", "ses", "t=now,v1=" + strings.Repeat("a", 64), body, webhook.ErrMalformedHeader},
	}
	for _, c := range cases {
		if err := v.Verify(c.provider, c.header, c.body); err != c.want {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestVerify_SignatureCannotBeMovedToAnotherTimestamp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := verifierAt(now, map[string][]string{"ses": {string(secretA)}})
	old := webhook.Sign(secretA, now.Add(-time.Hour), body) // genuine but stale
	hexPart := old[strings.Index(old, "v1="):]
	forged := "t=" + "1800000000," + hexPart // fresh timestamp, old signature
	if err := v.Verify("ses", forged, body); err != webhook.ErrBadSignature {
		t.Errorf("a signature must not survive a changed timestamp, got %v", err)
	}
}

func TestVerify_RotationAcceptsOldAndNewSecrets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := verifierAt(now, map[string][]string{"ses": {string(secretA), string(secretB)}})
	for _, s := range [][]byte{secretA, secretB} {
		if err := v.Verify("ses", webhook.Sign(s, now, body), body); err != nil {
			t.Errorf("secret %s refused during rotation: %v", s, err)
		}
	}
	// Two signatures in one header (a sender mid-rotation): either is enough.
	hdr := webhook.Sign(secretB, now, body) + ",v1=" + strings.Repeat("0", 64)
	if err := v.Verify("ses", hdr, body); err != nil {
		t.Errorf("one matching v1 among several should pass: %v", err)
	}
}

func TestVerify_TooShortSecretsAreNotUsable(t *testing.T) {
	v := webhook.NewVerifier(map[string][]string{"ses": {"short"}}, 0)
	if len(v.Providers()) != 0 {
		t.Fatalf("a secret under %d bytes must not enable a provider", webhook.MinSecretLength)
	}
	if err := v.Verify("ses", webhook.Sign([]byte("short"), time.Now(), body), body); err != webhook.ErrNoSecret {
		t.Errorf("got %v, want ErrNoSecret", err)
	}
}

func handlerWith(store *mockWebhookStore, v *webhook.Verifier) *chi.Mux {
	h := webhook.NewHandler(webhook.NewProcessor(store, zap.NewNop()), zap.NewNop()).WithVerifier(v)
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return r
}

func post(r http.Handler, provider string, payload []byte, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/notifications/webhooks/"+provider, bytes.NewReader(payload))
	if header != "" {
		req.Header.Set(webhook.SignatureHeader, header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// NP-26: an unauthenticated or forged callback changes nothing: no delivery event,
// no suppression, nothing in the DLQ.
func TestHandler_ForgedCallbacksChangeNoState(t *testing.T) {
	store := newMockWebhookStore()
	payload := []byte(`{"event_id":"evt-forged","event_type":"HARD_BOUNCE","recipient_email":"victim@example.com","provider_message_id":"<m@x>"}`)
	v := webhook.NewVerifier(map[string][]string{"ses": {string(secretA)}}, 0)
	r := handlerWith(store, v)

	attempts := []struct {
		name, provider, header string
	}{
		{"unsigned", "ses", ""},
		{"wrong secret", "ses", webhook.Sign(secretB, time.Now(), payload)},
		{"stale replay", "ses", webhook.Sign(secretA, time.Now().Add(-time.Hour), payload)},
		{"provider without a secret", "sendgrid", webhook.Sign(secretA, time.Now(), payload)},
		{"malformed", "ses", "t=,v1="},
	}
	for _, a := range attempts {
		w := post(r, a.provider, payload, a.header)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 (%s)", a.name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "expired") {
			t.Errorf("%s: the response must not say which check failed: %s", a.name, w.Body.String())
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) != 0 || len(store.suppressions) != 0 || len(store.dlq) != 0 {
		t.Fatalf("forged callbacks changed state: events=%d suppressions=%d dlq=%d", len(store.events), len(store.suppressions), len(store.dlq))
	}
}

func TestHandler_WithoutAVerifierTheIngressIsClosed(t *testing.T) {
	store := newMockWebhookStore()
	r := handlerWith(store, nil)
	payload := []byte(`{"event_id":"e","event_type":"DELIVERED"}`)
	if w := post(r, "ses", payload, webhook.Sign(secretA, time.Now(), payload)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503: a handler with no verifier must not process anything", w.Code)
	}
}

func TestHandler_OversizedBodyIsRefusedBeforeVerification(t *testing.T) {
	r := handlerWith(newMockWebhookStore(), webhook.NewVerifier(map[string][]string{"ses": {string(secretA)}}, 0))
	big := bytes.Repeat([]byte("a"), 2*1024*1024+1)
	if w := post(r, "ses", big, webhook.Sign(secretA, time.Now(), big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", w.Code)
	}
}
