// Package release keeps the active, verified knowledge and claim releases
// and the revocation ledger in memory. A turn takes one Snapshot at start
// (the release tuple is pinned) and re-checks revocations at commit
// (Doc 08 §06).
package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// Snapshot is an immutable view used for one turn.
type Snapshot struct {
	KB        *knowledge.Index
	Claims    *claims.Resolver
	Routes    []knowledge.Route
	Watermark int64
	rev       *revocationSet
}

// SourceRevoked implements knowledge.Revocations.
func (s *Snapshot) SourceRevoked(sourceID, sourceVersionID string) bool {
	return s.rev.sources[sourceID] || s.rev.sources[sourceVersionID]
}

// ClaimRevoked implements claims.RevocationSet.
func (s *Snapshot) ClaimRevoked(claimID string) bool { return s.rev.claims[claimID] }

// Route returns the effective route for key and market, or false.
func (s *Snapshot) Route(key, market string, now time.Time) (knowledge.Route, bool) {
	for _, r := range s.Routes {
		if r.RouteKey != key || now.Before(r.EffectiveFrom) || !now.Before(r.EffectiveUntil) {
			continue
		}
		if !routeMarketOK(r.MarketCodes, market) {
			continue
		}
		if r.RequiresClaim != "" && (s.Claims == nil || !s.Claims.HasVerified(r.RequiresClaim, market, now, s)) {
			continue
		}
		return r, true
	}
	return knowledge.Route{}, false
}

func routeMarketOK(codes []string, market string) bool {
	for _, c := range codes {
		if c == knowledge.MarketAll || (market != "" && c == market) {
			return true
		}
	}
	return false
}

type revocationSet struct {
	sources map[string]bool
	claims  map[string]bool
}

// Runtime loads and refreshes releases.
type Runtime struct {
	store  store.Store
	pubKey string

	mu          sync.RWMutex
	snap        *Snapshot
	kbID        string
	claimsID    string
	lastRefresh time.Time
	lastErr     error
}

// New creates a Runtime. pubKey is the base64 Ed25519 verification key.
func New(s store.Store, pubKey string) *Runtime {
	return &Runtime{store: s, pubKey: pubKey, snap: &Snapshot{rev: &revocationSet{sources: map[string]bool{}, claims: map[string]bool{}}}}
}

// ErrNotLoaded means no verified release is active.
var ErrNotLoaded = errors.New("no verified knowledge/claim release is active")

// Refresh reloads releases whose pointer moved and applies new revocations.
// Any verification failure fails closed: the runtime reports not ready
// until a valid release is promoted.
func (r *Runtime) Refresh(ctx context.Context) error {
	err := r.refresh(ctx)
	r.mu.Lock()
	r.lastErr = err
	if err == nil {
		r.lastRefresh = time.Now()
	}
	r.mu.Unlock()
	return err
}

func (r *Runtime) refresh(ctx context.Context) error {
	r.mu.RLock()
	cur := *r.snap
	kbID, claimsID := r.kbID, r.claimsID
	r.mu.RUnlock()

	next := cur
	newKB, err := r.store.GetPointer(ctx, knowledge.KindKB)
	if err != nil {
		return fmt.Errorf("kb pointer: %w", err)
	}
	if newKB != kbID || cur.KB == nil {
		var m knowledge.Manifest
		if err := r.load(ctx, newKB, knowledge.KindKB, &m); err != nil {
			r.invalidate()
			return err
		}
		next.KB, next.Routes = knowledge.NewIndex(m), m.Routes
	}
	newClaims, err := r.store.GetPointer(ctx, claims.KindClaims)
	if err != nil {
		return fmt.Errorf("claims pointer: %w", err)
	}
	if newClaims != claimsID || cur.Claims == nil {
		var m claims.Manifest
		if err := r.load(ctx, newClaims, claims.KindClaims, &m); err != nil {
			r.invalidate()
			return err
		}
		next.Claims = claims.NewResolver(m)
	}
	revs, err := r.store.Revocations(ctx, cur.Watermark)
	if err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	if len(revs) > 0 {
		// Copy-on-write so snapshots held by in-flight turns never change.
		rs := &revocationSet{sources: copySet(cur.rev.sources), claims: copySet(cur.rev.claims)}
		for _, v := range revs {
			if v.TargetKind == "claim" {
				rs.claims[v.TargetID] = true
			} else {
				rs.sources[v.TargetID] = true
			}
			if v.Watermark > next.Watermark {
				next.Watermark = v.Watermark
			}
		}
		next.rev = rs
	}
	r.mu.Lock()
	r.snap, r.kbID, r.claimsID = &next, newKB, newClaims
	r.mu.Unlock()
	return nil
}

func (r *Runtime) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := *r.snap
	s.KB, s.Claims = nil, nil
	r.snap, r.kbID, r.claimsID = &s, "", ""
}

func (r *Runtime) load(ctx context.Context, id, kind string, into any) error {
	rel, err := r.store.GetRelease(ctx, id)
	if err != nil {
		return fmt.Errorf("release %s: %w", id, err)
	}
	if rel.Kind != kind {
		return fmt.Errorf("release %s has kind %s, want %s", id, rel.Kind, kind)
	}
	if err := signing.Verify(r.pubKey, rel.Manifest, rel.Signature); err != nil {
		return fmt.Errorf("release %s: %w", id, err)
	}
	dec := json.NewDecoder(bytes.NewReader(rel.Manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("release %s manifest: %w", id, err)
	}
	var head struct {
		ReleaseID string `json:"release_id"`
		Kind      string `json:"kind"`
	}
	_ = json.Unmarshal(rel.Manifest, &head)
	if head.ReleaseID != id || head.Kind != kind {
		return fmt.Errorf("release %s manifest header mismatch", id)
	}
	return nil
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Snapshot returns the current immutable view, or ErrNotLoaded.
func (r *Runtime) Snapshot() (*Snapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snap.KB == nil || r.snap.Claims == nil {
		return nil, ErrNotLoaded
	}
	return r.snap, nil
}

// Ready is true when verified releases are loaded and revocations were
// refreshed recently enough to be enforceable (Doc 08 §10).
func (r *Runtime) Ready(maxAge time.Duration) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap.KB != nil && r.snap.Claims != nil && r.lastErr == nil && time.Since(r.lastRefresh) <= maxAge
}

// RevokedSince reports whether any of the given source/claim ids was revoked
// after watermark. It reads the ledger directly so it cannot be stale.
func (r *Runtime) RevokedSince(ctx context.Context, watermark int64, sourceIDs, claimIDs []string) (bool, error) {
	revs, err := r.store.Revocations(ctx, watermark)
	if err != nil {
		return true, err // fail closed
	}
	want := map[string]bool{}
	for _, id := range append(append([]string{}, sourceIDs...), claimIDs...) {
		want[id] = true
	}
	for _, v := range revs {
		if want[v.TargetID] {
			return true, nil
		}
	}
	return false, nil
}

// Run refreshes every interval until ctx ends.
func (r *Runtime) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Refresh(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// PublicConfig implements httpapi.ConfigSource from the active releases.
func (r *Runtime) PublicConfig() contract.ConfigResponse {
	cfg := contract.ConfigResponse{
		AssistantDisplayName: "ZoikoSuite assistant", // neutral until brand clearance (DEC-04)
		Capabilities:         contract.Capabilities{},
		SupportedLocales:     []string{"en-US"},
		MarketChoices:        []string{},
		PublicRoutes:         []contract.PublicRoute{},
		PrivacyNoticeKey:     "privacy-notice-pending-dpo",
	}
	f := false
	cfg.Capabilities.Authenticated = &f
	snap, err := r.Snapshot()
	if err != nil {
		return cfg
	}
	cfg.ConfigRevision = snap.KB.ReleaseID() + "/" + snap.Claims.ReleaseID()
	cfg.MarketChoices = snap.Claims.Markets()
	now := time.Now()
	seen := map[string]bool{}
	for _, rt := range snap.Routes {
		if seen[rt.RouteKey] || now.Before(rt.EffectiveFrom) || !now.Before(rt.EffectiveUntil) {
			continue
		}
		if rt.RequiresClaim != "" && !anyMarketVerified(snap, rt.RequiresClaim, now) {
			continue
		}
		seen[rt.RouteKey] = true
		cfg.PublicRoutes = append(cfg.PublicRoutes, contract.PublicRoute{RouteKey: rt.RouteKey, URL: rt.URL})
	}
	sort.Slice(cfg.PublicRoutes, func(i, j int) bool { return cfg.PublicRoutes[i].RouteKey < cfg.PublicRoutes[j].RouteKey })
	return cfg
}

func anyMarketVerified(s *Snapshot, subject string, now time.Time) bool {
	for _, m := range s.Claims.Markets() {
		if s.Claims.HasVerified(subject, m, now, s) {
			return true
		}
	}
	return false
}
