// Package handler is the HTTP surface of currency-registry-svc (REF-02).
//
// Named commands use the spec's colon form (POST /v1/currencies/{id}:activate).
// chi matches the whole path segment, so the segment is split on its LAST colon
// here rather than relying on router-specific suffix matching.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/currency-registry-svc/internal/domain"
	svcmiddleware "zoiko.io/currency-registry-svc/internal/middleware"
	"zoiko.io/currency-registry-svc/internal/service"
	"zoiko.io/currency-registry-svc/internal/telemetry"
)

// AuthZClient asks authorization-svc whether a principal may perform an action.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Authorization actions asked of authorization-svc.
const (
	ActionImport         = "CURRENCY_IMPORT"
	ActionActivate       = "CURRENCY_ACTIVATE"
	ActionRestrict       = "CURRENCY_RESTRICT"
	ActionRetire         = "CURRENCY_RETIRE"
	ActionTenantEnable   = "CURRENCY_TENANT_ENABLE"
	ActionTenantDisable  = "CURRENCY_TENANT_DISABLE"
	PlatformScopeID      = "00000000-0000-0000-0000-00000000f001"
	maxBodyBytes         = 4 << 20
	defaultPageLimit     = 100
	maxPageLimit         = 500
	headerIdempotencyKey = "Idempotency-Key"
)

// Handler serves the REF-02 API.
type Handler struct {
	svc     *service.Service
	authz   AuthZClient
	log     *zap.Logger
	metrics *telemetry.Domain
}

// New builds the handler. metrics may be nil (tests).
func New(svc *service.Service, authz AuthZClient, log *zap.Logger, metrics *telemetry.Domain) *Handler {
	return &Handler{svc: svc, authz: authz, log: log, metrics: metrics}
}

// RegisterRoutes mounts the API.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/currency-imports", h.CreateImport)
	r.Get("/v1/currency-imports", h.GetImport)

	r.Get("/v1/currencies", h.ListCurrencies)
	r.Get("/v1/currencies:resolve-numeric", h.ResolveNumeric)
	r.Get("/v1/currencies:validate", h.Validate)
	r.Get("/v1/currencies/{code}", h.GetCurrency)
	r.Get("/v1/currencies/{code}/minor-unit-versions", h.MinorUnitVersions)
	r.Post("/v1/currencies/{idcmd}", h.CurrencyCommand)

	r.Get("/v1/tenants/{tenant}/currency-support", h.ListTenantSupport)
	r.Post("/v1/tenants/{tenant}/currency-support/{codecmd}", h.TenantSupportCommand)
}

// ── request context ──────────────────────────────────────────────────────────

type reqCtx struct {
	tenant, actor, legalEntity string
	meta                       service.Meta
}

// context extracts and validates the trusted request context shared by all
// commands. It writes the refusal itself and reports ok=false.
func (h *Handler) context(w http.ResponseWriter, r *http.Request, needIdempotency bool) (reqCtx, bool) {
	var rc reqCtx
	rc.tenant = svcmiddleware.TenantFromContext(r.Context())
	if rc.tenant == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "tenant context missing (X-Tenant-Id)")
		return rc, false
	}
	rc.actor = r.Header.Get("X-Principal-Id")
	if rc.actor == "" {
		rc.actor = r.Header.Get("X-Workload-Id")
	}
	if rc.actor == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "caller identity missing (X-Principal-Id or X-Workload-Id)")
		return rc, false
	}
	rc.legalEntity = r.Header.Get("X-Legal-Entity-Id")
	if rc.legalEntity == "" {
		rc.legalEntity = PlatformScopeID
	}
	corr := r.Header.Get("X-Correlation-ID")
	if corr == "" {
		corr = chimw.GetReqID(r.Context())
	}
	rc.meta = service.Meta{
		Actor: rc.actor, TenantID: rc.tenant, CorrelationID: corr, CausationID: r.Header.Get("X-Causation-Id"),
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
	}
	if needIdempotency && rc.meta.IdempotencyKey == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "Idempotency-Key header is required for commands")
		return rc, false
	}
	return rc, true
}

// readBody reads the capped body and returns it with its request fingerprint.
func readBody(w http.ResponseWriter, r *http.Request, op string) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "request body unreadable or too large")
		return nil, "", false
	}
	sum := sha256.Sum256(append([]byte(op+"\n"), b...))
	return b, hex.EncodeToString(sum[:]), true
}

func decodeStrict(w http.ResponseWriter, body []byte, dst any) bool {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// authorize asks authorization-svc. It writes the refusal and reports false on
// denial (403 FORBIDDEN) or when the dependency is down (503
// DEPENDENCY_UNAVAILABLE, fail closed).
func (h *Handler) authorize(w http.ResponseWriter, ctx context.Context, rc reqCtx, action, command string) bool {
	err := h.authz.CheckAllowed(ctx, rc.actor, rc.legalEntity, action)
	switch {
	case err == nil:
		h.countAuthz(action, telemetry.AuthZGranted)
		return true
	case errors.Is(err, domain.ErrAuthorizationDenied):
		h.countAuthz(action, telemetry.AuthZDenied)
		h.countCmd(command, telemetry.OutcomeForbidden)
		writeErr(w, http.StatusForbidden, domain.CodeForbidden, "not permitted to perform "+action)
	default:
		h.countAuthz(action, telemetry.AuthZUnavailable)
		h.countCmd(command, telemetry.OutcomeUnavailable)
		h.log.Error("authorization-svc unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "authorization-svc unavailable; failing closed")
	}
	return false
}

// ── POST /v1/currency-imports ────────────────────────────────────────────────

type importRequest struct {
	SourceName    string             `json:"source_name"`
	SourceVersion string             `json:"source_version"`
	ManifestHash  string             `json:"manifest_hash"`
	EffectiveAt   *time.Time         `json:"effective_at"`
	Reason        string             `json:"reason"`
	Rows          []domain.ImportRow `json:"rows"`
}

// CreateImport is ImportCurrencyUpdate (POST /v1/currency-imports).
func (h *Handler) CreateImport(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeInvalid)
		return
	}
	body, hash, ok := readBody(w, r, "POST /v1/currency-imports")
	if !ok {
		return
	}
	var req importRequest
	if !decodeStrict(w, body, &req) {
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeInvalid)
		return
	}
	if req.Reason == "" {
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason is required")
		return
	}
	if !h.authorize(w, r.Context(), rc, ActionImport, telemetry.CmdImport) {
		return
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, req.Reason
	imp, replayed, err := h.svc.ImportCurrencyUpdate(r.Context(), service.ImportInput{
		Meta: meta, SourceName: req.SourceName, SourceVersion: req.SourceVersion, ManifestHash: req.ManifestHash,
		EffectiveAt: req.EffectiveAt, Rows: req.Rows,
	})
	if err != nil {
		h.fail(w, telemetry.CmdImport, err)
		return
	}
	switch {
	case replayed:
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeReplayed)
		h.countImport(telemetry.OutcomeReplayed)
		writeJSON(w, http.StatusOK, imp)
	case imp.Status == domain.ImportQuarantined:
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeQuarantined)
		h.countImport(telemetry.OutcomeQuarantined)
		writeJSON(w, http.StatusCreated, imp)
	default:
		h.countCmd(telemetry.CmdImport, telemetry.OutcomeOK)
		h.countImport(telemetry.OutcomeOK)
		writeJSON(w, http.StatusCreated, imp)
	}
}

// GetImport returns the evidence record for (source_name, source_version).
func (h *Handler) GetImport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("source_name") == "" || q.Get("source_version") == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "source_name and source_version are required")
		return
	}
	imp, err := h.svc.GetImport(r.Context(), q.Get("source_name"), q.Get("source_version"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, imp)
}

// ── POST /v1/currencies/{id}:activate|:restrict|:retire ──────────────────────

type commandRequest struct {
	ExpectedVersion *int64     `json:"expected_version"`
	Reason          string     `json:"reason"`
	EffectiveAt     *time.Time `json:"effective_at"`
	ApproverID      string     `json:"approver_id"`
}

func splitCommand(seg string) (id, cmd string, ok bool) {
	i := strings.LastIndex(seg, ":")
	if i <= 0 || i == len(seg)-1 {
		return "", "", false
	}
	return seg[:i], seg[i+1:], true
}

// expectedVersion reads expected_version from the body, falling back to the
// canonical X-Expected-Version header.
func expectedVersion(r *http.Request, body *int64) (int64, bool) {
	if body != nil {
		return *body, true
	}
	if v := r.Header.Get("X-Expected-Version"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// CurrencyCommand dispatches the colon-suffixed lifecycle commands.
func (h *Handler) CurrencyCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd, ok := splitCommand(chi.URLParam(r, "idcmd"))
	action := map[string]string{service.CommandActivate: ActionActivate, service.CommandRestrict: ActionRestrict, service.CommandRetire: ActionRetire}[cmd]
	if !ok || action == "" {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected {id}:activate, {id}:restrict or {id}:retire")
		return
	}
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(cmd, telemetry.OutcomeInvalid)
		return
	}
	body, hash, ok := readBody(w, r, "POST /v1/currencies/"+id+":"+cmd)
	if !ok {
		return
	}
	var req commandRequest
	if !decodeStrict(w, body, &req) {
		h.countCmd(cmd, telemetry.OutcomeInvalid)
		return
	}
	ev, haveEV := expectedVersion(r, req.ExpectedVersion)
	if !haveEV || ev < 1 {
		h.countCmd(cmd, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "expected_version is required (body field or X-Expected-Version) and must be >= 1")
		return
	}
	if req.Reason == "" {
		h.countCmd(cmd, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason is required")
		return
	}
	if !h.authorize(w, r.Context(), rc, action, cmd) {
		return
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, req.Reason
	c, replayed, err := h.svc.Transition(r.Context(), service.TransitionInput{
		Meta: meta, CurrencyID: id, Command: cmd, ExpectedVersion: ev, EffectiveAt: req.EffectiveAt, ApproverID: req.ApproverID,
	})
	if err != nil {
		h.fail(w, cmd, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(cmd, telemetry.OutcomeReplayed)
	} else {
		h.countCmd(cmd, telemetry.OutcomeOK)
	}
	w.Header().Set("ETag", etag(c))
	writeJSON(w, http.StatusOK, c)
}

// ── tenant overlay ───────────────────────────────────────────────────────────

type overlayRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	Reason          string `json:"reason"`
}

// TenantSupportCommand handles .../currency-support/{code}:enable|:disable.
func (h *Handler) TenantSupportCommand(w http.ResponseWriter, r *http.Request) {
	code, cmd, ok := splitCommand(chi.URLParam(r, "codecmd"))
	if !ok || (cmd != "enable" && cmd != "disable") {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected {code}:enable or {code}:disable")
		return
	}
	metricCmd, action := telemetry.CmdEnable, ActionTenantEnable
	if cmd == "disable" {
		metricCmd, action = telemetry.CmdDisable, ActionTenantDisable
	}
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(metricCmd, telemetry.OutcomeInvalid)
		return
	}
	pathTenant := chi.URLParam(r, "tenant")
	if pathTenant != rc.tenant {
		// Cross-tenant: the trusted tenant context is the only tenant a caller
		// may touch. Refused before anything is read or written.
		h.countCmd(metricCmd, telemetry.OutcomeForbidden)
		writeErr(w, http.StatusForbidden, domain.CodeContextInvalid, "tenant in path does not match the caller's trusted tenant context")
		return
	}
	body, hash, ok := readBody(w, r, "POST /v1/tenants/"+pathTenant+"/currency-support/"+code+":"+cmd)
	if !ok {
		return
	}
	var req overlayRequest
	if !decodeStrict(w, body, &req) {
		h.countCmd(metricCmd, telemetry.OutcomeInvalid)
		return
	}
	ev, haveEV := expectedVersion(r, req.ExpectedVersion)
	if !haveEV || ev < 0 {
		h.countCmd(metricCmd, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "expected_version is required (0 when no overlay exists yet)")
		return
	}
	if req.Reason == "" {
		h.countCmd(metricCmd, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason is required")
		return
	}
	if !h.authorize(w, r.Context(), rc, action, metricCmd) {
		return
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, req.Reason
	ts, replayed, err := h.svc.SetTenantSupport(r.Context(), service.TenantSupportInput{
		Meta: meta, TargetTenantID: pathTenant, AlphaCode: code, Enable: cmd == "enable", ExpectedVersion: ev,
	})
	if err != nil {
		h.fail(w, metricCmd, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(metricCmd, telemetry.OutcomeReplayed)
	} else {
		h.countCmd(metricCmd, telemetry.OutcomeOK)
	}
	writeJSON(w, http.StatusOK, ts)
}

// ListTenantSupport lists the caller's own tenant overlay.
func (h *Handler) ListTenantSupport(w http.ResponseWriter, r *http.Request) {
	tenant := svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "tenant context missing (X-Tenant-Id)")
		return
	}
	if chi.URLParam(r, "tenant") != tenant {
		writeErr(w, http.StatusForbidden, domain.CodeContextInvalid, "tenant in path does not match the caller's trusted tenant context")
		return
	}
	items, err := h.svc.ListTenantSupport(r.Context(), tenant)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if items == nil {
		items = []domain.TenantSupport{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ── queries ──────────────────────────────────────────────────────────────────

// GetCurrency is GetCurrency / GetCurrencyAsOf (GET /v1/currencies/{code}?as_of=).
//
// Unknown code -> 404 NOT_FOUND. A RETIRED currency is returned normally (200,
// status RETIRED) so history stays readable; callers that need "usable for new
// postings" use :validate.
func (h *Handler) GetCurrency(w http.ResponseWriter, r *http.Request) {
	var asOf *time.Time
	if v := r.URL.Query().Get("as_of"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "as_of must be an RFC3339 timestamp")
			return
		}
		asOf = &t
	}
	c, err := h.svc.GetCurrency(r.Context(), chi.URLParam(r, "code"), asOf)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	// Revalidate-always: cacheable, but a cache may never serve a stale
	// lifecycle state without asking (a restrict/retire must be seen).
	w.Header().Set("Cache-Control", "no-cache")
	if asOf == nil {
		tag := etag(c)
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, http.StatusOK, c)
}

// ListCurrencies is ListSupportedCurrencies (GET /v1/currencies?status=SUPPORTED).
func (h *Handler) ListCurrencies(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListCurrencies(r.Context(), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if items == nil {
		items = []domain.Currency{}
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

// ResolveNumeric is ResolveNumericCurrencyCode (GET /v1/currencies:resolve-numeric?code=).
func (h *Handler) ResolveNumeric(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "code is required")
		return
	}
	c, err := h.svc.ResolveNumeric(r.Context(), code)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, c)
}

// MinorUnitVersions returns the append-only precision history.
func (h *Handler) MinorUnitVersions(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.ListMinorUnitVersions(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, v)
}

// Validate is the call other services make to satisfy "unknown currency blocks
// financial command". It always answers 200 with supported true/false for a
// well-formed query.
func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "code is required")
		return
	}
	op := q.Get("operation")
	if op == "" {
		op = domain.OperationPost
	}
	scoped := q.Get("tenant_scoped") == "true"
	res, err := h.svc.Validate(r.Context(), code, op, svcmiddleware.TenantFromContext(r.Context()), scoped)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if h.metrics != nil {
		if res.Supported {
			h.metrics.Validations.WithLabelValues("supported").Inc()
		} else {
			h.metrics.Validations.WithLabelValues("not_supported").Inc()
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func etag(c *domain.Currency) string {
	return `"` + c.CurrencyID + "-v" + strconv.FormatInt(c.Version, 10) + `"`
}

func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "limit must be between 1 and 500")
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "offset must not be negative")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// statusFor maps a typed error to its HTTP status.
func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeContextInvalid:
		return http.StatusUnprocessableEntity
	case domain.CodeVersionConflict, domain.CodeInvalidTransition, domain.CodeDuplicateCandidate, domain.CodeReferenceRetired:
		return http.StatusConflict
	case domain.CodeSourceUnverified:
		return http.StatusUnprocessableEntity
	case domain.CodeSoDDenied, domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeDependencyUnavailable:
		return http.StatusServiceUnavailable
	case domain.CodeRuleAmbiguous:
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// fail renders a service error. Typed errors keep their code; anything else is
// an internal fault reported as DEPENDENCY_UNAVAILABLE (the database), never
// leaking its text.
func (h *Handler) fail(w http.ResponseWriter, command string, err error) {
	if de, ok := domain.AsError(err); ok {
		outcome := telemetry.OutcomeRefused
		if de.Code == domain.CodeContextInvalid {
			outcome = telemetry.OutcomeInvalid
		}
		h.countCmd(command, outcome)
		writeErr(w, statusFor(de.Code), de.Code, de.Message)
		return
	}
	h.countCmd(command, telemetry.OutcomeUnavailable)
	h.log.Error("currency-registry store error", zap.Error(err))
	writeErr(w, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "store unavailable")
}

func (h *Handler) countCmd(command, outcome string) {
	if h.metrics != nil && command != "" {
		h.metrics.Commands.WithLabelValues(command, outcome).Inc()
	}
}

func (h *Handler) countImport(outcome string) {
	if h.metrics != nil {
		h.metrics.Imports.WithLabelValues(outcome).Inc()
	}
}

func (h *Handler) countAuthz(action, outcome string) {
	if h.metrics != nil {
		h.metrics.AuthZDecisions.WithLabelValues(action, outcome).Inc()
	}
}

func writeErr(w http.ResponseWriter, status int, code domain.Code, msg string) {
	writeJSON(w, status, map[string]string{"code": string(code), "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
