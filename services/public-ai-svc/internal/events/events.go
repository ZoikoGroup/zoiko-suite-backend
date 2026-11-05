// Package events builds the six public-ai CloudEvents (Doc 05 §20–§21) and
// relays them from the transactional outbox. Payloads are redacted by
// construction: no field can carry visitor text, prompts, model output or
// contact data.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// Topics (Doc 05 §22): never mixed with financial domains.
const (
	TopicTelemetry  = "publicai.telemetry.v1"
	TopicControl    = "publicai.control.v1"
	TopicGovernance = "kb.governance.v1"
)

// The six event types. No others may be emitted (ADR-005).
const (
	TypeTurnCompleted     = "chat.turn.completed.v1"
	TypeHandoffClicked    = "chat.handoff.clicked.v1"
	TypeReleasePromoted   = "kb.release.promoted.v1"
	TypeSourceRevoked     = "kb.source.revoked.v1"
	TypeKillSwitchChanged = "publicai.kill_switch.changed.v1"
	TypeSecuritySignal    = "chat.security.signal.v1"
)

// Sources per producer (Doc 05 §20): a consumer can tell who emitted an
// event, and replay accepts only an event from the producer of its type.
const (
	sourceOrchestrator = "urn:zoikosuite:publicai:orchestrator"
	sourceRelease      = "urn:zoikosuite:publicai:release"
	sourceControl      = "urn:zoikosuite:publicai:control"
)

// sourceOf is the producer that emits an event type.
func sourceOf(typ string) string {
	switch typ {
	case TypeReleasePromoted, TypeSourceRevoked:
		return sourceRelease
	case TypeKillSwitchChanged:
		return sourceControl
	}
	return sourceOrchestrator
}

// Envelope is CloudEvents 1.0 structured JSON.
type Envelope struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`
	Data            any    `json:"data"`
}

// newID is a random UUIDv4 (RFC 9562), the CloudEvents id consumers dedupe on.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Backoff is the wait before relay attempt n (1-based): doubling from base,
// capped at two minutes.
func Backoff(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < 2*time.Minute; i++ {
		d *= 2
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

func build(topic, typ string, data any, at time.Time) (store.OutboxEvent, error) {
	env := Envelope{SpecVersion: "1.0", ID: newID(), Source: sourceOf(typ), Type: typ,
		Time: at.UTC().Format(time.RFC3339Nano), DataContentType: "application/json", Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return store.OutboxEvent{}, err
	}
	return store.OutboxEvent{EventID: env.ID, Topic: topic, EventType: typ, Payload: b, CreatedAt: at.UTC()}, nil
}

// LatencyBucket maps a duration to the coarse buckets of Doc 05 §21.
func LatencyBucket(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "<2s"
	case d < 5*time.Second:
		return "2s-5s"
	case d < 15*time.Second:
		return "5s-15s"
	default:
		return ">15s"
	}
}

type turnCompleted struct {
	RequestID     string `json:"request_id"`
	IntentID      string `json:"intent_id"`
	Status        string `json:"status"`
	LatencyBucket string `json:"latency_bucket"`
	ReleaseID     string `json:"release_id"`
}

// TurnCompleted never carries raw text and never triggers a CRM write.
func TurnCompleted(requestID, intentID, status string, latency time.Duration, releaseID string, at time.Time) (store.OutboxEvent, error) {
	return build(TopicTelemetry, TypeTurnCompleted, turnCompleted{requestID, intentID, status, LatencyBucket(latency), releaseID}, at)
}

type handoffClicked struct {
	RouteKey             string `json:"route_key"`
	MarketCodeIfExplicit string `json:"market_code_if_explicit,omitempty"`
	ConsentFlag          bool   `json:"consent_flag"`
}

// HandoffClicked records a click on an official route. It never means a
// case, ticket or lead was created (Doc 09 §25).
func HandoffClicked(routeKey, market string, consent bool, at time.Time) (store.OutboxEvent, error) {
	return build(TopicTelemetry, TypeHandoffClicked, handoffClicked{routeKey, market, consent}, at)
}

type releasePromoted struct {
	ReleaseID   string   `json:"release_id"`
	Kind        string   `json:"kind"`
	SignerRefs  []string `json:"signer_refs"`
	Digest      string   `json:"digest"`
	EffectiveAt string   `json:"effective_at"`
	// ReleaseEpoch orders promotions of one kind (Doc 05 §22): consumers
	// ignore an announcement older than the last one they applied.
	ReleaseEpoch int64 `json:"release_epoch"`
}

// ReleasePromotedFor announces that the pointer for kind ("kb" or "claims")
// moved to releaseID.
func ReleasePromotedFor(kind, releaseID, digest string, signers []string, at time.Time) (store.OutboxEvent, error) {
	return build(TopicGovernance, TypeReleasePromoted, releasePromoted{releaseID, kind, signers, digest, at.UTC().Format(time.RFC3339), at.UTC().UnixNano()}, at)
}

// ReleasePromoted is ReleasePromotedFor with the kind inferred from the id
// prefix. Deprecated: callers that know the kind use ReleasePromotedFor.
func ReleasePromoted(releaseID, digest string, signers []string, at time.Time) (store.OutboxEvent, error) {
	return ReleasePromotedFor(kindOf(releaseID), releaseID, digest, signers, at)
}

type sourceRevoked struct {
	SourceVersionID string `json:"source_version_id"`
	ReasonCode      string `json:"reason_code"`
	EffectiveAt     string `json:"effective_at"`
	// RevocationEpoch is the revocation ledger watermark of this entry,
	// filled in by the store in the same transaction (Doc 05 §18/§22).
	RevocationEpoch int64 `json:"revocation_epoch"`
}

// SourceRevoked announces a revocation. Pass the event to the store call
// that appends the ledger entry: it stamps revocation_epoch.
func SourceRevoked(sourceVersionID, reason string, at time.Time) (store.OutboxEvent, error) {
	return build(TopicGovernance, TypeSourceRevoked, sourceRevoked{sourceVersionID, reason, at.UTC().Format(time.RFC3339), 0}, at)
}

type killSwitchChanged struct {
	Flag        string `json:"flag"`
	ActorRef    string `json:"actor_ref"`
	ApprovalRef string `json:"approval_ref"`
	Environment string `json:"environment"`
	// ControlEpoch orders control changes: replay never re-announces an
	// event older than the flag's current change (Doc 05 §22).
	ControlEpoch int64 `json:"control_epoch"`
}

func KillSwitchChanged(flag, actor, approval, environment string, at time.Time) (store.OutboxEvent, error) {
	return build(TopicControl, TypeKillSwitchChanged, killSwitchChanged{flag, actor, approval, environment, at.UTC().UnixNano()}, at)
}

type securitySignal struct {
	ReasonClass string `json:"reason_class"`
	RequestID   string `json:"request_id"`
	Severity    string `json:"severity"`
}

// SecuritySignal carries a reason class only, never the prompt body.
func SecuritySignal(reasonClass, requestID, severity string, at time.Time) (store.OutboxEvent, error) {
	return build(TopicTelemetry, TypeSecuritySignal, securitySignal{reasonClass, requestID, severity}, at)
}
