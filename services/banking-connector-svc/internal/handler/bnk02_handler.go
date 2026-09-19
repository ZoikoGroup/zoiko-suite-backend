// BNK-02's own HTTP surface — consent/token/health lifecycle for bank
// connections, added to this service's existing /v1/banking routes.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/clients"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/events"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

const (
	BANKING_CONNECTION_MANAGE = "BANKING_CONNECTION_MANAGE"
	BANKING_CONNECTION_REVOKE = "BANKING_CONNECTION_REVOKE"
)

// BNK02Handler embeds *Handler — same "separate registration function"
// pattern used for every other capability added to an existing service
// in this build — so its extra dependencies (the two narrow clients)
// don't leak into the original Handler's constructor signature.
type BNK02Handler struct {
	*Handler
	bnk02Store store.BNK02Store
	bankAccts  clients.BankAccountClient
	vault      clients.VaultClient
}

// RegisterBNK02Routes mounts BNK-02's routes on r. Called separately from
// RegisterRoutes in cmd/server/main.go.
func RegisterBNK02Routes(r chi.Router, h *Handler, bnk02Store store.BNK02Store, bankAccts clients.BankAccountClient, vault clients.VaultClient) {
	bh := &BNK02Handler{Handler: h, bnk02Store: bnk02Store, bankAccts: bankAccts, vault: vault}
	r.Route("/v1/banking/connections", func(r chi.Router) {
		r.Post("/request", bh.InitiateConnection)
		r.Get("/{id}/events", bh.GetConnectionEvents)
		r.Post("/{id}/complete-authorization", bh.CompleteConnectionAuthorization)
		r.Post("/{id}/activate", bh.ActivateConnection)
		r.Post("/{id}/refresh", bh.RefreshConnection)
		r.Post("/{id}/trigger-reconsent", bh.TriggerReconsent)
		r.Post("/{id}/suspend", bh.SuspendConnection)
		r.Post("/{id}/revoke", bh.RevokeConnection)
		r.Post("/{id}/reconnect", bh.ReconnectProvider)
		r.Post("/{id}/rotate-credential", bh.RotateConnectionCredential)
	})
	r.Post("/v1/banking/region-policies", bh.CreateRegionPolicy)
}

func (h *BNK02Handler) fetchConnectionForAuth(w http.ResponseWriter, r *http.Request, id string) (*domain.BankConnection, bool) {
	conn, err := h.store.GetConnectionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return nil, false
		}
		h.logger.Error("fetchConnectionForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to load bank connection")
		return nil, false
	}
	return conn, true
}

func (h *BNK02Handler) writeConnectionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrConnectionNotFound):
		writeError(w, http.StatusNotFound, "bank connection not found")
	case errors.Is(err, domain.ErrInvalidConnectionTransition):
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits this action")
	default:
		h.logger.Error("bank connection operation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "bank connection operation failed")
	}
}

// enforceRegionPolicy is BNK-02's region/residency check: it must run
// before REQUESTED->AUTHORIZING (CompleteConnectionAuthorization) and
// before AUTHORIZING->ACTIVE (ActivateConnection). A legal entity with no
// configured bank_region_policies rows has no restriction — see
// IsRegionAllowed's own doc comment — so this is a no-op for every legal
// entity that hasn't opted in. Writes the HTTP response itself and
// returns false on refusal, matching fetchConnectionForAuth's convention.
func (h *BNK02Handler) enforceRegionPolicy(w http.ResponseWriter, r *http.Request, tenantID string, conn *domain.BankConnection) bool {
	allowed, err := h.bnk02Store.IsRegionAllowed(r.Context(), tenantID, conn.LegalEntityID, conn.Region)
	if err != nil {
		h.logger.Error("enforceRegionPolicy: store unavailable", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to evaluate region policy")
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, domain.ErrConnectionRegionNotAllowed.Error())
		return false
	}
	return true
}

type createRegionPolicyRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Region        string `json:"region"`
}

// CreateRegionPolicy handles POST /v1/banking/region-policies — the
// operational entry point that opts a legal entity into region/residency
// enforcement by naming its first allowed region. Authorized per legal
// entity, the same boundary as InitiateConnection: deciding which regions
// a legal entity's connections may use is a business decision scoped to
// that entity, not a platform-level control activity.
func (h *BNK02Handler) CreateRegionPolicy(w http.ResponseWriter, r *http.Request) {
	var req createRegionPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Region == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and region are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	rp, err := h.bnk02Store.CreateRegionPolicy(r.Context(), domain.CreateRegionPolicyParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, Region: req.Region, ActorPrincipalID: principalID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrRegionPolicyAlreadyExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		h.logger.Error("CreateRegionPolicy: store unavailable", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create region policy")
		return
	}
	writeJSON(w, http.StatusCreated, rp)
}

type initiateConnectionRequest struct {
	LegalEntityID  string   `json:"legal_entity_id"`
	BankAccountID  string   `json:"bank_account_id"`
	ProviderRef    string   `json:"provider_ref"`
	BankName       string   `json:"bank_name"`
	Region         string   `json:"region"`
	RequestedScope []string `json:"requested_scope"`
	CorrelationID  string   `json:"correlation_id"`
}

// InitiateConnection handles POST /v1/banking/connections/request —
// BNK-02's real entry point. Validates bank_account_id against
// treasury-svc's real BNK-01 before ever creating a REQUESTED row, so a
// connection can never point at an account that doesn't exist or belongs
// to another tenant.
func (h *BNK02Handler) InitiateConnection(w http.ResponseWriter, r *http.Request) {
	var req initiateConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.BankAccountID == "" || req.BankName == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, bank_account_id, bank_name and correlation_id are required")
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, BANKING_CONNECTION_CREATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if _, err := h.bankAccts.VerifyBankAccount(r.Context(), tenantID, req.BankAccountID, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("InitiateConnection: bank account verification failed", zap.Error(err))
		writeError(w, http.StatusFailedDependency, "referenced bank_account_id could not be verified against treasury-svc")
		return
	}

	conn, created, err := h.bnk02Store.InitiateConnection(r.Context(), domain.InitiateConnectionParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, BankAccountID: req.BankAccountID,
		ProviderRef: req.ProviderRef, BankName: req.BankName, Region: req.Region,
		RequestedScope: req.RequestedScope, CreatedByPrincipalID: principalID, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		_ = h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "banking.connection.requested", AggregateID: conn.ConnectionID, TenantID: tenantID,
			LegalEntityID: req.LegalEntityID, ActorID: principalID,
			CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: conn,
		})
	}
	writeJSON(w, status, conn)
}

func (h *BNK02Handler) GetConnectionEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	events, err := h.bnk02Store.ListConnectionEvents(r.Context(), tenantID, id)
	if err != nil {
		h.logger.Error("GetConnectionEvents: store unavailable", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list connection events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"events": events, "total": len(events)})
}

type completeAuthorizationRequest struct {
	TokenLeaseRef  string    `json:"token_lease_ref"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
	GrantedScope   []string  `json:"granted_scope"`
}

func (h *BNK02Handler) CompleteConnectionAuthorization(w http.ResponseWriter, r *http.Request) {
	var req completeAuthorizationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TokenLeaseRef == "" {
		writeError(w, http.StatusBadRequest, "token_lease_ref is required")
		return
	}
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanCompleteAuthorization(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits completing authorization")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if !h.enforceRegionPolicy(w, r, tenantID, conn) {
		return
	}
	updated, err := h.bnk02Store.CompleteConnectionAuthorization(r.Context(), domain.CompleteConnectionAuthorizationParams{
		ConnectionID: id, TenantID: tenantID, TokenLeaseRef: req.TokenLeaseRef, TokenExpiresAt: req.TokenExpiresAt,
		GrantedScope: req.GrantedScope, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *BNK02Handler) ActivateConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanActivate(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not AUTHORIZING and cannot be activated")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if !h.enforceRegionPolicy(w, r, tenantID, conn) {
		return
	}
	updated, err := h.bnk02Store.ActivateConnection(r.Context(), domain.ActivateConnectionParams{ConnectionID: id, TenantID: tenantID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type refreshConnectionRequest struct {
	NewTokenLeaseRef  string    `json:"new_token_lease_ref"`
	NewTokenExpiresAt time.Time `json:"new_token_expires_at"`
}

func (h *BNK02Handler) RefreshConnection(w http.ResponseWriter, r *http.Request) {
	var req refreshConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanRefresh(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits a refresh")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	updated, err := h.bnk02Store.RefreshConnection(r.Context(), domain.RefreshConnectionParams{
		ConnectionID: id, TenantID: tenantID, NewTokenLeaseRef: req.NewTokenLeaseRef, NewTokenExpiresAt: req.NewTokenExpiresAt, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (h *BNK02Handler) TriggerReconsent(w http.ResponseWriter, r *http.Request) {
	var req reasonBody
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanTriggerReconsent(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits triggering reconsent")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	updated, err := h.bnk02Store.TriggerReconsent(r.Context(), domain.TriggerReconsentParams{ConnectionID: id, TenantID: tenantID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *BNK02Handler) SuspendConnection(w http.ResponseWriter, r *http.Request) {
	var req reasonBody
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanSuspendConnection(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not ACTIVE/DEGRADED and cannot be suspended")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	// Real vault call: a suspended connection's lease is revoked, not just
	// locally flagged — see internal/clients/vault.go's own doc comment.
	if err := h.vault.RevokeLease(r.Context(), tenantID, conn.TokenLeaseRef, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("SuspendConnection: vault revoke failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	updated, err := h.bnk02Store.SuspendConnection(r.Context(), domain.SuspendConnectionParams{ConnectionID: id, TenantID: tenantID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// RevokeConnection is the real fix for the spec's own named negative path
// "revoked bank consent still used": the vault lease is revoked for real
// first, then the connection is marked REVOKED — a terminal state a DB
// trigger (migration 003) refuses to ever mutate again.
func (h *BNK02Handler) RevokeConnection(w http.ResponseWriter, r *http.Request) {
	var req reasonBody
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanRevokeConnection(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is already REVOKED")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_REVOKE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if err := h.vault.RevokeLease(r.Context(), tenantID, conn.TokenLeaseRef, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("RevokeConnection: vault revoke failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	updated, err := h.bnk02Store.RevokeConnection(r.Context(), domain.RevokeConnectionParams{ConnectionID: id, TenantID: tenantID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *BNK02Handler) ReconnectProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanReconnectProvider(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits reconnecting")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	updated, err := h.bnk02Store.ReconnectProvider(r.Context(), domain.ReconnectProviderParams{ConnectionID: id, TenantID: tenantID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type rotateCredentialRequest struct {
	NewTokenLeaseRef  string    `json:"new_token_lease_ref"`
	NewTokenExpiresAt time.Time `json:"new_token_expires_at"`
}

func (h *BNK02Handler) RotateConnectionCredential(w http.ResponseWriter, r *http.Request) {
	var req rotateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NewTokenLeaseRef == "" {
		writeError(w, http.StatusBadRequest, "new_token_lease_ref is required")
		return
	}
	id := chi.URLParam(r, "id")
	conn, ok := h.fetchConnectionForAuth(w, r, id)
	if !ok {
		return
	}
	if !domain.CanRotateCredential(conn.Status) {
		writeError(w, http.StatusConflict, "bank connection is not in a state that permits credential rotation")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_CONNECTION_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	// Revoke the old lease for real before switching to the new reference
	// — a rotation must not leave the old credential live.
	if err := h.vault.RevokeLease(r.Context(), tenantID, conn.TokenLeaseRef, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("RotateConnectionCredential: vault revoke of old lease failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	updated, err := h.bnk02Store.RotateConnectionCredential(r.Context(), domain.RotateConnectionCredentialParams{
		ConnectionID: id, TenantID: tenantID, NewTokenLeaseRef: req.NewTokenLeaseRef, NewTokenExpiresAt: req.NewTokenExpiresAt, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeConnectionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
