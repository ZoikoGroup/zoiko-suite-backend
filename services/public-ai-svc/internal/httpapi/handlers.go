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
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/limits"
	"zoiko.io/public-ai-svc/internal/orchestrator"
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
	issued, err := s.deps.Sessions.Create(r.Context(), key, hashJSON(req), req.Locale, req.MarketCode, req.PageContext)
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

func (s *Server) postTurn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rid := w.Header().Get("X-Request-ID")

	sess, err := s.deps.Sessions.Authenticate(ctx, r.Header.Get(sessionHeader))
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
		// Same key + same payload: the original result, no second model call.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(begun.Turn.Response)
		return
	}

	// The turn runs on a context detached from client cancellation so a
	// disconnect cannot leave the slot locked; it is bounded by its own deadline.
	runCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), lockUntil)
	defer cancel()
	res := s.deps.Answerer.Answer(runCtx, orchestrator.Input{
		RequestID: rid, SessionID: sess.SessionID, TurnID: turnID, Message: req.Message,
		Locale: firstNonEmpty(req.Locale, sess.Locale), MarketCode: firstNonEmpty(req.MarketCode, sess.MarketCode),
		PageContext: firstNonEmpty(req.PageContext, sess.PageContext),
	})

	env := contract.Envelope{
		SchemaVersion: contract.SchemaVersion, TurnID: turnID, RequestID: rid,
		TurnSequence: begun.Turn.Sequence, Status: res.Status, IntentID: res.IntentID, Text: res.Text,
		Citations: nonNilC(res.Citations), Actions: nonNilA(res.Actions),
		KnowledgeReleaseID: res.KnowledgeReleaseID, PolicyVersion: s.cfg.PolicyVersion, Flags: res.Flags,
	}
	if err := env.Validate(); err != nil {
		// The orchestrator produced something outside the schema: never send it.
		s.log.Error("response failed schema validation", zap.String("request_id", rid), zap.String("reason", err.Error()))
		env.Status, env.IntentID, env.Text = contract.Unavailable, "UNKNOWN", unavailableText
		env.Citations, env.Actions, env.Flags = []contract.Citation{}, []contract.Action{}, nil
		if env.KnowledgeReleaseID == "" {
			env.KnowledgeReleaseID = "none"
		}
	}
	body, _ := json.Marshal(env)

	done := begun.Turn
	done.Status, done.IntentID, done.Response = string(env.Status), env.IntentID, body
	evs := s.turnEvents(env, res, now)
	if err := s.deps.Store.CompleteTurn(runCtx, done, usageBuckets(now), res.Tokens, evs); err != nil {
		s.log.Error("turn completion failed", zap.String("request_id", rid), zap.Error(err))
		_ = s.deps.Store.AbortTurn(context.WithoutCancel(ctx), sess.SessionID, turnID)
		problem.Write(w, r, problem.ServiceUnavailable, "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
func (s *Server) turnEvents(env contract.Envelope, res orchestrator.Result, start time.Time) []store.OutboxEvent {
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
	return out
}
