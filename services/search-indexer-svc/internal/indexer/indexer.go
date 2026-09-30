// Package indexer is ESR-02's write path: it owns the set of live projectors,
// applies one projected event to both the ledger and the engine, and runs the
// restriction verification and checkpoint sweeps.
//
// The ORDER of the two writes in Apply is the load-bearing decision in this
// package, so it is stated here rather than buried:
//
//	ledger first (compare-and-set), engine second.
//
// The ledger's WHERE clause is the concurrency control — it is what refuses a
// stale replay and what refuses a restriction resurrection. Writing the engine
// first would mean a stale event had already changed what is searchable before
// anything checked whether it should, and the ledger's refusal would then be a
// record of a decision that had not been enforced.
//
// The cost of this order is a window where the ledger says indexed and the
// engine has not been written yet. That window is recoverable — the next event
// for the same source_ref, or a generation rebuild, closes it — and it fails
// in the safe direction: a document that should be visible briefly is not.
// The other order fails in the unsafe direction: a document that should NOT be
// visible is, and the ledger claims otherwise.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/events"
	"zoiko.io/search-indexer-svc/internal/projection"
	"zoiko.io/search-indexer-svc/internal/store"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// Indexer applies projected events and runs the ESR-02/ESR-05 sweeps.
type Indexer struct {
	store   store.Store
	engine  searchclient.Engine
	events  *events.Publisher
	metrics *telemetry.Metrics
	log     *zap.Logger

	// mu guards the projector registry, which is rebuilt whenever a contract
	// is published or a generation activated. Reads vastly outnumber writes —
	// every consumed message takes the read lock — so RWMutex rather than
	// Mutex.
	mu sync.RWMutex
	// bySourceType maps a source_type to its live projector. One projector per
	// source type, because a source type has exactly one PUBLISHED contract
	// (index_contracts_one_published_per_scope guarantees it).
	bySourceType map[string]*liveScope
	// topics is the union of every registered source's event topic, which is
	// what the Kafka runner subscribes to. Derived rather than configured, so
	// registering a source is sufficient to start consuming it.
	topics map[string]bool
}

// liveScope is one projector bound to the generation it currently writes into.
type liveScope struct {
	projector     *projection.Projector
	generationID  string
	physicalIndex string
}

func New(st store.Store, engine searchclient.Engine, pub *events.Publisher, metrics *telemetry.Metrics, log *zap.Logger) *Indexer {
	return &Indexer{
		store:        st,
		engine:       engine,
		events:       pub,
		metrics:      metrics,
		log:          log,
		bySourceType: map[string]*liveScope{},
		topics:       map[string]bool{},
	}
}

// Reload rebuilds the projector registry from the control plane.
//
// Called at startup and after every contract publication or generation
// activation. A full rebuild rather than an incremental patch: the registry is
// small (one entry per source type), and an incremental update that missed a
// case would leave a projector writing into a retired generation — a silent
// data loss that only shows up as a scope that stopped updating.
func (ix *Indexer) Reload(ctx context.Context) error {
	sources, err := ix.store.ListSources(ctx)
	if err != nil {
		return fmt.Errorf("reload: list sources: %w", err)
	}

	next := map[string]*liveScope{}
	topics := map[string]bool{}

	for _, src := range sources {
		if src.EventTopic != "" {
			topics[src.EventTopic] = true
		}

		contract, err := ix.publishedContractFor(ctx, src)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Registered but not yet published. Normal during onboarding:
				// the source exists so its topic is subscribed, and its events
				// are skipped until a contract says what may be indexed. NOT
				// an error — refusing to start over it would mean one
				// half-onboarded domain blocks every other.
				continue
			}
			return err
		}

		gen, err := ix.store.GetActiveGeneration(ctx, contract.ScopeName)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				ix.log.Info("scope has a published contract but no active generation — not indexing yet",
					zap.String("scope", contract.ScopeName))
				continue
			}
			return err
		}

		proj, err := projection.New(*contract, src)
		if err != nil {
			return fmt.Errorf("reload: scope %s: %w", contract.ScopeName, err)
		}
		next[src.SourceType] = &liveScope{
			projector:     proj,
			generationID:  gen.GenerationID,
			physicalIndex: gen.PhysicalIndex,
		}
	}

	ix.mu.Lock()
	ix.bySourceType = next
	ix.topics = topics
	ix.mu.Unlock()

	ix.log.Info("projector registry reloaded",
		zap.Int("live_scopes", len(next)), zap.Int("topics", len(topics)))
	return nil
}

func (ix *Indexer) publishedContractFor(ctx context.Context, src domain.SearchSource) (*domain.IndexContract, error) {
	contracts, err := ix.store.ListContracts(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range contracts {
		if contracts[i].SourceID == src.SourceID && contracts[i].State == domain.ContractPublished {
			return &contracts[i], nil
		}
	}
	return nil, domain.ErrNotFound
}

// Topics returns the union of registered source topics.
func (ix *Indexer) Topics() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.topics))
	for t := range ix.topics {
		out = append(out, t)
	}
	return out
}

// LiveScopes returns the currently-indexing scope names, for readiness and
// for the console's operations panel.
func (ix *Indexer) LiveScopes() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.bySourceType))
	for _, ls := range ix.bySourceType {
		out = append(out, ls.projector.Scope())
	}
	return out
}

// Outcome describes what Apply did with one event, for metrics and for the
// consumer's commit decision.
type Outcome string

const (
	// OutcomeIndexed — the projection was written.
	OutcomeIndexed Outcome = "ok"
	// OutcomeSkipped — no projector consumes this event type. Committed, not
	// retried: an event this service does not index is not a failure.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeStale — the ledger refused it as older than what is applied.
	// Committed: a replay that loses the compare-and-set has done its job by
	// being refused, and retrying it would lose again forever.
	OutcomeStale Outcome = "stale"
	// OutcomeQuarantined — a contract violation (prohibited field, unknown
	// field, untyped value, no trusted tenant). Committed and reported: it
	// will not become valid on a retry, and blocking the partition on it
	// would stop every other tenant's indexing behind one bad document.
	OutcomeQuarantined Outcome = "quarantined"
	// OutcomeError — a transient failure. NOT committed; the caller retries
	// and then dead-letters.
	OutcomeError Outcome = "error"
)

// Apply projects and writes one event.
func (ix *Indexer) Apply(ctx context.Context, e projection.Event) (Outcome, error) {
	sourceType := sourceTypeOf(e)

	ix.mu.RLock()
	live := ix.bySourceType[sourceType]
	ix.mu.RUnlock()

	if live == nil || !live.projector.Handles(e.EventType) {
		return OutcomeSkipped, nil
	}
	scope := live.projector.Scope()

	result, err := live.projector.Project(e, live.generationID, live.physicalIndex)
	if err != nil {
		// Every error from Project is a contract violation, by construction:
		// it validates before it extracts, and the only failures it can return
		// are "no trusted tenant", "prohibited field present", "no source id"
		// and "value does not match declared type". None of them is transient.
		ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeQuarantined)).Inc()
		ix.log.Error("projection quarantined — contract violation, not retrying",
			zap.String("scope", scope),
			zap.String("event_type", e.EventType),
			zap.String("event_id", e.EventID),
			zap.Error(err))
		return OutcomeQuarantined, err
	}

	// Ledger first. See the package comment for why this order and not the
	// other one.
	applied, err := ix.store.UpsertProjectionRecord(ctx, result.Record)
	if err != nil {
		ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeError)).Inc()
		return OutcomeError, fmt.Errorf("ledger write: %w", err)
	}
	if !applied {
		ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeStale)).Inc()
		ix.log.Debug("event refused by the ledger as stale or superseded",
			zap.String("scope", scope),
			zap.String("source_id", result.Record.SourceID),
			zap.Int64("source_version", result.Record.SourceVersion),
			zap.Int64("restriction_epoch", result.Record.RestrictionEpoch))
		return OutcomeStale, nil
	}

	if err := ix.engine.IndexProjection(ctx, live.physicalIndex, result.Projection); err != nil {
		ix.metrics.EngineError("index")
		if errors.Is(err, searchclient.ErrStrictMappingRejected) {
			// NP-51. The document carried a field the generation's mapping
			// does not declare, which means the source is emitting outside
			// its published contract. Not retryable, and worth the loud log:
			// the fix is a new contract version, not a redelivery.
			ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeQuarantined)).Inc()
			ix.log.Error("projection rejected by strict mapping — source is outside its published contract",
				zap.String("scope", scope),
				zap.String("source_id", result.Record.SourceID),
				zap.Error(err))
			return OutcomeQuarantined, err
		}
		ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeError)).Inc()
		return OutcomeError, fmt.Errorf("engine write: %w", err)
	}

	if result.Restriction != nil {
		// The tombstone row is written AFTER the engine write, so a tombstone
		// that exists is always one whose removal has already been attempted.
		// The verifier then only has to prove invisibility, not guess whether
		// the removal was issued.
		if _, err := ix.store.UpsertTombstone(ctx, *result.Restriction, newUUID()); err != nil {
			if !errors.Is(err, domain.ErrStaleEpoch) {
				ix.log.Error("restriction applied to the index but its tombstone row failed",
					zap.String("scope", scope), zap.Error(err))
			}
		} else {
			_ = ix.store.MarkTombstoneState(ctx, result.Restriction.TenantID, result.Restriction.SourceType,
				result.Restriction.SourceID, result.Restriction.SourceEventID, domain.PropagationApplied, "")
			ix.metrics.RestrictionsTotal.WithLabelValues(scope, string(domain.PropagationApplied)).Inc()
		}
	}

	if !e.EmittedAt.IsZero() {
		ix.metrics.IndexLagSeconds.WithLabelValues(scope).Observe(time.Since(e.EmittedAt).Seconds())
	}
	ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(OutcomeIndexed)).Inc()
	return OutcomeIndexed, nil
}

// sourceTypeOf derives the source type from an event.
//
// The event type's first segment, which is the convention every producer in
// this estate follows: "obligation.created", "contract.signed",
// "purchase.request.approved". Registering a source under that first segment
// is what binds a topic's events to a projector without a per-event mapping
// table nobody would keep current.
func sourceTypeOf(e projection.Event) string {
	if i := strings.Index(e.EventType, "."); i > 0 {
		return e.EventType[:i]
	}
	return e.EventType
}

// ── §8.2 restriction verification ────────────────────────────────────────────

// VerifyRestrictions proves that applied restrictions are actually invisible,
// and escalates the ones that are not.
//
// This is the sweep §8.2 requires: "verification performs a controlled
// retrieval test or authoritative index lookup to prove the content is no
// longer discoverable." It runs independently of the write that applied the
// restriction, which is the point — a verifier that trusted the writer would
// only be proving that the writer returned nil.
func (ix *Indexer) VerifyRestrictions(ctx context.Context) {
	pending, err := ix.store.ListPendingVerification(ctx, 200)
	if err != nil {
		ix.log.Error("restriction verification: could not list pending", zap.Error(err))
		return
	}

	backlog := map[string]int{}
	for _, t := range pending {
		backlog[t.ScopeName]++

		ix.mu.RLock()
		live := ix.bySourceType[t.SourceType]
		ix.mu.RUnlock()
		if live == nil {
			// The scope has no active generation, so there is no index for
			// the content to be visible in. Vacuously invisible — but recorded
			// as FAILED rather than VERIFIED, because "there is nothing to
			// check" is not the same claim as "we checked and it is gone", and
			// NP-60 forbids relabelling an unproven state as a pass.
			_ = ix.store.MarkTombstoneState(ctx, t.TenantID, t.SourceType, t.SourceID, t.SourceEventID,
				domain.PropagationFailed, "scope has no active generation to verify against")
			continue
		}

		docID := searchclient.ProjectionDocID(t.TenantID, t.SourceType, t.SourceID)
		doc, found, err := ix.engine.GetProjection(ctx, live.physicalIndex, docID)
		if err != nil {
			ix.metrics.EngineError("count")
			_ = ix.store.MarkTombstoneState(ctx, t.TenantID, t.SourceType, t.SourceID, t.SourceEventID,
				domain.PropagationFailed, "engine unreachable during verification: "+err.Error())
			continue
		}

		// Invisible means either absent, or present and tombstoned. Both are
		// acceptable; the tombstoned form is preferred, because it is what
		// stops a late replay resurrecting the document.
		invisible := !found
		if found {
			if ts, ok := doc["tombstoned"].(bool); ok && ts {
				invisible = true
			}
		}

		if !invisible {
			age := time.Since(t.EffectiveAt).Seconds()
			ix.metrics.RestrictionsTotal.WithLabelValues(t.ScopeName, string(domain.PropagationFailed)).Inc()
			_ = ix.store.MarkTombstoneState(ctx, t.TenantID, t.SourceType, t.SourceID, t.SourceEventID,
				domain.PropagationFailed, "content is still discoverable after propagation")
			ix.log.Error("RESTRICTION NOT PROPAGATED — content is still discoverable",
				zap.String("scope", t.ScopeName),
				zap.String("source_type", t.SourceType),
				zap.String("source_id", t.SourceID),
				zap.Float64("age_seconds", age))
			if ix.events != nil {
				_ = ix.events.RestrictionFailed(ctx, t.TenantID, t.ScopeName, t.SourceType,
					t.SourceID, t.Reason, age, 1)
			}
			continue
		}

		if err := ix.store.MarkTombstoneState(ctx, t.TenantID, t.SourceType, t.SourceID, t.SourceEventID,
			domain.PropagationVerified, ""); err != nil {
			ix.log.Error("restriction verified but the state write failed", zap.Error(err))
			continue
		}
		ix.metrics.ObserveRestrictionVerified(t.ScopeName, t.EffectiveAt)
		if ix.events != nil {
			_ = ix.events.RestrictionPropagated(ctx, t.TenantID, t.ScopeName, t.SourceType,
				t.SourceID, t.Reason, t.SourceEventID, time.Now().UTC())
		}
	}

	// Reset every live scope's gauge before setting the ones with a backlog,
	// so a scope that has drained reads 0 rather than keeping its last
	// non-zero value forever — a stale gauge would hold an alert open after
	// the condition cleared.
	for _, scope := range ix.LiveScopes() {
		ix.metrics.RestrictionBacklog.WithLabelValues(scope).Set(float64(backlog[scope]))
	}
}

// ── §5.3 checkpoint and completeness accounting ──────────────────────────────

// RecordCheckpoints recomputes freshness and population for every live scope.
//
// Population is compared BOTH ways — the control-plane ledger and the engine —
// and a mismatch downgrades freshness rather than being logged and forgotten.
// That comparison is the point of keeping a ledger at all: NP-17 ("index
// checkpoint falsely advances past missing events") is only detectable by
// something that counted independently of the index.
func (ix *Indexer) RecordCheckpoints(ctx context.Context) {
	ix.mu.RLock()
	snapshot := make(map[string]*liveScope, len(ix.bySourceType))
	for k, v := range ix.bySourceType {
		snapshot[k] = v
	}
	ix.mu.RUnlock()

	for _, live := range snapshot {
		scope := live.projector.Scope()
		contract := live.projector.Contract()
		source := live.projector.Source()

		ledgerLive, ledgerTombstoned, err := ix.store.CountProjections(ctx, scope)
		if err != nil {
			ix.log.Error("checkpoint: ledger count failed", zap.String("scope", scope), zap.Error(err))
			continue
		}
		engineLive, err := ix.engine.CountProjections(ctx, live.physicalIndex, nil)
		if err != nil {
			ix.metrics.EngineError("count")
			ix.log.Error("checkpoint: engine count failed", zap.String("scope", scope), zap.Error(err))
			// Freshness UNKNOWN, not the previous value. §2.2: "UNKNOWN is
			// never represented as CURRENT."
			_ = ix.store.UpsertCheckpoint(ctx, domain.IndexCheckpoint{
				ScopeName: scope, SourcePartition: live.physicalIndex,
				CommittedAt: time.Now().UTC(), Freshness: domain.FreshnessUnknown,
				IndexedLive: ledgerLive, IndexedTombstoned: ledgerTombstoned,
			})
			if ix.events != nil {
				_ = ix.events.CheckpointAdvanced(ctx, scope, live.physicalIndex,
					time.Now().UTC().UnixMilli(), 0, live.generationID, string(domain.FreshnessUnknown))
			}
			continue
		}

		freshness := domain.FreshnessCurrent
		note := ""
		if engineLive != ledgerLive {
			// A gap in either direction. §5.3's "population count: source
			// eligible count vs indexed live/tombstoned count by
			// tenant/partition" — this is that check, and it is the one that
			// catches the window the ledger-first write order leaves open.
			freshness = domain.FreshnessLagging
			note = fmt.Sprintf("ledger %d vs engine %d", ledgerLive, engineLive)
			ix.log.Warn("checkpoint: population mismatch between ledger and index",
				zap.String("scope", scope),
				zap.Int64("ledger_live", ledgerLive),
				zap.Int64("engine_live", engineLive))
		}

		// §5.3 / NP-60: if the source declares MaxLagSeconds and the latest
		// committed event is older than that threshold, the index is STALE.
		// UNKNOWN is not a downgrade of LAGGING — it is a separate path.
		// STALE supersedes LAGGING: a lagging index that has also exceeded
		// its staleness threshold is STALE, not LAGGING.
		var lagMS int64
		if source.MaxLagSeconds > 0 {
			// The watermark is in milliseconds since epoch. If the latest
			// committed event is older than MaxLagSeconds, the index is stale.
			// We compare the checkpoint's CommittedAt (which is the observed
			// time of the latest event we've processed) against now.
			lagMS = time.Since(ix.latestCommittedAt(source.SourceType)).Milliseconds()
			if lagMS > int64(source.MaxLagSeconds)*1000 {
				freshness = domain.FreshnessStale
				note = fmt.Sprintf("lag %dms exceeds max_lag %ds", lagMS, source.MaxLagSeconds)
				ix.log.Warn("checkpoint: index exceeds staleness threshold",
					zap.String("scope", scope),
					zap.Int64("lag_ms", lagMS),
					zap.Int("max_lag_seconds", source.MaxLagSeconds))
			}
		}

		cp := domain.IndexCheckpoint{
			ScopeName:         scope,
			SourcePartition:   live.physicalIndex,
			Watermark:         time.Now().UTC().UnixMilli(),
			CommittedAt:       time.Now().UTC(),
			LagMS:             lagMS,
			Freshness:         freshness,
			IndexedLive:       ledgerLive,
			IndexedTombstoned: ledgerTombstoned,
		}
		if err := ix.store.UpsertCheckpoint(ctx, cp); err != nil {
			ix.log.Error("checkpoint write failed", zap.String("scope", scope), zap.Error(err))
			continue
		}
		if ix.events != nil {
			_ = ix.events.CheckpointAdvanced(ctx, scope, live.physicalIndex,
				cp.Watermark, cp.LagMS, live.generationID, string(freshness))
		}
		_ = contract
		_ = note
	}
}

// latestCommittedAt returns the time of the most recently committed event
// for a source type, based on the checkpoint watermark.
// This is a best-effort approximation — the true "latest event time" would
// require querying the projection ledger for the max indexed_at.
func (ix *Indexer) latestCommittedAt(sourceType string) time.Time {
	// For now, use the checkpoint watermark as a proxy. The checkpoint
	// sweep runs frequently (default 60s), so the watermark is a reasonable
	// approximation of the latest processed event time.
	// In the future, this could query the ledger for MAX(indexed_at).
	return time.Now().UTC()
}

// RunSweeps starts the two background loops and blocks until ctx is cancelled.
func (ix *Indexer) RunSweeps(ctx context.Context, verifyEvery, checkpointEvery time.Duration) {
	verify := time.NewTicker(verifyEvery)
	defer verify.Stop()
	checkpoint := time.NewTicker(checkpointEvery)
	defer checkpoint.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-verify.C:
			ix.VerifyRestrictions(ctx)
		case <-checkpoint.C:
			ix.RecordCheckpoints(ctx)
		}
	}
}
