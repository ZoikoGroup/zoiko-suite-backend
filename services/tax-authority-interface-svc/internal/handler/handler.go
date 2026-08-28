package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

const (
	principalIDHeader = "X-Principal-Id"

	ActionInterfaceCreate = "TAX_INTERFACE_CREATE"
	ActionFilingSubmit    = "TAX_FILING_SUBMIT"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/tax-authority", func(r chi.Router) {
		r.Post("/interfaces", h.CreateInterface)
		r.Get("/interfaces", h.ListInterfaces)
		r.Get("/interfaces/{id}", h.GetInterfaceByID)
		r.Post("/filings", h.SubmitTaxFiling)
		r.Get("/filings", h.ListSubmissions)
	})
}

func (h *Handler) CreateInterface(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}

	var req domain.CreateInterfaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Jurisdiction == "" || req.AuthorityName == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction, and authority_name are required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, ActionInterfaceCreate); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to create tax interface")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		return
	}

	tf := &domain.TaxInterface{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Jurisdiction:  req.Jurisdiction,
		AuthorityName: req.AuthorityName,
		Protocol:      req.Protocol,
		Status:        "ACTIVE",
	}

	if err := h.store.CreateInterface(r.Context(), tf); err != nil {
		h.logger.Error("failed to create tax interface", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create tax interface")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "tax.interface.created", AggregateID: tf.InterfaceID, TenantID: tenantID,
		LegalEntityID: tf.LegalEntityID, Jurisdiction: tf.Jurisdiction, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: tf,
	})
	writeJSON(w, http.StatusCreated, tf)
}

func (h *Handler) GetInterfaceByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tf, err := h.store.GetInterfaceByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrInterfaceNotFound) {
			writeError(w, http.StatusNotFound, "tax interface not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax interface")
		return
	}
	writeJSON(w, http.StatusOK, tf)
}

func (h *Handler) ListInterfaces(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	interfaces, err := h.store.ListInterfaces(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax interfaces")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"interfaces": interfaces,
		"total":      len(interfaces),
	})
}

func (h *Handler) SubmitTaxFiling(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}

	var req domain.SubmitTaxFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.InterfaceID == "" || req.TaxPeriod == "" || req.FilingType == "" {
		writeError(w, http.StatusBadRequest, "interface_id, tax_period, and filing_type are required")
		return
	}

	iface, err := h.store.GetInterfaceByID(r.Context(), req.InterfaceID)
	if err != nil {
		if errors.Is(err, domain.ErrInterfaceNotFound) {
			writeError(w, http.StatusNotFound, "tax interface not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify tax interface")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, iface.LegalEntityID, ActionFilingSubmit); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to submit tax filing")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		return
	}

	// This service has no real transmission mechanism to any tax authority —
	// no outbound HTTP client to a filing endpoint exists anywhere in this
	// codebase (verified: internal/ contains only an authz client and an
	// mTLS helper, nothing that calls out to authority-provided infrastructure).
	//
	// The status this row previously received was TaxFilingSubmitted with a
	// hardcoded AckReference ("TAX-ACK-991823") — a fabricated acknowledgement
	// applied to every filing regardless of whether anything was sent. That is
	// not a tenant-isolation or authorization defect; it is the service
	// reporting a false real-world fact. ZS-SVC-F-001 (Tax/E-Invoicing/
	// Regulatory Compliance) names this exact anti-pattern: "Transport success
	// is not filing acceptance" / "Authority rejection hidden as submitted",
	// and its negative-path acceptance tests require Pending/Unknown handling
	// rather than an assumed-successful terminal state.
	//
	// Until a real transmission adapter exists, this endpoint records the
	// filing intent honestly as PENDING with no acknowledgement — the state
	// that is actually true — rather than fabricating SUBMITTED. A caller
	// checking status sees the real state of the world: recorded, not yet
	// transmitted, not yet acknowledged.
	sub := &domain.TaxFilingSubmission{
		InterfaceID: req.InterfaceID,
		TenantID:    tenantID,
		TaxPeriod:   req.TaxPeriod,
		FilingType:  req.FilingType,
		TaxAmount:   req.TaxAmount,
		Status:      domain.TaxFilingPending,
	}

	if err := h.store.CreateSubmission(r.Context(), sub); err != nil {
		h.logger.Error("failed to create tax filing submission", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to submit tax filing")
		return
	}

	// tax.filing.submitted is deliberately NOT published here. That event
	// name asserts a fact (transmitted to the authority) that has not
	// happened; publishing it would propagate the same fabrication to every
	// downstream consumer (workflow, evidence, obligations tracking) that
	// trusts this service's events. tax.filing.recorded reflects what
	// actually occurred: the request was accepted and persisted locally.
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "tax.filing.recorded", AggregateID: sub.SubmissionID, TenantID: tenantID,
		LegalEntityID: iface.LegalEntityID, Jurisdiction: iface.Jurisdiction, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: sub,
	})
	writeJSON(w, http.StatusAccepted, sub)
}

func (h *Handler) ListSubmissions(w http.ResponseWriter, r *http.Request) {
	interfaceID := r.URL.Query().Get("interface_id")
	submissions, err := h.store.ListSubmissions(r.Context(), interfaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax filing submissions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"submissions": submissions,
		"total":       len(submissions),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
