package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// ── GET /v1/iam/access-reviews ───────────────────────────────────────────────

// ListAccessReviews handles GET /v1/iam/access-reviews (ZS-IAM-001 §21, §24).
// Returns only reviews assigned to the authenticated reviewer within the tenant scope.
func (h *Handler) ListAccessReviews(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	reviewerID := r.Header.Get("X-Principal-Id")
	if reviewerID == "" {
		reviewerID = r.URL.Query().Get("principal_id")
		if reviewerID == "" {
			reviewerID = r.URL.Query().Get("reviewer_id")
		}
	}
	if reviewerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id header or principal_id query parameter is required",
		})
		return
	}

	tenantScope := r.Header.Get("X-Tenant-Id")
	if tenantScope == "" {
		tenantScope = r.URL.Query().Get("tenant_id")
	}
	if tenantScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id header or tenant_id query parameter is required",
		})
		return
	}

	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))

	reviews, err := h.store.ListAccessReviews(r.Context(), tenantScope, reviewerID, statusFilter)
	if err != nil {
		h.log.Error("ListAccessReviews: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.String("reviewer_id", reviewerID),
			zap.String("tenant_id", tenantScope),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if reviews == nil {
		reviews = []domain.AccessReview{}
	}

	writeJSON(w, http.StatusOK, reviews)
}

// ── POST /v1/iam/access-reviews/{id}:decide ──────────────────────────────────

// DecideAccessReview handles POST /v1/iam/access-reviews/{id}:decide (ZS-IAM-001 §21, §24).
func (h *Handler) DecideAccessReview(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	reviewID := chi.URLParam(r, "id")
	if reviewID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_review_id"})
		return
	}

	reviewerID := r.Header.Get("X-Principal-Id")
	if reviewerID == "" {
		reviewerID = r.URL.Query().Get("principal_id")
	}
	if reviewerID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id header is required",
		})
		return
	}

	tenantScope := r.Header.Get("X-Tenant-Id")
	if tenantScope == "" {
		tenantScope = r.URL.Query().Get("tenant_id")
	}
	if tenantScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id header is required",
		})
		return
	}

	var req domain.AccessReviewDecideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}

	req.Decision = strings.ToUpper(strings.TrimSpace(req.Decision))
	switch req.Decision {
	case domain.ReviewDecisionKeep, domain.ReviewDecisionRevoke, domain.ReviewDecisionModify, domain.ReviewDecisionEscalate:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_decision",
			"message": "decision must be KEEP, REVOKE, MODIFY, or ESCALATE",
		})
		return
	}

	if strings.TrimSpace(req.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_reason",
			"message": "decision reason is required",
		})
		return
	}

	existingReview, err := h.store.GetAccessReview(r.Context(), reviewID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrAccessReviewNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "not_found",
				"message": "access review not found",
			})
			return
		}
		h.log.Error("DecideAccessReview: store unavailable (get review)",
			zap.String("correlation_id", correlationID),
			zap.String("review_id", reviewID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if existingReview == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": "access review not found",
		})
		return
	}

	// Ownership check: reviewer must match assignment
	if existingReview.ReviewerPrincipalID != reviewerID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "access review is assigned to another reviewer",
		})
		return
	}

	// Status check: do not allow decisions on already completed or expired reviews
	if existingReview.Status == domain.ReviewStatusCompleted || existingReview.Status == domain.ReviewStatusExpired {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "already_completed",
			"message": "access review is already completed or expired",
		})
		return
	}

	// Certification Outcome Enforcement (Phase 5.6)
	if req.Decision == domain.ReviewDecisionRevoke {
		// Automatically revoke/invalidate corresponding active assignment(s)
		assignments, aErr := h.store.ListRoleAssignments(r.Context(), tenantScope, existingReview.TargetPrincipalID, existingReview.RoleID, true)
		if aErr == nil {
			for _, assign := range assignments {
				entityMatch := assign.LegalEntityID == nil || *assign.LegalEntityID == existingReview.LegalEntityID
				bookMatch := true
				if existingReview.BookID != nil && *existingReview.BookID != "" {
					bookMatch = assign.BookID != nil && *assign.BookID == *existingReview.BookID
				}
				if entityMatch && bookMatch {
					_, _ = h.store.RevokeRoleAssignment(r.Context(), assign.PrincipalRoleAssignmentID, tenantScope)
				}
			}
		}
	}

	updatedReview, err := h.store.RecordAccessReviewDecision(r.Context(), reviewID, tenantScope, req.Decision, req.Reason, reviewerID)
	if err != nil {
		h.log.Error("DecideAccessReview: store unavailable (record decision)",
			zap.String("correlation_id", correlationID),
			zap.String("review_id", reviewID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Publish canonical event: iam.access_review.completed
	if pubErr := h.publisher.PublishAccessReviewCompleted(r.Context(), *updatedReview); pubErr != nil {
		h.log.Error("DecideAccessReview: failed to publish iam.access_review.completed",
			zap.String("correlation_id", correlationID),
			zap.String("review_id", reviewID),
			zap.Error(pubErr),
		)
	}

	writeJSON(w, http.StatusOK, updatedReview)
}
