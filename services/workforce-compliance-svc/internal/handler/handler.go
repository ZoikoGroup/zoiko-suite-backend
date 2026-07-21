package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workforce-compliance-svc/internal/authz"
	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/employee"
	"zoiko.io/workforce-compliance-svc/internal/events"
	"zoiko.io/workforce-compliance-svc/internal/jurisdiction"
	"zoiko.io/workforce-compliance-svc/internal/store"
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
	r.Route("/v1/compliance", func(r chi.Router) {
		r.Post("/work-auth", h.CreateWorkAuth)
		r.Get("/work-auth/employee/{employee_id}", h.GetWorkAuth)
		r.Post("/work-auth/{id}/verify", h.VerifyWorkAuth)

		r.Post("/visas", h.CreateVisa)
		r.Get("/visas/employee/{employee_id}", h.GetVisa)
		r.Post("/visas/{id}/flag-expiry", h.FlagVisaExpiry)

		r.Post("/hours", h.LogHours)

		r.Get("/alerts", h.ListAlerts)
		r.Post("/alerts/{id}/resolve", h.ResolveAlert)
	})
}

func (h *Handler) CreateWorkAuth(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, "COMPLIANCE_WORK_AUTH_CREATE", "work_authorization"); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	var req domain.CreateWorkAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.LegalEntityID == "" || req.DocumentType == "" || req.DocumentNumber == "" || req.IssueDate == "" || req.EffectiveFrom == "" {
		http.Error(w, "missing required fields (employee_id, legal_entity_id, document_type, document_number, issue_date, effective_from)", http.StatusBadRequest)
		return
	}

	// Inter-service validation: verify employee exists
	if _, err := h.empValidator.ValidateEmployee(r.Context(), principalID, req.LegalEntityID, req.EmployeeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	auth := &domain.WorkAuthorization{
		LegalEntityID:  req.LegalEntityID,
		EmployeeID:     req.EmployeeID,
		DocumentType:   req.DocumentType,
		DocumentNumber: req.DocumentNumber,
		IssueDate:      req.IssueDate,
		ExpiryDate:     req.ExpiryDate,
		EffectiveFrom:  req.EffectiveFrom,
		Status:         domain.VerificationStatusPending,
	}

	if err := h.store.CreateWorkAuth(r.Context(), auth); err != nil {
		h.logger.Error("failed to create work authorization", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(auth)
}

func (h *Handler) GetWorkAuth(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")
	auth, err := h.store.GetWorkAuth(r.Context(), empID)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			http.Error(w, "work authorization not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(auth)
}

func (h *Handler) VerifyWorkAuth(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.authz.CheckAllowed(r.Context(), principalID, "COMPLIANCE_WORK_AUTH_VERIFY", id); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	auth, err := h.store.VerifyWorkAuth(r.Context(), id, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			http.Error(w, "work authorization not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.publisher.PublishWorkAuthVerified(r.Context(), principalID, *auth)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(auth)
}

func (h *Handler) CreateVisa(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, "COMPLIANCE_VISA_CREATE", "visa_record"); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	var req domain.CreateVisaRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.LegalEntityID == "" || req.VisaType == "" || req.IssuingCountry == "" || req.ExpirationDate == "" {
		http.Error(w, "missing required fields (employee_id, legal_entity_id, visa_type, issuing_country, expiration_date)", http.StatusBadRequest)
		return
	}

	// Inter-service validation: verify employee exists
	if _, err := h.empValidator.ValidateEmployee(r.Context(), principalID, req.LegalEntityID, req.EmployeeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	visa := &domain.VisaRecord{
		LegalEntityID:   req.LegalEntityID,
		EmployeeID:      req.EmployeeID,
		VisaType:        req.VisaType,
		IssuingCountry:  req.IssuingCountry,
		ExpirationDate:  req.ExpirationDate,
		GracePeriodDays: req.GracePeriodDays,
		Status:          domain.VerificationStatusVerified,
	}

	if err := h.store.CreateVisaRecord(r.Context(), visa); err != nil {
		h.logger.Error("failed to create visa record", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(visa)
}

func (h *Handler) GetVisa(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")
	visa, err := h.store.GetVisaRecord(r.Context(), empID)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			http.Error(w, "visa record not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(visa)
}

func (h *Handler) FlagVisaExpiry(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	visa, err := h.store.FlagVisaExpiration(r.Context(), id)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Raise compliance alert
	alert := &domain.ComplianceAlert{
		LegalEntityID: visa.LegalEntityID,
		EmployeeID:    visa.EmployeeID,
		Category:      "VISA_EXPIRATION",
		Severity:      domain.AlertSeverityCritical,
		Message:       fmt.Sprintf("Visa %s (%s) for employee %s expires on %s", visa.VisaType, visa.VisaID, visa.EmployeeID, visa.ExpirationDate),
	}
	_ = h.store.CreateComplianceAlert(r.Context(), alert)

	h.publisher.PublishVisaExpirationFlagged(r.Context(), principalID, *visa)
	h.publisher.PublishComplianceAlertRaised(r.Context(), principalID, *alert)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(visa)
}

func (h *Handler) LogHours(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	var req domain.LogWorkingHoursRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.LegalEntityID == "" || req.WorkDate == "" || req.HoursWorked == 0 {
		http.Error(w, "missing required fields (employee_id, legal_entity_id, work_date, hours_worked)", http.StatusBadRequest)
		return
	}

	// Inter-service check: get statutory max allowed weekly hours for jurisdiction
	maxAllowed := 48.0
	if h.jurisdictionRules != nil {
		if limit, err := h.jurisdictionRules.GetWorkingHourLimit(r.Context(), req.LegalEntityID); err == nil && limit > 0 {
			maxAllowed = limit
		}
	}

	// Accumulate weekly hours
	prevHours, _ := h.store.GetWeeklyHours(r.Context(), req.EmployeeID, req.WorkDate)
	totalWeekly := prevHours + req.HoursWorked

	isBreached := totalWeekly > maxAllowed

	logEntry := &domain.WorkingHourLog{
		LegalEntityID:     req.LegalEntityID,
		EmployeeID:        req.EmployeeID,
		WorkDate:          req.WorkDate,
		HoursWorked:       req.HoursWorked,
		OvertimeHours:     req.OvertimeHours,
		WeeklyAccumulated: totalWeekly,
		IsBreached:        isBreached,
		MaxAllowedHours:   maxAllowed,
	}

	if err := h.store.LogWorkingHours(r.Context(), logEntry); err != nil {
		h.logger.Error("failed to log working hours", zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if isBreached {
		alert := &domain.ComplianceAlert{
			LegalEntityID: req.LegalEntityID,
			EmployeeID:    req.EmployeeID,
			Category:      "HOUR_LIMIT_BREACH",
			Severity:      domain.AlertSeverityWarning,
			Message:       fmt.Sprintf("Statutory weekly working hour limit of %.1f hours breached by employee %s (accumulated: %.1f hrs)", maxAllowed, req.EmployeeID, totalWeekly),
		}
		_ = h.store.CreateComplianceAlert(r.Context(), alert)

		h.publisher.PublishWorkingHoursBreach(r.Context(), principalID, *logEntry)
		h.publisher.PublishComplianceAlertRaised(r.Context(), principalID, *alert)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(logEntry)
}

func (h *Handler) ListAlerts(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	alerts, err := h.store.ListComplianceAlerts(r.Context(), legalEntityID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if alerts == nil {
		alerts = []domain.ComplianceAlert{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(alerts)
}

func (h *Handler) ResolveAlert(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.store.ResolveComplianceAlert(r.Context(), id, principalID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
}
