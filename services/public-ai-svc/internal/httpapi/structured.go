package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/problem"
)

// FlagSource reads the effective feature flags.
type FlagSource interface {
	Values() flags.Values
}

// structuredOn reports whether structured responses are enabled (ADR-008).
// Without a flag source the contract stays as it was.
func (s *Server) structuredOn() bool {
	return s.deps.Flags != nil && s.deps.Flags.Values().StructuredResponse
}

func followUp(it policy.MenuItem) contract.FollowUp {
	return contract.FollowUp{ID: it.ID, Label: it.Label, IntentID: it.IntentID, Message: it.Message}
}

// getMenu serves the guided topic menu (ADR-008): the Doc 02 §22 families
// and their questions. 404 while structured responses are off.
func (s *Server) getMenu(w http.ResponseWriter, r *http.Request) {
	if !s.structuredOn() {
		problem.Write(w, r, problem.NotFound, "")
		return
	}
	resp := contract.MenuResponse{SchemaVersion: contract.SchemaVersion, Categories: []contract.MenuCategory{}}
	for _, c := range policy.Menu {
		mc := contract.MenuCategory{ID: c.ID, Label: c.Label, Items: []contract.FollowUp{}}
		for _, it := range c.Items {
			mc.Items = append(mc.Items, followUp(it))
		}
		resp.Categories = append(resp.Categories, mc)
	}
	body, _ := json.Marshal(resp)
	// Same caching as /config: public, policy-versioned copy (Doc 05 §06).
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}

// structure adds the structured fields to a turn: the typed outcome,
// approved follow-ups, at most one primary and one secondary CTA, and
// citation titles and URLs resolved from the signed registries. Nothing
// here comes from model output.
func (s *Server) structure(env *contract.Envelope, market string) {
	env.Outcome = contract.OutcomeFor(env.Status)

	// Follow-ups after an answer or a route offer; refusals and abstentions
	// keep their own pre-approved suggestions, CLARIFY its options.
	// Never after a safety reply (X01-X10): a security report, say, must stay
	// on its official route (Doc 02 X05) without unrelated questions.
	if (env.Status == contract.Answered || env.Status == contract.Handoff) && len(env.Suggestions) == 0 && !strings.HasPrefix(env.IntentID, "X") {
		for _, it := range policy.FollowUps(env.IntentID) {
			env.FollowUps = append(env.FollowUps, followUp(it))
		}
	}

	links := map[string]contract.CitationLink{}
	if s.deps.Config != nil {
		for _, l := range s.deps.Config.PublicConfig().CitationLinks {
			links[l.URLKey] = l
		}
	}
	validCitationCount := 0
	for i := range env.Citations {
		c := &env.Citations[i]
		// Try to resolve from the route registry (snapshot) by url_key.
		var url string
		var routeOk bool
		if s.deps.Routes != nil {
			url, routeOk = s.deps.Routes.ResolveRoute(c.URLKey, market)
		}
		if routeOk && !knowledge.IsPlaceholderURL(url) {
			// route found
			c.RouteKey = c.URLKey
			c.Title = policy.RouteTitles[c.RouteKey] // display text only
			c.URL = url
			validCitationCount++
			continue
		}
		// fallback to CitationLinks (already have title/URL)
		if l, ok := links[c.URLKey]; ok {
			c.Title, c.URL = l.Title, l.URL
			c.RouteKey = c.URLKey // keep route key for consistency
			validCitationCount++
			continue
		}
		// fallback to policy.RouteTitles (display title only)
		if title, ok := policy.RouteTitles[c.URLKey]; ok {
			c.Title = title
			c.RouteKey = c.URLKey
			c.URL = s.routeURL(c.RouteKey, market)
			validCitationCount++
			continue
		}
		// no entry: drop citation
		c.Title = ""
		c.RouteKey = ""
		c.URL = ""
	}
	// If the answer was ANSWERED but no valid citations remain, downgrade to ABSTAIN
	if validCitationCount == 0 && env.Status == contract.Answered {
		env.Status = contract.Abstain
	}

	var ctas []*contract.CTALink
	for i, a := range env.Actions {
		if len(ctas) == 2 {
			break
		}
		label := policy.RouteTitles[a.RouteKey]
		// Doc 02 §06: the intent's default CTA label names its default route;
		// any other route keeps its own title (no "Compare plans" on a demo link).
		if i == 0 && policy.IntentCTA[env.IntentID] != "" && a.RouteKey == policy.DefaultRoute[env.IntentID] {
			label = policy.IntentCTA[env.IntentID]
		}
		// Doc 02 T06: a trial answer that links the pricing page (not a
		// sign-up page) offers "Review trial options".
		if i == 0 && env.IntentID == "I06" && a.RouteKey == "pricing" {
			label = policy.TrialPricingCTA
		}
		if label == "" {
			continue
		}
		ctas = append(ctas, &contract.CTALink{RouteKey: a.RouteKey, Label: label, URL: s.routeURL(a.RouteKey, market)})
	}
	if len(ctas) > 0 {
		env.CTA = &contract.CTA{Primary: ctas[0]}
		if len(ctas) > 1 {
			env.CTA.Secondary = ctas[1]
		}
	}
}

// routeURL is the approved URL for a route key, or "" when the registry has
// none or only a placeholder (the widget then shows no link).
func (s *Server) routeURL(key, market string) string {
	if s.deps.Routes == nil {
		return ""
	}
	url, ok := s.deps.Routes.ResolveRoute(key, market)
	if !ok || knowledge.IsPlaceholderURL(url) {
		return ""
	}
	return url
}
