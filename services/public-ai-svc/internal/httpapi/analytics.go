package httpapi

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/store"
)

// postAnalytics records the two Doc 02 §27 events only the browser sees:
// chat_opened (before any session exists) and source_opened. The body is
// enums and ids only; a grant, when sent, must match the session id.
func (s *Server) postAnalytics(w http.ResponseWriter, r *http.Request) {
	var req contract.AnalyticsRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}
	if r.Header.Get(sessionHeader) != "" || req.SessionID != "" {
		sess, err := s.authenticate(r)
		if err != nil {
			problem.Write(w, r, problem.SessionExpired, "")
			return
		}
		if req.SessionID != sess.SessionID {
			problem.Write(w, r, problem.ScopeDenied, "")
			return
		}
	}
	a := events.Analytics{Name: req.Event}
	switch req.Event {
	case events.ChatOpened:
		a.RouteID, a.Locale = firstNonEmpty(req.PageContext, "other"), contract.ServedLocale(req.Locale)
		if s.deps.Config != nil {
			a.ReleaseVersion = s.deps.Config.PublicConfig().ConfigRevision
		}
	case events.SourceOpened:
		a.LinkID, a.IntentID = req.LinkID, req.IntentID
	}
	s.appendAnalytics(r, []events.Analytics{a})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) appendAnalytics(r *http.Request, as []events.Analytics) {
	if s.deps.Store == nil {
		return
	}
	var evs []store.OutboxEvent
	now := time.Now().UTC()
	for _, a := range as {
		if ev, err := events.AnalyticsEvent(a, now); err == nil {
			evs = append(evs, ev)
		}
	}
	if len(evs) == 0 {
		return
	}
	if err := s.deps.Store.AppendEvents(r.Context(), evs); err != nil {
		s.log.Warn("analytics event not recorded", zap.String("request_id", r.Header.Get("X-Request-ID")), zap.Error(err))
	}
}

// turnAnalytics are the per-turn §27 events, derived from the envelope the
// visitor received: ids, enums and buckets only.
func (s *Server) turnAnalytics(env contract.Envelope, res orchestrator.Result, latency time.Duration, market string) []events.Analytics {
	out := []events.Analytics{{Name: events.IntentClassified, IntentID: env.IntentID,
		ConfidenceBucket: firstNonEmpty(res.Confidence, "none"), SafetyDisposition: env.SafetyDisposition}}
	switch env.Status {
	case contract.Clarify:
		slot := "market_code"
		if env.Clarification != nil {
			slot = env.Clarification.Slot
		}
		out = append(out, events.Analytics{Name: events.ClarificationShown, SlotName: slot, ReasonCode: env.Reason})
	default:
		out = append(out, events.Analytics{Name: events.AnswerDelivered, AnswerClass: env.AnswerClass,
			SourceCount: events.IntPtr(len(env.Citations)), StatusClass: string(env.Status), LatencyBucket: events.LatencyBucket(latency)})
	}
	if len(env.Actions) > 0 && env.Status != contract.Answered && env.Status != contract.Clarify {
		key := env.Actions[0].RouteKey
		available := false
		if s.deps.Routes != nil {
			_, available = s.deps.Routes.ResolveRoute(key, market)
		}
		out = append(out, events.Analytics{Name: events.HandoffOffered, HandoffCategory: key, RouteAvailable: events.BoolPtr(available)})
	}
	if res.SecuritySignal != "" {
		out = append(out, events.Analytics{Name: events.SafetyIntercepted, ReasonCode: res.SecuritySignal, SeverityBucket: "medium"})
	}
	if res.ConflictID != "" {
		out = append(out, events.Analytics{Name: events.SourceConflict, ConflictIDs: []string{res.ConflictID}, OwnerQueueID: res.ConflictOwner})
	}
	return out
}
