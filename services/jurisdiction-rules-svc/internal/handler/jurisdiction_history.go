package handler

// REF-01 (ZS-SVC-I-001 s4.11): GetJurisdictionAsOf, GetHierarchy and the
// evidence on RetireJurisdiction.
//
//	GET  /v1/jurisdictions/{jurisdiction_id}/as-of?as_of=      GetJurisdictionAsOf (public read)
//	GET  /v1/jurisdictions/{jurisdiction_id}/hierarchy?as_of=&depth=   GetHierarchy (public read)
//	POST /v1/admin/jurisdictions/{jurisdiction_id}/deactivate  RetireJurisdiction; the optional
//	     body {reason, evidence_ref, superseded_by_jurisdiction_id} is the retirement evidence.
//
// Like the other jurisdiction reads they are public: a jurisdiction is a
// PUBLIC-classified reference fact. They are mounted only when the store
// supports them, so a handler built over a narrower store is unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

type jurisdictionHistoryReader interface {
	GetJurisdictionAsOf(ctx context.Context, jurisdictionID string, at time.Time) (*domain.JurisdictionAsOf, error)
	GetHierarchy(ctx context.Context, rootID string, at time.Time, depth int) (*domain.Hierarchy, error)
}

type jurisdictionRetirer interface {
	RetireJurisdictionWithQuerier(ctx context.Context, q store.Querier, p domain.RetireJurisdictionParams) (*domain.Jurisdiction, bool, error)
}

// defaultHierarchyDepth is how far below the root a hierarchy read goes when the
// caller does not say.
const defaultHierarchyDepth = 5

// retireRequest is the optional body of a retirement.
type retireRequest struct {
	Reason                     string  `json:"reason"`
	EvidenceRef                string  `json:"evidence_ref"`
	SupersededByJurisdictionID *string `json:"superseded_by_jurisdiction_id"`
}

const maxRetireTextLen = 2000

// retire applies the retirement with whatever evidence the caller gave. A store
// without the evidence-carrying method falls back to a bare deactivation.
// expectedRowVersion is the If-Match version (API-3); nil is unconditional.
func (h *Handler) retire(ctx context.Context, q store.Querier, id, actor string, req retireRequest, expectedRowVersion *int64) (*domain.Jurisdiction, bool, error) {
	if r, ok := h.store.(jurisdictionRetirer); ok {
		return r.RetireJurisdictionWithQuerier(ctx, q, domain.RetireJurisdictionParams{
			JurisdictionID: id, ActorID: actor, Reason: strings.TrimSpace(req.Reason),
			EvidenceRef: strings.TrimSpace(req.EvidenceRef), SupersededBy: req.SupersededByJurisdictionID,
			ExpectedRowVersion: expectedRowVersion,
		})
	}
	return h.store.DeactivateJurisdictionWithQuerier(ctx, q, id, actor, expectedRowVersion)
}

// readRetireRequest reads the optional body, writing a 400 and returning false
// when it is malformed or over-long. An empty body is a retirement with no
// evidence, exactly as before.
func readRetireRequest(w http.ResponseWriter, r *http.Request) (retireRequest, bool) {
	var req retireRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
			return req, false
		}
	}
	if len(req.Reason) > maxRetireTextLen || len(req.EvidenceRef) > maxRetireTextLen {
		writeError(w, r, http.StatusBadRequest, "input_too_long", "reason and evidence_ref must be at most 2000 characters")
		return req, false
	}
	return req, true
}

// asOfQuery reads the optional as_of parameter (RFC 3339 or YYYY-MM-DD); absent
// means now. A malformed value is a 400, not "now": answering today's hierarchy
// to a question about a mistyped date would be a wrong answer that looks right.
func asOfQuery(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("as_of"))
	if raw == "" {
		return time.Now().UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), true
	}
	writeError(w, r, http.StatusBadRequest, "invalid_as_of", "as_of must be an RFC 3339 timestamp or a YYYY-MM-DD date")
	return time.Time{}, false
}

// writeHistoryError answers the refusals the history reads have.
func (h *Handler) writeHistoryError(w http.ResponseWriter, r *http.Request, err error, correlationID string) {
	var ne *domain.NotEffectiveError
	switch {
	case errors.As(err, &ne):
		apicontract.WriteProblem(w, r, apicontract.Problem{
			Status: http.StatusNotFound, Code: "jurisdiction_not_effective", Detail: ne.Error(), CorrelationID: correlationID,
		})
	case errors.Is(err, domain.ErrInvalidDepth):
		writeError(w, r, http.StatusBadRequest, "invalid_depth", err.Error())
	case errors.Is(err, domain.ErrHierarchyTooLarge):
		apicontract.WriteProblem(w, r, apicontract.Problem{
			Status: http.StatusUnprocessableEntity, Code: "hierarchy_too_large", Detail: err.Error(), CorrelationID: correlationID,
		})
	default:
		h.writeStoreError(w, r, err, correlationID)
	}
}

// GetJurisdictionAsOf handles GET /v1/jurisdictions/{jurisdiction_id}/as-of.
func (h *Handler) GetJurisdictionAsOf(w http.ResponseWriter, r *http.Request) {
	reader, ok := h.store.(jurisdictionHistoryReader)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the store does not support point-in-time reads")
		return
	}
	at, ok := asOfQuery(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	out, err := reader.GetJurisdictionAsOf(r.Context(), chi.URLParam(r, "jurisdiction_id"), at)
	if err != nil {
		h.writeHistoryError(w, r, err, correlationID)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetHierarchy handles GET /v1/jurisdictions/{jurisdiction_id}/hierarchy.
func (h *Handler) GetHierarchy(w http.ResponseWriter, r *http.Request) {
	reader, ok := h.store.(jurisdictionHistoryReader)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the store does not support point-in-time reads")
		return
	}
	at, ok := asOfQuery(w, r)
	if !ok {
		return
	}
	depth := defaultHierarchyDepth
	if raw := strings.TrimSpace(r.URL.Query().Get("depth")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_depth", "depth must be an integer")
			return
		}
		depth = n
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	out, err := reader.GetHierarchy(r.Context(), chi.URLParam(r, "jurisdiction_id"), at, depth)
	if err != nil {
		h.writeHistoryError(w, r, err, correlationID)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
