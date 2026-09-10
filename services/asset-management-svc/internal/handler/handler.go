// Package handler exposes asset-management-svc's REST API.
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

	"zoiko.io/asset-management-svc/internal/clients"
	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateAsset(ctx context.Context, a *domain.FixedAsset) error
	GetAsset(ctx context.Context, assetID string) (*domain.FixedAsset, error)
	ListAssets(ctx context.Context, legalEntityID string) ([]domain.FixedAsset, error)
	// GetNetBookValueTotal backs GET /v1/assets/net-book-value — see
	// internal/store/depreciation_store.go's own doc comment. Serves the
	// AST/INV/PRJ domain spec's own §9 "Assets → GL" reconciliation
	// assertion, consumed by financial-close-svc's ACC-06.
	GetNetBookValueTotal(ctx context.Context, legalEntityID, bookID string) (float64, error)
	AddComponent(ctx context.Context, c *domain.AssetComponent) error
	AssignBookProfile(ctx context.Context, b *domain.AssetBookAssignment) error
	RegisterAsset(ctx context.Context, assetID, principalID string, at time.Time) error
	CapitalizeAsset(ctx context.Context, assetID, principalID string, at time.Time) error
	SuspendAsset(ctx context.Context, assetID, principalID, reason string, at time.Time) error
	ReactivateAsset(ctx context.Context, assetID string) error
	UpdateMetadata(ctx context.Context, assetID string, description, custodianID, locationID, tagSerial *string) error
	MergeAssets(ctx context.Context, sourceAssetID, targetAssetID, principalID string, at time.Time) error
	SplitAsset(ctx context.Context, newAsset *domain.FixedAsset, sourceAssetID string, componentIDs []string) error

	// AST-02 (Depreciation) — see internal/store/depreciation_store.go's
	// own doc comments for the authority boundary these implement.
	CreateDepreciationSchedule(ctx context.Context, sch *domain.DepreciationSchedule) error
	GetCurrentDepreciationSchedule(ctx context.Context, scheduleID string) (*domain.DepreciationSchedule, error)
	RecalculateSchedule(ctx context.Context, scheduleID string, newVersion *domain.DepreciationSchedule, at time.Time) error
	CreateDepreciationRun(ctx context.Context, r *domain.DepreciationRun) error
	GetDepreciationRun(ctx context.Context, runID string) (*domain.DepreciationRun, error)
	FreezeDepreciationPopulation(ctx context.Context, runID, legalEntityID string, at time.Time) (frozenCount int, err error)
	ValidateDepreciationRun(ctx context.Context, runID string, at time.Time) (lineCount int, err error)
	ApproveDepreciationRun(ctx context.Context, runID, principalID string, at time.Time) error
	MarkDepreciationRunEmitted(ctx context.Context, runID, journalID string, at time.Time) error
	SupersedeDepreciationRun(ctx context.Context, runID, principalID string, at time.Time) error

	// AST-03 (Asset Event) — see internal/store/asset_event_store.go's own
	// doc comments for the authority boundary these implement.
	CreateAssetEvent(ctx context.Context, e *domain.AssetEvent) error
	GetAssetEvent(ctx context.Context, eventID string) (*domain.AssetEvent, error)
	ListAssetEvents(ctx context.Context, assetID string) ([]domain.AssetEvent, error)
	ValidateAssetEvent(ctx context.Context, eventID string, at time.Time) error
	ApproveAssetEvent(ctx context.Context, eventID, principalID string, at time.Time) error
	ApplyAssetEvent(ctx context.Context, eventID string, at time.Time, journalID *string) error
	ReverseAssetEvent(ctx context.Context, eventID, principalID, reason string, at time.Time) error
	SupersedeAssetEvent(ctx context.Context, eventID, principalID, reason string, at time.Time) error
}

// DepreciationLedgerClient is AST-02/AST-03's shared real "ACC-04"
// dependency — see internal/clients/ledger.go's own doc comments. One
// client, one WithLedgerClient wiring, both capabilities' methods.
type DepreciationLedgerClient interface {
	PostDepreciationAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []clients.LedgerLine) (journalID string, err error)
	ReverseDepreciationJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error
	PostAssetEventAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []clients.LedgerLine) (journalID string, err error)
	ReverseAssetEventJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error

	// CheckPeriodOpen is AST-03's own real "hard-closed-period" dependency
	// on financial-close-svc — see internal/clients/ledger.go's own doc
	// comment.
	CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error
}

// Publisher is the event-publishing contract the handler depends on —
// the spec's own named Events: "AssetRegistered; AssetComponentAdded;
// AssetBookAssigned; AssetCapitalizationRequested; AssetMetadataChanged;
// AssetSuspended."
type Publisher interface {
	PublishAssetRegistered(ctx context.Context, correlationID, actorID string, a domain.FixedAsset)
	PublishAssetComponentAdded(ctx context.Context, correlationID, actorID string, a domain.FixedAsset, c domain.AssetComponent)
	PublishAssetBookAssigned(ctx context.Context, correlationID, actorID string, a domain.FixedAsset, b domain.AssetBookAssignment)
	PublishAssetCapitalizationRequested(ctx context.Context, correlationID, actorID string, a domain.FixedAsset)
	PublishAssetMetadataChanged(ctx context.Context, correlationID, actorID string, a domain.FixedAsset)
	PublishAssetSuspended(ctx context.Context, correlationID, actorID string, a domain.FixedAsset)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc — the spec's own
// Permissions field: "asset.read; asset.create; asset.approve;
// asset.component.manage; asset.book.assign; asset.metadata.manage,"
// mapped onto this platform's ASSET_* namespace convention (every other
// service in this repo uses a namespaced upper-snake action, not the
// doc's dotted form).
const (
	actionAssetCreate          = "ASSET_CREATE"
	actionAssetApprove         = "ASSET_APPROVE_REGISTRATION"
	actionAssetComponentManage = "ASSET_COMPONENT_MANAGE"
	actionAssetBookAssign      = "ASSET_BOOK_ASSIGN"
	actionAssetMetadataManage  = "ASSET_METADATA_MANAGE"
	actionAssetCapitalize      = "ASSET_REQUEST_CAPITALIZATION"
	actionAssetSuspend         = "ASSET_SUSPEND"
	actionAssetView            = "ASSET_VIEW"
	// actionAssetMergeSplit is deliberately its own action, distinct from
	// every other asset action — the spec's own SoD: "merge/split
	// requires independent approval and lineage preservation."
	actionAssetMergeSplit = "ASSET_MERGE_SPLIT"

	// AST-02 (Depreciation) actions — the spec's own Permissions field:
	// "asset.depreciation.read; asset.depreciation.run;
	// asset.depreciation.approve; asset.depreciation.supersede."
	// actionDepreciationApprove is deliberately distinct from
	// actionDepreciationRun — the spec's own SoD: "Run preparer cannot
	// self-approve material exceptions," the same maker/checker posture
	// ACC-03/AST-01 already apply to their own approval steps.
	actionDepreciationScheduleBuild = "DEPRECIATION_SCHEDULE_BUILD"
	actionDepreciationRun           = "DEPRECIATION_RUN"
	actionDepreciationApprove       = "DEPRECIATION_APPROVE"
	actionDepreciationSupersede     = "DEPRECIATION_SUPERSEDE"
	actionDepreciationView          = "DEPRECIATION_VIEW"

	// AST-03 (Asset Event) actions — the spec's own Permissions field:
	// "asset.event.read; asset.event.create; asset.event.approve;
	// asset.event.apply; asset.event.correct." actionAssetEventApprove is
	// deliberately distinct from actionAssetEventCreate — the spec's own
	// SoD: "Event initiator cannot approve material impairment/
	// revaluation/disposal where maker-checker applies."
	actionAssetEventCreate  = "ASSET_EVENT_CREATE"
	actionAssetEventApprove = "ASSET_EVENT_APPROVE"
	actionAssetEventApply   = "ASSET_EVENT_APPLY"
	actionAssetEventCorrect = "ASSET_EVENT_CORRECT"
	actionAssetEventView    = "ASSET_EVENT_VIEW"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	ledger    DepreciationLedgerClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

// WithLedgerClient sets AST-02's own real dependency on general-ledger-svc.
// Left unconfigured, EmitDepreciationAccountingEvent and
// SupersedeDepreciationRun refuse with a clear error rather than a nil
// dereference — a deployment that hasn't wired it up yet gets a loud
// failure at the one place it matters, not a silent skip (unlike
// intercompany-accounting-svc's own optional entity-registry check,
// posting to the ledger is never optional for this capability).
func (h *Handler) WithLedgerClient(c DepreciationLedgerClient) *Handler {
	h.ledger = c
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/assets", func(r chi.Router) {
		r.Post("/", h.CreateAssetCandidate)
		r.Get("/", h.ListAssets)
		r.Get("/net-book-value", h.GetNetBookValueTotal)
		r.Get("/{id}", h.GetAsset)
		r.Post("/{id}/approve", h.ApproveAssetRegistration)
		r.Post("/{id}/components", h.AddComponent)
		r.Post("/{id}/book-profiles", h.AssignAssetBookProfile)
		r.Post("/{id}/request-capitalization", h.RequestCapitalization)
		r.Post("/{id}/transfer-custody", h.TransferCustody)
		r.Post("/{id}/transfer-location", h.TransferLocation)
		r.Post("/{id}/amend-metadata", h.AmendNonFinancialMetadata)
		r.Post("/{id}/suspend", h.SuspendAsset)
		r.Post("/{id}/merge", h.MergeAssetControlled)
		r.Post("/{id}/split", h.SplitAssetControlled)
		r.Get("/{id}/available-actions", h.GetAvailableActions)
	})
	r.Route("/v1/depreciation-schedules", func(r chi.Router) {
		r.Post("/", h.BuildDepreciationSchedule)
		r.Get("/{id}", h.GetDepreciationSchedule)
		r.Post("/{id}/recalculate", h.RecalculateSchedule)
	})
	r.Route("/v1/depreciation-runs", func(r chi.Router) {
		r.Post("/", h.CreateDepreciationRun)
		r.Get("/{id}", h.GetDepreciationRun)
		r.Post("/{id}/freeze", h.FreezeDepreciationPopulation)
		r.Post("/{id}/validate", h.ValidateDepreciationRun)
		r.Post("/{id}/approve", h.ApproveDepreciationRun)
		r.Post("/{id}/emit", h.EmitDepreciationAccountingEvent)
		r.Post("/{id}/supersede", h.SupersedeDepreciationRun)
	})
	r.Route("/v1/asset-events", func(r chi.Router) {
		r.Post("/", h.CreateAssetEvent)
		r.Post("/record-disposal", h.RecordDisposal)
		r.Post("/record-impairment", h.RecordImpairment)
		r.Post("/record-revaluation", h.RecordRevaluation)
		r.Post("/record-component-replacement", h.RecordComponentReplacement)
		r.Get("/", h.ListAssetEvents)
		r.Get("/{id}", h.GetAssetEvent)
		r.Post("/{id}/validate", h.ValidateAssetEvent)
		r.Post("/{id}/approve", h.ApproveAssetEvent)
		r.Post("/{id}/apply", h.ApplyAssetEvent)
		r.Post("/{id}/reverse", h.ReverseAssetEvent)
		r.Post("/{id}/supersede", h.SupersedeAssetEvent)
	})
}

// ── POST /v1/assets ──────────────────────────────────────────────────────────

func (h *Handler) CreateAssetCandidate(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetCandidateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.AssetCategory == "" || req.Description == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, asset_category and description are required")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionAssetCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	a := &domain.FixedAsset{
		AssetID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		AssetCategory: req.AssetCategory, TagSerial: req.TagSerial, Description: req.Description,
		CustodianID: req.CustodianID, LocationID: req.LocationID,
		AcquisitionSourceRef: req.AcquisitionSourceRef, AcquisitionDate: req.AcquisitionDate, InServiceDate: req.InServiceDate,
		Status: domain.AssetStatusCandidate, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAsset(r.Context(), a); err != nil {
		h.log.Error("failed to create asset candidate", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// ── GET /v1/assets/{id}, GET /v1/assets ──────────────────────────────────────

func (h *Handler) GetAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// GetNetBookValueTotal is a read-only aggregate over real, live schedule
// data — never a caller-declared or cached figure. financial-close-svc's
// ACC-06 calls this directly; see internal/store's own doc comment for
// the calculation and why book_id is required.
func (h *Handler) GetNetBookValueTotal(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	bookID := r.URL.Query().Get("book_id")
	if legalEntityID == "" || bookID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and book_id are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionAssetView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	total, err := h.store.GetNetBookValueTotal(r.Context(), legalEntityID, bookID)
	if err != nil {
		h.log.Error("GetNetBookValueTotal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]float64{"net_book_value_total": total})
}

func (h *Handler) ListAssets(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionAssetView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListAssets(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListAssets: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.FixedAsset{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/assets/{id}/approve ─────────────────────────────────────────────

// ApproveAssetRegistration moves CANDIDATE -> REGISTERED — the spec's own
// SoD: "Creator cannot approve capitalization where maker-checker is
// required." Enforced here the same way ACC-03 enforces submit/approve
// separation: the approver must differ from the asset's own creator.
func (h *Handler) ApproveAssetRegistration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if a.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", "the principal who created this asset candidate may not also approve its registration")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.RegisterAsset(r.Context(), id, principalID, now); err != nil {
		h.writeAssetErr(w, err)
		return
	}
	a.Status, a.RegisteredAt, a.RegisteredByPrincipalID = domain.AssetStatusRegistered, &now, &principalID
	h.publisher.PublishAssetRegistered(r.Context(), getCorrelationID(r), principalID, *a)
	writeJSON(w, http.StatusOK, a)
}

// ── POST /v1/assets/{id}/components ──────────────────────────────────────────

func (h *Handler) AddComponent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AddComponentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "description is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetComponentManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	c := &domain.AssetComponent{
		ComponentID: uuid.NewString(), AssetID: id, Description: req.Description, CostSourceRef: req.CostSourceRef,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.AddComponent(r.Context(), c); err != nil {
		h.log.Error("failed to add asset component", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishAssetComponentAdded(r.Context(), getCorrelationID(r), principalID, *a, *c)
	writeJSON(w, http.StatusCreated, c)
}

// ── POST /v1/assets/{id}/book-profiles ───────────────────────────────────────

func (h *Handler) AssignAssetBookProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AssignAssetBookProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.BookID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "book_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetBookAssign); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	b := &domain.AssetBookAssignment{
		AssignmentID: uuid.NewString(), AssetID: id, BookID: req.BookID,
		UsefulLifeMonthsProposal: req.UsefulLifeMonthsProposal, ResidualValueProposal: req.ResidualValueProposal,
		Status: domain.BookAssignmentStatusActive, AssignedAt: time.Now().UTC(), AssignedByPrincipalID: principalID,
	}
	if err := h.store.AssignBookProfile(r.Context(), b); err != nil {
		if errors.Is(err, domain.ErrRetroactiveBookProfileChange) {
			writeError(w, http.StatusUnprocessableEntity, "retroactive_book_profile_change", err.Error())
			return
		}
		h.log.Error("failed to assign asset book profile", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishAssetBookAssigned(r.Context(), getCorrelationID(r), principalID, *a, *b)
	writeJSON(w, http.StatusCreated, b)
}

// ── POST /v1/assets/{id}/request-capitalization ──────────────────────────────

// RequestCapitalization moves REGISTERED -> ACTIVE ("Capitalized/Active").
// Refuses without a recorded acquisition_source_ref — the spec's own
// negative path, "Asset capitalized without source evidence."
func (h *Handler) RequestCapitalization(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if a.AcquisitionSourceRef == "" {
		writeError(w, http.StatusUnprocessableEntity, "capitalization_requires_evidence", domain.ErrCapitalizationRequiresEvidence.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetCapitalize); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.CapitalizeAsset(r.Context(), id, principalID, now); err != nil {
		h.writeAssetErr(w, err)
		return
	}
	a.Status, a.CapitalizedAt, a.CapitalizedByPrincipalID = domain.AssetStatusActive, &now, &principalID
	correlationID := getCorrelationID(r)
	h.publisher.PublishAssetCapitalizationRequested(r.Context(), correlationID, principalID, *a)
	writeJSON(w, http.StatusOK, a)
}

// ── POST /v1/assets/{id}/transfer-custody, transfer-location, amend-metadata ─

func (h *Handler) TransferCustody(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.TransferCustodyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CustodianID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "custodian_id is required")
		return
	}
	h.amendMetadata(w, r, id, nil, &req.CustodianID, nil, nil)
}

func (h *Handler) TransferLocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.TransferLocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LocationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "location_id is required")
		return
	}
	h.amendMetadata(w, r, id, nil, nil, &req.LocationID, nil)
}

func (h *Handler) AmendNonFinancialMetadata(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AmendNonFinancialMetadataRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	h.amendMetadata(w, r, id, req.Description, req.CustodianID, req.LocationID, req.TagSerial)
}

// amendMetadata is the shared path for every AST-01 command that changes
// custody/location/descriptive metadata only — the spec's own SoD:
// "custodian/location metadata change cannot alter accounting basis."
// None of these ever touch Status, book assignments or acquisition
// evidence.
func (h *Handler) amendMetadata(w http.ResponseWriter, r *http.Request, id string, description, custodianID, locationID, tagSerial *string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetMetadataManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.UpdateMetadata(r.Context(), id, description, custodianID, locationID, tagSerial); err != nil {
		h.writeAssetErr(w, err)
		return
	}
	updated, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	h.publisher.PublishAssetMetadataChanged(r.Context(), getCorrelationID(r), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/assets/{id}/suspend ─────────────────────────────────────────────

func (h *Handler) SuspendAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SuspendAssetRequest
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
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetSuspend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SuspendAsset(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeAssetErr(w, err)
		return
	}
	a.Status, a.SuspendedAt, a.SuspendedByPrincipalID, a.SuspensionReason = domain.AssetStatusSuspended, &now, &principalID, &req.Reason
	correlationID := getCorrelationID(r)
	h.publisher.PublishAssetSuspended(r.Context(), correlationID, principalID, *a)
	writeJSON(w, http.StatusOK, a)
}

// ── POST /v1/assets/{id}/merge ────────────────────────────────────────────────

// MergeAssetControlled implements the spec's own negative path, "Physical
// asset merged across legal entities" — refused before the store is even
// asked, and re-verified by the store itself against the real rows.
func (h *Handler) MergeAssetControlled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.MergeAssetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TargetAssetID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "target_asset_id is required")
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
	source, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, source.LegalEntityID, actionAssetMergeSplit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.MergeAssets(r.Context(), id, req.TargetAssetID, principalID, now); err != nil {
		if errors.Is(err, domain.ErrMergeAcrossLegalEntities) {
			writeError(w, http.StatusUnprocessableEntity, "merge_across_legal_entities", err.Error())
			return
		}
		h.writeAssetErr(w, err)
		return
	}
	source.Status, source.MergedIntoAssetID = domain.AssetStatusMerged, &req.TargetAssetID
	writeJSON(w, http.StatusOK, source)
}

// ── POST /v1/assets/{id}/split ────────────────────────────────────────────────

func (h *Handler) SplitAssetControlled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SplitAssetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.ComponentIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_components_named", domain.ErrNoComponentsNamed.Error())
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "description is required")
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
	source, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, source.LegalEntityID, actionAssetMergeSplit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	newAsset := &domain.FixedAsset{
		AssetID: uuid.NewString(), LegalEntityID: source.LegalEntityID, AssetCategory: source.AssetCategory,
		Description: req.Description, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.SplitAsset(r.Context(), newAsset, id, req.ComponentIDs); err != nil {
		if errors.Is(err, domain.ErrComponentNotOnAsset) {
			writeError(w, http.StatusUnprocessableEntity, "component_not_on_asset", err.Error())
			return
		}
		h.log.Error("failed to split asset", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	newAsset.Status = domain.AssetStatusCandidate
	newAsset.SplitFromAssetID = &id
	writeJSON(w, http.StatusCreated, newAsset)
}

// ── GET /v1/assets/{id}/available-actions ────────────────────────────────────

// GetAvailableActions derives from the exact same map every lifecycle
// handler checks against — ValidAssetTransitions — so it can never
// advertise an action that would then be refused.
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetAsset(r.Context(), id)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionAssetView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_status":     a.Status,
		"available_statuses": domain.ValidAssetTransitions[a.Status],
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

func (h *Handler) writeAssetErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAssetNotFound):
		writeError(w, http.StatusNotFound, "asset_not_found", "")
	case errors.Is(err, domain.ErrComponentNotFound):
		writeError(w, http.StatusNotFound, "component_not_found", "")
	case errors.Is(err, domain.ErrInvalidAssetTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("asset store unavailable", zap.Error(err))
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
