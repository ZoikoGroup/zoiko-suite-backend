package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
)

// Authorization actions. Each is checked against authorization-svc for the
// connector's legal entity before the command runs. BRIDGE_CREATE and
// BRIDGE_INGEST predate connector governance and keep their names.
const (
	BRIDGE_CREATE = "BRIDGE_CREATE"
	BRIDGE_INGEST = "BRIDGE_INGEST"

	ActionUpdate         = "CONNECTOR_UPDATE"
	ActionSubmit         = "CONNECTOR_SUBMIT"
	ActionApprove        = "CONNECTOR_APPROVE"
	ActionActivate       = "CONNECTOR_ACTIVATE"
	ActionSuspend        = "CONNECTOR_SUSPEND"
	ActionRevoke         = "CONNECTOR_REVOKE"
	ActionArchive        = "CONNECTOR_ARCHIVE"
	ActionConsent        = "CONNECTOR_CONSENT_REFRESH"
	ActionProfileUpgrade = "CONNECTOR_PROFILE_UPGRADE"
	ActionHealthReport   = "CONNECTOR_HEALTH_REPORT"
	ActionCheckpoint     = "CONNECTOR_CHECKPOINT_ADVANCE"
	ActionResetRequest   = "CONNECTOR_CHECKPOINT_RESET_REQUEST"
	ActionResetDecide    = "CONNECTOR_CHECKPOINT_RESET_APPROVE"
	ActionProfilePublish = "EXTERNAL_PROFILE_PUBLISH"
)

type Handler struct {
	svc    *connector.Service
	store  store.Store
	authz  *authz.Client
	logger *zap.Logger
}

func New(svc *connector.Service, st store.Store, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, store: st, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/bridges", func(r chi.Router) {
		r.Post("/", h.CreateBridge)
		r.Get("/", h.ListBridges)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetByID)
			r.Patch("/", h.Update)
			r.Post("/ingest", h.IngestPayload)
			r.Get("/logs", h.ListLogs)

			// Lifecycle commands.
			r.Post("/submit", h.Submit)
			r.Post("/approve", h.Approve)
			r.Post("/reject", h.Reject)
			r.Post("/activate", h.Activate)
			r.Post("/suspend", h.Suspend)
			r.Post("/revoke", h.Revoke)
			r.Post("/archive", h.Archive)
			r.Post("/consent", h.RefreshConsent)
			r.Post("/upgrade-profile", h.UpgradeProfile)

			// Evidence and status.
			r.Get("/changes", h.ListChanges)
			r.Get("/approvals", h.ListApprovals)
			r.Get("/profile-status", h.ProfileStatus)
			r.Get("/health", h.GetHealth)
			r.Post("/health", h.ReportHealth)
			r.Get("/health/transitions", h.ListHealthTransitions)

			// Sync checkpoints.
			r.Get("/checkpoints", h.ListCheckpoints)
			r.Post("/checkpoints/{stream}/advance", h.AdvanceCheckpoint)
			r.Post("/checkpoints/{stream}/reset-requests", h.RequestReset)
		})
	})
	r.Post("/v1/checkpoint-reset-requests/{rid}/approve", h.ApproveReset)
	r.Post("/v1/checkpoint-reset-requests/{rid}/reject", h.RejectReset)

	// Provider profiles: platform reference data.
	r.Route("/v1/external-profiles", func(r chi.Router) {
		r.Post("/", h.PublishProfile)
		r.Get("/", h.ListProfiles)
		r.Get("/{key}/versions/{version}", h.GetProfile)
		r.Post("/{key}/versions/{version}/status", h.SetProfileStatus)
	})

	// Route alias for Postman compatibility
	r.Route("/v1/api-bridge/connections", func(r chi.Router) {
		r.Post("/", h.CreateBridge)
		r.Get("/", h.ListBridges)
		r.Get("/{id}", h.GetByID)
	})
}

func populateAliases(b *domain.ApiBridge) {
	if b == nil {
		return
	}
	b.ConnectionID = b.BridgeID
	if b.ProviderName == "" {
		b.ProviderName = b.BridgeName
	}
	if b.BaseURL == "" {
		b.BaseURL = b.EndpointURL
	}
	if b.Scopes == nil {
		b.Scopes = []string{}
	}
}

// ── request helpers ───────────────────────────────────────────────────────────

const maxBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	// An empty body is not malformed: a command carrying its version in
	// X-Expected-Version has nothing else to say. A command that needs fields
	// still fails on the missing fields, with a message that says which.
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeProblem(w, r, errBadBody)
		return false
	}
	return true
}

var errBadBody = errors.New("request body is not valid JSON of the expected shape")

func (h *Handler) actor(r *http.Request) connector.Actor {
	corr := r.Header.Get("X-Correlation-ID")
	return connector.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: corr}
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeProblem(w, r, errNoPrincipal)
		return "", false
	}
	return principalID, true
}

var errNoPrincipal = errors.New("X-Principal-Id header is required")

// authorize checks action for the connector's legal entity. Denial is 403; an
// unreachable authorization-svc is 503 (fail closed, never allow silently).
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, action); err != nil {
		if !errors.Is(err, authz.ErrAuthorizationDenied) {
			h.logger.Error("authorization check failed", zap.Error(err))
		}
		if !errors.Is(err, authz.ErrAuthorizationDenied) {
			// anything but a clear denial is the absence of a decision: fail closed
			err = fmt.Errorf("%w: %v", authz.ErrAuthzServiceUnavailable, err)
		}
		writeProblem(w, r, err)
		return false
	}
	return true
}

// command runs a lifecycle command on a connector: it resolves the caller,
// looks the connector up (for its legal entity), authorizes the action, then
// calls run. A missing or foreign connector is 404 before any authorization is
// asked, so nothing about another tenant's connector is revealed.
func (h *Handler) command(w http.ResponseWriter, r *http.Request, action string,
	run func(tenant string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error)) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	b, err := h.store.GetBridgeByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if !h.authorize(w, r, principalID, b.LegalEntityID, action) {
		return
	}
	out, err := run(middleware.GetTenantID(r.Context()), h.actor(r), b)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	populateAliases(out)
	writeJSON(w, http.StatusOK, out)
}

type versioned struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

// versionOf reads expected_version (and reason) from the body. expected_version
// is required: a command applied to "whatever version is current" is a command
// applied to a connector the caller has not seen.
func versionOf(w http.ResponseWriter, r *http.Request) (versioned, bool) {
	var v versioned
	if !decode(w, r, &v) {
		return v, false
	}
	n, err := expectedVersion(r, v.ExpectedVersion)
	if err != nil {
		writeProblem(w, r, err)
		return v, false
	}
	if n < 1 {
		writeProblem(w, r, errNeedVersion)
		return v, false
	}
	v.ExpectedVersion = n
	return v, true
}

var errNeedVersion = errors.New("expected_version is required")

// ── create / read ─────────────────────────────────────────────────────────────

func (h *Handler) CreateBridge(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateBridgeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || (req.BridgeName == "" && req.ProviderName == "") {
		writeProblem(w, r, errCreateFields)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, BRIDGE_CREATE) {
		return
	}
	b, created, err := h.svc.Create(r.Context(), h.actor(r), connector.CreateInput{
		CreateBridgeRequest: req, TenantID: middleware.GetTenantID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	populateAliases(b)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // an idempotent replay returns the original
	}
	writeJSON(w, status, b)
}

var errCreateFields = errors.New("legal_entity_id and bridge_name/provider_name are required")

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	b, err := h.store.GetBridgeByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	populateAliases(b)
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) ListBridges(w http.ResponseWriter, r *http.Request) {
	bridges, err := h.store.ListBridges(r.Context(), r.URL.Query().Get("legal_entity_id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	for i := range bridges {
		populateAliases(&bridges[i])
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bridges":     bridges,
		"connections": bridges,
		"total":       len(bridges),
	})
}

// ── update ────────────────────────────────────────────────────────────────────

type updateRequest struct {
	ExpectedVersion        int64     `json:"expected_version"`
	BridgeName             *string   `json:"bridge_name"`
	EndpointURL            *string   `json:"endpoint_url"`
	AuthType               *string   `json:"auth_type"`
	Purpose                *string   `json:"purpose"`
	ProviderKey            *string   `json:"provider_key"`
	ProviderProfileVersion *int      `json:"provider_profile_version"`
	CredentialRef          *string   `json:"credential_ref"`
	DestinationRegion      *string   `json:"destination_region"`
	SyncMode               *string   `json:"sync_mode"`
	Scopes                 *[]string `json:"scopes"`
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || n < 1 {
		if err == nil {
			err = errNeedVersion
		}
		writeProblem(w, r, err)
		return
	}
	req.ExpectedVersion = n
	h.command(w, r, ActionUpdate, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Update(r.Context(), a, b.BridgeID, req.ExpectedVersion, connector.Patch{
			BridgeName: req.BridgeName, EndpointURL: req.EndpointURL, AuthType: req.AuthType, Purpose: req.Purpose,
			ProviderKey: req.ProviderKey, ProviderProfileVersion: req.ProviderProfileVersion, CredentialRef: req.CredentialRef,
			DestinationRegion: req.DestinationRegion, SyncMode: req.SyncMode, Scopes: req.Scopes,
		})
	})
}

// ── lifecycle ─────────────────────────────────────────────────────────────────

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionSubmit, func(tenant string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Submit(r.Context(), tenant, a, b.BridgeID, v.ExpectedVersion)
	})
}

func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionApprove, func(tenant string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Approve(r.Context(), tenant, a, b.BridgeID, v.ExpectedVersion, v.Reason)
	})
}

func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionApprove, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Reject(r.Context(), a, b.BridgeID, v.ExpectedVersion, v.Reason)
	})
}

func (h *Handler) Activate(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionActivate, func(tenant string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Activate(r.Context(), tenant, a, b.BridgeID, v.ExpectedVersion)
	})
}

func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionSuspend, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Suspend(r.Context(), a, b.BridgeID, v.ExpectedVersion, v.Reason)
	})
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionRevoke, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Revoke(r.Context(), a, b.BridgeID, v.ExpectedVersion, v.Reason)
	})
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	v, ok := versionOf(w, r)
	if !ok {
		return
	}
	h.command(w, r, ActionArchive, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.Archive(r.Context(), a, b.BridgeID, v.ExpectedVersion)
	})
}

type consentRequest struct {
	ExpectedVersion  int64      `json:"expected_version"`
	CredentialRef    *string    `json:"credential_ref"`
	ConsentExpiresAt *time.Time `json:"consent_expires_at"`
}

func (h *Handler) RefreshConsent(w http.ResponseWriter, r *http.Request) {
	var req consentRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || n < 1 {
		if err == nil {
			err = errNeedVersion
		}
		writeProblem(w, r, err)
		return
	}
	req.ExpectedVersion = n
	h.command(w, r, ActionConsent, func(tenant string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.RefreshConsent(r.Context(), tenant, a, b.BridgeID, req.ExpectedVersion, req.CredentialRef, req.ConsentExpiresAt)
	})
}

type upgradeRequest struct {
	ExpectedVersion        int64 `json:"expected_version"`
	ProviderProfileVersion int   `json:"provider_profile_version"`
}

func (h *Handler) UpgradeProfile(w http.ResponseWriter, r *http.Request) {
	var req upgradeRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || n < 1 {
		if err == nil {
			err = errNeedVersion
		}
		writeProblem(w, r, err)
		return
	}
	req.ExpectedVersion = n
	h.command(w, r, ActionProfileUpgrade, func(_ string, a connector.Actor, b *domain.ApiBridge) (*domain.ApiBridge, error) {
		return h.svc.UpgradeProfile(r.Context(), a, b.BridgeID, req.ExpectedVersion, req.ProviderProfileVersion)
	})
}

// ── ingest ────────────────────────────────────────────────────────────────────

func (h *Handler) IngestPayload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.IngestPayloadRequest
	if !decode(w, r, &req) {
		return
	}
	bridge, err := h.store.GetBridgeByID(r.Context(), id)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, bridge.LegalEntityID, BRIDGE_INGEST) {
		return
	}
	log, err := h.svc.Ingest(r.Context(), h.actor(r), id, connector.IngestInput{PayloadSummary: req.PayloadSummary, Scope: req.Scope})
	if err != nil {
		if !errors.Is(err, connector.ErrNotLive) && !errors.Is(err, connector.ErrScopeDenied) {
			h.logger.Warn("ingestion refused", zap.String("bridge_id", id), zap.Error(err))
		}
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, log)
}

func (h *Handler) ListLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// 404 for a connector that is not the caller's, as on every other route; an
	// empty 200 would answer for an id that does not exist or belongs to someone else.
	if _, err := h.store.GetBridgeByID(r.Context(), id); err != nil {
		writeProblem(w, r, err)
		return
	}
	logs, err := h.store.ListIngestionLogs(r.Context(), id)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": logs, "total": len(logs)})
}

// ── evidence ──────────────────────────────────────────────────────────────────

func (h *Handler) ListChanges(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.ListChanges(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": c, "total": len(c)})
}

func (h *Handler) ListApprovals(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.ListApprovals(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": a, "total": len(a)})
}

func (h *Handler) ProfileStatus(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.ProfileStatusOf(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *Handler) GetHealth(w http.ResponseWriter, r *http.Request) {
	health, err := h.store.GetHealth(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (h *Handler) ListHealthTransitions(w http.ResponseWriter, r *http.Request) {
	t, err := h.store.ListHealthTransitions(r.Context(), chi.URLParam(r, "id"), 100)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": t, "total": len(t)})
}

type healthRequest struct {
	Success    bool   `json:"success"`
	ReasonCode string `json:"reason_code"`
	Detail     string `json:"detail"`
}

func (h *Handler) ReportHealth(w http.ResponseWriter, r *http.Request) {
	var req healthRequest
	if !decode(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	b, err := h.store.GetBridgeByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if !h.authorize(w, r, principalID, b.LegalEntityID, ActionHealthReport) {
		return
	}
	out, err := h.svc.ReportHealth(r.Context(), h.actor(r), b.BridgeID,
		connector.HealthReport{Success: req.Success, Reason: req.ReasonCode, Detail: req.Detail})
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── checkpoints ───────────────────────────────────────────────────────────────

func (h *Handler) ListCheckpoints(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.ListCheckpoints(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": c, "total": len(c)})
}

type advanceRequest struct {
	ExpectedVersion int64      `json:"expected_version"` // 0 creates the stream
	Cursor          string     `json:"cursor"`
	Watermark       *time.Time `json:"watermark"`
}

func (h *Handler) AdvanceCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req advanceRequest
	if !decode(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	b, err := h.store.GetBridgeByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if !h.authorize(w, r, principalID, b.LegalEntityID, ActionCheckpoint) {
		return
	}
	cp, err := h.svc.AdvanceCheckpoint(r.Context(), h.actor(r), b.BridgeID, chi.URLParam(r, "stream"), req.Cursor, req.Watermark, req.ExpectedVersion)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

type resetRequestBody struct {
	ToCursor    string     `json:"to_cursor"`
	ToWatermark *time.Time `json:"to_watermark"`
	Reason      string     `json:"reason"`
}

func (h *Handler) RequestReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequestBody
	if !decode(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	b, err := h.store.GetBridgeByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if !h.authorize(w, r, principalID, b.LegalEntityID, ActionResetRequest) {
		return
	}
	out, err := h.svc.RequestCheckpointReset(r.Context(), h.actor(r), b.BridgeID, chi.URLParam(r, "stream"), req.ToCursor, req.ToWatermark, req.Reason)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) ApproveReset(w http.ResponseWriter, r *http.Request) { h.decideReset(w, r, true) }
func (h *Handler) RejectReset(w http.ResponseWriter, r *http.Request)  { h.decideReset(w, r, false) }

func (h *Handler) decideReset(w http.ResponseWriter, r *http.Request, approve bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// The approval is scoped by the caller's entity header like other
	// entity-less actions; the request itself is tenant-scoped in the store.
	if !h.authorize(w, r, principalID, r.Header.Get("X-Legal-Entity-Id"), ActionResetDecide) {
		return
	}
	out, err := h.svc.DecideCheckpointReset(r.Context(), h.actor(r), chi.URLParam(r, "rid"), approve)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── provider profiles ─────────────────────────────────────────────────────────

func (h *Handler) PublishProfile(w http.ResponseWriter, r *http.Request) {
	var p domain.Profile
	if !decode(w, r, &p) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenant := middleware.GetTenantID(r.Context())
	if !h.authorize(w, r, principalID, store.PlatformScopeID, ActionProfilePublish) {
		return
	}
	if err := h.svc.PublishProfile(r.Context(), tenant, h.actor(r), &p); err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListProfiles(r.Context(), r.URL.Query().Get("provider_key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": list, "total": len(list)})
}

func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	v, ok := profileVersion(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProfile(r.Context(), chi.URLParam(r, "key"), v)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type statusRequest struct {
	Status string `json:"status"`
}

func (h *Handler) SetProfileStatus(w http.ResponseWriter, r *http.Request) {
	v, ok := profileVersion(w, r)
	if !ok {
		return
	}
	var req statusRequest
	if !decode(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, store.PlatformScopeID, ActionProfilePublish) {
		return
	}
	p, err := h.svc.SetProfileStatus(r.Context(), middleware.GetTenantID(r.Context()), h.actor(r), chi.URLParam(r, "key"), v, req.Status)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// expectedVersion resolves the optimistic-lock version a command was issued
// against. The platform input contract carries it in X-Expected-Version; the
// body field is the equivalent for clients that cannot set headers. If both are
// given they must agree: a disagreement is a confused client, and acting on
// either would apply the command to a version it may not have seen.
func expectedVersion(r *http.Request, body int64) (int64, error) {
	raw := r.Header.Get("X-Expected-Version")
	if raw == "" {
		return body, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, errBadVersionHeader
	}
	if body != 0 && body != n {
		return 0, errVersionDisagree
	}
	return n, nil
}

var (
	errBadVersionHeader = errors.New("X-Expected-Version must be a positive integer")
	errVersionDisagree  = errors.New("X-Expected-Version and expected_version disagree")
)
