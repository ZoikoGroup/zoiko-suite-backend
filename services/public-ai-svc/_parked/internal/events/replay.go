package events

// Dead-letter replay (Doc 05 CT-042: "Authorization and freshness
// rechecked"). The relay moves an event to "<topic>.dlq" after repeated
// publish failures. Replay sends such events back to their topic only after
// rechecking each one: who authorised the replay, that the envelope is still
// one of ours with only its allowlisted fields (no chat text can ride along),
// and that it is still fresh enough to apply.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/store"
)

// typeTopics maps each event type to its topic and the only data fields it
// may carry (the producers in events.go).
var typeTopics = map[string]struct {
	topic  string
	fields []string
}{
	TypeTurnCompleted:     {TopicTelemetry, []string{"request_id", "intent_id", "status", "latency_bucket", "release_id"}},
	TypeHandoffClicked:    {TopicTelemetry, []string{"route_key", "market_code_if_explicit", "consent_flag"}},
	TypeSecuritySignal:    {TopicTelemetry, []string{"reason_class", "request_id", "severity"}},
	TypeReleasePromoted:   {TopicGovernance, []string{"release_id", "signer_refs", "digest", "effective_at"}},
	TypeSourceRevoked:     {TopicGovernance, []string{"source_version_id", "reason_code", "effective_at"}},
	TypeKillSwitchChanged: {TopicControl, []string{"flag", "actor_ref", "approval_ref", "environment", "control_epoch"}},
}

// DefaultReplayMaxAge is how old a dead-lettered event may be and still be
// replayed. Older events are reported, not applied.
const DefaultReplayMaxAge = 72 * time.Hour

// ReplayAuth names who replays and under which approved change.
type ReplayAuth struct {
	Actor    string
	Approval string
}

// Rejection is an event that was not replayed, and why.
type Rejection struct {
	EventID string
	Reason  string
}

// ReplayReport lists what was sent back and what was refused.
type ReplayReport struct {
	Replayed []string
	Rejected []Rejection
}

// ReplayLedger is what replay rechecks against: the active release
// pointers and the append-only revocation ledger (store.Store has both).
type ReplayLedger interface {
	GetPointer(ctx context.Context, kind string) (string, error)
	Revocations(ctx context.Context, afterWatermark int64) ([]store.Revocation, error)
	Flags(ctx context.Context) ([]store.Flag, error)
}

// Replayer rechecks and re-publishes dead-lettered envelopes.
type Replayer struct {
	Store  ReplayLedger
	Pub    Publisher
	MaxAge time.Duration
	Now    func() time.Time
}

// ErrReplayUnauthorized means the replay has no actor or approval reference.
var ErrReplayUnauthorized = errors.New("dlq replay needs actor and approval references")

// Replay rechecks each raw CloudEvents envelope and publishes the ones that
// pass to their original topic. Nothing is published without authorization.
func (r *Replayer) Replay(ctx context.Context, auth ReplayAuth, envelopes [][]byte) (ReplayReport, error) {
	var rep ReplayReport
	if strings.TrimSpace(auth.Actor) == "" || strings.TrimSpace(auth.Approval) == "" {
		return rep, ErrReplayUnauthorized
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	maxAge := r.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultReplayMaxAge
	}
	var ok []store.OutboxEvent
	for _, raw := range envelopes {
		ev, id, err := r.check(ctx, raw, now(), maxAge)
		if err != nil {
			rep.Rejected = append(rep.Rejected, Rejection{EventID: id, Reason: err.Error()})
			continue
		}
		ok = append(ok, ev)
	}
	if len(ok) == 0 {
		return rep, nil
	}
	if err := r.Pub.Publish(ctx, ok); err != nil {
		return rep, fmt.Errorf("publish: %w", err)
	}
	for _, e := range ok {
		rep.Replayed = append(rep.Replayed, e.EventID)
	}
	return rep, nil
}

// check validates one envelope and returns it as an outbox event for its
// original topic.
func (r *Replayer) check(ctx context.Context, raw []byte, now time.Time, maxAge time.Duration) (store.OutboxEvent, string, error) {
	var env struct {
		SpecVersion string                     `json:"specversion"`
		ID          string                     `json:"id"`
		Source      string                     `json:"source"`
		Type        string                     `json:"type"`
		Time        string                     `json:"time"`
		Data        map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return store.OutboxEvent{}, "", errors.New("not a CloudEvents JSON envelope")
	}
	if env.SpecVersion != "1.0" || env.Source != sourceOf(env.Type) || env.ID == "" {
		return store.OutboxEvent{}, env.ID, errors.New("not an envelope produced by this service")
	}
	spec, known := typeTopics[env.Type]
	if !known {
		return store.OutboxEvent{}, env.ID, fmt.Errorf("event type %q is not allowed", env.Type)
	}
	allowed := map[string]bool{}
	for _, f := range spec.fields {
		allowed[f] = true
	}
	for k := range env.Data {
		if !allowed[k] {
			return store.OutboxEvent{}, env.ID, fmt.Errorf("field %q is not allowed for %s", k, env.Type)
		}
	}
	at, err := time.Parse(time.RFC3339Nano, env.Time)
	if err != nil {
		return store.OutboxEvent{}, env.ID, errors.New("event time is missing or invalid")
	}
	if now.Sub(at) > maxAge {
		return store.OutboxEvent{}, env.ID, fmt.Errorf("stale: older than %s", maxAge)
	}
	if at.After(now.Add(5 * time.Minute)) {
		return store.OutboxEvent{}, env.ID, errors.New("event time is in the future")
	}
	switch env.Type {
	case TypeReleasePromoted:
		// An older release is never re-announced (CT-041): only the release
		// that is still active may be replayed.
		var id string
		_ = json.Unmarshal(env.Data["release_id"], &id)
		cur, err := r.Store.GetPointer(ctx, kindOf(id))
		if err != nil || cur != id {
			return store.OutboxEvent{}, env.ID, errors.New("stale: release is no longer the active one")
		}
	case TypeKillSwitchChanged:
		// Doc 05 §22: a control event older than the flag's current change
		// is stale and never re-announced.
		var name string
		var epoch int64
		_ = json.Unmarshal(env.Data["flag"], &name)
		_ = json.Unmarshal(env.Data["control_epoch"], &epoch)
		flags, err := r.Store.Flags(ctx)
		if err != nil {
			return store.OutboxEvent{}, env.ID, fmt.Errorf("flags unavailable: %w", err)
		}
		for _, f := range flags {
			if f.Name == name && f.UpdatedAt.UTC().UnixNano() > epoch {
				return store.OutboxEvent{}, env.ID, errors.New("stale: the flag changed again after this event")
			}
		}
	case TypeSourceRevoked:
		// Only a revocation that is in the append-only ledger is replayed.
		var target string
		_ = json.Unmarshal(env.Data["source_version_id"], &target)
		revs, err := r.Store.Revocations(ctx, 0)
		if err != nil {
			return store.OutboxEvent{}, env.ID, fmt.Errorf("revocation ledger unavailable: %w", err)
		}
		found := false
		for _, v := range revs {
			found = found || v.TargetID == target
		}
		if !found {
			return store.OutboxEvent{}, env.ID, errors.New("revocation is not in the ledger")
		}
	}
	return store.OutboxEvent{EventID: env.ID, Topic: spec.topic, EventType: env.Type, Payload: raw, CreatedAt: at.UTC()}, env.ID, nil
}

// kindOf is the release pointer a release id belongs to.
func kindOf(releaseID string) string {
	if strings.HasPrefix(releaseID, "claims") {
		return "claims"
	}
	return knowledge.KindKB
}
