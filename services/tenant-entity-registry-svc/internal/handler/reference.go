package handler

// HTTP surface for MDM-04 reference releases.
//
//	POST /v1/reference-sets                                         define a reference set (201)
//	GET  /v1/reference-sets                                         the reference sets of the tenant
//	GET  /v1/reference-sets/{setCode}/releases                      the releases of a set, staged and published
//	POST /v1/reference-sets/{setCode}/releases                      stage a release with its entries (201)
//	POST /v1/reference-sets/{setCode}/releases/{version}:publish    publish a staged release (by someone else)
//	GET  /v1/reference/{setCode}/{code}?as_of=&release=&history=    Resolve reference: release, validity and supersession
//
// Mounted only when the service satisfies ReferenceService, which *registry.Service does.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ReferenceService is the MDM-04 half of the service contract.
type ReferenceService interface {
	CreateReferenceSet(ctx context.Context, req domain.CreateReferenceSetRequest) (*domain.ReferenceSet, error)
	ListReferenceSets(ctx context.Context) ([]domain.ReferenceSet, error)
	StageReferenceRelease(ctx context.Context, setCode string, req domain.StageReferenceReleaseRequest) (*domain.ReferenceRelease, error)
	PublishReferenceRelease(ctx context.Context, setCode, version string, req domain.PublishReferenceReleaseRequest) (*domain.ReferenceRelease, error)
	ListReferenceReleases(ctx context.Context, setCode string) ([]domain.ReferenceRelease, error)
	ResolveReference(ctx context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error)
}

func registerReferenceRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(ReferenceService); !ok {
		return
	}
	r.Post("/reference-sets", h.CreateReferenceSet)
	r.Get("/reference-sets", h.ListReferenceSets)
	r.Get("/reference-sets/{setCode}/releases", h.ListReferenceReleases)
	r.Post("/reference-sets/{setCode}/releases", h.StageReferenceRelease)
	r.Post("/reference-sets/{setCode}/releases/{version}:publish", h.PublishReferenceRelease)
	r.Get("/reference/{setCode}/{code}", h.ResolveReference)
}

func (h *Handler) references() ReferenceService {
	rs, _ := h.svc.(ReferenceService)
	return rs
}

// CreateReferenceSet defines a reference set.
func (h *Handler) CreateReferenceSet(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReferenceSetRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.references().CreateReferenceSet(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListReferenceSets lists the reference sets of the tenant.
func (h *Handler) ListReferenceSets(w http.ResponseWriter, r *http.Request) {
	out, err := h.references().ListReferenceSets(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ListReferenceReleases lists the releases of a set.
func (h *Handler) ListReferenceReleases(w http.ResponseWriter, r *http.Request) {
	out, err := h.references().ListReferenceReleases(r.Context(), chi.URLParam(r, "setCode"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// StageReferenceRelease stages a release.
func (h *Handler) StageReferenceRelease(w http.ResponseWriter, r *http.Request) {
	var req domain.StageReferenceReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.references().StageReferenceRelease(r.Context(), chi.URLParam(r, "setCode"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// PublishReferenceRelease publishes a staged release.
func (h *Handler) PublishReferenceRelease(w http.ResponseWriter, r *http.Request) {
	var req domain.PublishReferenceReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.references().PublishReferenceRelease(r.Context(), chi.URLParam(r, "setCode"), chi.URLParam(r, "version"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveReference returns a code as it stood on as_of, or under one exact release.
func (h *Handler) ResolveReference(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.references().ResolveReference(r.Context(), domain.ReferenceQuery{SetCode: chi.URLParam(r, "setCode"), Code: chi.URLParam(r, "code"),
		AsOf: asOf, Release: q.Get("release"), History: q.Get("history") == "true"})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// The production service offers reference releases; if it ever stops, the routes would silently vanish.
var _ ReferenceService = (*registry.Service)(nil)
