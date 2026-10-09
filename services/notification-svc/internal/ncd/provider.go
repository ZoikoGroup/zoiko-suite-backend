package ncd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ── provider callbacks (§10.1 POST /provider-events/{binding}, INV-27) ──────

// ProviderEvent is one normalized-input callback event. Provider-specific
// payloads are XIC adapters' business (§10); this is the canonical form a
// certified adapter posts.
type ProviderEvent struct {
	EventID           string    `json:"event_id"`
	EventType         string    `json:"event_type"`
	AttemptToken      string    `json:"attempt_token,omitempty"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
	Detail            string    `json:"detail,omitempty"`
	// Corrects names an earlier provider event this one corrects (NP-59).
	Corrects string `json:"corrects_event_id,omitempty"`
}

// EventResult reports what happened to one event.
type EventResult struct {
	EventID   string `json:"event_id"`
	Status    string `json:"status"` // APPLIED, DUPLICATE, UNMATCHED, INVALID
	AttemptID string `json:"attempt_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// CallbackWindow bounds replay of a signed callback.
const CallbackWindow = 5 * time.Minute

// SecretLookup resolves a binding's callback secret from its environment
// variable name. The secret never lives in the database (INV-26).
var SecretLookup = os.Getenv

// VerifyCallback authenticates a provider callback: HMAC-SHA256 over
// "<timestamp>.<body>" with the binding's secret, inside the replay window.
// A binding with no configured secret accepts nothing — fail closed.
func (s *Service) VerifyCallback(ctx context.Context, bindingID, timestamp, signature string, body []byte) (*Binding, error) {
	b, err := s.verifyCallback(ctx, bindingID, timestamp, signature, body)
	if err != nil {
		// §13.1 callback integrity: invalid signatures, stale timestamps and
		// unknown bindings are counted, not only logged.
		s.metrics.Callback(bindingID, "rejected_"+AsError(err).Code)
	}
	return b, err
}

func (s *Service) verifyCallback(ctx context.Context, bindingID, timestamp, signature string, body []byte) (*Binding, error) {
	bindings, err := s.store.Bindings(ctx, s.now())
	if err != nil {
		return nil, err
	}
	var b *Binding
	for i := range bindings {
		if bindings[i].BindingID == bindingID {
			b = &bindings[i]
		}
	}
	if b == nil {
		return nil, NotFound("binding_not_found", "no provider binding %s", bindingID)
	}
	if !b.SupportsReceipts || b.CallbackSecretEnv == "" {
		return nil, Forbidden("callbacks_not_supported", "binding %s accepts no callbacks", bindingID)
	}
	secret := SecretLookup(b.CallbackSecretEnv)
	if secret == "" {
		return nil, Unavailable("callback_secret_unset", "binding %s has no callback secret configured (%s); callbacks are refused, not trusted",
			bindingID, b.CallbackSecretEnv)
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, Forbidden("invalid_signature", "X-NCD-Timestamp is missing or malformed")
	}
	at := time.Unix(ts, 0)
	if d := s.now().Sub(at); d > CallbackWindow || d < -CallbackWindow {
		return nil, Forbidden("stale_callback", "callback timestamp is outside the %s replay window", CallbackWindow)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimSpace(signature))) {
		return nil, Forbidden("invalid_signature", "callback signature does not verify (NP-26)")
	}
	return b, nil
}

// SignCallback produces the signature VerifyCallback accepts (adapters, tests).
func SignCallback(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// IngestProviderEvents applies authenticated callback events.
func (s *Service) IngestProviderEvents(ctx context.Context, b *Binding, evs []ProviderEvent) []EventResult {
	out := s.ingestProviderEvents(ctx, b, evs)
	for _, r := range out {
		s.metrics.Callback(b.BindingID, strings.ToLower(r.Status))
	}
	return out
}

func (s *Service) ingestProviderEvents(ctx context.Context, b *Binding, evs []ProviderEvent) []EventResult {
	out := make([]EventResult, 0, len(evs))
	for _, ev := range evs {
		r := EventResult{EventID: ev.EventID}
		if ev.EventID == "" || ev.EventType == "" || (ev.AttemptToken == "" && ev.ProviderMessageID == "") {
			r.Status, r.Detail = "INVALID", "event_id, event_type and attempt_token or provider_message_id are required"
			out = append(out, r)
			continue
		}
		ref, err := s.store.FindAttempt(ctx, b.BindingID, ev.AttemptToken, ev.ProviderMessageID)
		if err != nil || ref == nil {
			// §7.5: no invented delivery. The difference stays explicit.
			r.Status, r.Detail = "UNMATCHED", "no attempt of this binding matches; left for reconciliation"
			s.log.Warn("unmatched provider callback", zap.String("binding", b.BindingID), zap.String("event_id", ev.EventID))
			out = append(out, r)
			continue
		}
		r.AttemptID = ref.ID
		raw, _ := json.Marshal(ev)
		err = s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
			att, err := tx.GetAttempt(ref.ID)
			if err != nil {
				return err
			}
			if att.BindingID != b.BindingID {
				return Forbidden("binding_mismatch", "a callback from %s cannot change an attempt made through %s", b.BindingID, att.BindingID)
			}
			norm, ok := NormalizeProviderEvent(att.Channel, ev.EventType)
			if !ok {
				return Invalid("unknown_event_type", "event_type %q is not a normalized provider event", ev.EventType)
			}
			if ev.Detail != "" && (norm.AttemptState == AttemptFailed || norm.AttemptState == AttemptBounced) {
				att.FailureReason = norm.NormalizedState + ": " + ev.Detail
			}
			observed := ev.OccurredAt
			if observed.IsZero() {
				observed = s.now()
			}
			details := map[string]any{"detail": ev.Detail}
			if ev.Corrects != "" {
				details["corrects_provider_event_id"] = ev.Corrects
			}
			return s.applyFact(ctx, tx, s.workerActor(ref.TenantID, ev.EventID), att, norm, factMeta{
				source: "PROVIDER_CALLBACK", actor: "provider:" + b.BindingID, bindingID: b.BindingID,
				providerEventID: ev.EventID, providerMessageID: ev.ProviderMessageID,
				payloadHash: SHA256Hex("callback", string(raw)), observedAt: observed, details: details,
			})
		})
		switch {
		case err == nil:
			r.Status = "APPLIED"
		case errors.Is(err, ErrDuplicateEvent):
			r.Status, r.Detail = "DUPLICATE", "already recorded; no state change (NP-24)"
		default:
			e := AsError(err)
			r.Status, r.Detail = "INVALID", e.Detail
			if e.Kind == KindUnavailable {
				r.Status = "RETRY"
			}
		}
		out = append(out, r)
	}
	if len(out) > 0 {
		s.Kick()
	}
	return out
}

// ── reputation and stream controls (§7.4) ───────────────────────────────────

// Reputation thresholds. OD-15 leaves numeric values open; these are the
// service's recorded defaults.
const (
	reputationMinAttempts = 20
	complaintSpikeRate    = 0.003
	hardBounceSpikeRate   = 0.05
	reputationWindow      = 24 * time.Hour
)

// EvaluateReputation pauses a stream on a complaint or hard-bounce spike.
// CRITICAL is never paused — it only raises an alert — so a reputation
// problem elsewhere cannot silence security mail (NP-53).
func (s *Service) EvaluateReputation(ctx context.Context) error {
	rows, err := s.store.Reputation(ctx, s.now().Add(-reputationWindow))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Attempts < reputationMinAttempts {
			continue
		}
		reason := ""
		switch {
		case r.Stream == "MARKETING" && r.ComplaintRate > complaintSpikeRate:
			reason = "complaint spike: " + strconv.FormatFloat(r.ComplaintRate*100, 'f', 2, 64) + "% of " + strconv.Itoa(r.Attempts) + " attempts in 24h"
		case r.BounceRate > hardBounceSpikeRate:
			reason = "hard-bounce spike: " + strconv.FormatFloat(r.BounceRate*100, 'f', 2, 64) + "% of " + strconv.Itoa(r.Attempts) + " attempts in 24h"
		}
		if reason == "" {
			continue
		}
		row := r
		err := s.store.InTx(ctx, r.TenantID, func(tx Tx) error {
			st, _, err := tx.GetStreamControl(row.Stream)
			if err != nil {
				return err
			}
			if row.Stream != "CRITICAL" && st != "PAUSED" {
				if err := tx.SetStreamControl(row.Stream, "PAUSED", reason, workerPrincipal, true); err != nil {
					return err
				}
			}
			_, err = tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: row.TenantID, Kind: "REPUTATION_ALERT",
				Detail: row.Stream + " via " + row.BindingID + ": " + reason, CreatedAt: s.now()})
			return err
		})
		s.logErr("reputation evaluation failed", err, zap.String("tenant", r.TenantID))
	}
	return nil
}

// ReputationView is GET /v1/reputation.
type ReputationView struct {
	Window  string            `json:"window"`
	Rows    []ReputationRow   `json:"rows"`
	Streams map[string]string `json:"stream_states"`
}

// Reputation returns the caller tenant's deliverability metrics.
func (s *Service) Reputation(ctx context.Context, a Actor) (*ReputationView, error) {
	rows, err := s.store.Reputation(ctx, s.now().Add(-reputationWindow))
	if err != nil {
		return nil, err
	}
	v := &ReputationView{Window: "24h", Rows: []ReputationRow{}, Streams: map[string]string{}}
	for _, r := range rows {
		if r.TenantID == a.TenantID {
			v.Rows = append(v.Rows, r)
		}
	}
	err = s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		for _, st := range []string{"CRITICAL", "TRANSACTIONAL", "OPERATIONAL", "MARKETING"} {
			state, reason, err := tx.GetStreamControl(st)
			if err != nil {
				return err
			}
			if reason != "" {
				state += " (" + reason + ")"
			}
			v.Streams[st] = state
		}
		return nil
	})
	return v, err
}

// SetStream pauses or resumes a sender stream for the tenant (§13.2 runbooks).
func (s *Service) SetStream(ctx context.Context, a Actor, stream, state, reason string) error {
	switch stream {
	case "CRITICAL", "TRANSACTIONAL", "OPERATIONAL", "MARKETING":
	default:
		return Invalid("invalid_stream", "stream must be CRITICAL, TRANSACTIONAL, OPERATIONAL or MARKETING")
	}
	if state != "ACTIVE" && state != "PAUSED" {
		return Invalid("invalid_state", "state must be ACTIVE or PAUSED")
	}
	if strings.TrimSpace(reason) == "" {
		return Invalid("missing_fields", "reason is required")
	}
	return s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		return tx.SetStreamControl(stream, state, reason, a.PrincipalID, false)
	})
}

// ListBindings returns the certified provider routes and their health.
func (s *Service) ListBindings(ctx context.Context) ([]Binding, error) {
	return s.store.Bindings(ctx, s.now())
}

// SetCircuit opens or closes a binding's circuit breaker (§6.5, §13.2).
func (s *Service) SetCircuit(ctx context.Context, a Actor, bindingID string, open bool, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return Invalid("missing_fields", "reason is required")
	}
	state := "HEALTHY"
	if open {
		state = "CIRCUIT_OPEN"
	}
	err := s.store.SetBindingHealth(ctx, bindingID, state, reason, a.PrincipalID)
	if errors.Is(err, ErrNotFound) {
		return NotFound("binding_not_found", "no provider binding %s", bindingID)
	}
	return err
}
