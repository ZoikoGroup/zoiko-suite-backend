// Package callbacks verifies and applies bank/PSP callbacks (ZS-SVC-M-001 INT-07).
//
// A callback is machine-authenticated, not user-authenticated, so every rule that makes it safe is here:
//
//   - the tenant comes from the callback URL, and the signature is verified with THAT tenant's secret for
//     THAT provider (fetched from the vault for the one call). A callback for tenant A's URL can therefore
//     only ever touch tenant A's payments, and only if it was signed with tenant A's secret;
//   - authenticate first, parse second, and write nothing durable for an unauthenticated request (a failed
//     signature is refused, counted and logged, never stored, so a hostile caller cannot grow a table);
//   - the replay window is enforced for the timestamped scheme, an event is applied at most once, and an
//     older or lower-ranked callback never moves a payment backwards (the store decides, under a row lock);
//   - the provider's own status vocabulary is mapped by a certified profile; a status the profile does not
//     define is refused rather than guessed.
package callbacks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/signature"
	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
	"zoiko.io/payment-status-svc/internal/store"
)

// Provider is one provider's callback configuration.
type Provider struct {
	Callback adapter.Callback `json:"callback"`
	// SecretPath is the vault secret path template; {tenant} and {provider} are substituted.
	SecretPath string `json:"secret_path,omitempty"`
}

// Config is the set of providers this deployment accepts callbacks from, by provider key.
type Config map[string]Provider

var providerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)

// DefaultSecretPath is used when a provider does not set one.
const DefaultSecretPath = "kv/callbacks/{tenant}/{provider}"

// ParseConfig reads and validates the providers (JSON object keyed by provider key).
func ParseConfig(raw string) (Config, error) {
	if strings.TrimSpace(raw) == "" {
		return Config{}, nil
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("provider callbacks: %w", err)
	}
	for k, p := range c {
		if !providerKeyRe.MatchString(k) {
			return nil, fmt.Errorf("provider callbacks: %q is not a valid provider key", k)
		}
		for _, v := range p.Callback.StatusMap {
			if v == adapter.StatusAccepted {
				return nil, fmt.Errorf("provider callbacks: %s: a bank callback cannot map to ACCEPTED; only a booking confirms settlement", k)
			}
		}
		if err := p.Callback.Validate(); err != nil {
			return nil, fmt.Errorf("provider callbacks: %s: %w", k, err)
		}
	}
	return c, nil
}

// SecretSource releases the callback secret for one verification.
type SecretSource interface {
	Secrets(ctx context.Context, tenantID, secretPath string) ([][]byte, error)
}

// Errors from secret retrieval.
var (
	ErrSecretUnavailable = errors.New("callback secret unavailable")
	ErrSecretUnknown     = errors.New("no callback secret is configured for this tenant and provider")
)

// Result is the HTTP-level outcome of a callback.
type Result struct {
	Status  int
	Outcome string // the receipt outcome, or a refusal name
	Body    map[string]any
}

// Counters are exposed for operators; every refusal is counted by reason.
type Counters struct {
	SignatureFailed atomic.Int64
	Stale           atomic.Int64
	Malformed       atomic.Int64
	UnknownProvider atomic.Int64
	Applied         atomic.Int64
	Duplicate       atomic.Int64
}

// Processor handles callbacks.
type Processor struct {
	Config  Config
	Secrets SecretSource
	Store   store.CallbackStore
	Guard   signature.ReplayGuard
	Log     *zap.Logger
	Now     func() time.Time
	Count   Counters
	MaxBody int64
}

func (p *Processor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Handle authenticates, maps and applies one callback. tenantID comes from the URL and is only trusted
// after the signature verifies with that tenant's secret.
func (p *Processor) Handle(ctx context.Context, providerKey, tenantID string, hdr http.Header, body io.Reader) Result {
	prov, ok := p.Config[providerKey]
	if !ok {
		p.Count.UnknownProvider.Add(1)
		return refuse(http.StatusNotFound, "unknown_provider")
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		p.Count.Malformed.Add(1)
		return refuse(http.StatusNotFound, "unknown_provider") // do not reveal which part was wrong
	}
	max := p.MaxBody
	if max <= 0 {
		max = 256 << 10
	}
	raw, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil || int64(len(raw)) > max {
		p.Count.Malformed.Add(1)
		return refuse(http.StatusRequestEntityTooLarge, "body_too_large")
	}

	path := prov.SecretPath
	if path == "" {
		path = DefaultSecretPath
	}
	path = strings.NewReplacer("{tenant}", tenantID, "{provider}", providerKey).Replace(path)
	secrets, err := p.Secrets.Secrets(ctx, tenantID, path)
	switch {
	case errors.Is(err, ErrSecretUnknown):
		// no secret means nothing could have been signed with it
		p.Count.SignatureFailed.Add(1)
		return refuse(http.StatusUnauthorized, "signature_invalid")
	case err != nil:
		p.Log.Error("callback: secret retrieval failed", zap.String("provider", providerKey), zap.Error(err))
		return refuse(http.StatusServiceUnavailable, "secret_unavailable") // the provider will retry
	}
	defer func() {
		for _, s := range secrets {
			for i := range s {
				s[i] = 0
			}
		}
	}()

	sigHeader := prov.Callback.Header
	if sigHeader == "" {
		if prov.Callback.Scheme == "zoiko-v1" {
			sigHeader = "X-Zoiko-Signature"
		} else {
			sigHeader = "X-Webhook-Signature"
		}
	}
	h := adapter.CallbackHeaders{Signature: hdr.Get(sigHeader), EventID: hdr.Get("X-Zoiko-Event-Id")}
	ev, err := prov.Callback.VerifyCallback(h, raw, secrets, p.now())
	switch {
	case err == nil:
	case errors.Is(err, adapter.ErrCallbackSignature):
		p.Count.SignatureFailed.Add(1)
		p.Log.Warn("callback refused: signature verification failed", zap.String("provider", providerKey))
		return refuse(http.StatusUnauthorized, "signature_invalid")
	case errors.Is(err, adapter.ErrCallbackStale):
		p.Count.Stale.Add(1)
		p.Log.Warn("callback refused: outside the replay window", zap.String("provider", providerKey))
		return refuse(http.StatusUnauthorized, "outside_replay_window")
	case errors.Is(err, adapter.ErrCallbackStatus):
		// authenticated, but a status the profile does not define: nothing is guessed
		p.Count.Malformed.Add(1)
		return refuse(http.StatusUnprocessableEntity, "status_not_mapped")
	default:
		p.Count.Malformed.Add(1)
		return refuse(http.StatusBadRequest, "malformed_callback")
	}

	// the shared scheme also carries a replay guard: the same signed message is accepted once inside the window
	if prov.Callback.Scheme == "zoiko-v1" && p.Guard != nil {
		parsed, _ := signature.Parse(h.Signature)
		first, gerr := p.Guard.FirstSeen(ctx, signature.ReplayKey(tenantID+"|"+providerKey, h.EventID, parsed),
			p.now().Add(2*time.Duration(prov.Callback.ToleranceSeconds)*time.Second))
		if gerr == nil && !first {
			p.Count.Duplicate.Add(1)
			return Result{Status: http.StatusOK, Outcome: store.OutcomeDuplicate, Body: map[string]any{"outcome": store.OutcomeDuplicate, "applied": false}}
		}
	}

	mapped, status := mapStatus(ev.Status)
	sum := sha256.Sum256(raw)
	res, err := p.Store.ApplyProviderCallback(middleware.WithTenant(ctx, tenantID), store.CallbackInput{
		Provider: providerKey, Scheme: prov.Callback.Scheme, EventRef: ev.EventRef, PaymentRef: ev.PaymentRef, RawStatus: ev.RawStatus,
		Mapped: status, EventType: mapped, EventTime: ev.EventTime, BodyHash: hex.EncodeToString(sum[:]),
		MappingVersion: providerKey + ":" + adapterHash(prov.Callback), CorrelationID: hdr.Get("X-Correlation-Id"),
	})
	if err != nil {
		p.Log.Error("callback: store unavailable", zap.String("provider", providerKey), zap.Error(err))
		return refuse(http.StatusServiceUnavailable, "store_unavailable") // the provider will retry
	}
	switch res.Outcome {
	case store.OutcomeApplied:
		p.Count.Applied.Add(1)
	case store.OutcomeDuplicate:
		p.Count.Duplicate.Add(1)
	}
	out := map[string]any{"outcome": res.Outcome, "applied": res.Outcome == store.OutcomeApplied}
	if res.Detail != "" {
		out["detail"] = res.Detail
	}
	// 200 for every decided outcome, including "ignored": a provider that gets a non-2xx retries, and
	// retrying a decided event can only produce another receipt, never a different effect.
	// An unknown payment is 202: not decided, so the provider keeps retrying until we know it.
	code := http.StatusOK
	if res.Outcome == store.OutcomeUnknownPayment || res.Outcome == store.OutcomeAmbiguousPayment {
		code = http.StatusAccepted
	}
	return Result{Status: code, Outcome: res.Outcome, Body: out}
}

func refuse(code int, why string) Result {
	return Result{Status: code, Outcome: why, Body: map[string]any{"error": why}}
}

// mapStatus turns a profile class into a payment status and the legacy event type for it.
func mapStatus(class string) (eventType string, s domain.ExecutionStatus) {
	switch class {
	case adapter.StatusAcknowledged:
		return domain.EventPaymentAccepted, domain.StatusAccepted
	case adapter.StatusPending:
		return domain.EventPaymentPending, domain.StatusPending
	case "SETTLED":
		return domain.EventPaymentSettled, domain.StatusSettled
	default:
		return domain.EventPaymentRejected, domain.StatusRejected
	}
}

func adapterHash(c adapter.Callback) string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// ── vault ───────────────────────────────────────────────────────────────────

// VaultSecrets fetches callback secrets through the vault's narrow use operation. The secret is held for the
// one verification and zeroed after.
type VaultSecrets struct {
	Base      string
	Principal string
	HTTP      *http.Client
}

func NewVaultSecrets(base, principal string) *VaultSecrets {
	return &VaultSecrets{Base: strings.TrimRight(base, "/"), Principal: principal, HTTP: &http.Client{Timeout: 4 * time.Second}}
}

func (v *VaultSecrets) Secrets(ctx context.Context, tenantID, secretPath string) ([][]byte, error) {
	raw, _ := json.Marshal(map[string]string{
		"secret_path": secretPath, "requested_by_principal_id": v.Principal, "request_id": uuid.NewString(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.Base+"/v1/secrets/use", bytes.NewReader(raw))
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	rid := uuid.NewString()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", v.Principal)
	req.Header.Set("X-Request-Id", rid)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "provider-callback-verification")
	req.Header.Set("Idempotency-Key", rid)
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return nil, ErrSecretUnknown
	default:
		return nil, ErrSecretUnavailable
	}
	var out struct {
		CredentialBase64 string `json:"credential_base64"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return nil, ErrSecretUnavailable
	}
	b, err := base64.StdEncoding.DecodeString(out.CredentialBase64)
	if err != nil || len(b) == 0 {
		return nil, ErrSecretUnavailable
	}
	return [][]byte{b}, nil
}
