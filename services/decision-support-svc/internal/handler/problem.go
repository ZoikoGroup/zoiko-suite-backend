package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"zoiko.io/decision-support-svc/internal/domain"
)

// Problem is an RFC 9457 problem details body.
type Problem struct {
	Type          string               `json:"type"`
	Title         string               `json:"title"`
	Status        int                  `json:"status"`
	Detail        string               `json:"detail"`
	Instance      string               `json:"instance"`
	Code          string               `json:"code"`
	CorrelationID string               `json:"correlation_id,omitempty"`
	Errors        []domain.FieldError  `json:"errors,omitempty"`
	Transition    *transitionViolation `json:"transition,omitempty"`
}

type transitionViolation struct {
	From domain.Status `json:"from"`
	To   domain.Status `json:"to"`
}

const problemTypeBase = "https://zoiko.io/problems/decision-support/"

// problemSpec is the fixed status, code and title for one error.
type problemSpec struct {
	status int
	code   string
	title  string
}

// problemFor maps every error the service can return to its problem. Any
// error not listed is an infrastructure failure: 503, detail withheld.
func problemFor(err error) problemSpec {
	var fieldErr *domain.FieldError
	var transitionErr *domain.TransitionError

	switch {
	case errors.As(err, &fieldErr):
		return problemSpec{http.StatusBadRequest, "validation_failed", "Request validation failed"}

	case errors.Is(err, domain.ErrTenantMissing):
		return problemSpec{http.StatusUnauthorized, "tenant_missing", "Tenant scope missing"}
	case errors.Is(err, domain.ErrPrincipalMissing):
		return problemSpec{http.StatusUnauthorized, "principal_missing", "Caller identity missing"}

	case errors.Is(err, domain.ErrTenantMismatch):
		return problemSpec{http.StatusForbidden, "tenant_mismatch", "Tenant scope mismatch"}
	case errors.Is(err, domain.ErrLegalEntityScopeMismatch):
		return problemSpec{http.StatusForbidden, "legal_entity_scope_mismatch", "Legal entity scope mismatch"}
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return problemSpec{http.StatusForbidden, "forbidden", "Not authorized"}
	case errors.Is(err, domain.ErrSegregationOfDuties):
		return problemSpec{http.StatusForbidden, "segregation_of_duties", "Segregation of duties violation"}

	case errors.Is(err, domain.ErrRecommendationNotFound):
		return problemSpec{http.StatusNotFound, "not_found", "Recommendation not found"}

	case errors.As(err, &transitionErr):
		return problemSpec{http.StatusConflict, "illegal_transition", "Illegal status transition"}
	case errors.Is(err, domain.ErrReviewDeadlinePassed):
		return problemSpec{http.StatusConflict, "review_deadline_passed", "Review deadline passed"}
	case errors.Is(err, domain.ErrReplacementNotPending):
		return problemSpec{http.StatusConflict, "replacement_not_pending", "Replacement not awaiting review"}

	case errors.Is(err, domain.ErrVersionMismatch):
		return problemSpec{http.StatusPreconditionFailed, "version_mismatch", "Precondition failed"}
	case errors.Is(err, domain.ErrPreconditionRequired):
		return problemSpec{http.StatusPreconditionRequired, "if_match_required", "Precondition required"}

	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return problemSpec{http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency-Key reused"}
	case errors.Is(err, domain.ErrOversightProhibited):
		return problemSpec{http.StatusUnprocessableEntity, "oversight_prohibited", "AI prohibited for this oversight class"}
	case errors.Is(err, domain.ErrReplacementNotFound):
		return problemSpec{http.StatusUnprocessableEntity, "replacement_not_found", "Replacement not found"}
	case errors.Is(err, domain.ErrReplacementMismatched):
		return problemSpec{http.StatusUnprocessableEntity, "replacement_mismatched", "Replacement does not match"}

	case errors.Is(err, domain.ErrAuthzServiceUnavailable):
		return problemSpec{http.StatusServiceUnavailable, "authz_unavailable", "Authorization service unavailable"}

	default:
		return problemSpec{http.StatusServiceUnavailable, "store_unavailable", "Service unavailable"}
	}
}

// writeProblem renders err as application/problem+json.
func (h *Handler) writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	spec := problemFor(err)

	problem := Problem{
		Type:          problemTypeBase + spec.code,
		Title:         spec.title,
		Status:        spec.status,
		Detail:        err.Error(),
		Instance:      r.URL.Path,
		Code:          spec.code,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}

	var fieldErr *domain.FieldError
	if errors.As(err, &fieldErr) {
		problem.Errors = []domain.FieldError{*fieldErr}
	}
	var transitionErr *domain.TransitionError
	if errors.As(err, &transitionErr) {
		problem.Transition = &transitionViolation{From: transitionErr.From, To: transitionErr.To}
	}

	// Never echo infrastructure error text to the caller; log it instead.
	if spec.status == http.StatusServiceUnavailable {
		h.logError(r, "decision-support request failed", err)
		problem.Detail = spec.title
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(spec.status)
	_ = json.NewEncoder(w).Encode(problem)
}
