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

const (
	actionWorkAuthCreate = "COMPLIANCE_WORK_AUTH_CREATE"
	actionWorkAuthVerify = "COMPLIANCE_WORK_AUTH_VERIFY"
	actionVisaCreate     = "COMPLIANCE_VISA_CREATE"
	actionVisaFlag       = "COMPLIANCE_VISA_FLAG"
	actionHoursLog       = "COMPLIANCE_HOURS_LOG"
	actionAlertResolve   = "COMPLIANCE_ALERT_RESOLVE"
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

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		http.Error(w, "authorization-svc unavailable", http.StatusServiceUnavailable)
	}
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrIdentityMissing):
		http.Error(w, "missing X-Tenant-Id header", http.StatusBadRequest)
	case errors.Is(err, domain.ErrRecordNotFound):
		http.Error(w, "compliance record not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrAlertNotFound):
		http.Error(w, "compliance alert not found", http.StatusNotFound)
	default:
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h *Handler) CreateWorkAuth(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		http.Error(w, "missing X-Principal-Id header", http.StatusUnauthorized)
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
	if req.CorrelationID == "" {
		http.Error(w, "missing required field: correlation_id", http.StatusBadRequest)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionWorkAuthCreate); err != nil {
		h.writeAuthzErr(w, err)
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
		CorrelationID:  req.CorrelationID,
	}

	created, err := h.store.CreateWorkAuth(r.Context(), auth)
	if err != nil {
		h.logger.Error("failed to create work authorization", zap.Error(err))
		h.writeStoreErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !created {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(auth)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(auth)
}

func (h *Handler) GetWorkAuth(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")
	auth, err := h.store.GetWorkAuth(r.Context(), empID)
	if err != nil {
		h.writeStoreErr(w, err)
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
	target, err := h.store.GetWorkAuthByID(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, target.LegalEntityID, actionWorkAuthVerify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	auth, err := h.store.VerifyWorkAuth(r.Context(), id, principalID)
	if err != nil {
		h.writeStoreErr(w, err)
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

	var req domain.CreateVisaRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EmployeeID == "" || req.LegalEntityID == "" || req.VisaType == "" || req.IssuingCountry == "" || req.ExpirationDate == "" {
		http.Error(w, "missing required fields (employee_id, legal_entity_id, visa_type, issuing_country, expiration_date)", http.StatusBadRequest)
		return
	}
	if req.CorrelationID == "" {
		http.Error(w, "missing required field: correlation_id", http.StatusBadRequest)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionVisaCreate); err != nil {
		h.writeAuthzErr(w, err)
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
		CorrelationID:   req.CorrelationID,
	}

	created, err := h.store.CreateVisaRecord(r.Context(), visa)
	if err != nil {
		h.logger.Error("failed to create visa record", zap.Error(err))
		h.writeStoreErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !created {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(visa)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(visa)
}

func (h *Handler) GetVisa(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")
	visa, err := h.store.GetVisaRecord(r.Context(), empID)
	if err != nil {
		h.writeStoreErr(w, err)
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
	alreadyFlagged := errors.Is(err, domain.ErrAlreadyFlagged)
	if err != nil && !alreadyFlagged {
		h.writeStoreErr(w, err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, visa.LegalEntityID, actionVisaFlag); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if !alreadyFlagged {
		// Raise compliance alert — only on the transition into "flagged",
		// never on a replay, or every retried call would raise a second
		// duplicate alert for the same visa.
		alert := &domain.ComplianceAlert{
			LegalEntityID: visa.LegalEntityID,
			EmployeeID:    visa.EmployeeID,
			Category:      "VISA_EXPIRATION",
			Severity:      domain.AlertSeverityCritical,
			Message:       fmt.Sprintf("Visa %s (%s) for employee %s expires on %s", visa.VisaType, visa.VisaID, visa.EmployeeID, visa.ExpirationDate),
		}
		if err := h.store.CreateComplianceAlert(r.Context(), alert); err != nil {
			h.logger.Error("failed to create compliance alert", zap.String("visa_id", visa.VisaID), zap.Error(err))
		} else {
			h.publisher.PublishComplianceAlertRaised(r.Context(), principalID, *alert)
		}

		h.publisher.PublishVisaExpirationFlagged(r.Context(), principalID, *visa)
	}

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
	if req.CorrelationID == "" {
		http.Error(w, "missing required field: correlation_id", http.StatusBadRequest)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionHoursLog); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Inter-service check: get statutory max allowed weekly hours for jurisdiction
	maxAllowed := 48.0
	if h.jurisdictionRules != nil {
		if limit, err := h.jurisdictionRules.GetWorkingHourLimit(r.Context(), req.LegalEntityID); err == nil && limit > 0 {
			maxAllowed = limit
		}
	}

	// Accumulate weekly hours — fail closed: if the prior-hours lookup
	// fails, we cannot know the employee's real weekly total, so we must
	// not silently treat it as zero (that would under-report a breach).
	prevHours, err := h.store.GetWeeklyHours(r.Context(), req.EmployeeID, req.WorkDate)
	if err != nil {
		h.logger.Error("failed to read prior weekly hours", zap.Error(err))
		h.writeStoreErr(w, err)
		return
	}
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
		CorrelationID:     req.CorrelationID,
	}

	created, err := h.store.LogWorkingHours(r.Context(), logEntry)
	if err != nil {
		h.logger.Error("failed to log working hours", zap.Error(err))
		h.writeStoreErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !created {
		// Replay of a prior request — do not re-raise the breach alert.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(logEntry)
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
		if err := h.store.CreateComplianceAlert(r.Context(), alert); err != nil {
			h.logger.Error("failed to create compliance alert", zap.String("employee_id", req.EmployeeID), zap.Error(err))
		} else {
			h.publisher.PublishComplianceAlertRaised(r.Context(), principalID, *alert)
		}

		h.publisher.PublishWorkingHoursBreach(r.Context(), principalID, *logEntry)
	}

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(logEntry)
}

func (h *Handler) ListAlerts(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	alerts, err := h.store.ListComplianceAlerts(r.Context(), legalEntityID)
	if err != nil {
		h.writeStoreErr(w, err)
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

	alert, err := h.store.GetComplianceAlert(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, alert.LegalEntityID, actionAlertResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ResolveComplianceAlert(r.Context(), id, principalID); err != nil {
		h.writeStoreErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
}
