// COM-04 Usage Metering HTTP surface (ZS-SVC-Q-001 §4.4, §10).
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Registering a meter is seller/product authority (it decides what can be
// billed at all). Ingesting usage is a granted workload — the decision you
// made was to check that through authorization-svc the same way every other
// command here is checked, since this platform has no per-caller mTLS
// identity extraction anywhere yet. Certifying/reopening a statement is a
// billing-operations authority, distinct from the workload that merely
// reports facts.
const (
	ActionMeterDefinitionManage = "COMMERCIAL_METER_DEFINITION_MANAGE"
	ActionUsageIngest           = "COMMERCIAL_USAGE_INGEST"
	ActionUsageCertify          = "COMMERCIAL_USAGE_CERTIFY"
	ActionUsageRead             = "COMMERCIAL_USAGE_READ"
)

const (
	CodeMeterDefinitionNotFound  = "METER_DEFINITION_NOT_FOUND"
	CodeMeterDefinitionExists    = "METER_DEFINITION_EXISTS"
	CodeMeterDefinitionRetired   = "METER_DEFINITION_RETIRED"
	CodeUsageEventExists         = "USAGE_EVENT_EXISTS"
	CodeUsageEventNotFound       = "USAGE_EVENT_NOT_FOUND"
	CodeUsageEventNotCorrectable = "USAGE_EVENT_NOT_CORRECTABLE"
	CodeStatementNotFound        = "USAGE_STATEMENT_NOT_FOUND"
	CodeStatementInvalidState    = "USAGE_STATEMENT_INVALID_STATE"
	CodeNoOpenTermForUsage       = "NO_OPEN_TERM_FOR_USAGE"
)

// usageFailure maps COM-04 errors; writeFailure consults it last.
func usageFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrMeterDefinitionNotFound):
		return http.StatusNotFound, CodeMeterDefinitionNotFound, true
	case errors.Is(err, domain.ErrMeterDefinitionExists):
		return http.StatusConflict, CodeMeterDefinitionExists, true
	case errors.Is(err, domain.ErrMeterDefinitionRetired):
		return http.StatusConflict, CodeMeterDefinitionRetired, true
	case errors.Is(err, domain.ErrUsageEventExists):
		return http.StatusConflict, CodeUsageEventExists, true
	case errors.Is(err, domain.ErrUsageEventNotFound):
		return http.StatusNotFound, CodeUsageEventNotFound, true
	case errors.Is(err, domain.ErrUsageEventNotCorrectable):
		return http.StatusConflict, CodeUsageEventNotCorrectable, true
	case errors.Is(err, domain.ErrStatementNotFound):
		return http.StatusNotFound, CodeStatementNotFound, true
	case errors.Is(err, domain.ErrStatementInvalidState):
		return http.StatusConflict, CodeStatementInvalidState, true
	case errors.Is(err, domain.ErrStatementNotOpenForWindow), errors.Is(err, domain.ErrNoOpenTermForCorrection):
		return http.StatusUnprocessableEntity, CodeNoOpenTermForUsage, true
	case errors.Is(err, domain.ErrReopenNeedsIndependentActor):
		return http.StatusForbidden, CodeSoDViolation, true
	}
	return 0, "", false
}

type UsageHandler struct {
	store  store.UsageStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewUsageHandler(st store.UsageStore, az AuthzChecker, logger *zap.Logger) *UsageHandler {
	return &UsageHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *UsageHandler) WithClock(now func() time.Time) *UsageHandler {
	h.now = now
	return h
}

func RegisterUsageRoutes(r chi.Router, h *UsageHandler) {
	r.Route("/v1/commercial/meter-definitions", func(r chi.Router) {
		r.Post("/", h.RegisterMeterDefinition)
		r.Get("/", h.ListMeterDefinitions)
		r.Get("/{key}/{version}", h.GetMeterDefinition)
		r.Post("/{key}/{version}", h.MeterDefinitionAction)
	})
	r.Post("/v1/commercial/usage-events", h.RegisterUsageEvent)
	r.Post("/v1/commercial/usage-events:correct", h.CorrectUsageEvent)
	r.Get("/v1/commercial/usage-events:dedup", h.GetDedupStatus)
	r.Get("/v1/commercial/usage", h.GetUsage)
	r.Post("/v1/commercial/usage-statements:close", h.CloseUsageWindow)
	r.Route("/v1/commercial/usage-statements", func(r chi.Router) {
		r.Get("/{id}", h.GetUsageStatement)
		r.Get("/{id}/aggregation", h.ExplainAggregation)
		r.Post("/{id}", h.StatementAction)
	})
	r.Get("/v1/commercial/subscriptions/{id}/late-usage", h.GetLateEvents)
}

func (h *UsageHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *UsageHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

// authorizeAt checks principal against action at scope — platformScopeID
// for a seller/operator action, or an organization id for a tenant reading
// its own data.
func (h *UsageHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *UsageHandler) authorize(w http.ResponseWriter, r *http.Request, principal, action string) bool {
	return h.authorizeAt(w, r, principal, platformScopeID, action)
}

func (h *UsageHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorize(w, r, principal, action)
}

// readScope resolves who a usage read is about: an operator naming an
// organization (needs the platform usage-read grant), or the caller's own
// verified tenant.
func (h *UsageHandler) readScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	if org := r.URL.Query().Get("organization_id"); org != "" {
		if !h.authorize(w, r, principal, ActionUsageRead) {
			return "", false
		}
		return org, true
	}
	org := svcmiddleware.TenantFromContext(r.Context())
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Tenant-Id is required"})
		return "", false
	}
	if !h.authorizeAt(w, r, principal, org, ActionUsageRead) {
		return "", false
	}
	return org, true
}

// ── Meter definitions ────────────────────────────────────────────────────────

type meterDefinitionRequest struct {
	MeterKey                string  `json:"meter_key"`
	MeterVersion            int     `json:"meter_version"`
	DisplayName             string  `json:"display_name"`
	Unit                    string  `json:"unit"`
	AggregationMethod       string  `json:"aggregation_method"`
	UniqueDimension         *string `json:"unique_dimension"`
	AllowNegativeCorrection bool    `json:"allow_negative_correction"`
}

func (h *UsageHandler) RegisterMeterDefinition(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionMeterDefinitionManage)
	if !ok {
		return
	}
	var req meterDefinitionRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.MeterVersion < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "meter_version", Detail: "must be 1 or greater"})
		return
	}
	m := &domain.MeterDefinition{MeterKey: req.MeterKey, MeterVersion: req.MeterVersion, DisplayName: req.DisplayName,
		Unit: req.Unit, AggregationMethod: req.AggregationMethod, UniqueDimension: req.UniqueDimension,
		AllowNegativeCorrection: req.AllowNegativeCorrection, CreatedAt: h.now(), CreatedByPrincipalID: principal}
	if err := domain.ValidateMeterDefinition(m); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "RegisterMeterDefinition",
		domain.RegisteredMeterKey(m.MeterKey, m.MeterVersion), raw, false)
	if !ok {
		return
	}
	created, err := h.store.RegisterMeterDefinition(r.Context(), m, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func meterKeyVersion(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	key := chi.URLParam(r, "key")
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "version", Detail: "must be a positive integer"})
		return "", 0, false
	}
	return key, version, true
}

func (h *UsageHandler) GetMeterDefinition(w http.ResponseWriter, r *http.Request) {
	key, version, ok := meterKeyVersion(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetMeterDefinition(r.Context(), key, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *UsageHandler) ListMeterDefinitions(w http.ResponseWriter, r *http.Request) {
	ms, err := h.store.ListMeterDefinitions(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ms == nil {
		ms = []domain.MeterDefinition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"meters": ms})
}

// MeterDefinitionAction dispatches POST /meter-definitions/{key}/{version}:retire.
func (h *UsageHandler) MeterDefinitionAction(w http.ResponseWriter, r *http.Request) {
	rawVersion, action, found := strings.Cut(chi.URLParam(r, "version"), ":")
	if !found || action != "retire" {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected /meter-definitions/{key}/{version}:retire"})
		return
	}
	key := chi.URLParam(r, "key")
	version, err := strconv.Atoi(rawVersion)
	if err != nil || version < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "version", Detail: "must be a positive integer"})
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionMeterDefinitionManage)
	if !ok {
		return
	}
	var req lifecycleRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required to retire a meter"})
		return
	}
	m, err := h.store.RetireMeterDefinition(r.Context(), key, version, principal, req.Reason, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── Ingestion ────────────────────────────────────────────────────────────────

type registerUsageEventRequest struct {
	OrganizationID string            `json:"organization_id"`
	SubscriptionID string            `json:"subscription_id"`
	MeterKey       string            `json:"meter_key"`
	MeterVersion   int               `json:"meter_version"`
	UsageEventID   string            `json:"usage_event_id"`
	Quantity       string            `json:"quantity"`
	Dimensions     map[string]string `json:"dimensions"`
	OccurredAt     time.Time         `json:"occurred_at"`
	SourceService  string            `json:"source_service"`
}

func (req *registerUsageEventRequest) validate() *Problem {
	bad := func(field, detail string) *Problem {
		return &Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: field, Detail: detail}
	}
	if _, err := uuid.Parse(req.OrganizationID); err != nil {
		return bad("organization_id", "must be the customer's organization id")
	}
	if _, err := domain.ParseCommercialID(domain.PrefixSubscription, req.SubscriptionID); err != nil {
		return bad("subscription_id", "invalid subscription id")
	}
	if req.MeterKey == "" {
		return bad("meter_key", "is required")
	}
	if req.MeterVersion < 1 {
		return bad("meter_version", "must be 1 or greater")
	}
	if strings.TrimSpace(req.UsageEventID) == "" || len(req.UsageEventID) > 255 {
		return bad("usage_event_id", "is required (at most 255 characters) and must be stable across retries")
	}
	if req.Quantity == "" {
		return bad("quantity", "is required")
	}
	if req.OccurredAt.IsZero() {
		return bad("occurred_at", "is required")
	}
	if strings.TrimSpace(req.SourceService) == "" {
		return bad("source_service", "is required")
	}
	return nil
}

func (h *UsageHandler) RegisterUsageEvent(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionUsageIngest)
	if !ok {
		return
	}
	var req registerUsageEventRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if p := req.validate(); p != nil {
		writeProblem(w, r, *p)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "RegisterUsageEvent", req.MeterKey+"/"+req.UsageEventID, raw, false)
	if !ok {
		return
	}
	in := domain.EventInput{Quantity: req.Quantity, Dimensions: req.Dimensions, OccurredAt: req.OccurredAt.UTC().Truncate(time.Microsecond)}
	e, err := h.store.RegisterUsageEvent(r.Context(), req.OrganizationID, req.SubscriptionID, req.MeterKey, req.MeterVersion,
		req.UsageEventID, in, req.SourceService, h.now(), cmd.claim)
	if err != nil {
		var replay *domain.IdempotentReplayError
		if errors.As(err, &replay) {
			if existing, gerr := h.store.GetDedupStatus(r.Context(), req.MeterKey, req.UsageEventID); gerr == nil {
				w.Header().Set("Idempotent-Replayed", "true")
				writeJSON(w, http.StatusCreated, existing)
				return
			}
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

type correctUsageEventRequest struct {
	MeterKey        string            `json:"meter_key"`
	OriginalEventID string            `json:"original_usage_event_id"`
	NewEventID      string            `json:"new_usage_event_id"`
	Quantity        string            `json:"quantity"`
	Dimensions      map[string]string `json:"dimensions"`
	OccurredAt      time.Time         `json:"occurred_at"`
	Reason          string            `json:"reason"`
}

func (h *UsageHandler) CorrectUsageEvent(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionUsageIngest)
	if !ok {
		return
	}
	var req correctUsageEventRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.MeterKey == "" || strings.TrimSpace(req.OriginalEventID) == "" || strings.TrimSpace(req.NewEventID) == "" ||
		req.Quantity == "" || req.OccurredAt.IsZero() || strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "meter_key, original_usage_event_id, new_usage_event_id, quantity, occurred_at and reason are all required"})
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "CorrectUsageEvent", req.MeterKey+"/"+req.NewEventID, raw, false)
	if !ok {
		return
	}
	in := domain.EventInput{Quantity: req.Quantity, Dimensions: req.Dimensions, OccurredAt: req.OccurredAt.UTC().Truncate(time.Microsecond)}
	e, err := h.store.CorrectUsageEvent(r.Context(), req.MeterKey, req.OriginalEventID, req.NewEventID, in, req.Reason, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *UsageHandler) GetDedupStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.sellerPrincipal(w, r, ActionUsageIngest); !ok {
		return
	}
	meterKey, id := r.URL.Query().Get("meter_key"), r.URL.Query().Get("usage_event_id")
	if meterKey == "" || id == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Detail: "meter_key and usage_event_id are required"})
		return
	}
	e, err := h.store.GetDedupStatus(r.Context(), meterKey, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ── Statement lifecycle ──────────────────────────────────────────────────────

type closeWindowRequest struct {
	SubscriptionID string `json:"subscription_id"`
	TermNo         int    `json:"term_no"`
	MeterKey       string `json:"meter_key"`
}

func (h *UsageHandler) CloseUsageWindow(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionUsageCertify)
	if !ok {
		return
	}
	var req closeWindowRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if _, err := domain.ParseCommercialID(domain.PrefixSubscription, req.SubscriptionID); err != nil || req.TermNo < 1 || req.MeterKey == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "subscription_id, term_no and meter_key are required"})
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "CloseUsageWindow", req.SubscriptionID+"/"+strconv.Itoa(req.TermNo)+"/"+req.MeterKey, raw, false)
	if !ok {
		return
	}
	st, err := h.store.CloseUsageWindow(r.Context(), req.SubscriptionID, req.TermNo, req.MeterKey, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// StatementAction dispatches POST /usage-statements/{id}:{certify|reopen}.
func (h *UsageHandler) StatementAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || (action != "certify" && action != "reopen") {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :certify or :reopen"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixUsageStatement, rawID)
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionUsageCertify)
	if !ok {
		return
	}
	var req lifecycleRequest
	raw, ok := readBody(w, r, &req, true)
	if !ok {
		return
	}
	if action == "reopen" && strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required to reopen a statement"})
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "Statement:"+action, id, raw, false)
	if !ok {
		return
	}
	var st *domain.UsageStatement
	var err error
	if action == "certify" {
		st, err = h.store.CertifyUsageStatement(r.Context(), id, principal, h.now(), cmd.claim)
	} else {
		st, err = h.store.ReopenWindow(r.Context(), id, principal, req.Reason, h.now(), cmd.claim)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (h *UsageHandler) GetUsage(w http.ResponseWriter, r *http.Request) {
	org, ok := h.readScope(w, r)
	if !ok {
		return
	}
	subID, ok := parseID(w, r, domain.PrefixSubscription, r.URL.Query().Get("subscription_id"))
	if !ok {
		return
	}
	termNo, err := strconv.Atoi(r.URL.Query().Get("term_no"))
	if err != nil || termNo < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "term_no", Detail: "must be a positive integer"})
		return
	}
	meterKey := r.URL.Query().Get("meter_key")
	if meterKey == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "meter_key", Detail: "is required"})
		return
	}
	st, err := h.store.GetUsage(svcmiddleware.WithTenant(r.Context(), org), subID, termNo, meterKey)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *UsageHandler) GetUsageStatement(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixUsageStatement, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r)
	if !ok {
		return
	}
	st, err := h.store.GetUsageStatement(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *UsageHandler) ExplainAggregation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixUsageStatement, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r)
	if !ok {
		return
	}
	events, err := h.store.ExplainAggregation(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if events == nil {
		events = []domain.UsageEventRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"statement_id": id, "events": events})
}

func (h *UsageHandler) GetLateEvents(w http.ResponseWriter, r *http.Request) {
	subID, ok := parseID(w, r, domain.PrefixSubscription, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r)
	if !ok {
		return
	}
	events, err := h.store.GetLateEvents(svcmiddleware.WithTenant(r.Context(), org), subID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if events == nil {
		events = []domain.UsageEventRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": subID, "late_events": events})
}
