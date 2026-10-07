// Package events builds and publishes this service domain events.
//
// The package is split in two on purpose, and the split is the whole design:
//
//   - Build/ConfigUpdated/FeatureFlagUpdated turn a state change into a sealed
//     envelope. The store calls these INSIDE the transaction that records the
//     change and enqueues the result in event_outbox, so the fact that
//     something changed is committed atomically with the change itself.
//   - Publish hands already-built envelopes to Kafka. internal/outbox calls it
//     from a background relay, where a failure is a retry rather than a loss.
//
// Before the outbox existed these were one act: a Kafka write from the handler
// AFTER the commit, with the error logged and discarded. A broker hiccup during
// a config write therefore left the new version correctly recorded, the
// operator correctly told it was saved, and every consumer still holding the
// PREVIOUS value — with nothing anywhere reporting a fault. On this service
// that is the worst possible failure shape: the stale value a consumer keeps
// serving is perfectly valid, just superseded, so nothing downstream can
// detect it either.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// Contract constants, asserted against asyncapi.yaml by scripts/audit.sh.
const (
	EventVersion  = "1.0"
	SchemaVersion = "1.0"
	SourceService = "configuration-feature-flag-svc"

	// TypeConfigUpdated is emitted only when POST /v1/config performs a real
	// transition — a first value for the scope, or a genuinely different one.
	// Never on the idempotent no-op path: re-asserting a value that is already
	// in force is not a new fact, and a consumer that invalidated its cache on
	// one would be doing so for nothing.
	TypeConfigUpdated = "config.updated"
	// TypeFeatureFlagUpdated is the same rule for POST /v1/flags.
	TypeFeatureFlagUpdated = "feature_flag.updated"

	// The eight ZS-SVC-AA-001 events. Each is enqueued by the store, inside
	// the transaction that produced the fact it announces, and admitted by the
	// widened event_outbox CHECK in migration 000008 — see that migration's
	// comment for the "keep in step with internal/events and asyncapi.yaml"
	// rule.
	TypeSnapshotPublished   = "config.snapshot.published"
	TypeVersionPublished    = "config.version.published"
	TypeOverrideActivated   = "config.override.activated"
	TypeReleaseActivated    = "flag.release.activated"
	TypeKillSwitchActivated = "flag.kill_switch.activated"
	TypeChangeVerified      = "config.change.verified"
	TypeDriftDetected       = "config.drift.detected"
	TypeEmergencyExpired    = "config.emergency.expired"
)

// envelope is this platform event contract (Doc 03 §19): every published event
// carries event name, event version, timestamp, tenant ID, actor ID,
// correlation ID, source service, and payload schema version.
//
// domain.ConfigEntry and domain.FeatureFlag are independently-nullable-scoped
// like kill-switch-registry-svc — TenantID is nil for a global default, so
// tenant_id is correctly OMITTED in that case rather than fabricated. Neither
// struct has a legal_entity_id or jurisdiction field: config and feature flags
// are environment/tenant-scoped, not legal-entity-scoped.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Outbound is one sealed envelope on its way to the outbox.
//
// Key becomes the Kafka partition key. It is the aggregate id — config_id or
// flag_id — not the correlation id, which is what it used to be: keying on the
// correlation id put two changes to the SAME config key on different
// partitions whenever they arrived on different requests, so a consumer
// replaying them could apply an older value after a newer one and end up
// serving the superseded value permanently.
type Outbound struct {
	EventType string
	Key       string
	Body      []byte
}

// Build marshals one envelope.
func Build(eventType, correlationID, tenantID, actorID, key string, payload map[string]any) (Outbound, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	body, err := json.Marshal(envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md event_id collision writeup.
		EventID:       "evt-" + uuid.NewString(),
		EventType:     eventType,
		EventVersion:  EventVersion,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: SchemaVersion,
		SourceService: SourceService,
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	})
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s envelope: %w", eventType, err)
	}
	return Outbound{EventType: eventType, Key: key, Body: body}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ConfigUpdated builds config.updated for a config entry that just underwent a
// real transition.
//
// scope_is_global is in the payload rather than left for a consumer to infer
// from a null tenant_id. The two facts read very differently — "this value
// changed for one organisation" and "the default every organisation without
// its own value reads has changed" — and a consumer that treats a missing
// tenant_id as "unknown tenant" rather than "all tenants" silently ignores the
// wider of the two.
func ConfigUpdated(entry domain.ConfigEntry, correlationID string) (Outbound, error) {
	return Build(TypeConfigUpdated, correlationID, deref(entry.TenantID), entry.CreatedByPrincipalID, entry.ConfigID, map[string]any{
		"config_id":               entry.ConfigID,
		"key":                     entry.Key,
		"value":                   entry.Value,
		"environment":             entry.Environment,
		"tenant_id":               entry.TenantID,
		"scope_is_global":         entry.TenantID == nil,
		"effective_from":          entry.EffectiveFrom,
		"created_by_principal_id": entry.CreatedByPrincipalID,
	})
}

// FeatureFlagUpdated builds feature_flag.updated. Same not-on-no-op rule as
// ConfigUpdated.
func FeatureFlagUpdated(flag domain.FeatureFlag, correlationID string) (Outbound, error) {
	return Build(TypeFeatureFlagUpdated, correlationID, deref(flag.TenantID), flag.CreatedByPrincipalID, flag.FlagID, map[string]any{
		"flag_id":                 flag.FlagID,
		"key":                     flag.Key,
		"enabled":                 flag.Enabled,
		"environment":             flag.Environment,
		"tenant_id":               flag.TenantID,
		"scope_is_global":         flag.TenantID == nil,
		"rollout_percentage":      flag.RolloutPercentage,
		"effective_from":          flag.EffectiveFrom,
		"created_by_principal_id": flag.CreatedByPrincipalID,
	})
}

// ── ZS-SVC-AA-001 event builders ─────────────────────────────────────────────
//
// The same discipline as ConfigUpdated/FeatureFlagUpdated applies to all eight:
// built inside the store's transaction, enqueued to event_outbox atomically
// with the fact they announce, never published from the handler. A consumer
// that misses one is acting on a superseded snapshot/version/plan/switch —
// the exact staleness the AA-001 surface exists to make visible.

// SnapshotPublished builds config.snapshot.published for a mint: a new
// environment imprints was published and every consumer may now pin to it
// (INV-12). Partition key is the snapshot id, so all events about one imprint
// arrive in order.
func SnapshotPublished(snap domain.MintedSnapshot, actor, correlationID string) (Outbound, error) {
	return Build(TypeSnapshotPublished, correlationID, "", actor, snap.SnapshotID, map[string]any{
		"snapshot_id":             snap.SnapshotID,
		"environment":             snap.Environment,
		"epoch":                   snap.Epoch,
		"digest":                  snap.Digest,
		"issued_at":               snap.IssuedAt,
		"freshness_deadline":      snap.FreshnessDeadline,
		"created_by_principal_id": actor,
	})
}

// VersionPublished builds config.version.published for a definition publish:
// a new immutable ConfigDefinitionVersion is what every resolver reads from
// now on (INV-04). Partition key is the version id.
func VersionPublished(v domain.ConfigDefinitionVersion, key, actor, correlationID string) (Outbound, error) {
	return Build(TypeVersionPublished, correlationID, "", actor, v.VersionID, map[string]any{
		"version_id":                v.VersionID,
		"definition_id":             v.DefinitionID,
		"key":                       key,
		"version":                   v.Version,
		"digest":                    v.Digest,
		"lifecycle":                 v.Lifecycle,
		"published_at":              v.PublishedAt,
		"published_by_principal_id": actor,
	})
}

// OverrideActivated builds config.override.activated for a value set at one of
// the five INV-07 precedence layers. Partition key is the config key, keeping
// every override to one key ordered.
func OverrideActivated(p domain.ActivateOverrideParams, effectiveFrom time.Time) (Outbound, error) {
	scopeID := ""
	if p.ScopeID != nil {
		scopeID = *p.ScopeID
	}
	return Build(TypeOverrideActivated, p.CorrelationID, scopeID, p.ActorPrincipalID, p.Key, map[string]any{
		"key":                p.Key,
		"layer":              p.Layer,
		"environment":        p.Environment,
		"scope_id":           p.ScopeID,
		"value":              p.Value,
		"effective_from":     effectiveFrom,
		"actor_principal_id": p.ActorPrincipalID,
	})
}

// ReleaseActivated builds flag.release.activated for a published release plan
// version. Partition key is the plan id; the immutable targeting rules stay in
// the payload so a consumer can fingerprint the exact ruleset from the event
// alone (targeting_hash, TC-05).
func ReleaseActivated(p domain.ReleasePlan, correlationID string) (Outbound, error) {
	return Build(TypeReleaseActivated, correlationID, deref(p.TenantID), p.PublishedByPrincipalID, p.ReleasePlanID, map[string]any{
		"release_plan_id":           p.ReleasePlanID,
		"flag_key":                  p.FlagKey,
		"environment":               p.Environment,
		"tenant_id":                 p.TenantID,
		"version":                   p.Version,
		"strategy":                  p.Strategy,
		"salt":                      p.Salt,
		"bucket_count":              p.BucketCount,
		"targeting_hash":            p.TargetingHash,
		"targeting_rules":           p.TargetingRules,
		"published_at":              p.PublishedAt,
		"published_by_principal_id": p.PublishedByPrincipalID,
	})
}

// KillSwitchActivated builds flag.kill_switch.activated for a switch entering
// force — the single highest-blast-radius flag event this service emits, so it
// carries the reason and incident alongside the switch itself. Partition key
// is the switch id.
func KillSwitchActivated(k domain.KillSwitch, correlationID string) (Outbound, error) {
	return Build(TypeKillSwitchActivated, correlationID, deref(k.TenantID), k.CreatedByPrincipalID, k.KillSwitchID, map[string]any{
		"kill_switch_id":          k.KillSwitchID,
		"flag_key":                k.FlagKey,
		"environment":             k.Environment,
		"tenant_id":               k.TenantID,
		"safe_behavior":           k.SafeBehavior,
		"reason":                  k.Reason,
		"incident_id":             k.IncidentID,
		"expires_at":              k.ExpiresAt,
		"created_by_principal_id": k.CreatedByPrincipalID,
	})
}

// ChangeVerified builds config.change.verified once a change set reaches
// VERIFIED, the terminal success of the Table 7 lifecycle. Partition key is
// the change id, keeping every event about one change ordered.
func ChangeVerified(c domain.ConfigChange, actor, correlationID string) (Outbound, error) {
	return Build(TypeChangeVerified, correlationID, deref(c.TenantID), actor, c.ChangeID, map[string]any{
		"change_id":                c.ChangeID,
		"change_class":             c.ChangeClass,
		"environment":              c.Environment,
		"tenant_id":                c.TenantID,
		"status":                   c.Status,
		"verified_at":              c.VerifiedAt,
		"verified_by_principal_id": actor,
	})
}

// DriftDetected builds config.drift.detected for a recorded desired/observed
// disagreement (TC-08). Partition key is the drift id.
func DriftDetected(d domain.DriftEvent, correlationID string) (Outbound, error) {
	return Build(TypeDriftDetected, correlationID, deref(d.TenantID), "system:reconciliation", d.DriftID, map[string]any{
		"drift_id":             d.DriftID,
		"runtime_id":           d.RuntimeID,
		"environment":          d.Environment,
		"tenant_id":            d.TenantID,
		"drift_class":          d.DriftClass,
		"severity":             d.Severity,
		"desired_snapshot_id":  d.DesiredSnapshotID,
		"desired_digest":       d.DesiredDigest,
		"observed_snapshot_id": d.ObservedSnapshotID,
		"observed_digest":      d.ObservedDigest,
		"detected_at":          d.DetectedAt,
	})
}

// EmergencyExpired builds config.emergency.expired when the sweep flips a
// break-glass change to EXPIRED and reverts it. Partition key is the emergency
// change id.
func EmergencyExpired(e domain.EmergencyChange, correlationID string) (Outbound, error) {
	return Build(TypeEmergencyExpired, correlationID, deref(e.TenantID), "system:ops_sweep", e.EmergencyChangeID, map[string]any{
		"emergency_change_id": e.EmergencyChangeID,
		"key":                 e.Key,
		"environment":         e.Environment,
		"tenant_id":           e.TenantID,
		"status":              e.Status,
		"reverted_to_prior":   e.RevertedToPrior,
		"expires_at":          e.ExpiresAt,
		"actor_principal_id":  e.ActorPrincipalID,
	})
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so tests can assert envelope content without
// a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes sealed envelopes to the Kafka event backbone.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured, in
	// which case Publish drops the batch and says so. main.go refuses to start
	// with a nil producer in production or staging.
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic and writer.
// A nil producer makes every publish a logged no-op — see the struct comment.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer — storing that directly into the
	// MessageWriter interface field would make the interface itself non-nil,
	// defeating the p.producer == nil check in Publish.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes a whole batch in one call.
//
// One call, not a loop over WriteMessages. kafka-go batches internally and its
// BatchTimeout is what ends a partial batch, so a per-message loop waits that
// timeout out once per event — which turns a backlog of 200 into 200 sequential
// waits and makes the relay slower the further behind it gets.
func (p *Publisher) Publish(ctx context.Context, msgs []kafka.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if p.producer == nil {
		p.log.Info("simulating publish — no Kafka brokers configured", zap.Int("events", len(msgs)))
		return nil
	}
	// Topic is set on the Writer, not the Message — kafka-go rejects a Message
	// carrying a Topic when the Writer already has one.
	if err := p.producer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write to %s: %w", p.topic, err)
	}
	p.log.Debug("events published", zap.String("topic", p.topic), zap.Int("events", len(msgs)))
	return nil
}
