package kit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/entity"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// Authz is the authorization checker (authorization-svc).
type Authz interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Entities reconciles a caller-supplied legal entity with the verified tenant.
type Entities interface {
	VerifyInTenant(ctx context.Context, tenantID, legalEntityID string) error
}

// Deps is what every AR module needs to serve commands.
type Deps struct {
	DB       *DB
	Log      *zap.Logger
	Authz    Authz
	Entities Entities
	Clock    func() time.Time
}

// Now is the injectable clock.
func (d *Deps) Now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

// Caller is the verified identity and correlation of one request. Tenant and
// principal come from headers set by the gateway, never from the body; entity
// scope is server-reconciled in Guard (spec invariant 16).
type Caller struct {
	TenantID       string
	PrincipalID    string
	CorrelationID  string
	CausationID    string
	RequestID      string
	IdempotencyKey string
	LegalEntityID  string
}

// SvcCaller adapts a Caller for downstream service calls.
func (c Caller) SvcCaller() svcclient.Caller {
	return svcclient.Caller{TenantID: c.TenantID, PrincipalID: c.PrincipalID, CorrelationID: c.CorrelationID,
		RequestID: c.RequestID, IdempotencyKey: c.IdempotencyKey}
}

// Identify reads tenant + principal; it answers the error itself and returns
// false when either is missing. Reads use this alone (tenant-scoped, the
// settled AR posture); commands go through Guard.
func (d *Deps) Identify(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		WriteError(w, http.StatusUnauthorized, CodeTenantScope, domain.ErrTenantScopeMissing.Error())
		return Caller{}, false
	}
	principal := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principal == "" {
		WriteError(w, http.StatusUnauthorized, CodeForbidden, domain.ErrIdentityMissing.Error())
		return Caller{}, false
	}
	corr := r.Header.Get("X-Correlation-ID")
	return Caller{
		TenantID:       tenantID,
		PrincipalID:    principal,
		CorrelationID:  corr,
		CausationID:    r.Header.Get("X-Causation-ID"),
		RequestID:      r.Header.Get("X-Request-Id"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}, true
}

// Guard is the command preamble: identify the caller, reconcile the legal entity
// with the tenant BEFORE authorization (so a foreign entity is refused without
// disclosing whether the caller holds a grant on it), then check the action.
func (d *Deps) Guard(w http.ResponseWriter, r *http.Request, legalEntityID, action string) (Caller, bool) {
	c, ok := d.Identify(w, r)
	if !ok {
		return Caller{}, false
	}
	if legalEntityID == "" {
		WriteError(w, http.StatusBadRequest, CodeValidationFailed, "legal_entity_id is required")
		return Caller{}, false
	}
	if err := d.Entities.VerifyInTenant(r.Context(), c.TenantID, legalEntityID); err != nil {
		d.writeEntityErr(w, err, legalEntityID, c.TenantID)
		return Caller{}, false
	}
	if action != "" {
		if err := d.Authz.CheckAllowed(r.Context(), c.PrincipalID, legalEntityID, action); err != nil {
			d.writeAuthzErr(w, err)
			return Caller{}, false
		}
	}
	c.LegalEntityID = legalEntityID
	return c, true
}

// Authorize checks an additional action for an already-guarded caller (e.g. an
// approval step needing a different permission than the draft).
func (d *Deps) Authorize(w http.ResponseWriter, r *http.Request, c Caller, action string) bool {
	if err := d.Authz.CheckAllowed(r.Context(), c.PrincipalID, c.LegalEntityID, action); err != nil {
		d.writeAuthzErr(w, err)
		return false
	}
	return true
}

func (d *Deps) writeEntityErr(w http.ResponseWriter, err error, legalEntityID, tenantID string) {
	switch {
	case errors.Is(err, entity.ErrForeignTenant), errors.Is(err, entity.ErrNotFound):
		d.Log.Warn("refused: legal entity not in tenant", zap.String("legal_entity_id", legalEntityID), zap.String("tenant_id", tenantID))
		WriteError(w, http.StatusForbidden, CodeTenantScope, domain.ErrLegalEntityNotInTenant.Error())
	case errors.Is(err, entity.ErrNotActive):
		WriteError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error())
	default:
		d.Log.Error("legal entity reconciliation unavailable — failing closed", zap.Error(err))
		WriteError(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "entity registry unavailable")
	}
}

func (d *Deps) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		WriteError(w, http.StatusForbidden, CodeForbidden, "")
		return
	}
	d.Log.Error("authorization check failed — failing closed", zap.Error(err))
	WriteError(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "authorization service unavailable")
}

// MaxBodyBytes caps a command body.
const MaxBodyBytes = 256 << 10

// Decode reads a size-capped JSON body, refusing unknown fields (a misspelled
// key is a rejected request, not a silently ignored value).
func Decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			WriteError(w, http.StatusRequestEntityTooLarge, CodeValidationFailed, "request body too large")
		default:
			WriteError(w, http.StatusBadRequest, CodeValidationFailed, err.Error())
		}
		return false
	}
	return true
}

// Cmd wraps a state-changing handler with mandatory Idempotency-Key handling.
func (d *Deps) Cmd(operation string, next http.HandlerFunc) http.HandlerFunc {
	return d.DB.IdemWrap(operation, true, next)
}

// Query is a pass-through, named for symmetry in route tables.
func (d *Deps) Query(next http.HandlerFunc) http.HandlerFunc { return next }

// RequireReason refuses a blank reason_code (spec §14: mandatory for
// corrections, holds, disputes, write-offs, refunds, manual matches and
// privileged actions).
func RequireReason(w http.ResponseWriter, reason string) bool {
	if strings.TrimSpace(reason) == "" {
		WriteError(w, http.StatusBadRequest, CodeReasonRequired, "reason_code is required")
		return false
	}
	return true
}
