package authority

import (
	"context"
	"crypto/sha256"
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
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/eventing/signature"
)

// Callback outcomes (tax_authority_callbacks.outcome).
const (
	OutcomeApplied           = "APPLIED"
	OutcomeDuplicate         = "DUPLICATE"
	OutcomeStaleIgnored      = "STALE_IGNORED"
	OutcomeRegressionBlocked = "REGRESSION_BLOCKED"
	OutcomeUnknownFiling     = "UNKNOWN_SUBMISSION"
	OutcomeAmbiguousFiling   = "AMBIGUOUS_SUBMISSION"
)

// CallbackInput is one AUTHENTICATED authority callback.
type CallbackInput struct {
	Provider      string
	Scheme        string
	EventRef      string
	SubmissionRef string
	RawStatus     string
	Mapped        string // PENDING, ACKNOWLEDGED, ACCEPTED or REJECTED
	EventTime     *time.Time
	BodyHash      string
	CorrelationID string
}

// CallbackResult says what was done.
type CallbackResult struct {
	Outcome string
	Detail  string
	Filing  *Filing
}

// CallbackReceipt is a row of tax_authority_callbacks.
type CallbackReceipt struct {
	ReceiptID      string     `json:"receipt_id"`
	Provider       string     `json:"provider_key"`
	EventRef       string     `json:"event_ref"`
	SubmissionRef  string     `json:"submission_ref"`
	SubmissionID   *string    `json:"submission_id,omitempty"`
	Scheme         string     `json:"scheme"`
	ReportedStatus string     `json:"reported_status"`
	MappedStatus   string     `json:"mapped_status"`
	EventTime      *time.Time `json:"event_time,omitempty"`
	BodyHash       string     `json:"body_hash"`
	Outcome        string     `json:"outcome"`
	Detail         string     `json:"detail,omitempty"`
	ReceivedAt     time.Time  `json:"received_at"`
}

// CallbackStore is what the callback processor needs.
type CallbackStore interface {
	ApplyCallback(ctx context.Context, in CallbackInput) (*CallbackResult, error)
	ListCallbacks(ctx context.Context, submissionID string, limit int) ([]CallbackReceipt, error)
}

// CallbackProvider is one authority's callback configuration.
type CallbackProvider struct {
	Callback adapter.Callback `json:"callback"`
	// SecretPath is the vault secret path template; {tenant} and {provider} are substituted.
	SecretPath string `json:"secret_path,omitempty"`
}

// CallbackConfig is the set of authorities this deployment accepts callbacks from, by provider key.
type CallbackConfig map[string]CallbackProvider

// DefaultSecretPath is used when a provider does not set one.
const DefaultSecretPath = "kv/callbacks/{tenant}/{provider}"

var providerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)

// ParseCallbackConfig reads and validates the providers (JSON object keyed by provider key).
func ParseCallbackConfig(raw string) (CallbackConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return CallbackConfig{}, nil
	}
	var c CallbackConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("authority callbacks: %w", err)
	}
	for k, p := range c {
		if !providerKeyRe.MatchString(k) {
			return nil, fmt.Errorf("authority callbacks: %q is not a valid provider key", k)
		}
		if err := p.Callback.Validate(); err != nil {
			return nil, fmt.Errorf("authority callbacks: %s: %w", k, err)
		}
		for _, v := range p.Callback.StatusMap {
			if v == "SETTLED" {
				return nil, fmt.Errorf("authority callbacks: %s: a tax callback cannot map to SETTLED", k)
			}
		}
	}
	return c, nil
}

// SecretSource releases the callback secret for one verification.
type SecretSource interface {
	Secrets(ctx context.Context, tenantID, secretPath string) ([][]byte, error)
}

var (
	ErrSecretUnavailable = ext.ErrSecretUnavailable
	ErrSecretUnknown     = ext.ErrSecretUnknown
)

// CallbackResponse is the HTTP-level outcome of a callback.
type CallbackResponse struct {
	Status  int
	Outcome string
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

// CallbackProcessor verifies and applies authority callbacks. The tenant comes from the callback URL and is
// only trusted after the signature verifies with THAT tenant's secret for THAT provider; unauthenticated input
// writes nothing durable; an event is applied at most once; a callback never moves a filing backwards.
type CallbackProcessor struct {
	Config  CallbackConfig
	Secrets SecretSource
	Store   CallbackStore
	Guard   signature.ReplayGuard
	Log     *zap.Logger
	Now     func() time.Time
	Count   Counters
	MaxBody int64
	// TenantCtx returns a context scoped to a tenant.
	TenantCtx func(context.Context, string) context.Context
}

func (p *CallbackProcessor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func refuse(code int, why string) CallbackResponse {
	return CallbackResponse{Status: code, Outcome: why, Body: map[string]any{"error": why}}
}

// Handle authenticates, maps and applies one callback.
func (p *CallbackProcessor) Handle(ctx context.Context, providerKey, tenantID string, hdr http.Header, body io.Reader) CallbackResponse {
	prov, ok := p.Config[providerKey]
	if !ok {
		p.Count.UnknownProvider.Add(1)
		return refuse(http.StatusNotFound, "unknown_provider")
	}
	if tenantID == "" || len(tenantID) > 64 || strings.ContainsAny(tenantID, "/\\ ") {
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
		p.Count.SignatureFailed.Add(1)
		return refuse(http.StatusUnauthorized, "signature_invalid")
	case err != nil:
		p.Log.Error("callback: secret retrieval failed", zap.String("provider", providerKey), zap.Error(err))
		return refuse(http.StatusServiceUnavailable, "secret_unavailable")
	}
	defer func() {
		for _, s := range secrets {
			zero(s)
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
		p.Count.Malformed.Add(1)
		return refuse(http.StatusUnprocessableEntity, "status_not_mapped")
	default:
		p.Count.Malformed.Add(1)
		return refuse(http.StatusBadRequest, "malformed_callback")
	}

	if prov.Callback.Scheme == "zoiko-v1" && p.Guard != nil {
		parsed, _ := signature.Parse(h.Signature)
		first, gerr := p.Guard.FirstSeen(ctx, signature.ReplayKey(tenantID+"|"+providerKey, h.EventID, parsed),
			p.now().Add(2*time.Duration(prov.Callback.ToleranceSeconds)*time.Second))
		if gerr == nil && !first {
			p.Count.Duplicate.Add(1)
			return CallbackResponse{Status: http.StatusOK, Outcome: OutcomeDuplicate, Body: map[string]any{"outcome": OutcomeDuplicate, "applied": false}}
		}
	}

	sum := sha256.Sum256(raw)
	sctx := ctx
	if p.TenantCtx != nil {
		sctx = p.TenantCtx(ctx, tenantID)
	}
	res, err := p.Store.ApplyCallback(sctx, CallbackInput{
		Provider: providerKey, Scheme: prov.Callback.Scheme, EventRef: ev.EventRef, SubmissionRef: ev.PaymentRef, RawStatus: ev.RawStatus,
		Mapped: ev.Status, EventTime: ev.EventTime, BodyHash: hex.EncodeToString(sum[:]), CorrelationID: hdr.Get("X-Correlation-Id"),
	})
	if err != nil {
		p.Log.Error("callback: store unavailable", zap.String("provider", providerKey), zap.Error(err))
		return refuse(http.StatusServiceUnavailable, "store_unavailable")
	}
	switch res.Outcome {
	case OutcomeApplied:
		p.Count.Applied.Add(1)
	case OutcomeDuplicate:
		p.Count.Duplicate.Add(1)
	}
	out := map[string]any{"outcome": res.Outcome, "applied": res.Outcome == OutcomeApplied}
	if res.Detail != "" {
		out["detail"] = res.Detail
	}
	// 200 for every decided outcome (a provider that gets a non-2xx retries, and retrying a decided event can
	// only produce another receipt); 202 for a filing we do not know yet, so the authority keeps retrying.
	code := http.StatusOK
	if res.Outcome == OutcomeUnknownFiling || res.Outcome == OutcomeAmbiguousFiling {
		code = http.StatusAccepted
	}
	return CallbackResponse{Status: code, Outcome: res.Outcome, Body: out}
}

var _ = uuid.NewString
