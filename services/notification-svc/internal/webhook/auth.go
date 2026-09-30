package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// Provider webhooks were accepted from anyone. HandleWebhook read the body,
// normalized it and — for a bounce or complaint — wrote an email suppression,
// with no signature, no shared secret and no source check, on a route the
// envelope middleware exempts. A single forged POST could suppress any address
// in any tenant, silencing its mail, or mark a message delivered that never
// was. ZS-SVC-Y-001 INV-27: "Provider callbacks cannot mutate state without
// authentication, deduplication and valid state-transition checks."
//
// Each request must now carry X-Webhook-Timestamp (unix seconds) and
// X-Webhook-Signature ("sha256=" + hex HMAC-SHA256 over "<timestamp>.<body>")
// made with the provider's secret: NOTIFICATION_WEBHOOK_SECRET_<PROVIDER>, or
// NOTIFICATION_WEBHOOK_SECRET for every provider. With no secret configured
// the route refuses everything — failing open is the defect being fixed.

// ReplayWindow bounds how old a signed webhook may be.
const ReplayWindow = 5 * time.Minute

var (
	errSecretUnset = errors.New("no webhook secret is configured for this provider; webhooks are refused, not trusted")
	errBadSig      = errors.New("webhook signature missing or invalid")
	errStale       = errors.New("webhook timestamp outside the replay window")
)

// EnvSecret resolves a provider's webhook secret from the environment.
func EnvSecret(provider string) string {
	key := "NOTIFICATION_WEBHOOK_SECRET_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(provider))
	if v := os.Getenv(key); v != "" {
		return v
	}
	return os.Getenv("NOTIFICATION_WEBHOOK_SECRET")
}

// Sign produces the signature verify accepts.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func verify(secret, timestamp, signature string, body []byte, now time.Time) error {
	if secret == "" {
		return errSecretUnset
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errBadSig
	}
	if d := now.Sub(time.Unix(ts, 0)); d > ReplayWindow || d < -ReplayWindow {
		return errStale
	}
	if !hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(strings.TrimSpace(signature))) {
		return errBadSig
	}
	return nil
}
