// Package resolver is the ZS-JUR-001 Wave 2 runtime rule resolver (s9, s25, s30).
//
// It answers "which rule applies" from signed, certified pack ARTIFACTS, never
// from live rule rows or a government website. Every pack it consults is
// verified first (artifact digest, recorded digests, registered signing key,
// signature, and the certification's own signature) and the resolver FAILS
// CLOSED: if a pack that covers the jurisdiction cannot be verified, there is
// no answer (JUR-NEG-03, JUR-NEG-04), rather than a silently different one
// from an older or partial pack.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Source is what the resolver needs from the registry.
type Source interface {
	// ListEligiblePacks returns pack versions whose status is in statuses. When
	// ring and region are set, only versions with an ACTIVE deployment in that
	// ring and region are returned: a resolver instance sees only what has been
	// rolled out to it (s23).
	ListEligiblePacks(ctx context.Context, statuses []string, ring, region string) ([]domain.EligiblePack, error)
	// FindPackVersion returns one pack version at any status (for pinned replay).
	FindPackVersion(ctx context.Context, packRef, version string) (*domain.EligiblePack, error)
	// LoadPack reads the artifact, signature, key and certification of a version.
	LoadPack(ctx context.Context, packVersionID string) (*domain.LoadedPack, error)
}

// Config controls pack eligibility and caching.
type Config struct {
	// EligibleStatuses are the pack statuses a resolver may use for NEW
	// decisions. ZS-JUR-001 s23: only RELEASED is production eligible.
	EligibleStatuses []string
	// CacheTTL bounds how long a verified artifact and the eligible-pack
	// list are reused. It is also the longest a key revocation or a
	// withdrawal can go unnoticed unless the cache is invalidated.
	CacheTTL time.Duration
	// OnUnverified is called when a pack fails verification on load, so the
	// caller can emit a security event (JUR-NEG-04).
	OnUnverified func(packRef, version string, reasons []string)
	Now          func() time.Time
	// Ring and Region identify this resolver instance in the rollout. Both
	// empty means no deployment gating (lower environments only).
	Ring   string
	Region string
}

// Request asks which rule applies. PackRef (optionally with PackVersion)
// restricts the search; with both set the exact version is used, for replay
// of a past decision.
type Request struct {
	Jurisdiction string
	RuleDomain   string
	RuleCode     string
	At           time.Time
	PackRef      string
	PackVersion  string
}

// PackInfo identifies the pack release a decision rests on.
type PackInfo struct {
	PackRef         string `json:"pack_ref"`
	Version         string `json:"version"`
	PackVersionID   string `json:"pack_version_id"`
	Status          string `json:"status"`
	ArtifactDigest  string `json:"artifact_digest"`
	CertificationID string `json:"certification_id"`
}

// RuleInfo is the winning rule module.
type RuleInfo struct {
	RuleID           string     `json:"rule_id"`
	JurisdictionCode string     `json:"jurisdiction_code"`
	RuleDomain       string     `json:"rule_domain"`
	RuleCode         string     `json:"rule_code"`
	RuleName         string     `json:"rule_name"`
	EffectiveFrom    time.Time  `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to"`
	Precedence       *int       `json:"precedence"`
	ContentDigest    string     `json:"content_digest"`
	InterpretationID *string    `json:"interpretation_id"`
	Payload          rawJSON    `json:"payload"`
	// Parameters are the rule's typed calculation parameters (nil when it has none).
	Parameters rawJSON `json:"parameters"`
}

type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// SourceInfo is one authoritative source behind the winning rule (s30).
type SourceInfo struct {
	SourceID       string `json:"source_id"`
	Authority      string `json:"authority"`
	SourceType     string `json:"source_type"`
	AuthorityLevel string `json:"authority_level"`
	Title          string `json:"title"`
	SnapshotHash   string `json:"snapshot_hash"`
}

// PackConsulted is one pack the resolver looked at, with its own answer.
type PackConsulted struct {
	PackRef string `json:"pack_ref"`
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	RuleID  string `json:"rule_id,omitempty"`
}

// Decision is the explainable answer: jurisdiction, pack version, rule
// version, sources, basis, and the steps that led there.
type Decision struct {
	Outcome        string          `json:"outcome"`
	EffectiveAt    time.Time       `json:"effective_at"`
	Jurisdiction   string          `json:"jurisdiction"`
	RuleDomain     string          `json:"rule_domain"`
	RuleCode       string          `json:"rule_code"`
	Pack           *PackInfo       `json:"pack,omitempty"`
	Rule           *RuleInfo       `json:"rule,omitempty"`
	Sources        []SourceInfo    `json:"sources,omitempty"`
	Basis          string          `json:"basis,omitempty"`
	Considered     []string        `json:"considered_rule_ids,omitempty"`
	PacksConsulted []PackConsulted `json:"packs_consulted"`
	Explanation    []string        `json:"explanation"`
}

// Failure is one pack that could not be trusted.
type Failure struct {
	PackRef string   `json:"pack_ref"`
	Version string   `json:"version"`
	Reasons []string `json:"reasons"`
}

// UnverifiedError is returned when a pack covering the jurisdiction fails
// verification: the resolver refuses to answer rather than guess.
type UnverifiedError struct{ Failures []Failure }

func (e *UnverifiedError) Error() string {
	return fmt.Sprintf("%d pack version(s) covering the jurisdiction failed verification", len(e.Failures))
}

var (
	ErrPackNotFound      = errors.New("pack version not found")
	ErrPinnedNotEligible = errors.New("the pinned pack version is not eligible for resolution")
	ErrBadRequest        = errors.New("invalid resolution request")
)

type entry struct {
	doc      *domain.ArtifactDoc
	pack     PackInfo
	reasons  []string // empty means verified
	loadedAt time.Time
}

// Resolver resolves rules from verified artifacts.
type Resolver struct {
	src Source
	cfg Config

	mu         sync.Mutex
	eligible   []domain.EligiblePack
	eligibleAt time.Time
	entries    map[string]*entry

	hits, misses, refreshes int
}

// New builds a Resolver. At least one eligible status is required.
func New(src Source, cfg Config) (*Resolver, error) {
	if len(cfg.EligibleStatuses) == 0 {
		return nil, errors.New("resolver needs at least one eligible pack status")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CacheTTL < 0 {
		cfg.CacheTTL = 0
	}
	return &Resolver{src: src, cfg: cfg, entries: map[string]*entry{}}, nil
}

// Scope returns the ring and region this resolver instance serves (empty when ungated).
func (r *Resolver) Scope() (ring, region string) { return r.cfg.Ring, r.cfg.Region }

// Invalidate drops every cached pack and the eligible list, so the next
// resolution re-reads and re-verifies everything.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eligible, r.eligibleAt = nil, time.Time{}
	r.entries = map[string]*entry{}
}

// Stats reports cache behaviour for operations dashboards.
func (r *Resolver) Stats() (hits, misses, refreshes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, r.misses, r.refreshes
}

func (r *Resolver) fresh(t time.Time) bool {
	return r.cfg.CacheTTL > 0 && r.cfg.Now().Sub(t) < r.cfg.CacheTTL
}

func (r *Resolver) eligibleList(ctx context.Context) ([]domain.EligiblePack, error) {
	r.mu.Lock()
	if r.eligible != nil && r.fresh(r.eligibleAt) {
		out := r.eligible
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()
	list, err := r.src.ListEligiblePacks(ctx, r.cfg.EligibleStatuses, r.cfg.Ring, r.cfg.Region)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.EligiblePack{}
	}
	r.mu.Lock()
	r.eligible, r.eligibleAt = list, r.cfg.Now()
	r.refreshes++
	r.mu.Unlock()
	return list, nil
}

// load returns a verified-or-failed entry for a pack version, cached.
func (r *Resolver) load(ctx context.Context, p domain.EligiblePack) (*entry, error) {
	r.mu.Lock()
	if e, ok := r.entries[p.PackVersionID]; ok && r.fresh(e.loadedAt) {
		r.hits++
		r.mu.Unlock()
		return e, nil
	}
	r.misses++
	r.mu.Unlock()

	lp, err := r.src.LoadPack(ctx, p.PackVersionID)
	if err != nil {
		return nil, err
	}
	e := &entry{loadedAt: r.cfg.Now(), pack: PackInfo{PackRef: p.PackRef, Version: p.Version, PackVersionID: p.PackVersionID,
		Status: p.Status, ArtifactDigest: lp.ArtifactDigest}}
	e.reasons = verifyLoaded(lp)
	if lp.Cert != nil {
		e.pack.CertificationID = lp.Cert.CertificationID
	}
	if len(e.reasons) == 0 {
		doc, perr := domain.ParseArtifact([]byte(lp.ArtifactJSON))
		if perr != nil {
			e.reasons = append(e.reasons, "artifact_unreadable")
		} else {
			e.doc = doc
		}
	}
	if len(e.reasons) > 0 && r.cfg.OnUnverified != nil {
		r.cfg.OnUnverified(p.PackRef, p.Version, e.reasons)
	}
	r.mu.Lock()
	r.entries[p.PackVersionID] = e
	r.mu.Unlock()
	return e, nil
}

// verifyLoaded applies every trust check; an empty result means trusted.
func verifyLoaded(lp *domain.LoadedPack) []string { return domain.VerifyLoadedPack(lp) }

// Resolve answers a request. It returns *UnverifiedError (fail closed) when a
// pack that covers the jurisdiction is not trustworthy.
func (r *Resolver) Resolve(ctx context.Context, req Request) (*Decision, error) {
	if strings.TrimSpace(req.Jurisdiction) == "" || strings.TrimSpace(req.RuleDomain) == "" ||
		strings.TrimSpace(req.RuleCode) == "" || req.At.IsZero() {
		return nil, fmt.Errorf("%w: jurisdiction, rule_domain, rule_code and effective_at are required", ErrBadRequest)
	}
	if (req.PackVersion != "") && req.PackRef == "" {
		return nil, fmt.Errorf("%w: pack_version needs pack_ref", ErrBadRequest)
	}
	at := req.At.UTC()
	d := &Decision{EffectiveAt: at, Jurisdiction: req.Jurisdiction, RuleDomain: req.RuleDomain, RuleCode: req.RuleCode,
		PacksConsulted: []PackConsulted{}, Explanation: []string{}}
	step := func(format string, a ...any) { d.Explanation = append(d.Explanation, fmt.Sprintf(format, a...)) }

	entries, early, err := r.pick(ctx, req.Jurisdiction, req.PackRef, req.PackVersion, at, step)
	if err != nil {
		return nil, err
	}
	if early != "" {
		d.Outcome = early
		return d, nil
	}

	type hit struct {
		e   *entry
		res domain.Resolution
	}
	var resolved, ambiguous []hit
	for _, e := range entries {
		res := e.doc.Resolve(req.Jurisdiction, req.RuleDomain, req.RuleCode, at)
		d.PacksConsulted = append(d.PacksConsulted, PackConsulted{PackRef: e.pack.PackRef, Version: e.pack.Version, Outcome: res.Outcome, RuleID: res.RuleID})
		step("pack %s@%s: %s", e.pack.PackRef, e.pack.Version, res.Outcome)
		switch res.Outcome {
		case domain.OutcomeResolved:
			resolved = append(resolved, hit{e, res})
		case domain.OutcomeAmbiguous:
			ambiguous = append(ambiguous, hit{e, res})
		}
	}

	switch {
	case len(ambiguous) > 0:
		d.Outcome = domain.OutcomeAmbiguous
		for _, h := range ambiguous {
			d.Considered = append(d.Considered, h.res.Considered...)
		}
		step("rules overlap with no declared precedence: the resolver blocks instead of choosing")
	case len(resolved) > 1:
		// Two packs both define this rule for this jurisdiction and instant:
		// a cross-pack conflict (JUR-NEG-24) is blocked, not resolved by guess.
		d.Outcome = domain.OutcomeAmbiguous
		for _, h := range resolved {
			d.Considered = append(d.Considered, h.res.RuleID)
		}
		sort.Strings(d.Considered)
		step("%d packs each define %s/%s for %s at this instant: cross-pack conflict, blocked", len(resolved), req.RuleDomain, req.RuleCode, req.Jurisdiction)
	case len(resolved) == 1:
		h := resolved[0]
		d.Outcome = domain.OutcomeResolved
		pack := h.e.pack
		d.Pack = &pack
		d.Basis, d.Considered = h.res.Basis, h.res.Considered
		for i := range h.e.doc.Rules {
			ru := h.e.doc.Rules[i]
			if ru.RuleID != h.res.RuleID {
				continue
			}
			jc := ""
			for _, j := range h.e.doc.Jurisdictions {
				if j.ID == ru.JurisdictionID {
					jc = j.Code
				}
			}
			d.Rule = &RuleInfo{RuleID: ru.RuleID, JurisdictionCode: jc, RuleDomain: ru.RuleDomain, RuleCode: ru.RuleCode, RuleName: ru.RuleName,
				EffectiveFrom: ru.EffectiveFrom, EffectiveTo: ru.EffectiveTo, Precedence: ru.Precedence, ContentDigest: ru.ContentDigest,
				InterpretationID: ru.InterpretationID, Payload: rawJSON(ru.Payload), Parameters: rawJSON(ru.Parameters)}
			for _, sid := range ru.SourceIDs {
				if s, ok := h.e.doc.Sources[sid]; ok {
					d.Sources = append(d.Sources, SourceInfo{SourceID: s.SourceID, Authority: s.Authority, SourceType: s.SourceType,
						AuthorityLevel: s.AuthorityLevel, Title: s.Title, SnapshotHash: s.SnapshotHash})
				}
			}
			sort.Slice(d.Sources, func(a, b int) bool { return d.Sources[a].SourceID < d.Sources[b].SourceID })
		}
		step("rule %s selected by %s", h.res.RuleID, h.res.Basis)
	default:
		d.Outcome = domain.OutcomeNoRule
		step("no rule %s/%s is effective for %s at %s", req.RuleDomain, req.RuleCode, req.Jurisdiction, at.Format(time.RFC3339))
	}
	return d, nil
}

// pinnedAllowed: a pinned replay may use any status eligible for new
// decisions, and SUPERSEDED (s23: historical resolution still permitted).
// WITHDRAWN and EMERGENCY_BLOCKED never resolve.
func (r *Resolver) pinnedAllowed(status string) bool {
	if status == "SUPERSEDED" {
		return true
	}
	for _, s := range r.cfg.EligibleStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// CoverageEntry says which eligible pack versions name a jurisdiction.
type CoverageEntry struct {
	PackRef       string     `json:"pack_ref"`
	Version       string     `json:"version"`
	Status        string     `json:"status"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	InForce       bool       `json:"in_force"`
}

// Coverage lists the eligible pack versions whose scope names the
// jurisdiction and whether each is in force at the instant. It is a read
// model of what is supported, not a legal conclusion about any transaction.
func (r *Resolver) Coverage(ctx context.Context, jurisdiction string, at time.Time) ([]CoverageEntry, error) {
	list, err := r.eligibleList(ctx)
	if err != nil {
		return nil, err
	}
	out := []CoverageEntry{}
	for _, p := range list {
		if p.Covers(jurisdiction) {
			out = append(out, CoverageEntry{PackRef: p.PackRef, Version: p.Version, Status: p.Status,
				EffectiveFrom: p.EffectiveFrom, EffectiveTo: p.EffectiveTo, InForce: p.InForce(at.UTC())})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].PackRef != out[b].PackRef {
			return out[a].PackRef < out[b].PackRef
		}
		return domain.CompareVersions(out[a].Version, out[b].Version) < 0
	})
	return out, nil
}

// pick selects the pack versions that may answer a request and verifies every
// one of them BEFORE any is used. It returns no entries and an early outcome
// (UNSUPPORTED_JURISDICTION or NO_RULE) when there is nothing to consult, and
// *UnverifiedError when a covering pack cannot be trusted. It is shared by rule
// resolution and obligation calculation so both obey the same eligibility,
// version-selection, ring/region and fail-closed rules.
func (r *Resolver) pick(ctx context.Context, jurisdiction, packRef, packVersion string, at time.Time, step func(string, ...any)) ([]*entry, string, error) {
	var candidates []domain.EligiblePack
	if packRef != "" && packVersion != "" {
		p, err := r.src.FindPackVersion(ctx, packRef, packVersion)
		if err != nil {
			return nil, "", err
		}
		if !r.pinnedAllowed(p.Status) {
			return nil, "", fmt.Errorf("%w: %s@%s is %s", ErrPinnedNotEligible, p.PackRef, p.Version, p.Status)
		}
		step("replaying pinned pack %s@%s (status %s)", p.PackRef, p.Version, p.Status)
		if !p.Covers(jurisdiction) {
			step("jurisdiction %s is not in the scope of the pinned pack", jurisdiction)
			return nil, domain.OutcomeUnsupported, nil
		}
		if !p.InForce(at) {
			step("the pinned pack is not in force at %s", at.Format(time.RFC3339))
			return nil, domain.OutcomeNoRule, nil
		}
		candidates = []domain.EligiblePack{*p}
	} else {
		list, err := r.eligibleList(ctx)
		if err != nil {
			return nil, "", err
		}
		var covering []domain.EligiblePack
		for _, p := range list {
			if (packRef == "" || p.PackRef == packRef) && p.Covers(jurisdiction) {
				covering = append(covering, p)
			}
		}
		if len(covering) == 0 {
			step("no eligible pack (statuses %v) covers jurisdiction %s: unsupported, no treatment is guessed", r.cfg.EligibleStatuses, jurisdiction)
			return nil, domain.OutcomeUnsupported, nil
		}
		// Per pack, the highest version in force at the instant.
		best := map[string]domain.EligiblePack{}
		for _, p := range covering {
			if !p.InForce(at) {
				continue
			}
			if cur, ok := best[p.PackRef]; !ok || domain.CompareVersions(p.Version, cur.Version) > 0 {
				best[p.PackRef] = p
			}
		}
		if len(best) == 0 {
			step("%d pack(s) cover %s but none is in force at %s", len(covering), jurisdiction, at.Format(time.RFC3339))
			return nil, domain.OutcomeNoRule, nil
		}
		for _, p := range best {
			candidates = append(candidates, p)
		}
		sort.Slice(candidates, func(a, b int) bool { return candidates[a].PackRef < candidates[b].PackRef })
	}

	entries := make([]*entry, 0, len(candidates))
	var failures []Failure
	for _, p := range candidates {
		e, err := r.load(ctx, p)
		if err != nil {
			return nil, "", err
		}
		if len(e.reasons) > 0 {
			failures = append(failures, Failure{PackRef: p.PackRef, Version: p.Version, Reasons: e.reasons})
			continue
		}
		entries = append(entries, e)
	}
	if len(failures) > 0 {
		return nil, "", &UnverifiedError{Failures: failures}
	}
	return entries, "", nil
}
