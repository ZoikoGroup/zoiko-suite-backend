package context

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/store"
)

// ORG-01 reads (ZS-SVC-I-001): GetSubjectIdentity, ListIdentityLinks, GetAssuranceHistory and
// ListAuthenticationEvents. The commands of the same spec map onto routes that already exist and are named in
// openapi.yaml by their spec names: LinkIdentityProvider is POST /v1/federation/links, DisableIdentity is
// PUT /v1/principals/{id}/status, StepUp and Authenticate are /v1/authenticate[/step-up], RevokeSession is
// POST /v1/context/session/{id}/invalidate.
//
// Reading your own identity needs no grant (the same rule as your own roles); reading somebody else's does.

const (
	// ActionIdentityRead guards the subject, its links and its assurance history.
	ActionIdentityRead = "IDENTITY_SUBJECT_READ"
	// ActionAuthEventsRead guards the authentication-event read model, which says when and how a person signed
	// in and where it was refused: more revealing than who they are, so a separate action.
	ActionAuthEventsRead = "IDENTITY_AUTH_EVENT_READ"
)

// IdentityReader is the ORG-01 read model.
type IdentityReader interface {
	GetSubjectIdentity(ctx context.Context, tenantID, principalID string) (*store.SubjectIdentity, error)
	ListLinks(ctx context.Context, tenantID, principalID, issuer string, limit int) ([]federation.Link, error)
	ListAssuranceObservations(ctx context.Context, tenantID, principalID string, after *store.Cursor, limit int) ([]store.AssuranceObservation, string, error)
	ListAuthenticationEvents(ctx context.Context, tenantID, principalID string, f store.AuthenticationEventFilter, after *store.Cursor, limit int) ([]store.AuthenticationEvent, string, error)
}

// WithIdentityReader wires the ORG-01 reads. Nil answers 501, like the other optional command families.
func (h *Handler) WithIdentityReader(r IdentityReader) *Handler {
	h.identity = r
	return h
}

// subjectFor resolves the caller and the subject, authorizes the read and returns the subject. ok=false means
// the response has been written. A subject of another tenant is 404, never 403: a distinct answer would confirm
// the id exists elsewhere.
func (h *Handler) subjectFor(w http.ResponseWriter, r *http.Request, action string) (tenantID, subjectID string, ok bool) {
	if h.identity == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"identity reads are not configured in this deployment")
		return "", "", false
	}
	tenantID, ok = h.requireTenant(w, r)
	if !ok {
		return "", "", false
	}
	caller, ok := h.requirePrincipal(w, r)
	if !ok {
		return "", "", false
	}
	subjectID = chi.URLParam(r, "principalID")
	if !h.authorizeUnlessSelf(w, r, caller, subjectID, tenantID, action) {
		return "", "", false
	}
	return tenantID, subjectID, true
}

func (h *Handler) subjectExists(w http.ResponseWriter, r *http.Request, tenantID, subjectID string) bool {
	s, err := h.identity.GetSubjectIdentity(r.Context(), tenantID, subjectID)
	if err != nil {
		h.log.Error("identity read failed", zap.Error(err))
		writeCoded(w, r, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "could not read the identity")
		return false
	}
	if s == nil {
		writeCoded(w, r, http.StatusNotFound, "not_found", "subject identity not found")
		return false
	}
	return true
}

// page reads limit and cursor. ok=false means a 400 has been written.
func page(w http.ResponseWriter, r *http.Request) (limit int, after *store.Cursor, ok bool) {
	q := r.URL.Query()
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeCoded(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 200")
			return 0, nil, false
		}
		limit = n
	}
	if raw := q.Get("cursor"); raw != "" {
		c, valid := store.ParseCursor(raw)
		if !valid {
			writeCoded(w, r, http.StatusBadRequest, "invalid_request", "cursor is not one this service issued")
			return 0, nil, false
		}
		after = &c
	}
	return limit, after, true
}

// GetSubjectIdentity answers GET /v1/identities/{principalID}.
func (h *Handler) GetSubjectIdentity(w http.ResponseWriter, r *http.Request) {
	tenantID, subjectID, ok := h.subjectFor(w, r, ActionIdentityRead)
	if !ok {
		return
	}
	s, err := h.identity.GetSubjectIdentity(r.Context(), tenantID, subjectID)
	if err != nil {
		h.log.Error("identity read failed", zap.Error(err))
		writeCoded(w, r, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "could not read the identity")
		return
	}
	if s == nil {
		writeCoded(w, r, http.StatusNotFound, "not_found", "subject identity not found")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// ListIdentityLinks answers GET /v1/identities/{principalID}/links.
func (h *Handler) ListIdentityLinks(w http.ResponseWriter, r *http.Request) {
	tenantID, subjectID, ok := h.subjectFor(w, r, ActionIdentityRead)
	if !ok {
		return
	}
	limit, _, ok := page(w, r)
	if !ok {
		return
	}
	if !h.subjectExists(w, r, tenantID, subjectID) {
		return
	}
	links, err := h.identity.ListLinks(r.Context(), tenantID, subjectID, r.URL.Query().Get("issuer"), limit)
	if err != nil {
		h.log.Error("identity links read failed", zap.Error(err))
		writeCoded(w, r, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "could not read the links")
		return
	}
	if links == nil {
		links = []federation.Link{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": links})
}

// GetAssuranceHistory answers GET /v1/identities/{principalID}/assurance-history.
func (h *Handler) GetAssuranceHistory(w http.ResponseWriter, r *http.Request) {
	tenantID, subjectID, ok := h.subjectFor(w, r, ActionIdentityRead)
	if !ok {
		return
	}
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	if !h.subjectExists(w, r, tenantID, subjectID) {
		return
	}
	items, next, err := h.identity.ListAssuranceObservations(r.Context(), tenantID, subjectID, after, limit)
	if err != nil {
		h.log.Error("assurance history read failed", zap.Error(err))
		writeCoded(w, r, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "could not read the assurance history")
		return
	}
	if items == nil {
		items = []store.AssuranceObservation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// ListAuthenticationEvents answers GET /v1/identities/{principalID}/authentication-events.
func (h *Handler) ListAuthenticationEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, subjectID, ok := h.subjectFor(w, r, ActionAuthEventsRead)
	if !ok {
		return
	}
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := store.AuthenticationEventFilter{EventType: q.Get("event_type"), Outcome: q.Get("outcome")}
	for name, dst := range map[string]**time.Time{"since": &f.Since, "until": &f.Until} {
		if raw := q.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeCoded(w, r, http.StatusBadRequest, "invalid_request", name+" must be an RFC3339 timestamp")
				return
			}
			t = t.UTC()
			*dst = &t
		}
	}
	if !h.subjectExists(w, r, tenantID, subjectID) {
		return
	}
	items, next, err := h.identity.ListAuthenticationEvents(r.Context(), tenantID, subjectID, f, after, limit)
	if err != nil {
		h.log.Error("authentication events read failed", zap.Error(err))
		writeCoded(w, r, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, "could not read the authentication events")
		return
	}
	if items == nil {
		items = []store.AuthenticationEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
