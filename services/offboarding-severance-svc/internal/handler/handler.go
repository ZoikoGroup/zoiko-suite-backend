package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/offboarding-severance-svc/internal/authz"
	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/employee"
	"zoiko.io/offboarding-severance-svc/internal/events"
	"zoiko.io/offboarding-severance-svc/internal/jurisdiction"
	"zoiko.io/offboarding-severance-svc/internal/store"
)

type Handler struct {
	store             store.Store
	publisher         events.Publisher
	authz             authz.Authorizer
	empValidator      employee.Validator
	jurisdictionRules jurisdiction.Validator
	logger            *zap.Logger
}

func New(
	s store.Store,
	pub events.Publisher,
	a authz.Authorizer,
	v employee.Validator,
	j jurisdiction.Validator,
	l *zap.Logger,
) *Handler {
	return &Handler{
		store:             s,
		publisher:         pub,
		authz:             a,
		empValidator:      v,
		jurisdictionRules: j,
		logger:            l,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1", func(r chi.Router) {
		r.Post("/terminations", h.InitiateTermination)
		r.Get("/terminations", h.ListTerminations)
		r.Get("/terminations/{id}", h.GetTermination)
		r.Post("/terminations/{id}/approve", h.ApproveTermination)
		r.Post("/terminations/{id}/finalize", h.FinalizeTermination)

		r.Post("/offboarding/checklists", h.CreateChecklist)
		r.Get("/offboarding/checklists/employee/{employee_id}", h.GetEmployeeChecklist)
		r.Put("/offboarding/checklists/items/{item_id}", h.UpdateChecklistItem)
	})
}

func (h *Handler) InitiateTermination(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, "OFFBOARD_TERMINATE_INITIATE", "termination_request"); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	var req domain.InitiateTerminationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.LegalEntityID == "" || req.TerminationType == "" || req.ReasonCode == "" || req.LastWorkingDay == "" || req.EffectiveFrom == "" {
		http.Error(w, "missing required fields (employee_id, legal_entity_id, termination_type, reason_code, last_working_day, effective_from)", http.StatusBadRequest)
		return
	}

	// Inter-service validation: verify employee exists
	if _, err := h.empValidator.ValidateEmployee(r.Context(), principalID, req.LegalEntityID, req.EmployeeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Inter-service validation: check jurisdictional minimum notice period
	noticeDays := req.NoticePeriodDays
	if h.jurisdictionRules != nil {
		validatedDays, err := h.jurisdictionRules.ValidateNoticePeriod(r.Context(), req.LegalEntityID, noticeDays)
		if err == nil && validatedDays > noticeDays {
			noticeDays = validatedDays
		}
	}

	tReq := &domain.TerminationRequest{
		LegalEntityID:    req.LegalEntityID,
		EmployeeID:       req.EmployeeID,
		TerminationType:  req.TerminationType,
		ReasonCode:       req.ReasonCode,
		ReasonDetails:    req.ReasonDetails,
		NoticePeriodDays: noticeDays,
		LastWorkingDay:   req.LastWorkingDay,
		EffectiveFrom:    req.EffectiveFrom,
		Status:           domain.TerminationStatusInitiated,
		InitiatedBy:      principalID,
		SeveranceAmount:  req.SeveranceAmount,
		Currency:         req.Currency,
	}
	if tReq.Currency == "" {
		tReq.Currency = "USD"
	}

	if err := h.store.CreateTerminationRequest(r.Context(), tReq); err != nil {
		h.logger.Error("failed to create termination request", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.publisher.PublishTerminationInitiated(r.Context(), principalID, *tReq)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(tReq)
}

func (h *Handler) GetTermination(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	req, err := h.store.GetTerminationRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTerminationNotFound) {
			http.Error(w, "termination request not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(req)
}

func (h *Handler) ListTerminations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	reqs, err := h.store.ListTerminationRequests(r.Context(), legalEntityID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if reqs == nil {
		reqs = []domain.TerminationRequest{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reqs)
}

func (h *Handler) ApproveTermination(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.authz.CheckAllowed(r.Context(), principalID, "OFFBOARD_TERMINATE_APPROVE", id); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	tReq, err := h.store.ApproveTerminationRequest(r.Context(), id, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrTerminationNotFound) {
			http.Error(w, "termination request not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, domain.ErrAlreadyApproved) {
			http.Error(w, "termination request is already approved or completed", http.StatusConflict)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.publisher.PublishTerminationApproved(r.Context(), principalID, *tReq)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tReq)
}

func (h *Handler) FinalizeTermination(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.authz.CheckAllowed(r.Context(), principalID, "OFFBOARD_TERMINATE_FINALIZE", id); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	tReq, err := h.store.FinalizeEmployeeTermination(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTerminationNotFound) {
			http.Error(w, "termination request not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Inter-service update: set employee status in employee-master-svc
	_ = h.empValidator.TerminateEmployee(r.Context(), principalID, tReq.EmployeeID)

	h.publisher.PublishEmployeeTerminated(r.Context(), principalID, *tReq)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tReq)
}

func (h *Handler) CreateChecklist(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	var req domain.CreateChecklistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.TerminationID == "" || req.LegalEntityID == "" {
		http.Error(w, "missing required fields (employee_id, termination_id, legal_entity_id)", http.StatusBadRequest)
		return
	}

	// Default checklist items if none provided
	items := req.Items
	if len(items) == 0 {
		items = []domain.ChecklistItem{
			{Category: "ASSET_RETURN", Description: "Return company laptop and peripherals", Status: domain.ChecklistItemStatusPending},
			{Category: "ACCESS_REVOCATION", Description: "Revoke single sign-on & VPN access", Status: domain.ChecklistItemStatusPending},
			{Category: "FINAL_PAY_AUDIT", Description: "Verify final pay and severance calculation", Status: domain.ChecklistItemStatusPending},
			{Category: "EXIT_INTERVIEW", Description: "Conduct exit interview", Status: domain.ChecklistItemStatusPending},
		}
	}

	chk := &domain.OffboardingChecklist{
		LegalEntityID: req.LegalEntityID,
		EmployeeID:    req.EmployeeID,
		TerminationID: req.TerminationID,
		Items:         items,
	}

	if err := h.store.CreateOffboardingChecklist(r.Context(), chk); err != nil {
		h.logger.Error("failed to create offboarding checklist", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(chk)
}

func (h *Handler) GetEmployeeChecklist(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")
	chk, err := h.store.GetOffboardingChecklist(r.Context(), empID)
	if err != nil {
		if errors.Is(err, domain.ErrChecklistNotFound) {
			http.Error(w, "checklist not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(chk)
}

func (h *Handler) UpdateChecklistItem(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	itemID := chi.URLParam(r, "item_id")
	var req domain.UpdateChecklistItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Status == "" {
		http.Error(w, "missing status field", http.StatusBadRequest)
		return
	}

	completedBy := req.CompletedBy
	if completedBy == "" {
		completedBy = principalID
	}

	if err := h.store.UpdateChecklistItemStatus(r.Context(), itemID, req.Status, completedBy); err != nil {
		h.logger.Error("failed to update checklist item status", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}
