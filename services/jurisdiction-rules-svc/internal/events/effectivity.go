package events

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// EffectivityWatcher announces rules reaching the start or end of their
// effective interval (regulatory-rule.effective / .expired, ZS-JUR-001 s26).
// Each tick claims the crossings since the last one and writes their events to
// the outbox in the same transaction that moves the watermark: one replica
// announces each crossing once, and a restart skips none.
type EffectivityWatcher struct {
	log      *zap.Logger
	store    *store.PgStore
	pub      RegistryPublisher
	interval time.Duration
	now      func() time.Time
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewEffectivityWatcher builds a watcher that ticks every interval.
func NewEffectivityWatcher(log *zap.Logger, st *store.PgStore, pub RegistryPublisher, interval time.Duration) *EffectivityWatcher {
	return &EffectivityWatcher{log: log, store: st, pub: pub, interval: interval, now: time.Now, stopCh: make(chan struct{})}
}

// Start runs the watcher until Stop.
func (w *EffectivityWatcher) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-w.stopCh:
				return
			case <-t.C:
				if _, err := w.Tick(context.Background()); err != nil {
					w.log.Error("rule effectivity scan failed; it is retried next tick", zap.Error(err))
				}
			}
		}
	}()
}

// Stop ends the watcher.
func (w *EffectivityWatcher) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

// Tick announces the crossings since the last tick and returns how many.
func (w *EffectivityWatcher) Tick(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	uctx, unit := w.store.BeginUnit(ctx, "")
	defer unit.Rollback(uctx)
	crossings, err := w.store.ClaimRuleCrossings(uctx, w.now().UTC())
	if err != nil {
		return 0, err
	}
	for _, c := range crossings {
		eventType := EventRuleEffective
		if c.Kind == "expired" {
			eventType = EventRuleExpired
		}
		payload := map[string]any{
			"jurisdiction_rule_id": c.JurisdictionRuleID, "jurisdiction_id": c.JurisdictionID,
			"rule_domain": c.RuleDomain, "rule_code": c.RuleCode, "rule_version": c.RuleVersion,
			"rule_status": c.RuleStatus, "at": c.At, AggregateVersionKey: c.RowVersion,
		}
		if err := w.pub.PublishRegistryEvent(uctx, eventType, c.JurisdictionRuleID, "jurisdiction-rules-svc:effectivity-watcher", "", payload); err != nil {
			return 0, err
		}
	}
	if err := unit.Finish(uctx); err != nil {
		return 0, err
	}
	return len(crossings), nil
}
