package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/eventing/integration"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func profileVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	v, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || v < 1 {
		writeProblem(w, r, errBadVersion)
		return 0, false
	}
	return v, true
}

var errBadVersion = errors.New("version must be a positive integer")

// outcome is how an error is presented: an HTTP status, a stable INT-* code
// (empty for conditions outside the taxonomy, such as "not found"), and a
// detail that names the rule and never echoes caller input.
type outcome struct {
	status int
	code   integration.Code
	detail string
	retry  int
}

// classify maps a domain or service error to its presentation. The order
// matters: more specific errors come first.
func classify(err error) outcome {
	switch {
	// authorization (a missing caller is 401, a denial 403, an outage 503)
	case errors.Is(err, errNoPrincipal):
		return outcome{http.StatusUnauthorized, integration.CodeAuthDenied, "X-Principal-Id header is required", 0}
	case errors.Is(err, authz.ErrAuthorizationDenied):
		return outcome{http.StatusForbidden, integration.CodeAuthDenied, "authorization denied", 0}
	case errors.Is(err, authz.ErrAuthzServiceUnavailable):
		return outcome{http.StatusServiceUnavailable, "", "authorization service unavailable", 5}

	// not found: never distinguishes "does not exist" from "belongs to someone else"
	case errors.Is(err, domain.ErrBridgeNotFound):
		return outcome{http.StatusNotFound, "", "bridge endpoint not found", 0}
	case errors.Is(err, domain.ErrProfileNotFound):
		return outcome{http.StatusNotFound, "", "provider profile not found", 0}
	case errors.Is(err, domain.ErrCheckpointNotFound):
		return outcome{http.StatusNotFound, "", "sync checkpoint not found", 0}
	case errors.Is(err, domain.ErrResetNotFound):
		return outcome{http.StatusNotFound, "", "checkpoint reset request not found", 0}

	// concurrency and idempotency
	case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrCheckpointConflict):
		return outcome{http.StatusConflict, integration.CodeIdempotencyConflict, err.Error(), 0}
	case errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrProfileExists), errors.Is(err, domain.ErrResetNotPending):
		return outcome{http.StatusConflict, integration.CodeIdempotencyConflict, err.Error(), 0}
	case errors.Is(err, domain.ErrCheckpointRegress):
		return outcome{http.StatusConflict, integration.CodeCallbackConflict, err.Error(), 0}

	// separation of duties and scope
	case errors.Is(err, domain.ErrSelfApproval), errors.Is(err, connector.ErrNotPlatformScope), errors.Is(err, domain.ErrPlatformScopeRequired):
		return outcome{http.StatusForbidden, integration.CodeAuthDenied, err.Error(), 0}
	case errors.Is(err, connector.ErrScopeDenied), errors.Is(err, connector.ErrScopeNotSupported):
		return outcome{http.StatusForbidden, integration.CodeScopeDenied, err.Error(), 0}

	// lifecycle state
	case errors.Is(err, connector.ErrNotLive), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, connector.ErrDeprecationWindow),
		errors.Is(err, domain.ErrConnectorTerminal), errors.Is(err, domain.ErrNotApproved):
		return outcome{http.StatusConflict, integration.CodeScopeDenied, err.Error(), 0}

	// residency and egress
	case errors.Is(err, connector.ErrRegionDenied), errors.Is(err, connector.ErrEgressDenied), errors.Is(err, connector.ErrHostNotAllowed):
		return outcome{http.StatusForbidden, integration.CodeResidencyDenied, err.Error(), 0}

	// credentials
	case errors.Is(err, connector.ErrCredentialExpired):
		return outcome{http.StatusUnauthorized, integration.CodeCredentialInvalid, err.Error(), 0}
	case errors.Is(err, connector.ErrCredentialInvalid):
		return outcome{http.StatusUnprocessableEntity, integration.CodeCredentialInvalid, err.Error(), 0}

	// provider profile
	case errors.Is(err, connector.ErrProfileUnavailable):
		return outcome{http.StatusConflict, integration.CodeVersionUnsupported, err.Error(), 0}

	// dependencies
	case errors.Is(err, connector.ErrDependencyUnavailable):
		return outcome{http.StatusServiceUnavailable, "", "a dependency needed to check this change is unavailable; retry", 5}

	// bad input
	case errors.Is(err, domain.ErrIncompleteConfig), errors.Is(err, connector.ErrValidation):
		return outcome{http.StatusBadRequest, integration.CodeSchemaMismatch, err.Error(), 0}
	case errors.Is(err, errBadBody), errors.Is(err, errBadVersionHeader), errors.Is(err, errVersionDisagree), errors.Is(err, errNeedVersion), errors.Is(err, errCreateFields), errors.Is(err, errBadVersion):
		return outcome{http.StatusBadRequest, integration.CodeSchemaMismatch, err.Error(), 0}
	}
	// Anything unclassified is an internal fault: opaque, never the error text.
	return outcome{http.StatusInternalServerError, "", "internal error", 0}
}

// writeProblem answers with RFC 9457 problem details. It also carries the
// pre-governance {"error": "..."} member, so clients written against the old
// error shape keep reading the message where they always did.
func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	o := classify(err)
	body := map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(o.status),
		"status": o.status,
		"detail": o.detail,
		"error":  o.detail,
	}
	if r != nil {
		body["instance"] = r.URL.Path
		if c := r.Header.Get("X-Correlation-ID"); c != "" {
			body["correlation_id"] = c
		}
	}
	if o.code != "" {
		info := o.code.Info()
		body["type"] = integration.ProblemTypePrefix + string(o.code)
		body["title"] = info.Title
		body["code"] = string(o.code)
		body["retryable"] = info.Retryable
	}
	if o.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.retry))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(o.status)
	_ = json.NewEncoder(w).Encode(body)
}
