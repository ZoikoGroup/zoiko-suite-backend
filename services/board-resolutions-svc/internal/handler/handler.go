package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/board-resolutions-svc/internal/authz"
	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/events"
	"zoiko.io/board-resolutions-svc/internal/evidencereq"
	"zoiko.io/board-resolutions-svc/internal/middleware"
	"zoiko.io/board-resolutions-svc/internal/store"
)

// AuthZClient is the subset of authz.Client the handler depends on. Defined as an
// interface here so tests can supply a stub.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// EvidenceReqClient verifies evidence sufficiency before a resolution may be
// passed. Implementations must fail closed, same doctrine as AuthZClient.
type EvidenceReqClient interface {
	EvaluateSufficient(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []evidencereq.Artifact) error
}

const (
	actionMeetingCreate    = "MEETING_CREATE"
	actionResolutionCreate = "RESOLUTION_CREATE"
	actionResolutionVote   = "RESOLUTION_VOTE"
	actionResolutionPass   = "RESOLUTION_PASS"
)

type Handler struct {
	store       store.Store
	publisher   events.Publisher
	authz       AuthZClient
	evidenceReq EvidenceReqClient
	logger      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, er EvidenceReqClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, evidenceReq: er, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal identity missing")
		return "", false
	}
	return principalID, true
}

// requireTenant refuses a request that carries no tenant scope. The middleware
// used to substitute the literal tenant "default" for a missing header, so an
// unscoped request quietly read and wrote a shared bucket instead of being
// turned away.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := middleware.GetTenantID(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant scope missing")
		return "", false
	}
	return tenantID, true
}

// maxRequestBytes bounds a request body. A resolution's content is TEXT, so
// without a cap one request could stream unbounded memory into the decoder
// before any validation ran.
const maxRequestBytes = 1 << 20 // 1 MiB

// decodeJSON reads a JSON request body with a size cap and no tolerance for
// unknown fields — a misspelled "titel" used to be discarded silently, so the
// caller got a 400 for a missing title they believed they had sent, or worse,
// a resolution stored with a field they thought they had set.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

const (
	defaultLimit = 100
	maxLimit     = 500
)

// parsePaging bounds a register read. A discarded strconv error is the
// platform's recurring shape: limit=abc silently defaulted, and offset=-1
// reached Postgres and answered 500.
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest,
				"limit must be an integer between 1 and "+strconv.Itoa(maxLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// requireSelfAttribution refuses a body field that names a principal other
// than the authenticated caller.
//
// created_by and passed_by were taken verbatim from the request body. The
// segregation-of-duties check compares the resolution's created_by against the
// principal passing it — so a drafter could file their resolution under
// somebody else's name and then pass their own work, and the check would
// compare two different strings and allow it. The one control the doctrine
// rests on was defeated by a field the same caller filled in.
//
// The console already sends its own principal here, so this validates rather
// than ignores: silently rewriting the field would leave a caller believing an
// attribution that never happened.
func requireSelfAttribution(w http.ResponseWriter, field, value, principalID string) bool {
	if value != "" && value != principalID {
		writeError(w, http.StatusBadRequest,
			field+" must be the authenticated principal — attribution is taken from X-Principal-Id, not the request body")
		return false
	}
	return true
}

// writeStoreErr maps a store failure to the status it deserves. Everything
// used to answer 500 "failed to …", which reports a caller's bad date or a
// missing tenant as an outage.
func (h *Handler) writeStoreErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, "tenant scope missing")
	case errors.Is(err, domain.ErrInvalidField):
		writeError(w, http.StatusBadRequest, "a submitted field is not a valid value")
	case errors.Is(err, domain.ErrMeetingNotFound):
		writeError(w, http.StatusNotFound, "board meeting not found")
	case errors.Is(err, domain.ErrResolutionNotFound):
		writeError(w, http.StatusNotFound, "board resolution not found")
	case errors.Is(err, domain.ErrResolutionAlreadyFinalized):
		writeError(w, http.StatusConflict, "resolution is already finalized")
	case errors.Is(err, domain.ErrSelfApprovalNotAllowed):
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
	default:
		h.logger.Error(what, zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, what)
	}
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden")
	} else {
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	}
}

func (h *Handler) writeEvidenceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, evidencereq.ErrEvidenceMissing) {
		writeError(w, http.StatusUnprocessableEntity, "required evidence is missing")
	} else {
		writeError(w, http.StatusServiceUnavailable, "evidence-requirements-svc unavailable")
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/meetings", func(r chi.Router) {
		r.Post("/", h.CreateMeeting)
		r.Get("/", h.ListMeetings)
		r.Get("/{id}", h.GetMeeting)
	})

	r.Route("/v1/resolutions", func(r chi.Router) {
		r.Post("/", h.CreateResolution)
		r.Get("/", h.ListResolutions)
		r.Get("/{id}", h.GetResolution)
		r.Post("/{id}/vote", h.RecordVotes)
		r.Post("/{id}/pass", h.PassResolution)
	})
}

// --- Meeting Handlers ---

func (h *Handler) CreateMeeting(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.CreateMeetingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" || req.ScheduledAt.IsZero() {
		writeError(w, http.StatusBadRequest, "title and scheduled_at are required")
		return
	}
	// legal_entity_id was never checked. Empty, it went to authorization-svc,
	// which rejects an empty scope — so the refusal read as "authorization
	// service unavailable" rather than as the missing field it was.
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "effective_from is required (YYYY-MM-DD)")
		return
	}
	if !requireSelfAttribution(w, "created_by", req.CreatedBy, principalID) {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionMeetingCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	m := &domain.BoardMeeting{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Title:         req.Title,
		ScheduledAt:   req.ScheduledAt,
		Location:      req.Location,
		EffectiveFrom: req.EffectiveFrom,
		CreatedBy:     principalID,
	}

	if err := h.store.CreateMeeting(r.Context(), m); err != nil {
		h.writeStoreErr(w, "failed to create meeting", err)
		return
	}

	h.publish(r.Context(), "meeting.created", m.MeetingID, tenantID, m.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID"), m)
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) GetMeeting(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	m, err := h.store.GetMeeting(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, "failed to get meeting", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ListMeetings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	meetings, err := h.store.ListMeetings(r.Context(), domain.MeetingFilter{
		LegalEntityID: r.URL.Query().Get("legal_entity_id"),
		Limit:         limit,
		Offset:        offset,
	})
	if err != nil {
		h.writeStoreErr(w, "failed to list meetings", err)
		return
	}
	if meetings == nil {
		meetings = []domain.BoardMeeting{}
	}
	// "total" is the size of THIS page, which is all this response can honestly
	// claim — it is not a count of the register. Named accordingly now that the
	// read is paged, so a console cannot read a page size as a total.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"meetings": meetings, "total": len(meetings), "limit": limit, "offset": offset,
	})
}

// --- Resolution Handlers ---

func (h *Handler) CreateResolution(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.CreateResolutionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" || req.Content == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "title, content, and category are required")
		return
	}
	if !req.Category.IsValid() {
		writeError(w, http.StatusBadRequest,
			"category must be one of GOVERNANCE, FINANCIAL, OPERATIONAL, EXECUTIVE, STATUTORY")
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "effective_from is required (YYYY-MM-DD)")
		return
	}
	if !requireSelfAttribution(w, "created_by", req.CreatedBy, principalID) {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionResolutionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	res := &domain.BoardResolution{
		MeetingID:        req.MeetingID,
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		ResolutionNumber: req.ResolutionNumber,
		Title:            req.Title,
		Content:          req.Content,
		Category:         req.Category,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		CreatedBy:        principalID,
	}

	if err := h.store.CreateResolution(r.Context(), res); err != nil {
		h.writeStoreErr(w, "failed to create resolution", err)
		return
	}

	h.publish(r.Context(), "resolution.created", res.ResolutionID, tenantID, res.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID"), res)
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) GetResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	res, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, "failed to get resolution", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) ListResolutions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	resolutions, err := h.store.ListResolutions(r.Context(), domain.ResolutionFilter{
		LegalEntityID: r.URL.Query().Get("legal_entity_id"),
		MeetingID:     r.URL.Query().Get("meeting_id"),
		Status:        r.URL.Query().Get("status"),
		Limit:         limit,
		Offset:        offset,
	})
	if err != nil {
		h.writeStoreErr(w, "failed to list resolutions", err)
		return
	}
	if resolutions == nil {
		resolutions = []domain.BoardResolution{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"resolutions": resolutions, "total": len(resolutions), "limit": limit, "offset": offset,
	})
}

func (h *Handler) RecordVotes(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.RecordVotesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// A vote tally is a count of people. Negative counts were accepted and
	// stored, so a resolution could carry -5 votes against.
	if req.VotesFor < 0 || req.VotesAgainst < 0 || req.Abstentions < 0 {
		writeError(w, http.StatusBadRequest, "vote counts may not be negative")
		return
	}

	existing, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, "failed to get resolution", err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionResolutionVote); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	res, err := h.store.RecordVotes(r.Context(), id, &req)
	if err != nil {
		h.writeStoreErr(w, "failed to record votes", err)
		return
	}

	h.publish(r.Context(), "resolution.votes_recorded", id, tenantID, res.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID"), res)
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) PassResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.PassResolutionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// passed_by is no longer read from the body — the pass is attributed to
	// the authenticated principal. It stays accepted so the console's existing
	// payload is not a 400, but it must name the caller: see
	// requireSelfAttribution for why a self-declared attribution defeated the
	// segregation-of-duties check entirely.
	if !requireSelfAttribution(w, "passed_by", req.PassedBy, principalID) {
		return
	}

	existing, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, "failed to get resolution", err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionResolutionPass); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// PassResolution is the distinct closing action that finalizes a
	// resolution as PASSED — the resolution's drafter/creator may not be
	// the principal who closes it. (RecordVotes only tallies aggregate
	// vote counts and does not finalize status, so the doctrine's
	// self-approval check belongs here, not there.)
	if existing.CreatedBy == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	// No finalization path may skip required evidence states
	// (03-microservices.md §8.6). domain_code is the resolution's own
	// category (GOVERNANCE/FINANCIAL/OPERATIONAL/EXECUTIVE/STATUTORY) —
	// real data already on the record, not an invented dimension.
	//
	// correlationID must NOT default to the resolution's own ID:
	// evidence-requirements-svc's Evaluate is deliberately idempotent on
	// correlation_id (a genuine retry must replay, not re-evaluate) — falling
	// back to a fixed per-resolution value made every retry-after-attaching-
	// evidence permanently replay the FIRST attempt's result, even after
	// real evidence was attached. Caught live: a resolution blocked on the
	// first pass attempt stayed blocked forever on retry until this was
	// fixed. Each call without a caller-supplied X-Correlation-ID is a
	// distinct real-world attempt, not a retry — retries are exactly what
	// the header is for, and callers who want retry-safety must supply it.
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = "resolution-pass-" + uuid.New().String()
	}
	var artifacts []evidencereq.Artifact
	if req.DocumentVaultID != nil && *req.DocumentVaultID != "" {
		artifacts = append(artifacts, evidencereq.Artifact{
			EvidenceType: "SUPPORTING_DOCUMENT",
			ReferenceID:  *req.DocumentVaultID,
		})
	}
	if err := h.evidenceReq.EvaluateSufficient(r.Context(), tenantID, existing.LegalEntityID,
		string(existing.Category), actionResolutionPass, correlationID, principalID, artifacts); err != nil {
		h.writeEvidenceErr(w, err)
		return
	}

	res, err := h.store.PassResolution(r.Context(), id, principalID, &req)
	if err != nil {
		h.writeStoreErr(w, "failed to pass resolution", err)
		return
	}

	h.publish(r.Context(), "resolution.passed", id, tenantID, res.LegalEntityID, principalID, correlationID, res)
	writeJSON(w, http.StatusOK, res)
}

// publish emits a domain event and reports a failure rather than discarding
// it. Every call site was `_ = h.publisher.Publish(...)`, and the publisher's
// own Publish returned nil unconditionally — so a dropped event was
// unobservable from both ends.
//
// The context is detached from the request: the event describes a write that
// has already committed, and cancelling it because the caller hung up would
// drop the record of something that definitively happened.
func (h *Handler) publish(ctx context.Context, eventType, entityID, tenantID, legalEntityID, actorID, correlationID string, payload interface{}) {
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.publisher.Publish(pubCtx, events.PublishParams{
		EventType: eventType, EntityID: entityID, TenantID: tenantID,
		LegalEntityID: legalEntityID, ActorID: actorID, CorrelationID: correlationID, Payload: payload,
	}); err != nil {
		h.logger.Error("event publish failed — event dropped",
			zap.String("event_type", eventType),
			zap.String("entity_id", entityID),
			zap.Error(err))
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
