package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"zoiko.io/public-ai-svc/internal/metrics"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/limits"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

const sessionHeader = "X-Public-Chat-Session"

// decodeStrict decodes exactly one JSON object with no unknown fields.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		problem.Write(w, r, problem.PayloadTooLarge, "")
	} else {
		problem.Write(w, r, problem.InvalidRequest, "request body is not valid for this schema")
	}
	return false
}

func writeValidationError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *contract.FieldError
	switch {
	case errors.Is(err, contract.ErrUnsupportedSchema):
		problem.Write(w, r, problem.UnsupportedSchema, "")
	case errors.As(err, &fe) && fe.Field == "message" && strings.HasPrefix(fe.Reason, "too long"):
		problem.Write(w, r, problem.PayloadTooLarge, fe.Error()) // CT-007: 413, never truncated
	case errors.As(err, &fe) && fe.Field == "market_code":
		problem.Write(w, r, problem.InvalidRequest, fe.Error()) // CT-008: malformed market is 400; never inferred
	case errors.As(err, &fe):
		problem.Write(w, r, problem.ValidationFailed, fe.Error())
	default:
		problem.Write(w, r, problem.ValidationFailed, "")
	}
}

func hashJSON(v any) []byte {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return sum[:]
}

func capabilities() contract.Capabilities {
	return contract.Capabilities{Streaming: false, LeadCapture: false, FileUpload: false}
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		problem.Write(w, r, problem.InvalidRequest, "Idempotency-Key header is required")
		return
	}
	var req contract.SessionRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}
	issued, err := s.deps.Sessions.Create(r.Context(), key, hashJSON(req), req.Locale, req.MarketCode, req.PageContext, r.Header.Get("Origin"))
	switch {
	case errors.Is(err, session.ErrMismatch):
		problem.Write(w, r, problem.IdempotencyMismatch, "")
		return
	case errors.Is(err, session.ErrExpired):
		problem.Write(w, r, problem.SessionExpired, "")
		return
	case err != nil:
		s.log.Error("session create failed", zap.String("request_id", w.Header().Get("X-Request-ID")), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
		return
	}
	writeJSON(w, http.StatusCreated, contract.SessionResponse{
		SchemaVersion: contract.SchemaVersion,
		SessionID:     issued.Session.SessionID,
		SessionGrant:  issued.Grant,
		ExpiresAt:     issued.Session.ExpiresAt.UTC().Format(time.RFC3339),
		PolicyVersion: s.cfg.PolicyVersion,
		Capabilities:  capabilities(),
	})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	err := s.deps.Sessions.Delete(r.Context(), r.Header.Get(sessionHeader), chi.URLParam(r, "session_id"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, session.ErrExpired):
		problem.Write(w, r, problem.SessionExpired, "")
	case errors.Is(err, store.ErrNotFound):
		problem.Write(w, r, problem.NotFound, "")
	default:
		s.log.Error("session delete failed", zap.String("request_id", w.Header().Get("X-Request-ID")), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.PublicConfig()
	body, _ := json.Marshal(cfg)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if cfg.ConfigRevision == "" {
		cfg.ConfigRevision = etag[1 : len(etag)-1]
		body, _ = json.Marshal(cfg)
	}
	// Public config is safe to cache briefly (Doc 05 §06).
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}

// authenticate resolves the session grant and refuses it from any origin
// other than the one that created it (Doc 05 §08), reported like an expired
// grant so nothing about the session leaks.
func (s *Server) authenticate(r *http.Request) (store.Session, error) {
	sess, err := s.deps.Sessions.Authenticate(r.Context(), r.Header.Get(sessionHeader))
	if err == nil && !session.FromOrigin(sess, r.Header.Get("Origin")) {
		return store.Session{}, session.ErrExpired
	}
	return sess, err
}

// sessionBudget is the staged throttle for one session (Doc 06 §13): at 80 %
// of its token cap answers are shorter with no repair, at the cap only
// static approved content is served.
func (s *Server) sessionBudget(sess store.Session) string {
	limit := s.cfg.SessionTokenCap
	switch {
	case limit <= 0:
		return ""
	case sess.TokensUsed >= limit:
		return orchestrator.BudgetStatic
	case sess.TokensUsed*5 >= limit*4:
		return orchestrator.BudgetReduced
	}
	return ""
}

func (s *Server) postTurn(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rid := rw.Header().Get("X-Request-ID")
	// Dev diagnostics: one line per turn naming the failing stage and reason,
	// never the message text (Master prompt STEP 1).
	w := &statusWriter{ResponseWriter: rw}
	var diag turnDiag
	defer func() { s.logTurnDiag(rid, w.status, diag) }()

	sess, err := s.authenticate(r)
	if errors.Is(err, session.ErrExpired) {
		problem.Write(w, r, problem.SessionExpired, "")
		return
	} else if err != nil {
		s.log.Error("session lookup failed", zap.String("request_id", rid), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
		return
	}

	var req contract.TurnRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}
	if req.SessionID != sess.SessionID {
		problem.Write(w, r, problem.ScopeDenied, "")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		problem.Write(w, r, problem.InvalidRequest, "Idempotency-Key header is required")
		return
	}
	if !contract.ValidUUID(key) || !bytes.EqualFold([]byte(key), []byte(req.ClientTurnID)) {
		problem.Write(w, r, problem.IdempotencyHeaderMismatch, "")
		return
	}

	now := time.Now().UTC()
	turnID := "pub_t_" + hex.EncodeToString(hashJSON(rid + req.ClientTurnID)[:12])
	lockUntil := now.Add(2*s.cfg.ModelTimeout + 5*time.Second)
	begun, err := s.deps.Store.BeginTurn(ctx, store.Turn{
		TurnID: turnID, SessionID: sess.SessionID, ClientTurnID: req.ClientTurnID,
		RequestHash: hashJSON(req), CreatedAt: now,
	}, lockUntil)
	switch {
	case errors.Is(err, store.ErrTurnConflict):
		w.Header().Set("Retry-After", "1")
		problem.Write(w, r, problem.TurnConflict, "")
		return
	case errors.Is(err, store.ErrIdempotencyMismatch):
		problem.Write(w, r, problem.IdempotencyMismatch, "")
		return
	case errors.Is(err, store.ErrNotFound):
		problem.Write(w, r, problem.SessionExpired, "")
		return
	case err != nil:
		s.log.Error("turn begin failed", zap.String("request_id", rid), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
		return
	}
	if begun.Replay {
		// Same key + same payload: the original result, no second model call,
		// unless what it cites was revoked since (Doc 03 §19, Doc 05 §12).
		body := s.replayBody(ctx, rid, begun.Turn.Response)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	// The turn runs on a context detached from client cancellation so a
	// disconnect cannot leave the slot locked; it is bounded by its own deadline.
	runCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), lockUntil)
	defer cancel()
	var pending *orchestrator.Pending
	if len(sess.PendingContext) > 0 {
		var pd orchestrator.Pending
		if json.Unmarshal(sess.PendingContext, &pd) == nil {
			pending = &pd
		}
	}
	// Doc 05 §12: the visitor's Stop (the request is cancelled) cancels the
	// model call. Only the answer is cancelled: the turn is still committed
	// on runCtx, so the slot is released and nothing is left half-done.
	answerCtx, stopAnswer := context.WithCancel(runCtx)
	defer stopAnswer()
	go func() {
		select {
		case <-ctx.Done():
			stopAnswer()
		case <-answerCtx.Done():
		}
	}()
	res := s.deps.Answerer.Answer(answerCtx, orchestrator.Input{
		RequestID: rid, SessionID: sess.SessionID, TurnID: turnID, Message: req.Message, Pending: pending,
		Locale: contract.ServedLocale(firstNonEmpty(req.Locale, sess.Locale)), MarketCode: firstNonEmpty(req.MarketCode, sess.MarketCode),
		PageContext:   firstNonEmpty(req.PageContext, sess.PageContext),
		Budget:        s.sessionBudget(sess),
		SessionCapped: s.cfg.SessionTokenCap > 0 && sess.TokensUsed >= s.cfg.SessionTokenCap,
	})

	env := contract.Envelope{
		SchemaVersion: contract.SchemaVersion, TurnID: turnID, RequestID: rid,
		TurnSequence: begun.Turn.Sequence, Status: res.Status, IntentID: res.IntentID, Text: res.Text,
		Citations: nonNilC(res.Citations), Actions: nonNilA(res.Actions),
		KnowledgeReleaseID: res.KnowledgeReleaseID, PolicyVersion: s.cfg.PolicyVersion, Flags: res.Flags,
		AnswerClass: res.AnswerClass, StatusLabel: res.StatusLabel, MarketCode: res.MarketCode, Clarification: res.Clarification,
		SecondaryIntentID: res.SecondaryIntentID, Reason: res.Reason, Suggestions: res.Suggestions, Draft: res.Draft,
	}
	diag = turnDiag{status: string(res.Status), intent: res.IntentID, reason: res.Reason, pipeline: res.Diag}
	if env.AnswerClass == "" || env.Status == contract.RefusedAction {
		env.AnswerClass = contract.DefaultClass(env.Status)
	}
	env.SafetyDisposition = contract.DispositionFor(env.Status)
	structured := s.structuredOn()
	if structured {
		s.structure(&env, firstNonEmpty(res.MarketCode, firstNonEmpty(req.MarketCode, sess.MarketCode)))
	}
	if err := env.Validate(); err != nil {
		// The orchestrator produced something outside the schema: never send it.
		s.log.Error("response failed schema validation", zap.String("request_id", rid), zap.String("reason", err.Error()))
		env.Status, env.IntentID, env.Text = contract.Unavailable, "UNKNOWN", unavailableText
		env.Citations, env.Actions, env.Flags = []contract.Citation{}, []contract.Action{}, nil
		env.AnswerClass, env.StatusLabel, env.MarketCode, env.Clarification = contract.HandoffUnknown, "", "", nil
		env.SafetyDisposition = contract.DispositionRetry
		env.SecondaryIntentID, env.Reason, env.Suggestions, env.Draft = "", contract.ReasonValidationFailed, nil, false
		env.Outcome, env.FollowUps, env.CTA = "", nil, nil
		if structured {
			env.Outcome = contract.OutcomeSafeFallback
		}
		diag.reason = contract.ReasonValidationFailed
		if env.KnowledgeReleaseID == "" {
			env.KnowledgeReleaseID = "none"
		}
	}
	// Doc 05 §10: a security or spend reason is never rendered; the browser
	// gets a generic one, logs (diag) and metrics keep the detail.
	internalReason := env.Reason
	env.Reason = PublicReason(env.Reason)
	body, _ := json.Marshal(env)

	done := begun.Turn
	done.Status, done.IntentID, done.Response = string(env.Status), env.IntentID, body
	evs := s.turnEvents(env, res, now, firstNonEmpty(res.MarketCode, firstNonEmpty(req.MarketCode, sess.MarketCode)))
	if err := s.deps.Store.CompleteTurn(runCtx, done, usageBuckets(now), res.Tokens, evs); err != nil {
		_ = s.deps.Store.AbortTurn(context.WithoutCancel(ctx), sess.SessionID, turnID)
		if errors.Is(err, store.ErrSessionGone) {
			// SEC-T057: the session ended or was deleted mid-turn; nothing is kept.
			problem.Write(w, r, problem.SessionExpired, "")
			return
		}
		s.log.Error("turn completion failed", zap.String("request_id", rid), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
		return
	}
	// Carry-over for the next turn (intent and slots only); cleared otherwise.
	if res.Pending != nil || pending != nil {
		var data []byte
		if res.Pending != nil {
			data, _ = json.Marshal(res.Pending)
		}
		if err := s.deps.Store.SetPendingContext(runCtx, sess.SessionID, data); err != nil {
			s.log.Warn("pending context not saved", zap.String("request_id", rid), zap.Error(err))
		}
	}
	if res.RememberMarket != "" && res.RememberMarket != sess.MarketCode {
		// Doc 02 §10: a country the visitor named is used for the rest of the session.
		if err := s.deps.Store.SetSessionMarket(runCtx, sess.SessionID, res.RememberMarket); err != nil {
			s.log.Warn("session market not saved", zap.String("request_id", rid), zap.Error(err))
		}
	}
	metrics.ObserveTurn(string(env.Status), internalReason, time.Since(now))
	for _, a := range env.Actions {
		metrics.CTAOffered(a.RouteKey) // offers and clicks are separate measures (Doc 09 §20)
	}
	if res.SecuritySignal != "" {
		metrics.SecuritySignal(res.SecuritySignal)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// postFeedback records "useful / not useful" for one of the caller's own
// completed turns (Doc 01 FR-09, S09). Enums only; no free text.
func (s *Server) postFeedback(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		problem.Write(w, r, problem.SessionExpired, "")
		return
	}
	var req contract.FeedbackRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}
	if req.SessionID != sess.SessionID {
		problem.Write(w, r, problem.ScopeDenied, "")
		return
	}
	err = s.deps.Store.SetFeedback(r.Context(), sess.SessionID, chi.URLParam(r, "turn_id"), req.Rating, req.Reason, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem.Write(w, r, problem.NotFound, "")
	case err != nil:
		s.log.Error("feedback failed", zap.String("request_id", w.Header().Get("X-Request-ID")), zap.Error(err))
		problem.Write(w, r, problem.ServiceUnavailable, "")
	default:
		metrics.Feedback(req.Rating, req.Reason)
		s.appendAnalytics(r, []events.Analytics{{Name: events.FeedbackSubmitted, Helpfulness: req.Rating, FeedbackReason: req.Reason}})
		w.WriteHeader(http.StatusNoContent)
	}
}

// postHandoff records that the visitor opened an official route and returns
// its approved URL (Doc 05 §21 chat.handoff.clicked.v1). Route-only: no case,
// ticket, lead or booking is created (Doc 09 §11).
func (s *Server) postHandoff(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		problem.Write(w, r, problem.SessionExpired, "")
		return
	}
	var req contract.HandoffRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.SchemaVersion != contract.SchemaVersion {
		problem.Write(w, r, problem.UnsupportedSchema, "")
		return
	}
	if req.SessionID != sess.SessionID {
		problem.Write(w, r, problem.ScopeDenied, "")
		return
	}
	url, ok := s.deps.Routes.ResolveRoute(req.RouteKey, sess.MarketCode)
	if !ok {
		problem.Write(w, r, problem.NotFound, "")
		return
	}
	if ev, err := events.HandoffClicked(req.RouteKey, sess.MarketCode, false, time.Now().UTC()); err == nil {
		if err := s.deps.Store.AppendEvents(r.Context(), []store.OutboxEvent{ev}); err != nil {
			s.log.Warn("handoff event not recorded", zap.String("request_id", w.Header().Get("X-Request-ID")), zap.Error(err))
		}
	}
	metrics.HandoffClicked(req.RouteKey) // a click, never a case, lead or booking (Doc 09 §20)
	s.appendAnalytics(r, []events.Analytics{{Name: events.CTAOpened, ActionID: req.RouteKey}})
	writeJSON(w, http.StatusOK, contract.HandoffResponse{RouteKey: req.RouteKey, URL: url})
}

const unavailableText = "I can't answer right now. Please use the official ZoikoSuite pages for pricing, demos and support."

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nonNilC(c []contract.Citation) []contract.Citation {
	if c == nil {
		return []contract.Citation{}
	}
	return c
}

func nonNilA(a []contract.Action) []contract.Action {
	if a == nil {
		return []contract.Action{}
	}
	return a
}

// usageBuckets are the spend-ceiling counters a turn contributes to.
func usageBuckets(now time.Time) []string {
	day, month := limits.UsageBuckets(now)
	return []string{day, month}
}

// turnEvents returns the redacted outbox events for a completed turn. They
// carry identifiers, enums and coarse buckets only (Doc 05 §21).
func (s *Server) turnEvents(env contract.Envelope, res orchestrator.Result, start time.Time, market string) []store.OutboxEvent {
	now := time.Now().UTC()
	var out []store.OutboxEvent
	if ev, err := events.TurnCompleted(env.RequestID, env.IntentID, string(env.Status), now.Sub(start), env.KnowledgeReleaseID, now); err == nil {
		out = append(out, ev)
	}
	if res.SecuritySignal != "" {
		if ev, err := events.SecuritySignal(res.SecuritySignal, env.RequestID, "MEDIUM", now); err == nil {
			out = append(out, ev)
		}
	}
	// Doc 02 §27 analytics, committed with the turn.
	for _, a := range s.turnAnalytics(env, res, now.Sub(start), market) {
		if ev, err := events.AnalyticsEvent(a, now); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

// publicReasonOverride maps reasons that must never reach the browser (Doc
// 05 §10: security detections, abuse reports and spend state) to what the
// visitor may see. "" omits the field; the widget then treats the turn by
// its status alone. SENSITIVE_INPUT stays: the widget relies on it to hide
// the visitor's own text (Doc 01 §16).
var publicReasonOverride = map[string]string{
	contract.ReasonPromptInjection: "",
	contract.ReasonSecurityReport:  "",
	contract.ReasonToolInstruction: contract.ReasonValidationFailed,
	contract.ReasonSpendCeiling:    "",
}

// PublicReason is the reason sent to the browser for an internal reason.
func PublicReason(reason string) string {
	if pub, ok := publicReasonOverride[reason]; ok {
		return pub
	}
	return reason
}

// citationRevoker is implemented by the release runtime.
type citationRevoker interface {
	CitationsRevoked(ctx context.Context, sourceVersionIDs, claimIDs []string) (bool, error)
}

// replayBody returns a stored turn response for an idempotent replay. If any
// source version or claim it cites was revoked since it was stored, or that
// cannot be checked, the visitor gets a safe ABSTAIN for the same turn
// instead (Doc 03 §19, Doc 05 §12): a revoked fact is never re-served.
func (s *Server) replayBody(ctx context.Context, rid string, stored []byte) []byte {
	var env contract.Envelope
	if err := json.Unmarshal(stored, &env); err != nil || len(env.Citations) == 0 {
		return stored
	}
	var versions, claimIDs []string
	for _, c := range env.Citations {
		versions = append(versions, c.SourceVersionID)
		claimIDs = append(claimIDs, c.ClaimIDs...)
	}
	revoked := true // no checker wired: fail closed
	if rc, ok := s.deps.Config.(citationRevoker); ok {
		var err error
		if revoked, err = rc.CitationsRevoked(ctx, versions, claimIDs); err != nil {
			s.log.Warn("replay revocation check failed", zap.String("request_id", rid), zap.Error(err))
			revoked = true
		}
	}
	if !revoked {
		return stored
	}
	safe := contract.Envelope{
		SchemaVersion: env.SchemaVersion, TurnID: env.TurnID, RequestID: env.RequestID, TurnSequence: env.TurnSequence,
		Status: contract.Abstain, IntentID: env.IntentID, Text: policy.TplAbstainStale.Text,
		Citations: []contract.Citation{}, Actions: nonNilA(env.Actions),
		KnowledgeReleaseID: env.KnowledgeReleaseID, PolicyVersion: env.PolicyVersion,
		AnswerClass: contract.DefaultClass(contract.Abstain), MarketCode: env.MarketCode, Reason: contract.ReasonSourceRevoked,
		SafetyDisposition: contract.DispositionFor(contract.Abstain),
	}
	if env.Outcome != "" {
		safe.Outcome = contract.OutcomeFor(contract.Abstain)
	}
	if err := safe.Validate(); err != nil {
		s.log.Error("replay abstain failed schema validation", zap.String("request_id", rid), zap.String("reason", err.Error()))
		safe.Actions = []contract.Action{}
	}
	body, _ := json.Marshal(safe)
	return body
}
