package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// SchemaGate is the producer-side contract gate (INT-CTRL-005, INT-NP-006): it
// checks an outgoing event's data against its registered contract before the
// event leaves the outbox, so an incompatible payload is quarantined before
// any consumer sees it.
//
// It satisfies outbox.Gate structurally (this package does not import outbox).
//
// Failure policy, deliberately asymmetric:
//   - payload does not satisfy the contract            -> INT-SCHEMA-004 (quarantine)
//   - no contract registered, AllowUnregistered false   -> INT-SCHEMA-004 (quarantine)
//   - registry cannot answer                            -> ErrRegistryUnavailable
//     (the relay holds the event without spending its retry budget; an event is
//     never published unchecked because the registry was down)
type SchemaGate struct {
	Registry Registry
	// AllowUnregistered lets events with no registered contract through. It
	// exists to make a rollout possible, not as a destination: while true, a
	// producer with no contract is ungoverned.
	AllowUnregistered bool
	// Resolve maps an event to its registry name. Nil uses EventName.
	Resolve func(eventType string, rendered []byte) string
	// TTL bounds how long a fetched contract is reused. Zero means 60s.
	TTL time.Duration
	// Now is for tests.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	schema  *Schema
	missing bool
	at      time.Time
}

// maxCacheEntries bounds the cache so a stream of unique event names cannot
// grow it without limit.
const maxCacheEntries = 1024

// Check implements outbox.Gate.
func (g *SchemaGate) Check(ctx context.Context, eventType string, rendered []byte) error {
	if g.Registry == nil {
		return errors.New("schema gate has no registry")
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = EventName
	}
	name := resolve(eventType, rendered)
	if name == "" {
		return New(CodeSchemaMismatch, "event has no contract name")
	}
	s, missing, err := g.lookup(ctx, name)
	if err != nil {
		return err
	}
	if missing {
		if g.AllowUnregistered {
			return nil
		}
		return Newf(CodeSchemaMismatch, "no registered contract for %s", name)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rendered, &env); err != nil || len(env.Data) == 0 {
		return New(CodeSchemaMismatch, "event has no data to validate")
	}
	if v := ValidatePayload(s.JSONSchema, env.Data); len(v) > 0 {
		return Newf(CodeSchemaMismatch, "event data does not satisfy %s v%d: %s", s.EventName, s.Version, strings.Join(v, "; "))
	}
	return nil
}

func (g *SchemaGate) lookup(ctx context.Context, name string) (*Schema, bool, error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	ttl := g.TTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	g.mu.Lock()
	if c, ok := g.cache[name]; ok && now().Sub(c.at) < ttl {
		g.mu.Unlock()
		return c.schema, c.missing, nil
	}
	g.mu.Unlock()

	s, err := g.Registry.Schema(ctx, name, 0)
	switch {
	case err == nil:
		g.store(name, cached{schema: s, at: now()})
		return s, false, nil
	case errors.Is(err, ErrNoContract):
		g.store(name, cached{missing: true, at: now()})
		return nil, true, nil
	default:
		// Never cached: an outage must not outlive the outage.
		return nil, false, err
	}
}

func (g *SchemaGate) store(name string, c cached) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cache == nil {
		g.cache = make(map[string]cached)
	}
	if len(g.cache) >= maxCacheEntries {
		g.cache = make(map[string]cached)
	}
	g.cache[name] = c
}

// EventName derives the registry name of an event. The registry uses the
// pre-standard dotted names ("journal.posted"), so the legacy event_type
// carried in the envelope wins; otherwise the canonical type
// com.zoikosuite.<domain>.<aggregate>.<fact> is reduced to <aggregate>.<fact>.
func EventName(eventType string, rendered []byte) string {
	var env struct {
		LegacyType string `json:"event_type"`
	}
	// The envelope defaults its legacy type to the canonical one when a producer
	// names none; that is not a registry name, so it falls through to reduction.
	if json.Unmarshal(rendered, &env) == nil && env.LegacyType != "" && env.LegacyType != eventType {
		return env.LegacyType
	}
	const prefix = "com.zoikosuite."
	if strings.HasPrefix(eventType, prefix) {
		parts := strings.Split(strings.TrimPrefix(eventType, prefix), ".")
		if len(parts) >= 3 { // domain.aggregate.fact...
			return strings.Join(parts[1:], ".")
		}
	}
	return eventType
}
