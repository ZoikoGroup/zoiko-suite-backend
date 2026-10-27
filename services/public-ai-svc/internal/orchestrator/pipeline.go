package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/store"
	"zoiko.io/public-ai-svc/internal/validate"
)

// MaxCitations is how many citations an answer shows (Doc 01 §05 FR-08:
// 1–3 verified links), below the schema bound of 5 (ADR-005).
const MaxCitations = 3

// Releases is what the pipeline needs from the release runtime.
type Releases interface {
	Snapshot() (*release.Snapshot, error)
	RevokedSince(ctx context.Context, watermark int64, sourceIDs, claimIDs []string) (bool, error)
}

// FlagSource supplies the effective flags.
type FlagSource interface{ Values() flags.Values }

// Pipeline is the production Answerer.
type Pipeline struct {
	Releases Releases
	Flags    FlagSource
	Model    model.Gateway
	Store    store.Store // conflict records
	Log      *zap.Logger
	Now      func() time.Time
	// MaxOutputTokens bounds generation (token budget per turn).
	MaxOutputTokens int
}

// Security signal reason classes (no prompt content).
const (
	SignalInjection = "PROMPT_INJECTION"
	SignalSensitive = "SENSITIVE_INPUT"
	SignalSecurity  = "SECURITY_REPORT"
)

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Pipeline) log() *zap.Logger {
	if p.Log == nil {
		return zap.NewNop()
	}
	return p.Log
}

// Answer runs the fail-closed pipeline (Doc 05 §11, Doc 06 §08).
func (p *Pipeline) Answer(ctx context.Context, in Input) Result {
	now := p.now()
	fv := p.Flags.Values()
	snap, err := p.Releases.Snapshot()
	if err != nil {
		// No verified knowledge: never fall back to model memory (Doc 04 §08).
		return p.fromTemplate(policy.TplUnavailable, "UNKNOWN", nil, "", now, "none")
	}
	releaseID := snap.KB.ReleaseID()
	if !fv.PublicAIEnabled {
		return p.fromTemplate(policy.TplUnavailable, "UNKNOWN", snap, "", now, releaseID)
	}

	dec := policy.Classify(in.Message)
	market := dec.Slots.MarketCode
	if market == "" {
		market = in.MarketCode
	}

	// Exceptions are answered from pre-approved templates before retrieval.
	if dec.Exception != "" {
		res := p.fromTemplate(policy.ExceptionTemplates[dec.Exception], dec.Exception, snap, market, now, releaseID)
		switch dec.Exception {
		case policy.X06PromptInjection:
			res.SecuritySignal = SignalInjection
		case policy.X03SensitiveInput:
			res.SecuritySignal = SignalSensitive
		case policy.X05SecurityEscalate:
			res.SecuritySignal = SignalSecurity
		}
		return res
	}
	intent := dec.Primary
	if fv.DisabledIntents[intent] {
		return p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
	}
	if tpl, ok := policy.RouteTemplates[intent]; ok {
		return p.fromTemplate(tpl, intent, snap, market, now, releaseID)
	}
	// P1 claims first (Doc 05 §11 step 3).
	var verified []claims.Claim
	if req, ok := claimRequest(intent, dec, in.Message, snap.Claims, market, now); ok {
		if (req.Subject == claims.Price || req.Subject == claims.Trial) && !fv.PricingClaimsEnabled {
			return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, "pricing"), intent, snap, market, now, releaseID)
		}
		d := snap.Claims.Resolve(req, snap)
		switch d.Decision {
		case claims.MarketRequired:
			return p.fromTemplate(policy.TplClarifyMarket, intent, snap, market, now, releaseID)
		case claims.Contradicted:
			p.recordConflict(ctx, req, d, now)
			return p.fromTemplate(withRoute(policy.TplAbstainConflict, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		case claims.RevokedDecision, claims.ExpiredDecision:
			return p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		case claims.InsufficientEvidence:
			return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		case claims.Verified:
			verified = d.Verified
		}
	}

	// P2/P3 retrieval with mandatory filters (only when no claim answers it).
	var hits []knowledge.Hit
	if len(verified) == 0 {
		r := snap.KB.Search(knowledge.Query{Text: dec.Redaction.Text, Market: market, Locale: in.Locale, Now: now, MaxChunks: 4}, snap)
		switch r.Answerability {
		case knowledge.Answerable:
			hits = r.Hits
		case knowledge.Stale, knowledge.Revoked:
			return p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		default:
			if intent == "I29" {
				// Unclassified and nothing approved matches: out of scope.
				return p.fromTemplate(policy.TplOutOfScope, intent, snap, market, now, releaseID)
			}
			return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		}
	}

	cards := make([]model.ClaimCard, 0, len(verified))
	for _, c := range verified {
		cards = append(cards, model.ClaimCard{Ref: c.ClaimID, Statement: c.PublicStatement})
	}
	evidence := make([]model.Evidence, 0, len(hits))
	for _, h := range hits {
		evidence = append(evidence, model.Evidence{Ref: h.Chunk.ChunkID, Title: h.Chunk.DisplayTitle, Text: h.Chunk.PlainText})
	}

	var text string
	var draft model.Draft
	staticOnly := !fv.ModelGenerationEnabled || fv.FallbackMode == "static"
	switch {
	case staticOnly && len(cards) > 0:
		// Signed claim statements are static, pre-approved content.
		draft = model.Draft{ClaimRefs: refs(cards)}
		text = joinStatements(cards)
	case staticOnly:
		return p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
	default:
		text, draft, err = p.generate(ctx, intent, dec.Redaction.Text, cards, evidence)
		if err != nil {
			if ctx.Err() != nil || isProviderError(err) {
				return p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
			}
			return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		}
	}

	citations, sourceIDs := p.citations(snap, verified, hits, draft)
	if len(citations) == 0 {
		return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
	}
	// Revocation check at commit (Doc 08 §06): fail closed on any doubt.
	if revoked, err := p.Releases.RevokedSince(ctx, snap.Watermark, sourceIDs, refs(cards)); revoked || err != nil {
		return p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
	}

	routeKey := policy.DefaultRoute[intent]
	if intent == "I06" && len(verified) > 0 && fv.TrialCTAEnabled {
		routeKey = "signup_or_trial"
	}
	return Result{
		Status: contract.Answered, IntentID: intent, Text: text, Citations: citations,
		Actions: p.actions(snap, routeKey, market, now), KnowledgeReleaseID: releaseID,
		Tokens: int64(draft.InputTokens + draft.OutputTokens),
	}
}

func (p *Pipeline) generate(ctx context.Context, intent, msg string, cards []model.ClaimCard, evidence []model.Evidence) (string, model.Draft, error) {
	req := model.Request{PolicyVersion: "pub-policy-1.0", IntentID: intent, Message: msg, Claims: cards, Evidence: evidence, MaxOutputTokens: p.MaxOutputTokens}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // one repair attempt within budget
		d, err := p.Model.Generate(ctx, req)
		if err != nil {
			return "", model.Draft{}, providerError{err}
		}
		text, verr := validate.Check(d, cards, evidence)
		if verr == nil {
			return text, d, nil
		}
		p.log().Warn("draft rejected by output validation", zap.String("reason", verr.Error()), zap.Int("attempt", attempt))
		lastErr = verr
		req.RepairNote = "Previous draft was rejected: " + verr.Error()
	}
	return "", model.Draft{}, lastErr
}

type providerError struct{ error }

func isProviderError(err error) bool {
	_, ok := err.(providerError)
	return ok
}

// citations returns ≤3 citations backed by what the answer actually used,
// plus the source ids to re-check at commit.
func (p *Pipeline) citations(snap *release.Snapshot, verified []claims.Claim, hits []knowledge.Hit, d model.Draft) ([]contract.Citation, []string) {
	var out []contract.Citation
	var ids []string
	bySource := map[string]int{}
	add := func(svid, urlKey, claimID string) {
		if i, ok := bySource[svid]; ok {
			if claimID != "" {
				out[i].ClaimIDs = append(out[i].ClaimIDs, claimID)
			}
			return
		}
		if len(out) == MaxCitations {
			return
		}
		c := contract.Citation{SourceVersionID: svid, URLKey: urlKey, ClaimIDs: []string{}}
		if claimID != "" {
			c.ClaimIDs = append(c.ClaimIDs, claimID)
		}
		bySource[svid] = len(out)
		out = append(out, c)
		if s, ok := snap.KB.Source(svid); ok {
			ids = append(ids, s.SourceID)
		}
		ids = append(ids, svid)
	}
	used := map[string]bool{}
	for _, r := range d.ClaimRefs {
		used[r] = true
	}
	for _, c := range verified {
		if used[c.ClaimID] && len(c.EvidenceSourceVersionIDs) > 0 {
			add(c.EvidenceSourceVersionIDs[0], c.CitationURLKey, c.ClaimID)
		}
	}
	usedEv := map[string]bool{}
	for _, r := range d.EvidenceRefs {
		usedEv[r] = true
	}
	for _, h := range hits {
		if usedEv[h.Chunk.ChunkID] {
			add(h.Chunk.SourceVersionID, h.Chunk.URLKey, "")
		}
	}
	return out, ids
}

func (p *Pipeline) actions(snap *release.Snapshot, routeKey, market string, now time.Time) []contract.Action {
	if routeKey == "" || snap == nil {
		return []contract.Action{}
	}
	if _, ok := snap.Route(routeKey, market, now); !ok {
		return []contract.Action{} // missing/unpublished route → hide the CTA
	}
	return []contract.Action{{Type: contract.ActionOpenRoute, RouteKey: routeKey}}
}

func (p *Pipeline) fromTemplate(t policy.Template, intent string, snap *release.Snapshot, market string, now time.Time, releaseID string) Result {
	res := Result{Status: t.Status, IntentID: intent, Text: t.Text, Citations: []contract.Citation{},
		Actions: p.actions(snap, t.RouteKey, market, now), KnowledgeReleaseID: releaseID}
	if t.Human {
		res.Flags = &contract.Flags{RequiresHuman: true}
	}
	return res
}

func (p *Pipeline) recordConflict(ctx context.Context, req claims.Request, d claims.Decision, now time.Time) {
	if p.Store == nil {
		return
	}
	key := strings.Join([]string{req.Subject, req.SubjectID, req.Market, req.BillingTerm}, "|")
	sum := sha256.Sum256([]byte(key + "|" + strings.Join(d.ConflictKeys, ",")))
	err := p.Store.RecordConflict(ctx, store.Conflict{
		ConflictID: "conf-" + hex.EncodeToString(sum[:8]), SubjectKey: key, ClaimIDs: d.ConflictKeys,
		DetectionType: "AUTHORITY_MISMATCH", Severity: "HIGH", ResolutionState: "OPEN", DetectedAt: now,
	})
	if err != nil {
		p.log().Error("conflict record failed", zap.Error(err))
	}
}

func withRoute(t policy.Template, route string) policy.Template {
	if t.RouteKey == "" {
		t.RouteKey = route
	}
	return t
}

func refs(cards []model.ClaimCard) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Ref)
	}
	return out
}

func joinStatements(cards []model.ClaimCard) string {
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		parts = append(parts, c.Statement)
	}
	return strings.Join(parts, " ")
}

var (
	soc2Re = regexp.MustCompile(`(?i)\bsoc\s*-?\s*2\b`)
	isoRe  = regexp.MustCompile(`(?i)\biso\s*/?\s*(?:iec\s*)?27001\b`)
)

// claimRequest maps an intent to the P1 subject it must be answered from.
func claimRequest(intent string, dec policy.Decision, msg string, reg *claims.Resolver, market string, now time.Time) (claims.Request, bool) {
	q := claims.Request{Market: market, AsOf: now}
	switch intent {
	case "I05", "I07":
		q.Subject, q.SubjectID, q.BillingTerm = claims.Price, reg.PlanCode(dec.Slots.PlanName), dec.Slots.BillingCadence
	case "I06":
		q.Subject, q.SubjectID = claims.Trial, reg.PlanCode(dec.Slots.PlanName)
	case "I04":
		q.Subject, q.SubjectID = claims.MarketAvailability, "zoikosuite"
	case "I14":
		q.Subject = claims.Residency
	case "I12":
		switch {
		case soc2Re.MatchString(msg):
			q.Subject, q.SubjectID = claims.Certification, "SOC2"
		case isoRe.MatchString(msg):
			q.Subject, q.SubjectID = claims.Certification, "ISO27001"
		case strings.Contains(strings.ToLower(msg), "certif"):
			q.Subject = claims.Certification
		default:
			return q, false // general security questions come from approved trust pages
		}
	case "I10":
		if dec.Slots.Integration == "" {
			return q, false
		}
		q.Subject, q.SubjectID = claims.IntegrationStatus, dec.Slots.Integration
	default:
		return q, false
	}
	if (q.Subject == claims.Price || q.Subject == claims.Trial) && dec.Slots.PlanName != "" && q.SubjectID == "" {
		q.SubjectID = "UNKNOWN_PLAN" // a plan the registry does not know is never matched
	}
	return q, true
}
