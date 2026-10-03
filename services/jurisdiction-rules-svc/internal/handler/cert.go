package handler

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 3: test bundles, test runs, independent reviews and
// certification. Authoring tests, executing them, reviewing and certifying are
// separate authorization actions (s28), and the store additionally refuses a
// reviewer or certifier who built the thing they are judging.

// DefaultMinReviews is the number of independent approving reviewers required
// when the deployment does not say otherwise. ZS-JUR-001 requires independent
// review but leaves the count and the reviewer roles to a controlled decision
// (s20, s28 "enhanced review"), so it is configuration, not a constant of law.
const DefaultMinReviews = 1

// WithCertificationPolicy sets the number of independent approving reviews
// required before a pack version can be certified (minimum 1).
func (h *Handler) WithCertificationPolicy(minReviews int) *Handler {
	if minReviews < 1 {
		minReviews = 1
	}
	h.minReviews = minReviews
	return h
}

func registerCertRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/packs/{pack_ref}/versions/{version}/test-bundle", h.GetTestBundle)
	r.Get("/v1/packs/{pack_ref}/versions/{version}/test-runs", h.ListTestRuns)
	r.Get("/v1/packs/{pack_ref}/versions/{version}/reviews", h.ListReviews)
	r.Get("/v1/packs/{pack_ref}/versions/{version}/certification", h.GetCertification)

	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/test-bundle", h.SubmitTestBundle)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/test-runs", h.RunTests)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/reviews", h.AddReview)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/certify", h.CertifyPackVersion)
}

func (h *Handler) writeCertError(w http.ResponseWriter, err error, corr string) {
	var blocked *store.CertificationBlocked
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "certification_blocked", "message": "one or more certification gates are not met", "reasons": blocked.Reasons})
	case errors.Is(err, domain.ErrBundleInvalid):
		writeError(w, http.StatusBadRequest, "invalid_test_bundle", err.Error())
	case errors.Is(err, domain.ErrNoTestBundle):
		writeError(w, http.StatusConflict, "no_test_bundle", err.Error())
	case errors.Is(err, domain.ErrNotCertified):
		writeError(w, http.StatusNotFound, "not_certified", err.Error())
	default:
		h.writeCompileError(w, err, corr)
	}
}

// SubmitTestBundle appends a bundle revision (identical content is a no-op).
func (h *Handler) SubmitTestBundle(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "author_tests")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "test bundle too large")
		return
	}
	if _, err := domain.ParseTestBundle(raw); err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	canonical, err := domain.CanonicalJSON(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_test_bundle", err.Error())
		return
	}
	digest, _ := domain.DigestOf(canonical)
	rec, created, err := h.registry.SubmitTestBundle(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"), canonical, digest, actor)
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, rec)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) GetTestBundle(w http.ResponseWriter, r *http.Request) {
	rec, err := h.registry.GetLatestTestBundle(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"))
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// RunTests executes the latest bundle against the compiled artifact. The run
// is recorded whether it passes or fails; 201 either way, with passed in the body.
func (h *Handler) RunTests(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "execute_tests")
	if !ok {
		return
	}
	ref, version := chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
	rec, err := h.registry.RunTests(r.Context(), ref, version, actor)
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventPackTestsRun, rec.RunID, actor, map[string]any{
		"run_id": rec.RunID, "pack_ref": ref, "version": version, "passed": rec.Passed,
		"total": rec.Result.Total, "failed": rec.Result.Failed, "coverage_complete": rec.Result.CoverageComplete})
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) ListTestRuns(w http.ResponseWriter, r *http.Request) {
	limit, _, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	rs, err := h.registry.ListTestRuns(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"), limit)
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": rs})
}

type AddReviewRequest struct {
	Role     string `json:"role"`
	Decision string `json:"decision"`
	Findings string `json:"findings"`
}

var reviewRoleRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

// AddReview records an independent review of the compiled artifact.
func (h *Handler) AddReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "review")
	if !ok {
		return
	}
	var req AddReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	role := strings.ToUpper(strings.TrimSpace(req.Role))
	if !reviewRoleRe.MatchString(role) {
		writeError(w, http.StatusBadRequest, "invalid_role", "role must be an UPPER_SNAKE label such as TAX, LEGAL, ACCOUNTING or TECHNICAL")
		return
	}
	decision := strings.ToUpper(strings.TrimSpace(req.Decision))
	if decision != "APPROVE" && decision != "REJECT" {
		writeError(w, http.StatusBadRequest, "invalid_decision", "decision must be APPROVE or REJECT")
		return
	}
	if decision == "REJECT" && strings.TrimSpace(req.Findings) == "" {
		writeMissingField(w, "findings")
		return
	}
	ref, version := chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
	rec, err := h.registry.AddReview(r.Context(), ref, version, actor, role, decision, req.Findings)
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventPackReviewed, rec.ReviewID, actor, map[string]any{
		"review_id": rec.ReviewID, "pack_ref": ref, "version": version, "role": role, "decision": decision})
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) ListReviews(w http.ResponseWriter, r *http.Request) {
	rs, err := h.registry.ListReviews(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"))
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": rs})
}

// CertifyPackVersion certifies a pack version. 201 on success, 200 for a
// replay, 409 certification_blocked with every unmet gate otherwise.
func (h *Handler) CertifyPackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "certify")
	if !ok {
		return
	}
	if h.signer == nil {
		h.writeCompileError(w, domain.ErrSigningNotConfigured, "")
		return
	}
	min := h.minReviews
	if min < 1 {
		min = DefaultMinReviews
	}
	ref, version := chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
	rec, created, err := h.registry.CertifyPackVersion(r.Context(), ref, version, actor, min, h.signer)
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventPackCertified, rec.CertificationID, actor, map[string]any{
			"certification_id": rec.CertificationID, "pack_ref": ref, "version": version,
			"artifact_digest": rec.ArtifactDigest, "report_digest": rec.ReportDigest})
		writeJSON(w, http.StatusCreated, rec)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// GetCertification returns the certification with its live signature verdict.
func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	c, res, err := h.registry.GetCertification(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"))
	if err != nil {
		h.writeCertError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"certification": c, "verification": res})
}
