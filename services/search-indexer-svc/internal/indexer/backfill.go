package indexer

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/projection"
)

// Replayer reads a source topic from its earliest retained message up to the
// end offset at the moment the replay started, and hands each decoded event to
// fn. Satisfied by kafka.Replayer; an interface so backfill is testable without
// a broker.
type Replayer interface {
	Replay(ctx context.Context, topic string, fn func(projection.Event) error) (int, error)
}

// SetReplayer wires the source replay used to backfill new generations.
func (ix *Indexer) SetReplayer(r Replayer) { ix.replayer = r }

// SetBaseContext is the lifetime backfills run under — the process's, not a
// request's. A backfill started from an HTTP handler must outlive the request.
func (ix *Indexer) SetBaseContext(ctx context.Context) { ix.baseCtx = ctx }

// StartBackfill fills a candidate generation from its source, in the
// background. Returns false when the generation is not a registered candidate
// or is already being backfilled.
//
// Why a backfill at all: a generation used to receive only events that arrived
// after it was activated, and validation passed an empty index — so activating
// a rebuild replaced a populated index with an empty one (INV-21, INV-22,
// NP-18; found live 30 Sep 2026). §8.3's recovery model is "restore/rebuild
// from source; indexes are disposable projections", and the source this
// service has is the event topic.
func (ix *Indexer) StartBackfill(generationID string) bool {
	ix.mu.Lock()
	var target *liveScope
	for _, list := range ix.candidates {
		for _, c := range list {
			if c.generationID == generationID {
				target = c
			}
		}
	}
	if target == nil || ix.backfilling[generationID] {
		ix.mu.Unlock()
		return false
	}
	ix.backfilling[generationID] = true
	ix.mu.Unlock()

	ctx := ix.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer func() {
			ix.mu.Lock()
			delete(ix.backfilling, generationID)
			ix.mu.Unlock()
		}()
		ix.backfill(ctx, target)
	}()
	return true
}

// ResumeBackfills restarts the backfill of every candidate that was PENDING or
// RUNNING — a restart mid-backfill must not leave a generation that can never
// become READY. Safe to repeat: backfill writes are create-only.
func (ix *Indexer) ResumeBackfills() {
	ix.mu.RLock()
	var ids []string
	for _, list := range ix.candidates {
		for _, c := range list {
			if c.backfill == domain.BackfillPending || c.backfill == domain.BackfillRunning {
				ids = append(ids, c.generationID)
			}
		}
	}
	ix.mu.RUnlock()
	for _, id := range ids {
		ix.StartBackfill(id)
	}
}

// BackfillStats is what one backfill did, recorded as the generation's
// backfill_note.
type BackfillStats struct {
	Replayed, Written, Tombstones, Superseded, Failed int
}

func (s BackfillStats) String() string {
	return fmt.Sprintf("replayed=%d written=%d tombstones=%d superseded=%d failed=%d",
		s.Replayed, s.Written, s.Tombstones, s.Superseded, s.Failed)
}

// backfill replays the source topic into one candidate generation.
//
// The LEDGER decides what each record's state is, not the replayed event:
//
//   - ledger says tombstoned → the candidate gets a tombstone at the ledger's
//     epoch, whatever the replayed event says. A record erased through
//     POST /v1/restrictions has no restriction EVENT in the topic, so trusting
//     the replay would resurrect it into the new generation.
//   - the event is exactly the version the ledger holds → it is written.
//   - anything else is an older state the ledger has superseded → skipped;
//     the event that produced the ledger's current state is later in the
//     replay, or the record will fail completeness validation.
//
// Writes are CREATE-ONLY, so a live write into the candidate — which is always
// newer — can never be overwritten by the replay of an older one.
func (ix *Indexer) backfill(ctx context.Context, target *liveScope) {
	scope := target.projector.Scope()
	src := target.projector.Source()
	log := ix.log.With(zap.String("scope", scope), zap.String("generation_id", target.generationID))

	_ = ix.store.SetBackfillState(ctx, target.generationID, domain.BackfillRunning, "")
	if ix.replayer == nil {
		_ = ix.store.SetBackfillState(ctx, target.generationID, domain.BackfillFailed, "no source replayer configured")
		log.Error("backfill impossible — no source replayer configured")
		return
	}

	var stats BackfillStats
	_, err := ix.replayer.Replay(ctx, src.EventTopic, func(e projection.Event) error {
		if sourceTypeOf(e) != src.SourceType || !target.projector.Handles(e.EventType) {
			return nil
		}
		stats.Replayed++
		return ix.backfillOne(ctx, target, e, &stats)
	})

	state, note := domain.BackfillComplete, stats.String()
	switch {
	case err != nil:
		state, note = domain.BackfillFailed, note+"; "+err.Error()
	case stats.Failed > 0:
		// A record the live path indexed that this generation's contract
		// cannot project is a generation that cannot serve the scope.
		state = domain.BackfillFailed
	}
	if werr := ix.store.SetBackfillState(ctx, target.generationID, state, note); werr != nil {
		log.Error("backfill finished but its state could not be recorded", zap.Error(werr))
	}
	ix.mu.Lock()
	target.backfill = state
	ix.mu.Unlock()
	log.Info("backfill finished", zap.String("state", string(state)), zap.String("stats", note))
}

func (ix *Indexer) backfillOne(ctx context.Context, target *liveScope, e projection.Event, stats *BackfillStats) error {
	res, err := target.projector.Project(e, target.generationID, target.physicalIndex)
	if err != nil {
		// The live path quarantined this event too (same projection rules),
		// so the ledger holds nothing for it: not a gap in this generation.
		if !errors.Is(err, projection.ErrNoTrustedTenant) {
			ix.log.Debug("backfill: event does not project under this contract", zap.Error(err))
		}
		return nil
	}
	rec, err := ix.store.GetProjectionRecord(ctx, res.Record.TenantID, res.Record.ScopeName,
		res.Record.SourceType, res.Record.SourceID)
	if err != nil {
		return fmt.Errorf("ledger read: %w", err)
	}
	if rec == nil {
		stats.Superseded++
		return nil
	}

	doc := res.Projection
	switch {
	case rec.Tombstoned:
		doc = searchclient.Projection{
			DocID:            doc.DocID,
			TenantID:         doc.TenantID,
			LegalEntityID:    doc.LegalEntityID,
			ResidencyRegion:  doc.ResidencyRegion,
			SourceType:       doc.SourceType,
			SourceID:         doc.SourceID,
			SourceVersion:    rec.SourceVersion,
			RestrictionEpoch: rec.RestrictionEpoch,
			SensitivityClass: doc.SensitivityClass,
			RetrievalClass:   doc.RetrievalClass,
			IndexGeneration:  target.generationID,
			Tombstoned:       true,
			TombstoneReason:  "LEDGER_RESTRICTED",
			TombstoneSource:  rec.LastEventID,
			Fields:           map[string]any{},
		}
	case rec.RestrictionEpoch == res.Record.RestrictionEpoch && rec.SourceVersion == res.Record.SourceVersion:
		if _, err := ix.embed(ctx, target, res); err != nil {
			stats.Failed++
			ix.log.Error("backfill: record could not be embedded", zap.String("source_id", rec.SourceID), zap.Error(err))
			return nil
		}
		doc = res.Projection
	default:
		stats.Superseded++
		return nil
	}

	switch err := ix.engine.CreateProjection(ctx, target.physicalIndex, doc); {
	case err == nil:
		if doc.Tombstoned {
			stats.Tombstones++
		} else {
			stats.Written++
		}
	case errors.Is(err, searchclient.ErrProjectionExists):
		stats.Superseded++
	case errors.Is(err, searchclient.ErrStrictMappingRejected):
		stats.Failed++
		ix.log.Error("backfill: record rejected by the generation's mapping", zap.String("source_id", rec.SourceID), zap.Error(err))
	default:
		return fmt.Errorf("engine write: %w", err)
	}
	return nil
}
