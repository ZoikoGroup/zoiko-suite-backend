// Package handler exposes inventory-management-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateItem(ctx context.Context, it *domain.InventoryItem) error
	GetItem(ctx context.Context, itemID string) (*domain.InventoryItem, error)
	ListItems(ctx context.Context, legalEntityID string) ([]domain.InventoryItem, error)
	ActivateItem(ctx context.Context, itemID, principalID string, at time.Time) error
	RetireItem(ctx context.Context, itemID, principalID, reason string, at time.Time) error
	AmendProfile(ctx context.Context, itemID string, description, itemType, physicalCharacteristics *string) error
	LinkCatalogItem(ctx context.Context, itemID, catalogItemID string) error

	SetTrackingPolicy(ctx context.Context, p *domain.TrackingPolicy, at time.Time) error
	GetCurrentTrackingPolicy(ctx context.Context, itemID string) (*domain.TrackingPolicy, error)
	SetValuationPolicy(ctx context.Context, p *domain.ValuationPolicy) error
	GetCurrentValuationPolicy(ctx context.Context, itemID string) (*domain.ValuationPolicy, error)
	GetProfileAsOf(ctx context.Context, itemID string, at time.Time) (*domain.TrackingPolicy, *domain.ValuationPolicy, error)

	// INV-02 (Inventory Location) — see internal/store/location_store.go's
	// own doc comments for the authority boundary these implement.
	CreateLocation(ctx context.Context, l *domain.InventoryLocation, parentLocationID *string) error
	GetLocation(ctx context.Context, locationID string) (*domain.InventoryLocation, error)
	ListLocations(ctx context.Context, legalEntityID string, eligibleOnly bool) ([]domain.InventoryLocation, error)
	ActivateLocation(ctx context.Context, locationID, principalID string, at time.Time) error
	SuspendLocation(ctx context.Context, locationID, principalID, reason string, at time.Time) error
	SetQuarantine(ctx context.Context, locationID, principalID, reason string, quarantine bool, at time.Time) error
	RetireLocation(ctx context.Context, locationID, principalID, reason string, at time.Time) error
	AmendLocationMetadata(ctx context.Context, locationID string, description, custodianEntity *string) error
	GetCurrentParent(ctx context.Context, locationID string) (*string, error)
	GetAncestorChain(ctx context.Context, locationID string) ([]string, error)
	ReparentLocation(ctx context.Context, locationID, newParentLocationID, principalID string, at time.Time) error
	GetParentAsOf(ctx context.Context, locationID string, at time.Time) (*string, error)

	// INV-03 (Inventory Movement) — see internal/store/movement_store.go's
	// own doc comments for the authority boundary these implement.
	CreateMovement(ctx context.Context, m *domain.InventoryMovement) error
	GetMovement(ctx context.Context, movementID string) (*domain.InventoryMovement, error)
	ListMovements(ctx context.Context, itemID string) ([]domain.InventoryMovement, error)
	ValidateMovement(ctx context.Context, movementID string, at time.Time) error
	CommitMovement(ctx context.Context, movementID, principalID string, at time.Time) error
	GetOnHand(ctx context.Context, itemID, locationID string) (float64, error)
	GetOnHandAsOf(ctx context.Context, itemID, locationID string, at time.Time) (float64, error)
	CreateCorrectionMovement(ctx context.Context, originalMovementID, principalID, reason string, isSupersede bool, newMovementID string, at time.Time) (*domain.InventoryMovement, error)
}

// PeriodChecker is INV-03's own real "hard-closed-period" dependency on
// financial-close-svc — see internal/clients/close.go's own doc comment.
type PeriodChecker interface {
	CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error
}

// Publisher is the event-publishing contract the handler depends on —
// the spec's own named Events: "InventoryItemCreated; InventoryItemActivated;
// InventoryPolicyChanged; InventoryItemRetired."
type Publisher interface {
	PublishInventoryItemCreated(ctx context.Context, correlationID, actorID string, it domain.InventoryItem)
	PublishInventoryItemActivated(ctx context.Context, correlationID, actorID string, it domain.InventoryItem)
	PublishInventoryPolicyChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, itemID, policyType string)
	PublishInventoryItemRetired(ctx context.Context, correlationID, actorID string, it domain.InventoryItem)

	// INV-02 (Inventory Location) — the spec's own named Events:
	// "InventoryLocationCreated; InventoryLocationActivated;
	// InventoryLocationQuarantined; InventoryLocationChanged;
	// InventoryLocationRetired."
	PublishInventoryLocationCreated(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation)
	PublishInventoryLocationActivated(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation)
	PublishInventoryLocationQuarantined(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation)
	PublishInventoryLocationChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, locationID, changeType string)
	PublishInventoryLocationRetired(ctx context.Context, correlationID, actorID string, l domain.InventoryLocation)

	// INV-03 (Inventory Movement) — the spec's own named Events:
	// "InventoryMovementCommitted; InventoryMovementReversed;
	// InventoryTransferred; InventoryReceived; InventoryIssued;
	// InventoryMovementExceptionRaised."
	PublishInventoryMovementCommitted(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement)
	PublishInventoryMovementReversed(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement)
	PublishInventoryTransferred(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement)
	PublishInventoryReceived(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement)
	PublishInventoryIssued(ctx context.Context, correlationID, actorID string, m domain.InventoryMovement)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc — the spec's own
// Permissions field: "inventory.item.read; inventory.item.manage;
// inventory.policy.assign," mapped onto this platform's INVENTORY_*
// namespace convention. actionInventoryPolicyAssign is deliberately
// distinct from actionInventoryItemManage — the spec's own SoD:
// "commercial pricing/catalog editors cannot alter inventory accounting
// policy."
const (
	actionInventoryItemRead     = "INVENTORY_ITEM_READ"
	actionInventoryItemManage   = "INVENTORY_ITEM_MANAGE"
	actionInventoryPolicyAssign = "INVENTORY_POLICY_ASSIGN"

	// INV-02 (Inventory Location) actions — the spec's own Permissions
	// field: "inventory.location.read; inventory.location.manage;
	// inventory.location.quarantine."
	actionInventoryLocationRead       = "INVENTORY_LOCATION_READ"
	actionInventoryLocationManage     = "INVENTORY_LOCATION_MANAGE"
	actionInventoryLocationQuarantine = "INVENTORY_LOCATION_QUARANTINE"

	// INV-03 (Inventory Movement) actions — the spec's own Permissions
	// field: "inventory.movement.read; inventory.movement.create;
	// inventory.movement.adjust; inventory.movement.reverse."
	// actionInventoryMovementAdjust is deliberately distinct from
	// actionInventoryMovementCreate — the spec's own SoD: "source domain
	// cannot create arbitrary inventory adjustment disguised as
	// receipt/issue."
	actionInventoryMovementRead    = "INVENTORY_MOVEMENT_READ"
	actionInventoryMovementCreate  = "INVENTORY_MOVEMENT_CREATE"
	actionInventoryMovementAdjust  = "INVENTORY_MOVEMENT_ADJUST"
	actionInventoryMovementReverse = "INVENTORY_MOVEMENT_REVERSE"
)

type Handler struct {
	store         Store
	publisher     Publisher
	authz         AuthZClient
	periodChecker PeriodChecker
	log           *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

// WithPeriodChecker sets INV-03's own real dependency on
// financial-close-svc. Left unconfigured, CommitMovement and the two
// correction commands refuse with a clear error rather than a nil
// dereference — the same posture asset-management-svc's own
// WithLedgerClient takes for a never-optional dependency.
func (h *Handler) WithPeriodChecker(c PeriodChecker) *Handler {
	h.periodChecker = c
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/items", func(r chi.Router) {
		r.Post("/", h.CreateInventoryItem)
		r.Get("/", h.ListItems)
		r.Get("/{id}", h.GetInventoryItem)
		r.Post("/{id}/activate", h.ActivateInventoryItem)
		r.Post("/{id}/amend", h.AmendInventoryProfile)
		r.Post("/{id}/retire", h.RetireInventoryItem)
		r.Post("/{id}/link-catalog", h.LinkCommercialCatalogItem)
		r.Post("/{id}/tracking-policy", h.SetTrackingPolicy)
		r.Get("/{id}/tracking-policy", h.GetTrackingPolicy)
		r.Post("/{id}/valuation-policy", h.SetValuationPolicyFutureEffective)
		r.Get("/{id}/valuation-policy", h.GetValuationPolicy)
		r.Get("/{id}/profile-as-of", h.GetInventoryProfileAsOf)
		r.Get("/{id}/available-actions", h.GetAvailableActions)
	})
	r.Route("/v1/locations", func(r chi.Router) {
		r.Post("/", h.CreateInventoryLocation)
		r.Get("/", h.ListEligibleLocations)
		r.Get("/{id}", h.GetLocation)
		r.Post("/{id}/activate", h.ActivateLocation)
		r.Post("/{id}/suspend", h.SuspendLocation)
		r.Post("/{id}/amend", h.AmendLocationMetadata)
		r.Post("/{id}/reparent", h.ReparentLocationControlled)
		r.Post("/{id}/quarantine", h.SetQuarantineState)
		r.Post("/{id}/retire", h.RetireLocation)
		r.Get("/{id}/hierarchy", h.GetLocationHierarchy)
		r.Get("/{id}/as-of", h.GetLocationAsOf)
	})
	r.Route("/v1/movements", func(r chi.Router) {
		r.Post("/", h.CreateInventoryMovement)
		r.Post("/receive", h.ReceiveInventory)
		r.Post("/issue", h.IssueInventory)
		r.Post("/transfer", h.TransferInventory)
		r.Post("/adjust", h.AdjustInventoryFromApprovedCount)
		r.Get("/", h.ListMovements)
		r.Get("/{id}", h.GetMovement)
		r.Post("/{id}/validate", h.ValidateMovement)
		r.Post("/{id}/commit", h.CommitMovement)
		r.Post("/{id}/reverse", h.ReverseMovement)
		r.Post("/{id}/supersede", h.SupersedeMovement)
	})
	r.Get("/v1/on-hand", h.GetOnHand)
}

// ── POST /v1/items ────────────────────────────────────────────────────────

func (h *Handler) CreateInventoryItem(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryItemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.SKU == "" || req.Description == "" || req.BaseUOM == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, sku, description and base_uom are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInventoryItemManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	it := &domain.InventoryItem{
		ItemID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		SKU: req.SKU, Description: req.Description, BaseUOM: req.BaseUOM, ItemType: req.ItemType,
		PhysicalCharacteristics: req.PhysicalCharacteristics,
		Status:                  domain.ItemStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateItem(r.Context(), it); err != nil {
		if errors.Is(err, domain.ErrDuplicateSKU) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_sku", err.Error())
			return
		}
		h.log.Error("failed to create inventory item", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishInventoryItemCreated(r.Context(), getCorrelationID(r), principalID, *it)
	writeJSON(w, http.StatusCreated, it)
}

// ── GET /v1/items/{id}, GET /v1/items ────────────────────────────────────────

func (h *Handler) GetInventoryItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (h *Handler) ListItems(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListItems(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListItems: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.InventoryItem{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/items/{id}/activate ─────────────────────────────────────────────

// ActivateInventoryItem refuses (negative path: "Missing ... valuation
// policy ... blocks stock movement/valuation") without a valuation policy
// already assigned — see migration 000001's doc comment.
func (h *Handler) ActivateInventoryItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	vp, err := h.store.GetCurrentValuationPolicy(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if vp == nil {
		writeError(w, http.StatusUnprocessableEntity, "valuation_policy_required", domain.ErrValuationPolicyRequiredForActivation.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.store.ActivateItem(r.Context(), id, principalID, now); err != nil {
		h.writeItemErr(w, err)
		return
	}
	it.Status, it.ActivatedAt, it.ActivatedByPrincipalID = domain.ItemStatusActive, &now, &principalID
	h.publisher.PublishInventoryItemActivated(r.Context(), getCorrelationID(r), principalID, *it)
	writeJSON(w, http.StatusOK, it)
}

// ── POST /v1/items/{id}/amend ─────────────────────────────────────────────────

// AmendInventoryProfile never accepts base_uom or sku — see
// domain.AmendInventoryProfileRequest's own doc comment and migration
// 000001's negative path #2.
func (h *Handler) AmendInventoryProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AmendInventoryProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.AmendProfile(r.Context(), id, req.Description, req.ItemType, req.PhysicalCharacteristics); err != nil {
		h.writeItemErr(w, err)
		return
	}
	updated, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/items/{id}/retire ────────────────────────────────────────────────

func (h *Handler) RetireInventoryItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RetireInventoryItemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.RetireItem(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeItemErr(w, err)
		return
	}
	it.Status, it.RetiredAt, it.RetiredByPrincipalID, it.RetirementReason = domain.ItemStatusRetired, &now, &principalID, &req.Reason
	h.publisher.PublishInventoryItemRetired(r.Context(), getCorrelationID(r), principalID, *it)
	writeJSON(w, http.StatusOK, it)
}

// ── POST /v1/items/{id}/link-catalog ──────────────────────────────────────────

// LinkCommercialCatalogItem never touches valuation_method — see
// migration 000001's negative path #1 and the spec's own SoD.
func (h *Handler) LinkCommercialCatalogItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.LinkCommercialCatalogItemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CatalogItemID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "catalog_item_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.LinkCatalogItem(r.Context(), id, req.CatalogItemID); err != nil {
		h.writeItemErr(w, err)
		return
	}
	it.CatalogItemID = &req.CatalogItemID
	writeJSON(w, http.StatusOK, it)
}

// ── GET /v1/items/{id}/available-actions ──────────────────────────────────────

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_status":     it.Status,
		"available_statuses": domain.ValidItemTransitions[it.Status],
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return tenantID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
}

func (h *Handler) writeItemErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrItemNotFound):
		writeError(w, http.StatusNotFound, "item_not_found", "")
	case errors.Is(err, domain.ErrInvalidItemTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("inventory store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error_code": code, "error_message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
