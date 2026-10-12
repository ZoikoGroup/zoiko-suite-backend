// Package handler exposes supplier-financial-profile-svc's REST API — AP-01.
//
// Controls applied to every route (spec §16):
//   - X-Tenant-Id is required and every profile touched is verified to belong
//     to it; reads are authorized too (SUPPLIER_FINANCIAL_READ).
//   - Commands honour Idempotency-Key (stored result + Idempotent-Replay:
//     true; same key with a different request -> 422 IDEMPOTENCY_KEY_REUSED)
//     and expected_version (mismatch -> 409 STALE_VERSION). The key is
//     REQUIRED for the payee-related commands (propose high-risk change,
//     change payment-method preference, decide) and optional elsewhere;
//     expected_version is REQUIRED on decide and optional elsewhere.
//   - Errors carry a stable machine-readable "code" next to "error".
//   - Domain events go through the transactional outbox (written by the store
//     in the same transaction as the change), never fire-and-forget.
//
// Service-to-service reads: the list and eligibility endpoints are called by
// payment-proposal-svc / payment-authorization-svc, which do not forward
// X-Principal-Id today. When AllowServiceReads is true (default) those two
// reads are served tenant-scoped without a principal; when a principal IS
// supplied it is authorized. Set ALLOW_SERVICE_READS_WITHOUT_PRINCIPAL=false
// once the callers forward the header.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/supplier-financial-profile-svc/internal/authz"
	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	svcmiddleware "zoiko.io/supplier-financial-profile-svc/internal/middleware"
	"zoiko.io/supplier-financial-profile-svc/internal/payee"
	"zoiko.io/supplier-financial-profile-svc/internal/store"
)

// Action constants — AP-01's contract "Authorization / permissions" line,
// adapted to this platform's SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5): supplier.financial.read/
// manage, supplier.hold.manage, supplier.terms.manage, supplier.profile.approve.
const (
	SupplierFinancialRead        = "SUPPLIER_FINANCIAL_READ"
	SupplierFinancialManage      = "SUPPLIER_FINANCIAL_MANAGE"
	SupplierHoldManage           = "SUPPLIER_HOLD_MANAGE"
	SupplierTermsManage          = "SUPPLIER_TERMS_MANAGE"
	SupplierProfileApproveChange = "SUPPLIER_PROFILE_APPROVE_CHANGE"
)

// Stable error codes (spec §16 shared contract).
const (
	CodeValidation       = "VALIDATION_FAILED"
	CodeForbidden        = "FORBIDDEN"
	CodeSoDConflict      = "SOD_CONFLICT"
	CodeStaleVersion     = "STALE_VERSION"
	CodeKeyRequired      = "IDEMPOTENCY_KEY_REQUIRED"
	CodeKeyReused        = "IDEMPOTENCY_KEY_REUSED"
	CodeNotFound         = "NOT_FOUND"
	CodeUnauthenticated  = "UNAUTHENTICATED"
	CodeStoreUnavailable = "STORE_UNAVAILABLE"
	CodeAuthzUnavailable = "AUTHORIZATION_UNAVAILABLE"
	CodePayeeUnavailable = "PAYEE_SERVICE_UNAVAILABLE"
	CodeNoPayeeDest      = "NO_ACTIVE_PAYEE_DESTINATION"
	maxBodyBytes         = 1 << 20
)

// AuthzChecker is the real dependency on authorization-svc, including its
// dynamic own-object SoD layer — see internal/authz's package doc comment.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store store.Store
	authz AuthzChecker
	payee payee.Resolver
	log   *zap.Logger

	// AllowServiceReads permits the list/eligibility reads without a principal
	// (see the package doc comment).
	AllowServiceReads bool
}

func New(st store.Store, az AuthzChecker, pr payee.Resolver, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, payee: pr, log: log, AllowServiceReads: true}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap01/supplier-financial-profiles", func(r chi.Router) {
		r.Post("/", h.CreateProfile)
		r.Get("/", h.ListProfiles)
		r.Get("/eligibility", h.GetEligibility)
		r.Get("/{profileID}", h.GetProfile)
		r.Get("/{profileID}/as-of", h.GetProfileAsOf)
		r.Get("/{profileID}/history", h.ListProfileHistory)
		r.Get("/{profileID}/payee-reference", h.GetPayeeReference)
		r.Get("/{profileID}/available-actions", h.GetAvailableActions)
		r.Get("/{profileID}/last-payee-change", h.GetLastPayeeChange)
		r.Post("/{profileID}/activate", h.ActivateProfile)
		r.Post("/{profileID}/amend", h.AmendProfile)
		r.Post("/{profileID}/hold", h.PlaceHold)
		r.Post("/{profileID}/release-hold", h.ReleaseHold)
		r.Post("/{profileID}/suspend", h.SuspendProfile)
		r.Post("/{profileID}/unsuspend", h.UnsuspendProfile)
		r.Post("/{profileID}/retire", h.RetireProfile)

		r.Post("/{profileID}/payment-terms", h.ChangePaymentTerms)
		r.Get("/{profileID}/payment-terms", h.ListPaymentTerms)

		r.Post("/{profileID}/payment-method-preference", h.ChangePaymentMethodPreference)
		r.Post("/{profileID}/high-risk-changes", h.ProposeHighRiskChange)
		r.Get("/{profileID}/change-events", h.ListChangeEvents)
	})
	r.Route("/ap01/high-risk-changes", func(r chi.Router) {
		r.Post("/{changeRequestID}/decide", h.DecideHighRiskChange)
	})
}

// ── response helpers ─────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeStoreError maps a store/domain error onto status + stable code.
func (h *Handler) writeStoreError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrProfileNotFound), errors.Is(err, domain.ErrChangeRequestNotFound),
		errors.Is(err, domain.ErrNoRevisionAsOf), errors.Is(err, domain.ErrNoPayeeChange):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrStaleVersion):
		writeError(w, http.StatusConflict, CodeStaleVersion, err.Error())
	case errors.Is(err, domain.ErrSoDConflict):
		writeError(w, http.StatusForbidden, CodeSoDConflict, err.Error())
	case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrOverlappingPaymentTerms),
		errors.Is(err, domain.ErrChangeRequestNotPending), errors.Is(err, domain.ErrDuplicateProfile):
		writeError(w, http.StatusConflict, CodeValidation, err.Error())
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNoChanges), errors.Is(err, domain.ErrInvalidTenant):
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
	}
}

// ── request context helpers ──────────────────────────────────────────────────

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := svcmiddleware.TenantFromContext(r.Context())
	if t == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "X-Tenant-Id header is required")
		return "", false
	}
	return t, true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		return h.handleAuthzErr(w, err)
	}
	return true
}

func (h *Handler) handleAuthzErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, CodeForbidden, "not authorized to perform this action")
		return false
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, CodeAuthzUnavailable, "authorization service unavailable")
	return false
}

// profileInTenant verifies the row belongs to the verified tenant (RLS already
// scopes the query; this is the explicit application-level check).
func profileInTenant(p *domain.SupplierFinancialProfile, tenant string) bool {
	return p != nil && p.TenantID != nil && *p.TenantID == tenant
}

// loadProfile resolves tenant + principal + profile (tenant-verified) and
// authorizes action against the profile's legal entity. An empty action skips
// authorization (never used for reads).
func (h *Handler) loadProfile(w http.ResponseWriter, r *http.Request, profileID, action string) (*domain.SupplierFinancialProfile, string, bool) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return nil, "", false
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, "", false
	}
	p, err := h.store.FindProfile(r.Context(), profileID)
	if err == nil && !profileInTenant(p, tenant) {
		err = domain.ErrProfileNotFound
	}
	if err != nil {
		h.writeStoreError(w, "loadProfile", err)
		return nil, "", false
	}
	if action != "" && !h.authorize(w, r, principalID, p.LegalEntityID, action) {
		return nil, "", false
	}
	return p, principalID, true
}

// ── idempotency ──────────────────────────────────────────────────────────────

// command is a parsed, idempotency-scoped command request.
type command struct {
	body []byte
	idem *domain.IdemScope // nil when no Idempotency-Key was supplied
}

// parseCommand reads the body and validates/derives the Idempotency-Key scope.
func (h *Handler) parseCommand(w http.ResponseWriter, r *http.Request, op string, keyRequired bool) (*command, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return nil, false
	}
	c := &command{body: body}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		if keyRequired {
			writeError(w, http.StatusBadRequest, CodeKeyRequired, "Idempotency-Key header is required for this command")
			return nil, false
		}
		return c, true
	}
	sum := sha256.Sum256([]byte(op + "\n" + r.URL.Path + "\n" + string(body)))
	c.idem = &domain.IdemScope{Key: key, Operation: op, RequestHash: hex.EncodeToString(sum[:])}
	return c, true
}

// decode unmarshals the body into dst; an empty body leaves dst untouched.
func (c *command) decode(w http.ResponseWriter, dst any) bool {
	if len(strings.TrimSpace(string(c.body))) == 0 {
		return true
	}
	if err := json.Unmarshal(c.body, dst); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return false
	}
	return true
}

// replay serves a stored result for the command's Idempotency-Key, if any.
// It reports true when the request has been fully answered (replayed or
// rejected as key reuse).
func (h *Handler) replay(w http.ResponseWriter, r *http.Request, c *command) bool {
	if c.idem == nil {
		return false
	}
	rec, err := h.store.LookupIdempotency(r.Context(), c.idem.Key)
	if err != nil {
		h.writeStoreError(w, "replay", err)
		return true
	}
	if rec == nil {
		return false
	}
	if rec.RequestHash != c.idem.RequestHash {
		writeError(w, http.StatusUnprocessableEntity, CodeKeyReused, "Idempotency-Key was already used with a different request")
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(rec.StatusCode)
	_, _ = w.Write(append(append([]byte{}, rec.Response...), "\n"...))
	return true
}

// commandError reports a store error for a command, replaying the stored
// result when the failure is a lost race on the same Idempotency-Key.
func (h *Handler) commandError(w http.ResponseWriter, r *http.Request, c *command, op string, err error) {
	if errors.Is(err, domain.ErrIdempotencyRace) {
		if h.replay(w, r, c) {
			return
		}
		writeError(w, http.StatusConflict, CodeValidation, "a request with this Idempotency-Key is already in progress")
		return
	}
	h.writeStoreError(w, op, err)
}

// ── profiles ─────────────────────────────────────────────────────────────────

func (h *Handler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.parseCommand(w, r, "createSupplierFinancialProfile", false)
	if !ok {
		return
	}
	var req domain.CreateProfileRequest
	if !c.decode(w, &req) {
		return
	}
	if req.LegalEntityID == "" || req.SupplierRef == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "legal_entity_id and supplier_ref are required")
		return
	}
	if req.TenantID != "" && req.TenantID != tenant {
		writeError(w, http.StatusForbidden, CodeForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, SupplierFinancialManage) {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	p, err := h.store.CreateProfile(r.Context(), tenant, req, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "CreateProfile", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.loadProfile(w, r, chi.URLParam(r, "profileID"), SupplierFinancialRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// serviceReadPrincipal resolves the principal for the service-to-service reads
// (list, eligibility). Returns "" with ok=true when the caller is an internal
// service and AllowServiceReads permits it.
func (h *Handler) serviceReadPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" && !h.AllowServiceReads {
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// ListProfiles keeps its original {data,count} shape (payment-proposal-svc and
// payment-authorization-svc read it and use updated_at as the payee version).
// With a principal, only profiles in legal entities the principal may read
// are returned.
func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.serviceReadPrincipal(w, r)
	if !ok {
		return
	}
	profiles, err := h.store.ListProfiles(r.Context())
	if err != nil {
		h.writeStoreError(w, "ListProfiles", err)
		return
	}
	out := []domain.SupplierFinancialProfile{}
	allowed := map[string]bool{}
	for _, p := range profiles {
		if !profileInTenant(&p, tenant) {
			continue
		}
		if principalID != "" {
			okLE, seen := allowed[p.LegalEntityID]
			if !seen {
				err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, SupplierFinancialRead)
				switch {
				case err == nil:
					okLE = true
				case errors.Is(err, authzpkg.ErrAuthorizationDenied):
					okLE = false
				default:
					h.handleAuthzErr(w, err)
					return
				}
				allowed[p.LegalEntityID] = okLE
			}
			if !okLE {
				continue
			}
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": out, "count": len(out)})
}

// GetEligibility is the AP-03/AP-05 read contract: may new commitments be
// raised against this supplier? Only an ACTIVE, not-held profile is eligible.
func (h *Handler) GetEligibility(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.serviceReadPrincipal(w, r)
	if !ok {
		return
	}
	le := r.URL.Query().Get("legal_entity_id")
	ref := r.URL.Query().Get("supplier_ref")
	if le == "" || ref == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "legal_entity_id and supplier_ref are required")
		return
	}
	if principalID != "" && !h.authorize(w, r, principalID, le, SupplierFinancialRead) {
		return
	}
	p, err := h.store.FindProfileBySupplier(r.Context(), le, ref)
	if err == nil && !profileInTenant(p, tenant) {
		err = domain.ErrProfileNotFound
	}
	if err != nil {
		h.writeStoreError(w, "GetEligibility", err)
		return
	}
	writeJSON(w, http.StatusOK, domain.EligibilityOf(*p))
}

func (h *Handler) GetProfileAsOf(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	if _, _, ok := h.loadProfile(w, r, profileID, SupplierFinancialRead); !ok {
		return
	}
	atStr := r.URL.Query().Get("at")
	at, err := time.Parse(time.RFC3339, atStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, "at must be an RFC3339 timestamp")
		return
	}
	res, err := h.store.FindProfileAsOf(r.Context(), profileID, at)
	if err != nil {
		h.writeStoreError(w, "GetProfileAsOf", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) ListProfileHistory(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	if _, _, ok := h.loadProfile(w, r, profileID, SupplierFinancialRead); !ok {
		return
	}
	revs, err := h.store.ListProfileHistory(r.Context(), profileID)
	if err != nil {
		h.writeStoreError(w, "ListProfileHistory", err)
		return
	}
	if revs == nil {
		revs = []domain.ProfileRevision{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": revs, "count": len(revs)})
}

// GetLastPayeeChange exposes the principal who last changed payee-related
// fields (and who approved it) so AP-10 can refuse to let that principal
// authorize the resulting payment.
func (h *Handler) GetLastPayeeChange(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	if _, _, ok := h.loadProfile(w, r, profileID, SupplierFinancialRead); !ok {
		return
	}
	res, err := h.store.LastPayeeChange(r.Context(), profileID)
	if err != nil {
		h.writeStoreError(w, "GetLastPayeeChange", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// GetPayeeReference resolves the CURRENT controlled payee reference from ORG-10
// every time. It fails closed (503) when ORG-10 cannot answer and answers 404
// when ORG-10 has no active destination; it never returns a stored, stale or
// invoice-printed value. The ORG-10 party looked up is the profile's controlled
// payee_reference when set, else its supplier_ref (ORG-10 keys destinations by
// the party/counterparty id).
func (h *Handler) GetPayeeReference(w http.ResponseWriter, r *http.Request) {
	p, principalID, ok := h.loadProfile(w, r, chi.URLParam(r, "profileID"), SupplierFinancialRead)
	if !ok {
		return
	}
	partyRef := p.PayeeReference
	if partyRef == "" {
		partyRef = p.SupplierRef
	}
	d, err := h.payee.GetActiveDestination(r.Context(), svcmiddleware.TenantFromContext(r.Context()), principalID, p.LegalEntityID, partyRef)
	if err != nil {
		if errors.Is(err, payee.ErrNoActiveDestination) {
			writeError(w, http.StatusNotFound, CodeNoPayeeDest, "ORG-10 has no active payee destination for this supplier")
			return
		}
		h.log.Error("GetPayeeReference: ORG-10 unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, CodePayeeUnavailable, "payee identity service (ORG-10) unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"profile_id":          p.ProfileID,
		"profile_version":     p.Version,
		"supplier_ref":        p.SupplierRef,
		"legal_entity_id":     p.LegalEntityID,
		"party_ref":           partyRef,
		"payee_reference":     d.DestinationID,
		"destination_id":      d.DestinationID,
		"destination_version": d.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"source":              "ORG-10",
	})
}

type availableAction struct {
	Action     string `json:"action"`
	Permission string `json:"permission"`
}

// GetAvailableActions lists the commands valid in the profile's current state
// that the calling principal is also permitted to run.
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	p, principalID, ok := h.loadProfile(w, r, chi.URLParam(r, "profileID"), SupplierFinancialRead)
	if !ok {
		return
	}
	candidates := candidateActions(p.Status)
	granted := map[string]bool{}
	actions := []availableAction{}
	for _, a := range candidates {
		g, seen := granted[a.Permission]
		if !seen {
			err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, a.Permission)
			switch {
			case err == nil:
				g = true
			case errors.Is(err, authzpkg.ErrAuthorizationDenied):
				g = false
			default:
				h.handleAuthzErr(w, err)
				return
			}
			granted[a.Permission] = g
		}
		if g {
			actions = append(actions, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"profile_id": p.ProfileID, "status": p.Status, "version": p.Version, "actions": actions,
	})
}

func candidateActions(s domain.ProfileStatus) []availableAction {
	manage := func(a string) availableAction { return availableAction{a, SupplierFinancialManage} }
	hold := func(a string) availableAction { return availableAction{a, SupplierHoldManage} }
	terms := availableAction{"change_payment_terms", SupplierTermsManage}
	switch s {
	case domain.StatusDraft:
		return []availableAction{manage("activate"), manage("amend"), terms, manage("change_payment_method_preference"), manage("propose_high_risk_change"), manage("retire")}
	case domain.StatusActive:
		return []availableAction{manage("amend"), terms, manage("change_payment_method_preference"), manage("propose_high_risk_change"), hold("place_hold"), hold("suspend"), manage("retire")}
	case domain.StatusOnHold:
		return []availableAction{manage("amend"), terms, manage("change_payment_method_preference"), manage("propose_high_risk_change"), hold("release_hold"), manage("retire")}
	case domain.StatusSuspended:
		return []availableAction{manage("amend"), terms, manage("change_payment_method_preference"), manage("propose_high_risk_change"), hold("unsuspend"), manage("retire")}
	}
	return []availableAction{}
}

// transition is the shared handler for the status commands.
func (h *Handler) transition(w http.ResponseWriter, r *http.Request, cmd domain.Command, op, permission string) {
	profileID := chi.URLParam(r, "profileID")
	c, ok := h.parseCommand(w, r, op, false)
	if !ok {
		return
	}
	var req domain.TransitionRequest
	if !c.decode(w, &req) {
		return
	}
	_, principalID, ok := h.loadProfile(w, r, profileID, permission)
	if !ok {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	p, err := h.store.Transition(r.Context(), profileID, cmd, req.ExpectedVersion, req.Reason, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, op, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ActivateProfile(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdActivate, "activateSupplierFinancialProfile", SupplierFinancialManage)
}

func (h *Handler) PlaceHold(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdPlaceHold, "placeSupplierHold", SupplierHoldManage)
}

func (h *Handler) ReleaseHold(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdReleaseHold, "releaseSupplierHold", SupplierHoldManage)
}

// SuspendProfile / UnsuspendProfile move the profile into/out of SUSPENDED
// (requires a reason to suspend and supplier.hold.manage).
func (h *Handler) SuspendProfile(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdSuspend, "suspendSupplierFinancialProfile", SupplierHoldManage)
}

func (h *Handler) UnsuspendProfile(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdUnsuspend, "unsuspendSupplierFinancialProfile", SupplierHoldManage)
}

func (h *Handler) RetireProfile(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, domain.CmdRetire, "retireSupplierFinancialProfile", SupplierFinancialManage)
}

// AmendProfile applies low-risk fields (category, procurement category refs,
// invoice channel) immediately. High-risk fields (tax/withholding
// classification, AP account policy, risk/control flags) are NOT applied: each
// becomes a pending change request that a different principal must approve
// (maker != checker), and the response is 202 {profile, pending_change_requests}.
func (h *Handler) AmendProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	c, ok := h.parseCommand(w, r, "amendSupplierFinancialProfile", false)
	if !ok {
		return
	}
	var req domain.AmendProfileRequest
	if !c.decode(w, &req) {
		return
	}
	_, principalID, ok := h.loadProfile(w, r, profileID, SupplierFinancialManage)
	if !ok {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	res, err := h.store.AmendProfile(r.Context(), profileID, req, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "AmendProfile", err)
		return
	}
	writeJSON(w, res.HTTPStatus(), res.Body())
}

// ── payment terms ────────────────────────────────────────────────────────────

// ChangePaymentTerms handles POST .../payment-terms. Overlapping effective
// periods are rejected by a database EXCLUDE constraint and surface as
// 409 VALIDATION_FAILED.
func (h *Handler) ChangePaymentTerms(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	c, ok := h.parseCommand(w, r, "changePaymentTerms", false)
	if !ok {
		return
	}
	var req domain.ChangePaymentTermsRequest
	if !c.decode(w, &req) {
		return
	}
	if req.TermsCode == "" || req.EffectiveFrom.IsZero() {
		writeError(w, http.StatusBadRequest, CodeValidation, "terms_code and effective_from are required")
		return
	}
	if req.EffectiveTo != nil && !req.EffectiveTo.After(req.EffectiveFrom) {
		writeError(w, http.StatusBadRequest, CodeValidation, "effective_to must be after effective_from")
		return
	}
	_, principalID, ok := h.loadProfile(w, r, profileID, SupplierTermsManage)
	if !ok {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	t, err := h.store.ChangePaymentTerms(r.Context(), profileID, req, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "ChangePaymentTerms", err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) ListPaymentTerms(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	if _, _, ok := h.loadProfile(w, r, profileID, SupplierFinancialRead); !ok {
		return
	}
	terms, err := h.store.ListPaymentTerms(r.Context(), profileID)
	if err != nil {
		h.writeStoreError(w, "ListPaymentTerms", err)
		return
	}
	if terms == nil {
		terms = []domain.PaymentTermsPeriod{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": terms, "count": len(terms)})
}

// ── high-risk changes (maker-checker, own-object SoD) ───────────────────────

// ChangePaymentMethodPreference is the ChangePaymentMethodPreference command.
// A payment-method preference is a high-risk, payee-related field, so the
// command records a pending maker-checker change request (the preference is
// applied only when a different principal approves it via
// /ap01/high-risk-changes/{id}/decide). Idempotency-Key is required.
func (h *Handler) ChangePaymentMethodPreference(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	c, ok := h.parseCommand(w, r, "changePaymentMethodPreference", true)
	if !ok {
		return
	}
	var req domain.ChangePaymentMethodPreferenceRequest
	if !c.decode(w, &req) {
		return
	}
	if strings.TrimSpace(req.NewValue) == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "new_value is required")
		return
	}
	_, principalID, ok := h.loadProfile(w, r, profileID, SupplierFinancialManage)
	if !ok {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	cr, err := h.store.ProposeHighRiskChange(r.Context(), profileID, domain.ProposeHighRiskChangeRequest{
		ExpectedVersion: req.ExpectedVersion, Field: domain.FieldPaymentMethodPreference, NewValue: req.NewValue, Reason: req.Reason,
	}, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "ChangePaymentMethodPreference", err)
		return
	}
	writeJSON(w, http.StatusCreated, cr)
}

// ProposeHighRiskChange handles POST .../high-risk-changes. It does not apply
// the change — it records the proposal; applying it requires a SEPARATE call
// to DecideHighRiskChange by a different principal. Idempotency-Key required.
func (h *Handler) ProposeHighRiskChange(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	c, ok := h.parseCommand(w, r, "proposeHighRiskChange", true)
	if !ok {
		return
	}
	var req domain.ProposeHighRiskChangeRequest
	if !c.decode(w, &req) {
		return
	}
	if !req.Field.Valid() {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"field must be one of PAYEE_REFERENCE, PAYMENT_METHOD_PREFERENCE, AP_ACCOUNT_POLICY, TAX_WITHHOLDING_REF, TAX_CLASSIFICATION_REFS, RISK_CONTROL_FLAGS")
		return
	}
	if err := domain.ValidateHighRiskValue(req.Field, req.NewValue); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	_, principalID, ok := h.loadProfile(w, r, profileID, SupplierFinancialManage)
	if !ok {
		return
	}
	if h.replay(w, r, c) {
		return
	}
	cr, err := h.store.ProposeHighRiskChange(r.Context(), profileID, req, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "ProposeHighRiskChange", err)
		return
	}
	writeJSON(w, http.StatusCreated, cr)
}

// DecideHighRiskChange handles POST /ap01/high-risk-changes/{id}/decide —
// AP-01's SoD line: the proposer cannot approve their own change. Enforced
// three ways: a local maker!=checker guard (SOD_CONFLICT), authorization-svc's
// dynamic own-object SoD with the PROPOSER as resource owner, and again in the
// store transaction. Idempotency-Key and expected_version are REQUIRED.
func (h *Handler) DecideHighRiskChange(w http.ResponseWriter, r *http.Request) {
	changeRequestID := chi.URLParam(r, "changeRequestID")
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.parseCommand(w, r, "decideHighRiskChange", true)
	if !ok {
		return
	}
	var req domain.DecideHighRiskChangeRequest
	if !c.decode(w, &req) {
		return
	}
	if req.ExpectedVersion == nil {
		writeError(w, http.StatusBadRequest, CodeValidation, "expected_version is required to decide a high-risk change")
		return
	}

	cr, err := h.store.FindChangeRequest(r.Context(), changeRequestID)
	if err == nil && (cr.TenantID == nil || *cr.TenantID != tenant) {
		err = domain.ErrChangeRequestNotFound
	}
	if err != nil {
		h.writeStoreError(w, "DecideHighRiskChange", err)
		return
	}
	profile, err := h.store.FindProfile(r.Context(), cr.ProfileID)
	if err == nil && !profileInTenant(profile, tenant) {
		err = domain.ErrProfileNotFound
	}
	if err != nil {
		h.writeStoreError(w, "DecideHighRiskChange", err)
		return
	}

	if principalID == cr.ProposedByPrincipalID {
		writeError(w, http.StatusForbidden, CodeSoDConflict, domain.ErrSoDConflict.Error())
		return
	}
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, profile.LegalEntityID, SupplierProfileApproveChange, cr.ProposedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}
	if h.replay(w, r, c) {
		return
	}

	decided, updatedProfile, err := h.store.DecideHighRiskChange(r.Context(), changeRequestID, req, principalID, c.idem)
	if err != nil {
		h.commandError(w, r, c, "DecideHighRiskChange", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"change_request": decided, "profile": updatedProfile})
}

// ── change events ────────────────────────────────────────────────────────────

func (h *Handler) ListChangeEvents(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	if _, _, ok := h.loadProfile(w, r, profileID, SupplierFinancialRead); !ok {
		return
	}
	evs, err := h.store.ListChangeEvents(r.Context(), profileID)
	if err != nil {
		h.writeStoreError(w, "ListChangeEvents", err)
		return
	}
	if evs == nil {
		evs = []domain.ProfileChangeEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": evs, "count": len(evs)})
}
