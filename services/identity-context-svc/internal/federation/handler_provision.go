package federation

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// registerProvisioning mounts the administrative side of provisioning. The SCIM endpoints a directory calls are
// served separately (SCIMRouter), outside the platform's envelope: a directory has no platform identity.
func (h *Handler) registerProvisioning(r chi.Router) {
	base := "/v1/federation/connections/{connectionID}"
	r.Post(base+"/mapping", h.ConfigureMapping)
	r.Get(base+"/mapping", h.GetMapping)
	r.Post(base+"/mapping/versions/{version}/approve", h.ApproveMapping)
	r.Post(base+"/mapping/versions/{version}/reject", h.RejectMapping)

	r.Post(base+"/scim-token", h.IssueSCIMToken)
	r.Post(base+"/scim-token/revoke", h.RevokeSCIMToken)

	r.Get(base+"/provisioning/status", h.ProvisioningStatus)
	r.Get(base+"/provisioning/events", h.ProvisioningEvents)
	r.Get(base+"/provisioning/exceptions", h.SyncExceptions)
	r.Post(base+"/provisioning/exceptions/{exceptionID}/resolve", h.ResolveException)
	r.Get(base+"/provisioning/deprovision-latency", h.DeprovisionLatency)
	r.Post(base+"/provisioning/reconcile", h.Reconcile)
}

func (h *Handler) ConfigureMapping(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionProvision)
	if !ok {
		return
	}
	var cfg MappingConfig
	if err := decodeBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	m, err := h.prov.ConfigureMapping(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), cfg)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, m)
}

func (h *Handler) GetMapping(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	m, err := h.prov.GetMapping(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ApproveMapping(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionApprove)
	if !ok {
		return
	}
	n, ok := versionParam(w, r)
	if !ok {
		return
	}
	var b decisionBody
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	m, err := h.prov.ApproveMapping(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), n, b.Fingerprint, b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) RejectMapping(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionApprove)
	if !ok {
		return
	}
	n, ok := versionParam(w, r)
	if !ok {
		return
	}
	var b decisionBody
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	m, err := h.prov.RejectMapping(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), n, b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// IssueSCIMToken returns the token once. It is never stored, logged or shown again.
func (h *Handler) IssueSCIMToken(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionProvision)
	if !ok {
		return
	}
	id := chi.URLParam(r, "connectionID")
	token, err := h.prov.IssueSCIMToken(r.Context(), c.tenantID, c.principalID, c.correlationID, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusCreated, map[string]any{"connection_id": id, "token": token, "token_type": "Bearer",
		"scim_base_path": "/scim/v2/" + c.tenantID + "/" + id,
		"note":           "this token is shown once and replaces any earlier token for the connection"})
}

func (h *Handler) RevokeSCIMToken(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionProvision)
	if !ok {
		return
	}
	n, err := h.prov.RevokeSCIMToken(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (h *Handler) ProvisioningStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	st, err := h.prov.Status(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) ProvisioningEvents(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.prov.Events(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"), r.URL.Query().Get("status"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (h *Handler) SyncExceptions(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.prov.Exceptions(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"), r.URL.Query().Get("status"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionProvision)
	if !ok {
		return
	}
	var b struct {
		Resolution string `json:"resolution"`
	}
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	e, err := h.prov.ResolveException(r.Context(), c.tenantID, c.principalID, chi.URLParam(r, "exceptionID"), b.Resolution)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *Handler) DeprovisionLatency(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.prov.Latency(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Reconcile(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionProvision)
	if !ok {
		return
	}
	var req ReconcileRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	out, err := h.prov.Reconcile(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
