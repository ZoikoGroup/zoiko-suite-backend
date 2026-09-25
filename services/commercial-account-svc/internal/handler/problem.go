package handler

import (
	"encoding/json"
	"net/http"
)

// Problem is an RFC 9457 problem details body. Code is the stable
// machine-readable code from ZS-SVC-Q-001 §3 (or a narrower one), and is what
// a client should branch on; Title and Detail are for people.
type Problem struct {
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Status   int      `json:"status"`
	Detail   string   `json:"detail,omitempty"`
	Instance string   `json:"instance,omitempty"`
	Code     string   `json:"code"`
	Field    string   `json:"field,omitempty"`
	Reasons  []string `json:"reasons,omitempty"`
}

// Stable problem codes. The first group is the §3 contract verbatim.
const (
	CodeInvalidCommercialContext = "INVALID_COMMERCIAL_CONTEXT"
	CodePriceVersionInactive     = "PRICE_VERSION_INACTIVE"
	CodeMeterNotRegistered       = "METER_NOT_REGISTERED"
	CodeCrossPlaneAccessDenied   = "CROSS_PLANE_ACCESS_DENIED"

	CodePriceVersionImmutable    = "PRICE_VERSION_IMMUTABLE"
	CodePriceVersionInvalidState = "PRICE_VERSION_INVALID_STATE"
	CodePriceVersionInFlight     = "PRICE_VERSION_IN_FLIGHT"
	CodePriceBasisChanged        = "PRICE_BASIS_CHANGED"
	CodePublicationBlocked       = "PUBLICATION_BLOCKED"
	CodeProductCodeTaken         = "PRODUCT_CODE_TAKEN"
	CodeCurrencyMinorUnitsFixed  = "CURRENCY_MINOR_UNITS_FIXED"
	CodeSoDViolation             = "SOD_VIOLATION"
	CodeVersionConflict          = "VERSION_CONFLICT"
	CodePreconditionRequired     = "PRECONDITION_REQUIRED"
	CodeIdempotencyKeyRequired   = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyKeyReused     = "IDEMPOTENCY_KEY_REUSED"
	CodeNotFound                 = "NOT_FOUND"
	CodeUnauthenticated          = "UNAUTHENTICATED"
	CodeAuthorizationDenied      = "AUTHORIZATION_DENIED"
	CodeAuthorizationUnavailable = "AUTHORIZATION_UNAVAILABLE"
	CodeInternal                 = "INTERNAL"
)

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = "urn:zoiko:commercial:problem:" + p.Code
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
