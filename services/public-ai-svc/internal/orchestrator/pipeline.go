package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/store"
	"zoiko.io/public-ai-svc/internal/telemetry"
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
	SignalInjection       = "PROMPT_INJECTION"
	SignalSensitive       = "SENSITIVE_INPUT"
	SignalSecurity        = "SECURITY_REPORT"
	SignalToolInstruction = "TOOL_INSTRUCTION"
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
// Answer runs one turn inside a span that records the outcome, the release
// and retrieval counts: identifiers and enums only, never message or source
// text (Doc 08 §14).
func (p *Pipeline) Answer(ctx context.Context, in Input) Result {
	ctx, span := telemetry.Tracer().Start(ctx, "public_ai.turn")
	defer span.End()
	r := p.answer(ctx, in)
	span.SetAttributes(
		attribute.String("suivo.outcome", string(r.Status)),
		attribute.String("suivo.reason", r.Reason),
		attribute.String("suivo.intent", r.IntentID),
		attribute.String("suivo.release.kb", r.KnowledgeReleaseID),
		attribute.String("suivo.release.policy", model.PromptVersion),
		attribute.String("suivo.answer_class", r.AnswerClass),
		attribute.String("suivo.retrieval.mode", r.Diag.RetrievalMode),
		attribute.Int("suivo.retrieval.count", len(r.Diag.EvidenceIDs)),
		attribute.StringSlice("suivo.retrieval.chunk_ids", r.Diag.EvidenceIDs),
		attribute.String("suivo.claim.verdict", r.Diag.ClaimVerdict),
		attribute.Int("suivo.citation.count", len(r.Citations)),
		attribute.Bool("suivo.draft_content", r.Draft),
	)
	if r.Status == contract.Unavailable {
		telemetry.Fail(span, "unavailable")
	}
	return r
}

func (p *Pipeline) answer(ctx context.Context, in Input) Result {
	now := p.now()
	fv := p.Flags.Values()
	snap, err := p.Releases.Snapshot()
	if err != nil {
		// No verified knowledge: never fall back to model memory (Doc 04 §08).
		return p.fromTemplate(policy.TplUnavailable, "UNKNOWN", nil, "", now, "none")
	}
	releaseID := snap.KB.ReleaseID()
	if !fv.PublicAIEnabled {
		return because(p.fromTemplate(policy.TplUnavailable, "UNKNOWN", snap, "", now, releaseID), contract.ReasonKillSwitch)
	}

	dec := policy.Classify(in.Message)
	market := dec.Slots.MarketCode
	if market == "" {
		market = in.MarketCode
	}

	// Exceptions are answered from pre-approved templates before retrieval.
	if dec.Exception == policy.X01PrivateAction && policy.IsLivePlatform(dec.Redaction.Text) {
		// Live platform state, tenant data or dashboard counts: never visible
		// to the public assistant; no model call, no CTA, two public chips.
		res := p.fromTemplate(policy.TplLivePlatform, dec.Exception, snap, market, now, releaseID)
		res.Suggestions = policy.SuggestLivePlatform
		return res
	}
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
	if policy.IsGreeting(in.Message) && in.Pending == nil {
		return p.fromTemplate(policy.TplWelcome, "I01", snap, market, now, releaseID)
	}
	intent := dec.Primary
	if intent == "I29" && policy.IsModuleQuestion(dec.Redaction.Text) {
		// Fallback ladder: rules -> (model classifier, none in R0) -> a named
		// area, module or alias -> only then out of scope.
		intent = "I02"
	}
	diag := Diag{Slots: slotMap(dec.Slots, market)}
	unverifiedConnector := false
	clarifications := 0
	carried := false
	secondary := dec.Secondary
	// A message without its own intent continues the open question only when
	// it supplies something for it (a market, plan or billing term). Anything
	// else is a new question, so an unrelated message never gets the same
	// clarifying question again (Doc 02 §10, G34).
	supplies := dec.Slots.MarketCode != "" || dec.Slots.PlanName != "" || dec.Slots.BillingCadence != ""
	if pd := in.Pending; pd != nil && (intent == pd.Intent || ((intent == "I29" || intent == "I04") && supplies) ||
		policy.IsGeneralOverview(in.Message) || policy.IsOtherMarket(in.Message)) {
		// A reply to our clarifying question, or a follow-up to an answer
		// ("what about the UK, monthly?"): keep the original question's
		// intent; after a clarification also its slots.
		intent, secondary, clarifications, carried = pd.Intent, pd.Secondary, pd.Clarifications, true
		if pd.Answered {
			pd = &Pending{Intent: pd.Intent, Secondary: pd.Secondary}
		}
		if dec.Slots.PlanName == "" {
			dec.Slots.PlanName = pd.PlanName
		}
		if dec.Slots.BillingCadence == "" {
			dec.Slots.BillingCadence = pd.BillingCadence
		}
		if dec.Slots.Integration == "" {
			dec.Slots.Integration = pd.Integration
		}
	}
	// "Show general overview": answer the pending question without the
	// missing slot, from approved narrative only (no market-scoped claims).
	overview := carried && policy.IsGeneralOverview(in.Message)
	if carried && policy.IsOtherMarket(in.Message) {
		// "Other": a country we hold no verified claims for. Never guessed.
		// It is not a dead end (Doc 02 escalation matrix: unsupported market
		// -> demo): the question stays open so "Show general overview" still
		// answers it, and the approved next steps are offered.
		res := p.fromTemplate(policy.TplAbstainCoverage, intent, snap, "", now, releaseID)
		res.Suggestions = policy.SuggestCoverage
		res.Pending = &Pending{Intent: intent, Secondary: secondary, PlanName: dec.Slots.PlanName,
			BillingCadence: dec.Slots.BillingCadence, Clarifications: clarifications}
		return res
	}
	// "What plans are there, and is there a trial?" needs both answers, each
	// from its own typed claim for the same market (Master prompt T1-T3).
	plansAndTrial := (intent == "I05" && secondary == "I06") || (intent == "I06" && secondary == "I05")
	if !plansAndTrial {
		secondary = ""
	}
	if fv.DisabledIntents[intent] {
		return because(p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), contract.ReasonIntentDisabled)
	}
	if tpl, ok := policy.RouteTemplates[intent]; ok {
		return p.fromTemplate(tpl, intent, snap, market, now, releaseID)
	}
	// P1 claims first (Doc 05 §11 step 3).
	var verified []claims.Claim
	if intent == "I04" && market == "" {
		// "Which countries do you support?": list every market whose
		// availability is VERIFIED, verbatim from the signed registry.
		for _, m := range snap.Claims.Markets() {
			d := snap.Claims.Resolve(claims.Request{Subject: claims.MarketAvailability, SubjectID: "zoikosuite", Market: m, AsOf: now}, snap)
			if d.Decision == claims.Verified {
				verified = append(verified, d.Verified...)
			}
		}
	} else if reqs := claimRequests(intent, plansAndTrial, dec, in.Message, snap.Claims, market, now, carried); len(reqs) > 0 && !overview {
		for _, req := range reqs {
			if (req.Subject == claims.Price || req.Subject == claims.Trial) && !fv.PricingClaimsEnabled {
				return because(p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, "pricing"), intent, snap, market, now, releaseID), contract.ReasonPricingDisabled)
			}
			d := snap.Claims.Resolve(req, snap)
			diag.ClaimVerdict = d.Decision
			switch d.Decision {
			case claims.MarketRequired:
				if clarifications >= 2 {
					// Two failed clarifications: one verified route, no third question (Doc 02 §10).
					return p.fromTemplate(withRoute(policy.TplClarifyGiveUp, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
				}
				res := p.fromTemplate(policy.TplClarifyMarket, intent, snap, market, now, releaseID)
				res.StatusLabel, res.SecondaryIntentID = "MARKET_DEPENDENT", secondary
				res.Clarification = &contract.Clarification{Slot: "market_code", Question: policy.TplClarifyMarket.Text,
					Options: append(snap.Claims.Markets(), contract.OtherOption, contract.OverviewOption)}
				res.Pending = &Pending{Intent: intent, Secondary: secondary, PlanName: dec.Slots.PlanName, BillingCadence: dec.Slots.BillingCadence,
					Integration: dec.Slots.Integration, Clarifications: clarifications + 1}
				return res
			case claims.Contradicted:
				p.recordConflict(ctx, req, d, now)
				res := p.fromTemplate(withRoute(policy.TplAbstainConflict, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
				return keepAsking(res, snap, market, &Pending{Intent: intent, Secondary: secondary, PlanName: dec.Slots.PlanName,
					BillingCadence: dec.Slots.BillingCadence, Clarifications: clarifications})
			case claims.RevokedDecision, claims.ExpiredDecision:
				pd := &Pending{Intent: intent, Secondary: secondary, PlanName: dec.Slots.PlanName,
					BillingCadence: dec.Slots.BillingCadence, Clarifications: clarifications}
				if req.Subject == claims.Trial {
					return keepAsking(because(p.trialStale(intent, snap, market, now, releaseID), d.ReasonCode), snap, market, pd)
				}
				return keepAsking(because(p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), d.ReasonCode), snap, market, pd)
			case claims.InsufficientEvidence:
				if req.Subject == claims.IntegrationStatus {
					// No signed status for this connector: explain the capability
					// from approved articles, but the status stays Unverified.
					unverifiedConnector = true
					continue
				}
				if req.Subject == claims.PlanAvailability {
					// No plans are verified for this market: coverage is not assumed.
					return p.fromTemplate(policy.TplAbstainCoverage, intent, snap, market, now, releaseID)
				}
				if req.Subject == claims.Certification {
					// Doc 02 T10: due-diligence route, never an asserted status.
					return p.fromTemplate(policy.TplAbstainCertification, intent, snap, market, now, releaseID)
				}
				if req.Subject == claims.Residency {
					// Doc 02 T11 (G11): the approved residency wording, never a
					// single-country guarantee and never a bare "couldn't verify".
					return p.fromTemplate(policy.BoundaryTemplates["I14"], intent, snap, market, now, releaseID)
				}
				if (req.Subject == claims.Price || req.Subject == claims.Trial) && market != "" && !slices.Contains(snap.Claims.Markets(), market) {
					// G06: a named country with no claims at all is not covered
					// (as for "Other"); the markets with claims and the general
					// overview are offered, and the question stays open.
					res := p.fromTemplate(policy.TplAbstainCoverage, intent, snap, market, now, releaseID)
					return keepAsking(res, snap, market, &Pending{Intent: intent, Secondary: secondary,
						PlanName: dec.Slots.PlanName, BillingCadence: dec.Slots.BillingCadence, Clarifications: clarifications})
				}
				route := policy.DefaultRoute[intent]
				if (req.Subject == claims.Price || req.Subject == claims.Trial) && market != "" {
					route = "demo" // KB-025, A09: unsupported market or disabled trial → verified sales route
				}
				return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, route), intent, snap, market, now, releaseID)
			case claims.Verified:
				verified = append(verified, d.Verified...)
			}
		}
	}

	// P2/P3 retrieval with mandatory filters (only when no claim answers it).
	var hits []knowledge.Hit
	if len(verified) == 0 {
		qtext := dec.Redaction.Text
		if overview {
			// The overview reply carries no search terms of its own; search
			// the fixed general question for the pending intent instead.
			qtext = overviewQuery[intent]
		}
		kq := knowledge.Query{Text: qtext, Market: market, Locale: in.Locale, Now: now, MaxChunks: 4, IntentID: intent}
		r := snap.Retriever.Retrieve(ctx, kq, snap)
		if r.Answerability == knowledge.NotFound && intent != "I29" {
			r = snap.Retriever.ByIntent(kq, snap)
		}
		diag.RetrievalMode, diag.CandidatesBeforeACL, diag.CandidatesAfterACL, diag.TopScore =
			r.Mode, r.CandidatesBeforeACL, r.CandidatesAfterACL, r.TopScore
		for _, h := range r.Hits {
			diag.EvidenceIDs = append(diag.EvidenceIDs, h.Chunk.ChunkID)
		}
		// Doc 02 boundary reply when no retrieved article is about this
		// intent: never answer residency, roadmap, accessibility or AI from
		// an article that only shares a word with the question.
		// G14: admissibility or assurance is answered only from a passage
		// that addresses it; otherwise Doc 02 T14, never unrelated text.
		if policy.AssuranceQuestionRe.MatchString(dec.Redaction.Text) && !mentions(r.Hits, "admissib") && r.Answerability != knowledge.Revoked {
			res := p.fromTemplate(policy.TplEvidenceAssurance, intent, snap, market, now, releaseID)
			res.Diag = diag
			return res
		}
		if t, ok := policy.BoundaryTemplates[intent]; ok && !anyTagged(r.Hits, intent) && r.Answerability != knowledge.Revoked {
			res := p.fromTemplate(t, intent, snap, market, now, releaseID)
			res.Diag = diag
			return res
		}
		switch r.Answerability {
		case knowledge.Answerable:
			hits = withNeighbor(snap.KB, r.Hits, kq, snap)
			if regionalIntents[intent] && market != "" && !hasMarketEvidence(hits, market) {
				// G12/G13: a named country needs country-specific evidence; never assume coverage.
				return regionalCoverage(p.fromTemplate(policy.TplAbstainCoverage, intent, snap, market, now, releaseID))
			}
		case knowledge.Stale:
			return p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
		case knowledge.Revoked:
			return because(p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), contract.ReasonSourceRevoked)
		default:
			if regionalIntents[intent] && market != "" {
				// G13: a named country with no approved material at all is not
				// assumed covered (same reply as G12 with material elsewhere).
				return regionalCoverage(p.fromTemplate(policy.TplAbstainCoverage, intent, snap, market, now, releaseID))
			}
			if intent == "I29" {
				// Off-topic and nothing approved matches: out of scope, with
				// the help route (Doc 02 G34).
				return p.fromTemplate(policy.TplOutOfScope, intent, snap, market, now, releaseID)
			}
			// A help or support question with no approved guide: the support
			// route reply (Doc 02 T16), never "which area?" (G26).
			if supportOnly(intent) != "" {
				return p.fromTemplate(policy.RouteTemplates["I23"], intent, snap, market, now, releaseID)
			}
			// A ZoikoSuite question with no approved content yet: one question
			// and area chips.
			res := p.fromTemplate(withRoute(policy.TplNoApprovedInfo, supportOnly(intent)), intent, snap, market, now, releaseID)
			res.Suggestions, res.Diag = policy.SuggestAreas, diag
			return res
		}
	}

	cards := make([]model.ClaimCard, 0, len(verified))
	for _, c := range verified {
		cards = append(cards, model.ClaimCard{Ref: c.ClaimID, Statement: c.PublicStatement})
	}
	evidence := make([]model.Evidence, 0, len(hits))
	for _, h := range hits {
		approval := "APPROVED"
		if h.Chunk.Draft {
			approval = knowledge.ApprovalDraft
		}
		evidence = append(evidence, model.Evidence{
			Ref: h.Chunk.ChunkID, Title: h.Chunk.DisplayTitle, Text: h.Chunk.PlainText,
			SourceID: h.Chunk.SourceID, Authority: h.Chunk.AuthorityTier, Approval: approval,
			ValidUntil: h.Chunk.ValidUntil.UTC().Format(time.RFC3339), Markets: strings.Join(h.Chunk.MarketCodes, ","),
		})
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
		return because(p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), contract.ReasonGenerationDisabled)
	default:
		text, draft, err = p.generate(ctx, intent, modelQuestion(dec.Redaction.Text, overview, intent), cards, evidence)
		if errors.Is(err, validate.ErrToolInstruction) {
			// CT-033: no tool exists to invoke; the draft is discarded and a safety signal recorded.
			res := p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
			res.SecuritySignal = SignalToolInstruction
			return because(res, contract.ReasonToolInstruction)
		}
		if err != nil {
			if ctx.Err() != nil || isProviderError(err) {
				reason := contract.ReasonModelUnavailable
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					reason = contract.ReasonModelTimeout
				}
				return because(p.fromTemplate(withRoute(policy.TplUnavailable, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), reason)
			}
			return because(p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), contract.ReasonValidationFailed)
		}
	}

	citations, sourceIDs := p.citations(snap, verified, hits, draft)
	if len(citations) == 0 {
		return p.fromTemplate(withRoute(policy.TplAbstainNoEvidence, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID)
	}
	// CT-048: a kill switch thrown while this turn ran withholds the answer.
	if fv2 := p.Flags.Values(); !fv2.PublicAIEnabled || (!staticOnly && !fv2.ModelGenerationEnabled) {
		return because(p.fromTemplate(policy.TplUnavailable, intent, snap, market, now, releaseID), contract.ReasonKillSwitch)
	}
	// Revocation check at commit (Doc 08 §06): fail closed on any doubt.
	if revoked, err := p.Releases.RevokedSince(ctx, snap.Watermark, sourceIDs, refs(cards)); revoked || err != nil {
		return because(p.fromTemplate(withRoute(policy.TplAbstainStale, policy.DefaultRoute[intent]), intent, snap, market, now, releaseID), contract.ReasonSourceRevoked)
	}

	routeKey := policy.DefaultRoute[intent]
	if intent == "I06" && !plansAndTrial && len(verified) > 0 && fv.TrialCTAEnabled {
		routeKey = "signup_or_trial"
	}
	res := Result{
		Status: contract.Answered, IntentID: intent, Text: text, Citations: citations,
		Actions: p.actions(snap, routeKey, market, now), KnowledgeReleaseID: releaseID,
		Tokens: int64(draft.InputTokens + draft.OutputTokens), Reason: contract.ReasonAnswered, SecondaryIntentID: secondary,
	}
	res.AnswerClass, res.StatusLabel, res.MarketCode = qualify(verified, market)
	switch {
	case overview:
		res.StatusLabel = "MARKET_DEPENDENT" // the general answer; terms depend on the country
	case unverifiedConnector:
		res.StatusLabel, res.AnswerClass = "Unverified", contract.StatusQualified
	}
	// Local/dev only: answered from articles owners have not approved yet.
	res.Draft = len(verified) == 0 && anyDraft(hits)
	res.Diag = diag
	if (intent == "I05" || intent == "I06") && len(verified) == 0 {
		// Plans/trials explained without a market: a reply naming a country
		// ("uk") should get that market's verified prices or trial next.
		res.Pending = &Pending{Intent: intent, PlanName: dec.Slots.PlanName, BillingCadence: dec.Slots.BillingCadence}
	} else if (intent == "I05" || intent == "I06") && market != "" {
		// Answered for one market: "what about the UK, monthly?" asks the
		// same question again for the new market and billing only (G05).
		res.Pending = &Pending{Intent: intent, Secondary: secondary, Answered: true}
	}
	return res
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
		p.log().Warn("draft rejected by output validation", zap.String("reason", verr.Error()), zap.Int("attempt", attempt),
			zap.String("prompt_version", model.PromptVersion))
		lastErr = verr
		req.RepairNote = "Previous draft was rejected: " + verr.Error()
	}
	return "", model.Draft{}, lastErr
}

type providerError struct{ error }

// Unwrap lets the timeout check see context.DeadlineExceeded.
func (e providerError) Unwrap() error { return e.error }

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

// qualify derives the answer class and governed status label from the claims
// that backed an ANSWERED turn (Master prompt §4.1). Labels come only from the
// registry's normalized value; an unknown value shows no label.
func qualify(verified []claims.Claim, market string) (class, label, marketCode string) {
	if len(verified) == 0 {
		return contract.ProcessExplanation, "", ""
	}
	class = contract.VerifiedFact
	for _, c := range verified {
		if l := statusLabels[strings.ToLower(c.NormalizedValue)]; l != "" && label == "" &&
			(c.ClaimType == claims.IntegrationStatus || c.ClaimType == claims.Certification || c.ClaimType == claims.PlanAvailability) {
			label = l
		}
		if c.Market != "" && c.Market != "ALL" {
			marketCode = market
		}
	}
	if label != "" || marketCode != "" {
		class = contract.StatusQualified
	}
	if label == "" {
		label = "VERIFIED" // signed claim, no narrower governed status
	}
	return class, label, marketCode
}

func anyDraft(hits []knowledge.Hit) bool {
	for _, h := range hits {
		if h.Chunk.Draft {
			return true
		}
	}
	return false
}

// supportOnly keeps the support route only for support intents.
func supportOnly(intent string) string {
	switch intent {
	case "I21", "I22", "I23", "I24":
		return "support"
	}
	return ""
}

// slotMap lists the whitelisted slots present (values are enums or codes,
// never free text).
func slotMap(s policy.Slots, market string) map[string]string {
	m := map[string]string{}
	for k, v := range map[string]string{"market_code": market, "plan": s.PlanName, "billing_cadence": s.BillingCadence,
		"entity_band": s.EntityBand, "integration": s.Integration} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// overviewQuery is the general, market-free question answered when a visitor
// picks "Show general overview" instead of naming a market.
var overviewQuery = map[string]string{
	"I04": "Which countries and markets is ZoikoSuite available in?",
	"I05": "What plans are available and how is pricing structured?",
	"I06": "Is there a free trial and how does it work?",
	"I14": "Where is data hosted and stored?",
}

var statusLabels = map[string]string{
	"available": "Available", "limited": "Limited", "partner_supported": "Partner-Supported",
	"partner-supported": "Partner-Supported", "planned": "Planned", "not_available": "Not Available",
	"unverified": "Unverified", "current_architecture": "Current Architecture", "implemented": "Implemented",
	"built": "Implemented", "in_validation": "In Validation", "readiness": "Readiness",
}

// because overrides the reason of a template outcome whose template serves
// several causes (e.g. UNAVAILABLE for a kill switch vs a model failure).
func because(res Result, reason string) Result {
	if reason != "" {
		res.Reason = reason
	}
	return res
}

// trialStale is the market-named abstention for trial terms that cannot be
// verified (expired or revoked trial claim).
// regionalCoverage offers the coverage confirmation route as a next step
// (Doc 02 escalation matrix: regional payroll / tax / legal coverage -> demo).
func regionalCoverage(res Result) Result {
	res.Suggestions = policy.SuggestRegionalCoverage
	return res
}

// keepAsking leaves a market-scoped pricing or trial question open when its
// claim for that market cannot be stated (expired, revoked, conflicting), and
// offers the other markets and the general overview as next steps, so the
// reply is not a dead end and naming another country answers the same
// question (Doc 02 §10). The other markets are only those with claims.
func keepAsking(res Result, snap *release.Snapshot, market string, pd *Pending) Result {
	if market == "" || (pd.Intent != "I05" && pd.Intent != "I06") {
		return res
	}
	for _, m := range snap.Claims.Markets() {
		if m != market {
			name := policy.CountryName(m)
			res.Suggestions = append(res.Suggestions, contract.Chip{Label: name, Message: name})
		}
	}
	res.Suggestions = append(res.Suggestions, contract.Chip{Label: "Show general overview", Message: "Show general overview"})
	res.Pending = pd
	return res
}

func (p *Pipeline) trialStale(intent string, snap *release.Snapshot, market string, now time.Time, releaseID string) Result {
	res := p.fromTemplate(policy.TplAbstainTrialStale, intent, snap, market, now, releaseID)
	if text, err := policy.Render(res.Text, map[string]string{"market": policy.MarketName(market)}); err == nil {
		res.Text = text
	} else {
		res = p.fromTemplate(withRoute(policy.TplAbstainStale, "pricing"), intent, snap, market, now, releaseID)
	}
	return res
}

func (p *Pipeline) fromTemplate(t policy.Template, intent string, snap *release.Snapshot, market string, now time.Time, releaseID string) Result {
	res := Result{Status: t.Status, IntentID: intent, Text: t.Text, Citations: []contract.Citation{},
		Actions: p.actions(snap, t.RouteKey, market, now), KnowledgeReleaseID: releaseID, Reason: t.Reason}
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
	return model.ListStatements(parts)
}

// regionalIntents depend on country coverage (tax, payroll, legal).
var regionalIntents = map[string]bool{"I17": true, "I18": true, "I19": true}

func hasMarketEvidence(hits []knowledge.Hit, market string) bool {
	for _, h := range hits {
		for _, m := range h.Chunk.MarketCodes {
			if m == market {
				return true
			}
		}
	}
	return false
}

var (
	frameworkRe = regexp.MustCompile(`(?i)\b(fedramp|pci[ -]?dss|hipaa|hitrust|cyber essentials(?: plus)?|irap|c5|iso[ /-]?(?:iec )?(?:9001|27017|27018|27701|42001)|soc ?[13])\b`)
	nonAlnum    = regexp.MustCompile(`[^A-Za-z0-9]`)
	soc2Re      = regexp.MustCompile(`(?i)\bsoc\s*-?\s*2\b`)
	isoRe       = regexp.MustCompile(`(?i)\biso\s*/?\s*(?:iec\s*)?27001\b`)
)

// claimRequest maps an intent to the P1 subject it must be answered from.
// claimRequests lists the typed claims an intent needs, in answer order.
// A plans-and-trial question needs plan availability and trial terms for
// the same market; anything else needs at most one claim.
func claimRequests(intent string, plansAndTrial bool, dec policy.Decision, msg string, reg *claims.Resolver, market string, now time.Time, carried bool) []claims.Request {
	if plansAndTrial {
		return []claims.Request{
			{Subject: claims.PlanAvailability, Market: market, AsOf: now},
			{Subject: claims.Trial, SubjectID: reg.PlanCode(dec.Slots.PlanName), Market: market, AsOf: now},
		}
	}
	if q, ok := claimRequest(intent, dec, msg, reg, market, now, carried); ok {
		return []claims.Request{q}
	}
	return nil
}

func claimRequest(intent string, dec policy.Decision, msg string, reg *claims.Resolver, market string, now time.Time, carried bool) (claims.Request, bool) {
	q := claims.Request{Market: market, AsOf: now}
	switch intent {
	case "I05", "I07":
		if !carried && !policy.AsksPrice(msg) && dec.Slots.BillingCadence == "" {
			// "Which plans are there?" is answered from the approved pricing
			// page; a billing term ("monthly") is a price question.
			return q, false
		}
		q.Subject, q.SubjectID, q.BillingTerm = claims.Price, reg.PlanCode(dec.Slots.PlanName), dec.Slots.BillingCadence
	case "I06":
		q.Subject, q.SubjectID = claims.Trial, reg.PlanCode(dec.Slots.PlanName)
	case "I04":
		q.Subject, q.SubjectID = claims.MarketAvailability, "zoikosuite"
	case "I14":
		if market == "" {
			return q, false // general hosting/deployment questions come from approved pages
		}
		q.Subject = claims.Residency
	case "I12":
		switch {
		case soc2Re.MatchString(msg):
			q.Subject, q.SubjectID = claims.Certification, "SOC2"
		case isoRe.MatchString(msg):
			q.Subject, q.SubjectID = claims.Certification, "ISO27001"
		case frameworkRe.MatchString(msg):
			// A11: a named framework we hold no claim for must not borrow another one.
			q.Subject, q.SubjectID = claims.Certification, strings.ToUpper(nonAlnum.ReplaceAllString(frameworkRe.FindString(msg), ""))
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

// modelQuestion is the question the model answers: the visitor's own
// (redacted) words, or the fixed general question after "Show general overview".
func modelQuestion(redacted string, overview bool, intent string) string {
	if q := overviewQuery[intent]; overview && q != "" {
		return q
	}
	return redacted
}

// mentions reports whether any hit's text contains word (lower-case).
func mentions(hits []knowledge.Hit, word string) bool {
	for _, h := range hits {
		if strings.Contains(strings.ToLower(h.Chunk.PlainText), word) {
			return true
		}
	}
	return false
}

// anyTagged reports whether a hit's source is tagged for intent.
func anyTagged(hits []knowledge.Hit, intent string) bool {
	for _, h := range hits {
		for _, id := range h.Chunk.IntentIDs {
			if id == intent {
				return true
			}
		}
	}
	return false
}

// withNeighbor puts the next section of the top article second, when it is
// eligible and not already retrieved, so the answer can cover the whole topic
// (for example "Legal and Contracts" plus its approval-bound execution).
func withNeighbor(kb *knowledge.Index, hits []knowledge.Hit, q knowledge.Query, rev knowledge.Revocations) []knowledge.Hit {
	if kb == nil || len(hits) == 0 {
		return hits
	}
	n, ok := kb.Neighbor(hits[0].Chunk, q, rev)
	if !ok {
		return hits
	}
	rest := make([]knowledge.Hit, 0, len(hits))
	for _, h := range hits[1:] {
		if h.Chunk.ChunkID != n.ChunkID { // already retrieved lower: move it up
			rest = append(rest, h)
		}
	}
	out := append([]knowledge.Hit{hits[0], {Chunk: n}}, rest...)
	if max := q.MaxChunks; max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}
