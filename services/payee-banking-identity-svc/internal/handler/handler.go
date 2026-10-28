// Package handler exposes payee-banking-identity-svc's REST API — ORG-10.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payee-banking-identity-svc/internal/authz"
	"zoiko.io/payee-banking-identity-svc/internal/counterparty"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	svcmiddleware "zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/store"
)

// Action constants — ORG-10's own contract's "Permissions / authorization"
// line ("Payee-master admin; masked access; read of full destination
// strictly purpose-limited"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. ApproveDestination gets its own action
// distinct from ordinary Admin — the spec's own SoD line names exactly
// this maker/checker split ("supplier-profile editor cannot alone
// activate changed beneficiary details").
const (
	PayeeMasterAdmin          = "PAYEE_MASTER_ADMIN"
	PayeeMasterApprove        = "PAYEE_MASTER_APPROVE"
	PayeeMasterPrivilegedRead = "PAYEE_MASTER_PRIVILEGED_READ"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store store.Store
	authz AuthzChecker
	party counterparty.Client
	log   *zap.Logger
}

func New(st store.Store, az AuthzChecker, party counterparty.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, party: party, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/org10/destinations", func(r chi.Router) {
		r.Post("/", h.ProposePayeeDestination)
		r.Get("/{destinationID}", h.GetPayeeDestination)
		r.Get("/{destinationID}/history", h.GetPayeeChangeHistory)
		r.Post("/{destinationID}/verify", h.VerifyPayeeDestination)
		r.Post("/{destinationID}/approve", h.ApprovePayeeDestination)
		r.Post("/{destinationID}/activate", h.ActivateDestination)
		r.Post("/{destinationID}/suspend", h.SuspendDestination)
		r.Post("/{destinationID}/supersede", h.SupersedeDestination)
	})
	r.Get("/v1/control-populations/destination-changes", h.GetDestinationChangesPopulation)
	r.Get("/org10/parties/{partyRef}/versions", h.ListPayeeVersions)
	r.Get("/org10/parties/{partyRef}/active", h.GetActivePayeeDestination)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireIdempotencyKey is the ORG-10 command contract's retry identity:
// every one of the six named commands is a material write and must carry it.
func (h *Handler) requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, domain.ErrIdempotencyKeyRequired.Error())
		return "", false
	}
	if len(key) > 255 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must be at most 255 characters")
		return "", false
	}
	return key, true
}

func markIdempotentReplay(w http.ResponseWriter, replay bool) {
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

// authorizeOwnObject is the maker-checker gate for ApprovePayeeDestination
// — the proposer of a destination cannot also approve it.
func (h *Handler) authorizeOwnObject(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) bool {
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, legalEntityID, actionType, resourceOwnerPrincipalID); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func (h *Handler) fetchDestinationForAuth(w http.ResponseWriter, r *http.Request, destinationID string) (*domain.PayeeDestination, bool) {
	d, err := h.store.FindDestination(r.Context(), destinationID)
	if err != nil {
		if errors.Is(err, domain.ErrDestinationNotFound) {
			writeError(w, http.StatusNotFound, "payee destination not found")
			return nil, false
		}
		h.log.Error("fetchDestinationForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return d, true
}

// isPrivileged is one authorization-svc check for one legal entity. A list
// response calls it once per distinct entity, not once per version.
func (h *Handler) isPrivileged(r *http.Request, principalID, legalEntityID string) bool {
	return h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, PayeeMasterPrivilegedRead) == nil
}

// maskIfNotPrivileged is the literal enforcement of "full account never
// overexposed": AccountIdentifier is cleared for every reader except one
// holding PayeeMasterPrivilegedRead. AccountLast4 always stays — it's the
// masked value the spec's own control expects a normal reader to see.
func (h *Handler) maskIfNotPrivileged(r *http.Request, principalID, legalEntityID string, d *domain.PayeeDestination) {
	if !h.isPrivileged(r, principalID, legalEntityID) {
		d.AccountIdentifier = ""
	}
}

// maskVersions preserves maskIfNotPrivileged's per-entity decision while
// avoiding an authorization round trip for every version in the list. A
// party's versions normally share one legal entity, so this is one call in
// the common case (previously N calls for N versions).
func (h *Handler) maskVersions(r *http.Request, principalID string, list []domain.PayeeDestination) {
	privileged := make(map[string]bool)
	for i := range list {
		entity := list[i].LegalEntityID
		allowed, checked := privileged[entity]
		if !checked {
			allowed = h.isPrivileged(r, principalID, entity)
			privileged[entity] = allowed
		}
		if !allowed {
			list[i].AccountIdentifier = ""
		}
	}
}

// ProposePayeeDestination verifies the named party genuinely exists via
// counterparty-management-svc before ever creating a candidate against
// it. Negative-path enforcement lives one step later, at
// VerifyPayeeDestination — see internal/domain's package doc.
func (h *Handler) ProposePayeeDestination(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposeDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.PartyRef == "" || req.FinancialInstitution == "" || req.AccountIdentifier == "" ||
		req.CountryCode == "" || req.Currency == "" || req.PayeeName == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, party_ref, financial_institution, account_identifier, country_code, currency and payee_name are required")
		return
	}
	switch req.SourceType {
	case domain.SourceSupplierPortal, domain.SourceInvoiceOCR, domain.SourceEmail, domain.SourceManualEntry:
	default:
		writeError(w, http.StatusBadRequest, "source_type must be SUPPLIER_PORTAL, INVOICE_OCR, EMAIL, or MANUAL_ENTRY")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, PayeeMasterAdmin) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if _, err := h.party.GetParty(r.Context(), verifiedTenant, req.LegalEntityID, req.PartyRef); err != nil {
		if errors.Is(err, domain.ErrPartyNotFound) {
			writeError(w, http.StatusBadRequest, domain.ErrPartyNotFound.Error())
			return
		}
		h.log.Error("ProposePayeeDestination: counterparty-management-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrPartyServiceUnavailable.Error())
		return
	}

	created, replay, err := h.store.ProposeDestination(r.Context(), verifiedTenant, req, principalID, r.Header.Get("X-Correlation-ID"), idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrDuplicateDestination) {
			writeError(w, http.StatusConflict, domain.ErrDuplicateDestination.Error())
			return
		}
		h.log.Error("ProposePayeeDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	// com.zoikosuite.banking.payee-destination.proposed is enqueued inside
	// ProposeDestination's own transaction (the transactional outbox), not
	// published here after the fact — see pg_store.go.
	h.maskIfNotPrivileged(r, principalID, req.LegalEntityID, created)
	markIdempotentReplay(w, replay)
	if replay {
		// Match the platform's command pattern: the first response is 201,
		// a replay returns 200 with the same candidate and the replay header.
		writeJSON(w, http.StatusOK, created)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPayeeDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	h.maskIfNotPrivileged(r, principalID, d.LegalEntityID, d)
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) ListPayeeVersions(w http.ResponseWriter, r *http.Request) {
	partyRef := chi.URLParam(r, "partyRef")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	list, err := h.store.ListVersions(r.Context(), partyRef)
	if err != nil {
		h.log.Error("ListPayeeVersions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.maskVersions(r, principalID, list)
	if list == nil {
		list = []domain.PayeeDestination{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": list, "count": len(list)})
}

func (h *Handler) GetActivePayeeDestination(w http.ResponseWriter, r *http.Request) {
	partyRef := chi.URLParam(r, "partyRef")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "DEFAULT"
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	d, err := h.store.FindActiveDestination(r.Context(), partyRef, scope)
	if err != nil {
		if errors.Is(err, domain.ErrNoActiveDestination) {
			writeError(w, http.StatusNotFound, domain.ErrNoActiveDestination.Error())
			return
		}
		h.log.Error("GetActivePayeeDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.maskIfNotPrivileged(r, principalID, d.LegalEntityID, d)
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) GetPayeeChangeHistory(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.fetchDestinationForAuth(w, r, destinationID); !ok {
		return
	}
	events, err := h.store.ListEvents(r.Context(), destinationID)
	if err != nil {
		h.log.Error("GetPayeeChangeHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.ChangeEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}

// VerifyPayeeDestination is caller-attested (no real "BNK provider
// validation" exists anywhere in this codebase — see internal/domain's
// package doc) but the literal enforcement of "invoice-supplied bank data
// never activates destination" is structural: domain.CanVerify refuses a
// verification method that isn't genuinely independent of the candidate's
// own source.
func (h *Handler) VerifyPayeeDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	var req domain.VerifyDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, d.LegalEntityID, PayeeMasterAdmin) {
		return
	}
	// The state guard is deliberately in the store, after the idempotency
	// lookup: a retried VerifyPayeeDestination must replay its original
	// result even after the destination has moved on to APPROVAL_PENDING or
	// ACTIVE. A handler-side pre-check would turn that retry into a 409.
	if !domain.CanVerify(d.SourceType, req.VerificationMethod) {
		writeError(w, http.StatusConflict, domain.ErrVerificationNotIndependent.Error())
		return
	}
	if req.VerificationEvidenceRef == "" {
		writeError(w, http.StatusBadRequest, "verification_evidence_ref is required")
		return
	}

	updated, replay, err := h.store.VerifyDestination(r.Context(), destinationID, req, principalID, idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "destination is not in a state that accepts verification")
			return
		}
		h.log.Error("VerifyPayeeDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.maskIfNotPrivileged(r, principalID, updated.LegalEntityID, updated)
	markIdempotentReplay(w, replay)
	writeJSON(w, http.StatusOK, updated)
}

// ApprovePayeeDestination is the literal enforcement of the spec's own SoD
// line — the destination's own proposer cannot also approve it.
func (h *Handler) ApprovePayeeDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	if !h.authorizeOwnObject(w, r, principalID, d.LegalEntityID, PayeeMasterApprove, d.ProposedByPrincipalID) {
		return
	}

	updated, replay, err := h.store.ApproveDestination(r.Context(), destinationID, principalID, r.Header.Get("X-Correlation-ID"), idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "destination must be VERIFIED to approve")
			return
		}
		h.log.Error("ApprovePayeeDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	// com.zoikosuite.banking.payee-destination.approved is enqueued inside
	// ApproveDestination's own transaction — see pg_store.go.
	h.maskIfNotPrivileged(r, principalID, updated.LegalEntityID, updated)
	markIdempotentReplay(w, replay)
	writeJSON(w, http.StatusOK, updated)
}

// ActivateDestination is where "only one active version per party/scope"
// takes effect. AP-10 in payment-authorization-svc does re-fetch this
// service's active destination before requesting, approving, consuming and
// validating a payment, and invalidates on a changed/absent DestinationID
// (verifyStillEligible) — so the comment that "no real caller does that yet"
// was stale. AP-10 does NOT subscribe to the outbox event below; protection
// is pull-based between those checkpoints. See SPEC_DEVIATIONS A9.
func (h *Handler) ActivateDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, d.LegalEntityID, PayeeMasterAdmin) {
		return
	}

	updated, replay, err := h.store.ActivateDestination(r.Context(), destinationID, principalID, r.Header.Get("X-Correlation-ID"), idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "destination must be APPROVAL_PENDING to activate")
			return
		}
		h.log.Error("ActivateDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	// com.zoikosuite.banking.payee-destination.activated and, when this
	// activation replaces an existing ACTIVE row, that row's .superseded
	// event are enqueued inside ActivateDestination's own transaction — see
	// pg_store.go.
	h.maskIfNotPrivileged(r, principalID, updated.LegalEntityID, updated)
	markIdempotentReplay(w, replay)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) SuspendDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	var req domain.SuspendDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, d.LegalEntityID, PayeeMasterAdmin) {
		return
	}

	updated, replay, err := h.store.SuspendDestination(r.Context(), destinationID, req, principalID, r.Header.Get("X-Correlation-ID"), idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "destination must be ACTIVE to suspend")
			return
		}
		h.log.Error("SuspendDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	// com.zoikosuite.banking.payee-destination.suspended is enqueued inside
	// SuspendDestination's own transaction — see pg_store.go.
	h.maskIfNotPrivileged(r, principalID, updated.LegalEntityID, updated)
	markIdempotentReplay(w, replay)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) SupersedeDestination(w http.ResponseWriter, r *http.Request) {
	destinationID := chi.URLParam(r, "destinationID")
	var req domain.SupersedeDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	d, ok := h.fetchDestinationForAuth(w, r, destinationID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, d.LegalEntityID, PayeeMasterAdmin) {
		return
	}

	updated, replay, err := h.store.SupersedeDestination(r.Context(), destinationID, req, principalID, r.Header.Get("X-Correlation-ID"), idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "destination is not in a state that can be superseded")
			return
		}
		h.log.Error("SupersedeDestination: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	// com.zoikosuite.banking.payee-destination.superseded is enqueued inside
	// SupersedeDestination's own transaction — see pg_store.go.
	h.maskIfNotPrivileged(r, principalID, updated.LegalEntityID, updated)
	markIdempotentReplay(w, replay)
	writeJSON(w, http.StatusOK, updated)
}
