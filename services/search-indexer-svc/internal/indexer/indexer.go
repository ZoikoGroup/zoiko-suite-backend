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
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/embedding"
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
	// bySourceType maps a source_type to its ACTIVE generation's projector,
	// built from that generation's own contract. One per source type, because
	// a scope has exactly one ACTIVE generation.
	bySourceType map[string]*liveScope
	// candidates are the BUILDING / VALIDATING / READY generations per source
	// type, written alongside the active one so a rebuild is filled in
	// parallel and does not fall behind while it is certified.
	candidates map[string][]*liveScope
	// backfilling marks generations whose source replay is in progress.
	backfilling map[string]bool
	replayer    Replayer
	baseCtx     context.Context
	// topics is the union of every registered source's event topic, which is
	// what the Kafka runner subscribes to. Derived rather than configured, so
	// registering a source is sufficient to start consuming it.
	topics map[string]bool

	// lag measures consumer position at the broker for the checkpoint sweep.
	// Nil means freshness cannot be measured, which records UNKNOWN — never
	// CURRENT (§2.2).
	lag LagProbe
	// embedder produces vectors for semantic scopes. Nil behaves as
	// embedding.Unconfigured: a semantic scope's documents cannot be indexed.
	embedder embedding.Embedder
}

// SourceLag is a consumer group's measured position on one source topic.
type SourceLag struct {
	// Partitions is how many partitions the topic has.
	Partitions int
	// Backlog is the number of messages published but not yet committed by
	// this service's consumer group, summed over partitions.
	Backlog int64
	// Watermark is the sum of the group's committed offsets — §5.3's "highest
	// committed source position represented in the index", monotonic as the
	// consumer advances.
	Watermark int64
	// OldestPending is the broker timestamp of the oldest message not yet
	// consumed. Zero when Backlog is zero.
	OldestPending time.Time
}

// Lag is how far behind the index is as of now: the age of the oldest message
// it has not consumed. Zero when caught up — an idle source with nothing new to
// index is CURRENT however long ago its last event was, which a lag computed
// from "time since the last event" would get exactly wrong.
func (l SourceLag) Lag(now time.Time) time.Duration {
	if l.Backlog <= 0 || l.OldestPending.IsZero() {
		return 0
	}
	if d := now.Sub(l.OldestPending); d > 0 {
		return d
	}
	return 0
}

// LagProbe measures a source topic's consumer lag. Satisfied by
// kafka.LagProbe; an interface so the sweep is testable without a broker.
type LagProbe interface {
	TopicLag(ctx context.Context, topic string) (SourceLag, error)
}

// SetLagProbe wires the broker-side lag measurement into the checkpoint sweep.
func (ix *Indexer) SetLagProbe(p LagProbe) { ix.lag = p }

// SetEmbedder wires the embedding provider used for semantic scopes.
func (ix *Indexer) SetEmbedder(e embedding.Embedder) { ix.embedder = e }

// liveScope is one projector bound to the generation it currently writes into.
type liveScope struct {
	projector     *projection.Projector
	generationID  string
	physicalIndex string
	backfill      domain.BackfillState
}

func New(st store.Store, engine searchclient.Engine, pub *events.Publisher, metrics *telemetry.Metrics, log *zap.Logger) *Indexer {
	return &Indexer{
		store:        st,
		engine:       engine,
		events:       pub,
		metrics:      metrics,
		log:          log,
		bySourceType: map[string]*liveScope{},
		candidates:   map[string][]*liveScope{},
		backfilling:  map[string]bool{},
		topics:       map[string]bool{},
	}
}

// Reload rebuilds the projector registry from the control plane.
//
// Called at startup and after every contract publication or generation
// transition. A full rebuild rather than an incremental patch: the registry is
// small, and an incremental update that missed a case would leave a projector
// writing into a retired generation — a silent data loss that only shows up as
// a scope that stopped updating.
//
// EVERY GENERATION IS PROJECTED WITH ITS OWN CONTRACT. This used to project the
// ACTIVE generation with the scope's PUBLISHED contract, which is correct only
// until a new version is published: from then on the new contract's fields and
// embedding spec were written into the old generation's strict mapping, and
// every event was quarantined (found in the 30 Sep 2026 re-audit). A
// generation's mapping was built from one contract version, and only that
// version can write into it.
//
// Two sets per source type: the ACTIVE generation, and the CANDIDATES —
// BUILDING / VALIDATING / READY generations being filled for a cutover. Live
// events are written into both, so a candidate does not fall behind while it is
// backfilled and certified (INV-21's "parallel build").
func (ix *Indexer) Reload(ctx context.Context) error {
	sources, err := ix.store.ListSources(ctx)
	if err != nil {
		return fmt.Errorf("reload: list sources: %w", err)
	}
	contracts, err := ix.store.ListContracts(ctx, "")
	if err != nil {
		return fmt.Errorf("reload: list contracts: %w", err)
	}
	generations, err := ix.store.ListGenerations(ctx, "")
	if err != nil {
		return fmt.Errorf("reload: list generations: %w", err)
	}

	sourceByID := make(map[string]domain.SearchSource, len(sources))
	topics := map[string]bool{}
	for _, src := range sources {
		sourceByID[src.SourceID] = src
		if src.EventTopic != "" {
			topics[src.EventTopic] = true
		}
	}
	contractByID := make(map[string]domain.IndexContract, len(contracts))
	for _, c := range contracts {
		contractByID[c.ContractID] = c
	}

	active := map[string]*liveScope{}
	candidates := map[string][]*liveScope{}
	for _, g := range generations {
		switch g.State {
		case domain.GenerationActive, domain.GenerationBuilding, domain.GenerationValidating, domain.GenerationReady:
		default:
			continue
		}
		contract, ok := contractByID[g.ContractID]
		if !ok {
			continue
		}
		src, ok := sourceByID[contract.SourceID]
		if !ok {
			continue
		}
		proj, err := projection.New(contract, src)
		if err != nil {
			return fmt.Errorf("reload: generation %s: %w", g.GenerationID, err)
		}
		ls := &liveScope{
			projector:     proj,
			generationID:  g.GenerationID,
			physicalIndex: g.PhysicalIndex,
			backfill:      g.BackfillState,
		}
		if g.State == domain.GenerationActive {
			active[src.SourceType] = ls
		} else {
			candidates[src.SourceType] = append(candidates[src.SourceType], ls)
		}
	}
	for st := range candidates {
		sort.Slice(candidates[st], func(i, j int) bool {
			return candidates[st][i].generationID < candidates[st][j].generationID
		})
	}

	ix.mu.Lock()
	ix.bySourceType = active
	ix.candidates = candidates
	ix.topics = topics
	ix.mu.Unlock()

	ix.log.Info("projector registry reloaded",
		zap.Int("live_scopes", len(active)), zap.Int("candidate_scopes", len(candidates)),
		zap.Int("topics", len(topics)))
	return nil
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
//
// Into the ACTIVE generation (the primary: its failure fails the event) and
// into every CANDIDATE generation, each projected with its own contract. A
// candidate that cannot take the event — its contract cannot project it, or its
// embedding failed — is logged and skipped rather than failing the live scope:
// the candidate is then incomplete, and its completeness validation refuses it
// READY, which is the right place for that to surface.
func (ix *Indexer) Apply(ctx context.Context, e projection.Event) (Outcome, error) {
	sourceType := sourceTypeOf(e)

	ix.mu.RLock()
	primary := ix.bySourceType[sourceType]
	candidates := append([]*liveScope(nil), ix.candidates[sourceType]...)
	ix.mu.RUnlock()

	if primary == nil && len(candidates) > 0 {
		// No generation serving yet (a first build): the first candidate is
		// the one whose projection the ledger records.
		primary, candidates = candidates[0], candidates[1:]
	}
	if primary == nil || !primary.projector.Handles(e.EventType) {
		return OutcomeSkipped, nil
	}
	scope := primary.projector.Scope()

	result, err := primary.projector.Project(e, primary.generationID, primary.physicalIndex)
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

	// §10.1: a semantic scope's live document carries a vector from the
	// pinned model. Embedded BEFORE the ledger write, so a provider failure is
	// a retry of the whole event rather than a ledger row claiming a projection
	// the engine never received — the ledger-first window described in the
	// package comment, widened by a network call, would be refused as stale on
	// the retry and the document would never be written.
	if outcome, err := ix.embed(ctx, primary, result); err != nil {
		ix.metrics.ProjectionsTotal.WithLabelValues(scope, string(outcome)).Inc()
		ix.log.Error("projection could not be embedded",
			zap.String("scope", scope), zap.String("event_id", e.EventID),
			zap.String("outcome", string(outcome)), zap.Error(err))
		return outcome, err
	}

	type write struct {
		target *liveScope
		doc    searchclient.Projection
	}
	writes := []write{{primary, result.Projection}}
	for _, c := range candidates {
		if !c.projector.Handles(e.EventType) {
			continue
		}
		r, err := c.projector.Project(e, c.generationID, c.physicalIndex)
		if err == nil {
			_, err = ix.embed(ctx, c, r)
		}
		if err != nil {
			ix.log.Warn("candidate generation cannot take this event — it will fail completeness validation",
				zap.String("scope", scope), zap.String("generation_id", c.generationID),
				zap.String("event_id", e.EventID), zap.Error(err))
			continue
		}
		writes = append(writes, write{c, r.Projection})
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

	for i, w := range writes {
		err := ix.engine.IndexProjection(ctx, w.target.physicalIndex, w.doc)
		if err == nil {
			continue
		}
		ix.metrics.EngineError("index")
		if i > 0 {
			ix.log.Error("candidate generation missed a write — it will fail completeness validation",
				zap.String("scope", scope), zap.String("generation_id", w.target.generationID), zap.Error(err))
			continue
		}
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

// embed attaches the pinned-model vector to a semantic scope's live projection.
//
// Two failure classes, handled oppositely. A provider that answered with a
// different model or width is NP-35 — "pinned model/version mismatch blocks
// indexing" — and is quarantined: redelivering the event will get the same
// wrong model, and the DLQ copy is what an operator replays once the provider
// is pinned again. A provider that could not be reached, or no provider at all,
// is an ordinary transient error: retried, then dead-lettered.
//
// A document with no embeddable text gets no vector. It stays a lexical
// candidate; it is simply never a semantic one, which is the truthful answer
// for a record that has nothing in its pinned source fields.
func (ix *Indexer) embed(ctx context.Context, live *liveScope, result *projection.Result) (Outcome, error) {
	spec := live.projector.Contract().Embedding
	if spec == nil || result.Projection.Tombstoned {
		return OutcomeIndexed, nil
	}
	text := embedding.DocumentText(*spec, result.Projection.Fields)
	if text == "" {
		return OutcomeIndexed, nil
	}
	embedder := ix.embedder
	if embedder == nil {
		embedder = embedding.Unconfigured{}
	}
	vectors, err := embedder.Embed(ctx, *spec, []string{text})
	if err != nil {
		if errors.Is(err, embedding.ErrModelMismatch) {
			return OutcomeQuarantined, fmt.Errorf("%s: %w", domain.ReasonSemanticModelMismatch, err)
		}
		return OutcomeError, fmt.Errorf("embed: %w", err)
	}
	result.Projection.Embedding = vectors[0]
	result.Projection.EmbeddingModel = spec.PinnedModel()
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
			// NP-34: "restriction event updates all representations;
			// verification checks both." A tombstoned lexical document that
			// still carries its vector is still a nearest-neighbour answer
			// for the restricted record, so it is NOT invisible.
			if _, hasVector := doc[searchclient.EmbeddingVectorField]; hasVector {
				invisible = false
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

// laggingFraction is the share of a source's max_lag_seconds past which a
// scope is reported LAGGING before it is STALE. §8.3's "alert by freshness
// class; scope marked LAGGING/STALE" needs an early state that is not yet a
// block, and half the bound is the point at which an operator still has time
// to act before protected searches start refusing.
const laggingFraction = 0.5

// RecordCheckpoints recomputes freshness and population for every live scope.
func (ix *Indexer) RecordCheckpoints(ctx context.Context) {
	ix.mu.RLock()
	snapshot := make([]*liveScope, 0, len(ix.bySourceType))
	for _, v := range ix.bySourceType {
		snapshot = append(snapshot, v)
	}
	ix.mu.RUnlock()

	for _, live := range snapshot {
		ix.recordCheckpoint(ctx, live)
	}
}

// RecordCheckpointForScope measures one scope now, rather than at the next
// sweep. Called after a generation is activated: a new generation has no
// checkpoint row, and until it has one its freshness is UNKNOWN, which blocks
// protected searches — so the first measurement should not wait a full sweep
// interval.
func (ix *Indexer) RecordCheckpointForScope(ctx context.Context, scope string) {
	ix.mu.RLock()
	var target *liveScope
	for _, v := range ix.bySourceType {
		if v.projector.Scope() == scope {
			target = v
			break
		}
	}
	ix.mu.RUnlock()
	if target != nil {
		ix.recordCheckpoint(ctx, target)
	}
}

// recordCheckpoint measures one live scope and writes its checkpoint.
//
// Three independent measurements, and freshness is the WORST of them:
//
//  1. Source-to-index lag, from the broker (§5.3 "Lag"): the age of the oldest
//     message this consumer group has not committed. Past the source's
//     max_lag_seconds the scope is STALE; past half of it, LAGGING.
//  2. Population, ledger against engine (§5.3 "Population count"): a gap in
//     either direction is LAGGING — the window the ledger-first write order
//     leaves open (NP-17).
//  3. Measurability: if the lag or the engine count cannot be obtained, the
//     scope is UNKNOWN, not its previous value. §2.2: "UNKNOWN is never
//     represented as CURRENT."
func (ix *Indexer) recordCheckpoint(ctx context.Context, live *liveScope) {
	scope := live.projector.Scope()
	source := live.projector.Source()
	now := time.Now().UTC()

	ledgerLive, ledgerTombstoned, err := ix.store.CountProjections(ctx, scope)
	if err != nil {
		// No write at all rather than an UNKNOWN row: the database this would
		// be written to is the thing that just failed. The read side's
		// observed_at age check turns a checkpoint that stopped being written
		// into UNKNOWN.
		ix.log.Error("checkpoint: ledger count failed", zap.String("scope", scope), zap.Error(err))
		return
	}

	previous, _ := ix.store.GetCheckpoint(ctx, scope, live.physicalIndex)

	cp := domain.IndexCheckpoint{
		ScopeName:         scope,
		SourcePartition:   live.physicalIndex,
		Watermark:         -1,
		CommittedAt:       now,
		Freshness:         domain.FreshnessCurrent,
		IndexedLive:       ledgerLive,
		IndexedTombstoned: ledgerTombstoned,
	}
	cause := ""

	// 1. Lag, at the broker.
	if ix.lag == nil {
		cp.Freshness = domain.FreshnessUnknown
		cause = "no lag probe configured"
	} else if source.EventTopic == "" {
		// Nothing to measure is not "caught up".
		cp.Freshness = domain.FreshnessUnknown
		cause = "source has no event topic"
	} else if measured, err := ix.lag.TopicLag(ctx, source.EventTopic); err != nil {
		cp.Freshness = domain.FreshnessUnknown
		cause = "consumer position could not be measured: " + err.Error()
		ix.log.Error("checkpoint: lag probe failed", zap.String("scope", scope), zap.Error(err))
	} else if measured.Backlog > 0 && measured.OldestPending.IsZero() {
		// A backlog whose age cannot be read has an unknown lag, not zero.
		cp.Watermark = measured.Watermark
		cp.Freshness = domain.FreshnessUnknown
		cause = fmt.Sprintf("backlog of %d messages with no readable timestamp", measured.Backlog)
	} else {
		cp.Watermark = measured.Watermark
		cp.LagMS = measured.Lag(now).Milliseconds()
		if !measured.OldestPending.IsZero() {
			// committed_at is the source time the index is caught up TO: the
			// broker timestamp of the oldest message still to be consumed.
			// Caught up means caught up to now.
			cp.CommittedAt = measured.OldestPending.UTC()
		}
		maxLagMS := int64(source.MaxLagSeconds) * 1000
		switch {
		case maxLagMS > 0 && cp.LagMS > maxLagMS:
			cp.Freshness = domain.FreshnessStale
			cause = fmt.Sprintf("lag %dms exceeds max_lag_seconds %d", cp.LagMS, source.MaxLagSeconds)
		case maxLagMS > 0 && float64(cp.LagMS) > laggingFraction*float64(maxLagMS):
			cp.Freshness = domain.FreshnessLagging
			cause = fmt.Sprintf("lag %dms is past half of max_lag_seconds %d", cp.LagMS, source.MaxLagSeconds)
		}
	}

	// 2/3. Population, ledger against engine.
	engineLive, err := ix.engine.CountProjections(ctx, live.physicalIndex, nil)
	switch {
	case err != nil:
		ix.metrics.EngineError("count")
		ix.log.Error("checkpoint: engine count failed", zap.String("scope", scope), zap.Error(err))
		cp.Freshness = domain.FreshnessUnknown
		cause = "engine population could not be counted: " + err.Error()
	case engineLive != ledgerLive && cp.Freshness == domain.FreshnessCurrent:
		// Downgrades CURRENT only. A STALE or UNKNOWN scope that also has a
		// population gap is still STALE or UNKNOWN — the worse state wins.
		cp.Freshness = domain.FreshnessLagging
		cause = fmt.Sprintf("ledger %d vs engine %d", ledgerLive, engineLive)
		ix.log.Warn("checkpoint: population mismatch between ledger and index",
			zap.String("scope", scope),
			zap.Int64("ledger_live", ledgerLive),
			zap.Int64("engine_live", engineLive))
	}

	if err := ix.store.UpsertCheckpoint(ctx, cp); err != nil {
		ix.log.Error("checkpoint write failed", zap.String("scope", scope), zap.Error(err))
		return
	}
	ix.metrics.ObserveFreshness(scope, string(cp.Freshness), float64(cp.LagMS)/1000)
	if cp.Freshness != domain.FreshnessCurrent {
		ix.log.Warn("checkpoint: scope is not CURRENT",
			zap.String("scope", scope), zap.String("freshness", string(cp.Freshness)),
			zap.Int64("lag_ms", cp.LagMS), zap.String("cause", cause))
	}

	if ix.events == nil {
		return
	}
	// esr.index_checkpoint.advanced only when the position actually ADVANCED.
	// Emitting it every sweep — or, worse, on a sweep that could not measure
	// anything — tells DQC the index moved when it did not, which is NP-17
	// ("index checkpoint falsely advances past missing events") delivered as
	// an event.
	if cp.Watermark >= 0 && (previous == nil || cp.Watermark > previous.Watermark) {
		_ = ix.events.CheckpointAdvanced(ctx, scope, live.physicalIndex,
			cp.Watermark, cp.LagMS, live.generationID, string(cp.Freshness))
	}
	// A scope that has just become untrustworthy is announced once, on the
	// transition, as esr.search.degraded — §11.2 routes that event to
	// NCD/SRE/operations, which is who §8.3's "alert by freshness class" is
	// for. Not on every sweep: a standing condition is a gauge, not a stream.
	if cp.Freshness.Untrusted() && (previous == nil || !previous.Freshness.Untrusted()) {
		completeness := domain.CompletenessDegraded
		if cp.Freshness == domain.FreshnessUnknown {
			completeness = domain.CompletenessUnknown
		}
		_ = ix.events.SearchDegraded(ctx, "", scope,
			"index freshness "+string(cp.Freshness)+": "+cause,
			string(completeness), []string{live.physicalIndex}, "")
	}
}

// RunSweeps starts the two background loops and blocks until ctx is cancelled.
func (ix *Indexer) RunSweeps(ctx context.Context, verifyEvery, checkpointEvery time.Duration) {
	verify := time.NewTicker(verifyEvery)
	defer verify.Stop()
	checkpoint := time.NewTicker(checkpointEvery)
	defer checkpoint.Stop()

	// Measure once at start rather than a full interval later. Until a scope
	// has a checkpoint its freshness is UNKNOWN, which blocks protected
	// searches — so a restart must not open with a minute of refusals.
	ix.RecordCheckpoints(ctx)

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
