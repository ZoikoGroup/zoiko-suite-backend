package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/eventing/integration"
)

// Authorization actions of the contract registry (XIC-01). Registry content is platform data, so every check is
// made for the platform scope, never for a tenant's legal entity.
const (
	ActionConnectorCreate    = "XIC_CONNECTOR_CREATE"
	ActionConnectorDeprecate = "XIC_CONNECTOR_DEPRECATE"
	ActionContractCreate     = "XIC_CONTRACT_CREATE"
	ActionContractUpdate     = "XIC_CONTRACT_UPDATE"
	ActionContractValidate   = "XIC_CONTRACT_VALIDATE"
	ActionContractSubmit     = "XIC_CONTRACT_SUBMIT"
	ActionContractBlock      = "XIC_CONTRACT_BLOCK"
	ActionContractPublish    = "XIC_CONTRACT_PUBLISH"
	ActionMappingSimulate    = "XIC_MAPPING_SIMULATE"
	ActionEvidenceRead       = "XIC_EVIDENCE_READ"
	// ActionGatePrefix + a gate key is the action to attest that gate, so the person who may attest SECURITY
	// need not be the person who may attest PRIVACY.
	ActionGatePrefix = "XIC_GATE_"
)

// RegistryHandler serves the §6.5 registry API.
type RegistryHandler struct {
	svc    *registry.Service
	repo   registry.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewRegistry builds the registry handler.
func NewRegistry(svc *registry.Service, repo registry.Repo, az *authz.Client, logger *zap.Logger) *RegistryHandler {
	return &RegistryHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterRegistryRoutes mounts the registry. Commands use the spec's "{id}:command" form, dispatched by one
// route so the id is never confused with the command.
func RegisterRegistryRoutes(r chi.Router, h *RegistryHandler) {
	r.Route("/v1/connectors", func(r chi.Router) {
		r.Post("/", h.CreateConnector)
		r.Get("/", h.ListConnectors)
		r.Get("/{id}", h.GetConnector)
		r.Post("/{idcmd}", h.ConnectorCommand)
		r.Get("/{id}/compatibility", h.Compatibility)
		r.Post("/{id}/versions", h.CreateVersion)
		r.Get("/{id}/versions", h.ListVersions)
	})
	r.Route("/v1/contract-versions", func(r chi.Router) {
		r.Get("/{id}", h.GetVersion)
		r.Put("/{id}", h.ReplaceDraft)
		r.Post("/{idcmd}", h.VersionCommand)
		r.Get("/{id}/gates", h.ListGates)
		r.Get("/{id}/transitions", h.ListTransitions)
		r.Get("/{id}/validation-runs", h.ListRuns)
	})
	r.Route("/v1/mapping-versions", func(r chi.Router) {
		r.Get("/{id}", h.GetMapping)
		r.Post("/{idcmd}", h.MappingCommand)
	})
}

// ── plumbing ────────────────────────────────────────────────────────────────

const maxRegistryBody = 5 << 20

func decodeBig(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegistryBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeProblem(w, r, errBadBody)
		return false
	}
	return true
}

func (h *RegistryHandler) actor(r *http.Request) registry.Actor {
	return registry.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

// guard resolves the caller, authorizes action for the platform scope and returns the tenant. Registry writes are
// platform data: the authorization is asked for the platform scope whatever the caller's own tenant.
func (h *RegistryHandler) guard(w http.ResponseWriter, r *http.Request, action string) (tenant string, ok bool) {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeProblem(w, r, errNoPrincipal)
		return "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, registry.PlatformScopeID, action); err != nil {
		if !errors.Is(err, authz.ErrAuthorizationDenied) {
			h.logger.Error("authorization check failed", zap.Error(err))
			err = errors.Join(authz.ErrAuthzServiceUnavailable, err)
		}
		writeProblem(w, r, err)
		return "", false
	}
	return middleware.GetTenantID(r.Context()), true
}

func isPlatformTenant(r *http.Request) bool {
	return middleware.GetTenantID(r.Context()) == registry.PlatformScopeID
}

// split separates "{id}:{command}".
func split(idcmd string) (id, cmd string) {
	i := strings.LastIndex(idcmd, ":")
	if i < 0 {
		return idcmd, ""
	}
	return idcmd[:i], idcmd[i+1:]
}

func writeRegistryProblem(w http.ResponseWriter, r *http.Request, err error) {
	var ve *registry.ValidationError
	switch {
	case errors.As(err, &ve):
		body := problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, "the request does not satisfy the registry's rules")
		body["issues"] = ve.Issues
		emit(w, http.StatusUnprocessableEntity, body, 0)
	case registry.IsNotFound(err):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, registry.ErrNotPlatformScope):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, "registry content can only be written from the platform scope"), 0)
	case errors.Is(err, registry.ErrSelfAttest), errors.Is(err, registry.ErrSelfPublish):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, registry.ErrVersionConflict), errors.Is(err, registry.ErrIdempotency):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, integration.CodeIdempotencyConflict, err.Error()), 0)
	case errors.Is(err, registry.ErrConnectorExists), errors.Is(err, registry.ErrVersionExists), errors.Is(err, registry.ErrNotDraft),
		errors.Is(err, registry.ErrTransition), errors.Is(err, registry.ErrNotValidated), errors.Is(err, registry.ErrStaleValidation),
		errors.Is(err, registry.ErrGatesIncomplete), errors.Is(err, registry.ErrBaselineMoved), errors.Is(err, registry.ErrVersionOrder),
		errors.Is(err, registry.ErrDeprecateWindow):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	default:
		// authorization, request-shape and everything unclassified keep the service's existing presentation
		writeProblem(w, r, err)
	}
}

func problemBase(r *http.Request, status int, code integration.Code, detail string) map[string]any {
	body := map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail, "error": detail}
	if r != nil {
		body["instance"] = r.URL.Path
		if c := r.Header.Get("X-Correlation-ID"); c != "" {
			body["correlation_id"] = c
		}
	}
	if code != "" {
		info := code.Info()
		body["type"] = integration.ProblemTypePrefix + string(code)
		body["title"] = info.Title
		body["code"] = string(code)
		body["retryable"] = info.Retryable
	}
	return body
}

func emit(w http.ResponseWriter, status int, body map[string]any, retry int) {
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// redactConnector and redactVersion remove the principals behind a change from what a tenant reads: staff
// identities are not reference data.
func redactConnector(r *http.Request, c *registry.Connector) *registry.Connector {
	if c == nil || isPlatformTenant(r) {
		return c
	}
	cp := *c
	cp.CreatedBy = ""
	return &cp
}

func redactVersion(r *http.Request, v *registry.ContractVersion) *registry.ContractVersion {
	if v == nil || isPlatformTenant(r) {
		return v
	}
	cp := *v
	cp.CreatedBy, cp.PublishedBy, cp.BlockedReason = "", "", ""
	cp.MigrationPlanRef, cp.ReplayAnalysisRef, cp.RollbackRef = "", "", ""
	return &cp
}

func idempotencyKey(r *http.Request) string { return r.Header.Get("Idempotency-Key") }

// ── connectors ───────────────────────────────────────────────────────────────

func (h *RegistryHandler) CreateConnector(w http.ResponseWriter, r *http.Request) {
	var c registry.Connector
	if !decodeBig(w, r, &c) {
		return
	}
	tenant, ok := h.guard(w, r, ActionConnectorCreate)
	if !ok {
		return
	}
	out, created, err := h.svc.CreateConnector(r.Context(), tenant, h.actor(r), c, idempotencyKey(r))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

func (h *RegistryHandler) ListConnectors(w http.ResponseWriter, r *http.Request) {
	family, lifecycle := strings.ToUpper(r.URL.Query().Get("family")), strings.ToUpper(r.URL.Query().Get("lifecycle"))
	if family != "" && !contains(registry.Families, family) || lifecycle != "" && !contains([]string{"DRAFT", "REVIEW", "PUBLISHED", "DEPRECATED", "RETIRED"}, lifecycle) {
		writeRegistryProblem(w, r, &registry.ValidationError{Issues: []registry.Issue{{Path: "query", Rule: "family or lifecycle is not a defined value"}}})
		return
	}
	list, err := h.repo.ListConnectors(r.Context(), family, lifecycle)
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	for i := range list {
		list[i] = *redactConnector(r, &list[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": list, "total": len(list)})
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func (h *RegistryHandler) GetConnector(w http.ResponseWriter, r *http.Request) {
	c, err := h.repo.GetConnector(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, redactConnector(r, c))
}

type deprecateRequest struct {
	ExpectedVersion int64     `json:"expected_version"`
	SupportedUntil  time.Time `json:"supported_until"`
	Reason          string    `json:"reason"`
}

func (h *RegistryHandler) ConnectorCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	if cmd != "deprecate" {
		writeRegistryProblem(w, r, registry.ErrConnectorNotFound)
		return
	}
	var req deprecateRequest
	if !decodeBig(w, r, &req) {
		return
	}
	n, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || n < 1 {
		writeProblem(w, r, errNeedVersion)
		return
	}
	tenant, ok := h.guard(w, r, ActionConnectorDeprecate)
	if !ok {
		return
	}
	out, err := h.svc.DeprecateConnector(r.Context(), tenant, h.actor(r), id, n, req.SupportedUntil, req.Reason)
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *RegistryHandler) Compatibility(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Compatibility(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	out.Connector = redactConnector(r, out.Connector)
	writeJSON(w, http.StatusOK, out)
}

// ── contract versions ────────────────────────────────────────────────────────

func (h *RegistryHandler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	var in registry.VersionInput
	if !decodeBig(w, r, &in) {
		return
	}
	tenant, ok := h.guard(w, r, ActionContractCreate)
	if !ok {
		return
	}
	v, created, err := h.svc.CreateVersion(r.Context(), tenant, h.actor(r), chi.URLParam(r, "id"), in, idempotencyKey(r))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, v)
}

func (h *RegistryHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := h.repo.ListVersions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	out := make([]versionSummary, 0, len(vs))
	for i := range vs {
		v := &vs[i]
		out = append(out, versionSummary{ContractVersionID: v.ContractVersionID, ConnectorID: v.ConnectorID, Semver: v.Semver,
			Status: v.Status, EffectiveFrom: v.EffectiveFrom, SupportedUntil: v.SupportedUntil, CompatClass: v.CompatClass,
			ContentHash: v.ContentHash, Version: v.RowVersion, PublishedAt: v.PublishedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out, "total": len(out)})
}

func (h *RegistryHandler) GetVersion(w http.ResponseWriter, r *http.Request) {
	v, err := h.repo.GetVersion(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, redactVersion(r, v))
}

type replaceRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
	registry.VersionInput
}

func (h *RegistryHandler) ReplaceDraft(w http.ResponseWriter, r *http.Request) {
	var req replaceRequest
	if !decodeBig(w, r, &req) {
		return
	}
	n, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || n < 1 {
		writeProblem(w, r, errNeedVersion)
		return
	}
	tenant, ok := h.guard(w, r, ActionContractUpdate)
	if !ok {
		return
	}
	v, err := h.svc.ReplaceDraft(r.Context(), tenant, h.actor(r), chi.URLParam(r, "id"), n, req.VersionInput)
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type versionCommandBody struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
	registry.AttestInput
}

func (h *RegistryHandler) VersionCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	switch cmd {
	case "validate", "submit", "attest", "block", "publish":
	default:
		writeRegistryProblem(w, r, registry.ErrVersionNotFound)
		return
	}
	var body versionCommandBody
	if !decodeBig(w, r, &body) {
		return
	}
	n, err := expectedVersion(r, body.ExpectedVersion)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if cmd != "attest" && n < 1 {
		writeProblem(w, r, errNeedVersion)
		return
	}
	action := map[string]string{"validate": ActionContractValidate, "submit": ActionContractSubmit, "block": ActionContractBlock,
		"publish": ActionContractPublish}[cmd]
	if cmd == "attest" {
		if !contains(registry.RequiredGates, body.Gate) || body.Gate == registry.GateSchemaCompat {
			writeRegistryProblem(w, r, &registry.ValidationError{Issues: []registry.Issue{{Path: "gate", Rule: "a human-attested gate"}}})
			return
		}
		action = ActionGatePrefix + body.Gate
	}
	tenant, ok := h.guard(w, r, action)
	if !ok {
		return
	}
	a, ctx := h.actor(r), r.Context()
	switch cmd {
	case "validate":
		res, err := h.svc.Validate(ctx, tenant, a, id, n)
		if err != nil {
			var ve *registry.ValidationError
			if errors.As(err, &ve) && res != nil {
				// the run is recorded; the author reads which run and what it found
				pb := problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, "the contract did not pass validation")
				pb["issues"], pb["run_id"], pb["compat_report"] = ve.Issues, res.Run.RunID, res.Run.Report
				emit(w, http.StatusUnprocessableEntity, pb, 0)
				return
			}
			writeRegistryProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "submit":
		v, err := h.svc.SubmitReview(ctx, tenant, a, id, n)
		h.reply(w, r, v, err)
	case "attest":
		v, err := h.svc.Attest(ctx, tenant, a, id, body.AttestInput)
		h.reply(w, r, v, err)
	case "block":
		v, err := h.svc.Block(ctx, tenant, a, id, n, body.Reason)
		h.reply(w, r, v, err)
	case "publish":
		v, err := h.svc.Publish(ctx, tenant, a, id, n)
		h.reply(w, r, v, err)
	}
}

func (h *RegistryHandler) reply(w http.ResponseWriter, r *http.Request, v *registry.ContractVersion, err error) {
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// evidence reads are for the platform scope: they name the people behind each decision.
func (h *RegistryHandler) platformEvidence(w http.ResponseWriter, r *http.Request) bool {
	if !isPlatformTenant(r) {
		writeRegistryProblem(w, r, registry.ErrVersionNotFound)
		return false
	}
	if _, ok := h.guard(w, r, ActionEvidenceRead); !ok {
		return false
	}
	return true
}

func (h *RegistryHandler) ListGates(w http.ResponseWriter, r *http.Request) {
	if !h.platformEvidence(w, r) {
		return
	}
	g, err := h.repo.ListGates(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gates": g, "total": len(g)})
}

func (h *RegistryHandler) ListTransitions(w http.ResponseWriter, r *http.Request) {
	if !h.platformEvidence(w, r) {
		return
	}
	t, err := h.repo.ListTransitions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": t, "total": len(t)})
}

func (h *RegistryHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	if !h.platformEvidence(w, r) {
		return
	}
	runs, err := h.repo.ListValidationRuns(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"validation_runs": runs, "total": len(runs)})
}

// ── mappings ─────────────────────────────────────────────────────────────────

func (h *RegistryHandler) GetMapping(w http.ResponseWriter, r *http.Request) {
	m, err := h.repo.GetMapping(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type simulateRequest struct {
	Samples []registry.SimulationSample `json:"samples"`
}

func (h *RegistryHandler) MappingCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	if cmd != "simulate" {
		writeRegistryProblem(w, r, registry.ErrMappingNotFound)
		return
	}
	var req simulateRequest
	if !decodeBig(w, r, &req) {
		return
	}
	if _, ok := h.guard(w, r, ActionMappingSimulate); !ok {
		return
	}
	res, err := h.svc.Simulate(r.Context(), id, req.Samples)
	if err != nil {
		writeRegistryProblem(w, r, err)
		return
	}
	// a simulation changes nothing; 200, never 201/202
	writeJSON(w, http.StatusOK, map[string]any{"mapping_version_id": id, "results": res, "mutated": false})
}

// versionSummary is a row of the version list: the contract itself is read one version at a time.
type versionSummary struct {
	ContractVersionID string     `json:"contract_version_id"`
	ConnectorID       string     `json:"connector_id"`
	Semver            string     `json:"semver"`
	Status            string     `json:"status"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	SupportedUntil    *time.Time `json:"supported_until,omitempty"`
	CompatClass       string     `json:"compat_class,omitempty"`
	ContentHash       string     `json:"content_hash,omitempty"`
	Version           int64      `json:"version"`
	PublishedAt       *time.Time `json:"published_at,omitempty"`
}
