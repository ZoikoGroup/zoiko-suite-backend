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

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// Store defines persistence contract for treasury service.
type Store interface {
	CreateBankAccount(ctx context.Context, acct *domain.BankAccount) error
	GetBankAccount(ctx context.Context, bankAccountID string) (*domain.BankAccount, error)
	ListBankAccounts(ctx context.Context, legalEntityID string) ([]domain.BankAccount, error)
	UpdateBankAccountStatus(ctx context.Context, bankAccountID, status string) error
	CreateCashBalance(ctx context.Context, bal *domain.CashBalance) error
	GetLatestCashBalance(ctx context.Context, bankAccountID string) (*domain.CashBalance, error)
	SetLiquidityThreshold(ctx context.Context, threshold *domain.LiquidityThreshold) error
	GetLiquidityThreshold(ctx context.Context, legalEntityID, currencyCode string) (*domain.LiquidityThreshold, error)
	ExecuteTransfer(ctx context.Context, srcAcctID, tgtAcctID string, amount float64, currencyCode string, correlationID string) error
}

// Publisher defines Kafka event publication contract.
type Publisher interface {
	PublishCashPositionUpdated(ctx context.Context, correlationID string, balance domain.CashBalance)
	PublishEffectiveCashUpdated(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse)
	PublishLiquidityThresholdBreached(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse)
}

// AuthZClient defines authorization plane contract.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Clients defines third-party internal integration interfaces.
type Clients interface {
	GetPendingAPCommitments(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error)
	GetOutstandingObligations(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, float64, error)
	GetForecastedInflows(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error)
	GetLiquidityForecastData(ctx context.Context, tenantID, legalEntityID, currencyCode string) ([]domain.ExpectedCashFlow, []domain.ExpectedCashFlow, error)
}

const (
	actionRegisterAccount = "TREASURY_ACCOUNT_REGISTER"
	actionSetThreshold    = "TREASURY_THRESHOLD_SET"
	actionInitiateTransfer = "TREASURY_TRANSFER_INITIATE"
	actionViewPositions   = "TREASURY_POSITIONS_VIEW"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   Clients
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		clients:   clients,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/treasury", func(r chi.Router) {
		r.Post("/accounts", h.RegisterBankAccount)
		r.Get("/accounts", h.ListBankAccounts)
		r.Get("/positions", h.GetCashPositions)
		r.Post("/thresholds", h.SetLiquidityThreshold)
		r.Get("/effective-cash", h.GetEffectiveCash)
		r.Get("/forecasts", h.GetForecasts)
		r.Post("/transfers", h.InitiateTransfer)
	})
}

// ── POST /v1/treasury/accounts ──────────────────────────────────────────────────

func (h *Handler) RegisterBankAccount(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterBankAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_tenant", "X-Tenant-Id header is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRegisterAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	acct := &domain.BankAccount{
		BankAccountID:       uuid.New().String(),
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		AccountName:         req.AccountName,
		MaskedAccountNumber: req.MaskedAccountNumber,
		BankIdentifier:      req.BankIdentifier,
		CurrencyCode:        req.CurrencyCode,
		AccountStatus:       "ACTIVE",
	}

	if err := h.store.CreateBankAccount(r.Context(), acct); err != nil {
		h.log.Error("failed to create bank account", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	// Initialize with 0 balance trace
	bal := &domain.CashBalance{
		BalanceID:        uuid.New().String(),
		TenantID:         tenantID,
		BankAccountID:    acct.BankAccountID,
		LedgerBalance:    0.0,
		AvailableBalance: 0.0,
		AsOfTimestamp:    time.Now().UTC(),
		CorrelationID:    r.Header.Get("X-Correlation-ID"),
	}
	_ = h.store.CreateCashBalance(r.Context(), bal)

	writeJSON(w, http.StatusCreated, acct)
}

// ── GET /v1/treasury/accounts ────────────────────────────────────────────────────

func (h *Handler) ListBankAccounts(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionViewPositions); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListBankAccounts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/treasury/positions ───────────────────────────────────────────────────

func (h *Handler) GetCashPositions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	accts, err := h.store.ListBankAccounts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	var out []domain.CashPositionResponse
	for _, acct := range accts {
		bal, err := h.store.GetLatestCashBalance(r.Context(), acct.BankAccountID)
		if err != nil {
			continue
		}
		if bal != nil {
			out = append(out, domain.CashPositionResponse{
				BankAccountID:    acct.BankAccountID,
				AccountName:      acct.AccountName,
				CurrencyCode:     acct.CurrencyCode,
				LedgerBalance:    bal.LedgerBalance,
				AvailableBalance: bal.AvailableBalance,
				AsOfTimestamp:    bal.AsOfTimestamp,
			})
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// ── POST /v1/treasury/thresholds ──────────────────────────────────────────────────

func (h *Handler) SetLiquidityThreshold(w http.ResponseWriter, r *http.Request) {
	var req domain.SetThresholdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSetThreshold); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	threshold := &domain.LiquidityThreshold{
		ThresholdID:            uuid.New().String(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		CurrencyCode:           req.CurrencyCode,
		MinimumRequiredBalance: req.MinimumRequiredBalance,
		EscalationEmail:        req.EscalationEmail,
	}

	if err := h.store.SetLiquidityThreshold(r.Context(), threshold); err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, threshold)
}

// ── GET /v1/treasury/effective-cash ──────────────────────────────────────────────

func (h *Handler) GetEffectiveCash(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	currencyCode := q.Get("currency_code")

	if legalEntityID == "" || currencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and currency_code are required")
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// 1. Current bank balance
	accts, err := h.store.ListBankAccounts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	var bankSum float64
	for _, acct := range accts {
		if acct.CurrencyCode == currencyCode && acct.AccountStatus == "ACTIVE" {
			bal, err := h.store.GetLatestCashBalance(r.Context(), acct.BankAccountID)
			if err == nil && bal != nil {
				bankSum += bal.AvailableBalance
			}
		}
	}

	// 2. Pending AP Commitments — fail closed: if AP is unavailable the figure is
	// unreliable; return an error rather than presenting a partial cash position
	// as authoritative (consistent with evidence-manifest-svc doctrine).
	apSum, err := h.clients.GetPendingAPCommitments(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		h.log.Error("AP service unavailable — failing closed on effective cash", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable",
			"accounts-payable-svc is unreachable; effective cash figure cannot be computed reliably")
		return
	}

	// 3. Obligations (Tax, payroll, etc) — same fail-closed doctrine.
	payrollSum, taxSum, err := h.clients.GetOutstandingObligations(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		h.log.Error("Obligations service unavailable — failing closed on effective cash", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable",
			"obligations-svc is unreachable; effective cash figure cannot be computed reliably")
		return
	}

	effectiveCash := bankSum - apSum - payrollSum - taxSum

	// 4. Threshold check
	threshold, err := h.store.GetLiquidityThreshold(r.Context(), legalEntityID, currencyCode)
	var details *domain.ThresholdAlertDetail
	if err == nil && threshold != nil {
		details = &domain.ThresholdAlertDetail{
			MinimumRequiredBalance: threshold.MinimumRequiredBalance,
			IsBreached:             effectiveCash < threshold.MinimumRequiredBalance,
		}
	}

	resp := domain.EffectiveCashResponse{
		TenantID:                 tenantID,
		LegalEntityID:            legalEntityID,
		CurrencyCode:             currencyCode,
		CurrentBankBalance:       bankSum,
		PendingAPCommitments:     apSum,
		PayrollObligations:       payrollSum,
		TaxLiabilities:           taxSum,
		ReservedPendingApprovals: 0.0,
		EffectiveAvailableCash:   effectiveCash,
		AsOfTimestamp:            time.Now().UTC(),
		ThresholdDetails:         details,
	}

	if details != nil && details.IsBreached {
		h.publisher.PublishLiquidityThresholdBreached(r.Context(), r.Header.Get("X-Correlation-ID"), resp)
	}

	h.publisher.PublishEffectiveCashUpdated(r.Context(), r.Header.Get("X-Correlation-ID"), resp)

	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/treasury/transfers ──────────────────────────────────────────────────

func (h *Handler) InitiateTransfer(w http.ResponseWriter, r *http.Request) {
	var req domain.InitiateTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// BLOCKING FIX #1: Reject non-positive amounts before any account or balance
	// lookup. A negative amount reverses the transfer direction — draining the
	// target and crediting the source — bypassing the insufficient-funds check
	// entirely (srcBal < negativeAmount is trivially false). Zero-amount transfers
	// are a no-op that must not produce balance records.
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAmount))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	srcAcct, err := h.store.GetBankAccount(r.Context(), req.SourceBankAccountID)
	if err != nil || srcAcct == nil {
		writeError(w, http.StatusNotFound, "source_account_not_found", "")
		return
	}

	// Authorize against the source account's legal entity.
	if err := h.authz.CheckAllowed(r.Context(), principalID, srcAcct.LegalEntityID, actionInitiateTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// FIX #3: Also authorize against the TARGET account's legal entity.
	// A principal authorized over entity A must not be able to move funds into
	// (or out of, via negative amounts) an entity B account without authorization.
	tgtAcct, err := h.store.GetBankAccount(r.Context(), req.TargetBankAccountID)
	if err != nil || tgtAcct == nil {
		writeError(w, http.StatusNotFound, "target_account_not_found", "")
		return
	}
	if tgtAcct.LegalEntityID != srcAcct.LegalEntityID {
		if err := h.authz.CheckAllowed(r.Context(), principalID, tgtAcct.LegalEntityID, actionInitiateTransfer); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	// FIX #2: Fail CLOSED on liquidity threshold errors (not open).
	// Every other cross-service check in this codebase fails closed on store error.
	// If we cannot read the balance or threshold, block the transfer — do not silently
	// skip the check and allow the transfer to proceed unguarded.
	bal, balErr := h.store.GetLatestCashBalance(r.Context(), req.SourceBankAccountID)
	if balErr != nil {
		h.log.Error("failed to read cash balance for threshold check — failing closed", zap.Error(balErr))
		writeError(w, http.StatusServiceUnavailable, "balance_check_failed", "cannot verify balance before transfer")
		return
	}
	if bal != nil {
		threshold, threshErr := h.store.GetLiquidityThreshold(r.Context(), srcAcct.LegalEntityID, req.CurrencyCode)
		if threshErr != nil {
			h.log.Error("failed to read liquidity threshold — failing closed", zap.Error(threshErr))
			writeError(w, http.StatusServiceUnavailable, "threshold_check_failed", "cannot verify liquidity threshold before transfer")
			return
		}
		if threshold != nil && bal.AvailableBalance-req.Amount < threshold.MinimumRequiredBalance {
			h.log.Warn("transfer blocked: threshold breach on source account", zap.String("account_id", req.SourceBankAccountID))
			writeError(w, http.StatusPreconditionFailed, "minimum_balance_breach", string(domain.ErrMinimumBalanceBreach))
			return
		}
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	if req.CorrelationID != "" {
		correlationID = req.CorrelationID
	}

	if err := h.store.ExecuteTransfer(r.Context(), req.SourceBankAccountID, req.TargetBankAccountID, req.Amount, req.CurrencyCode, correlationID); err != nil {
		h.log.Error("transfer execution failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "transfer_failed", err.Error())
		return
	}

	// Publish updated balances
	if balSrc, err := h.store.GetLatestCashBalance(r.Context(), req.SourceBankAccountID); err == nil && balSrc != nil {
		h.publisher.PublishCashPositionUpdated(r.Context(), correlationID, *balSrc)
	}
	if balTgt, err := h.store.GetLatestCashBalance(r.Context(), req.TargetBankAccountID); err == nil && balTgt != nil {
		h.publisher.PublishCashPositionUpdated(r.Context(), correlationID, *balTgt)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "transferred", "correlation_id": correlationID})
}

// ── GET /v1/treasury/forecasts ───────────────────────────────────────────────────

func (h *Handler) GetForecasts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	currencyCode := q.Get("currency_code")

	if legalEntityID == "" || currencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and currency_code are required")
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// 1. Get current aggregate cash balance
	accts, err := h.store.ListBankAccounts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}

	var currentCash float64
	for _, acct := range accts {
		if acct.CurrencyCode == currencyCode && acct.AccountStatus == "ACTIVE" {
			bal, err := h.store.GetLatestCashBalance(r.Context(), acct.BankAccountID)
			if err == nil && bal != nil {
				currentCash += bal.AvailableBalance
			}
		}
	}

	// 2. Fetch forecast data from clients
	inflows, outflows, err := h.clients.GetLiquidityForecastData(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "forecast_failed", err.Error())
		return
	}

	now := time.Now().UTC()
	t7 := now.AddDate(0, 0, 7)
	t30 := now.AddDate(0, 0, 30)
	t90 := now.AddDate(0, 0, 90)

	var in7, out7 float64
	var in30, out30 float64
	var in90, out90 float64

	for _, flow := range inflows {
		if flow.DueDate.Before(t7) || flow.DueDate.Equal(t7) {
			in7 += flow.Amount
		}
		if flow.DueDate.Before(t30) || flow.DueDate.Equal(t30) {
			in30 += flow.Amount
		}
		if flow.DueDate.Before(t90) || flow.DueDate.Equal(t90) {
			in90 += flow.Amount
		}
	}

	for _, flow := range outflows {
		if flow.DueDate.Before(t7) || flow.DueDate.Equal(t7) {
			out7 += flow.Amount
		}
		if flow.DueDate.Before(t30) || flow.DueDate.Equal(t30) {
			out30 += flow.Amount
		}
		if flow.DueDate.Before(t90) || flow.DueDate.Equal(t90) {
			out90 += flow.Amount
		}
	}

	resp := domain.LiquidityForecastResponse{
		TenantID:           tenantID,
		LegalEntityID:      legalEntityID,
		CurrencyCode:       currencyCode,
		CurrentCashBalance: currentCash,
		AsOfTimestamp:      now,
		Forecast7Day: domain.ForecastIntervalDetail{
			IntervalDays:      7,
			ExpectedInflows:   in7,
			ExpectedOutflows:  out7,
			ForecastedBalance: currentCash + in7 - out7,
		},
		Forecast30Day: domain.ForecastIntervalDetail{
			IntervalDays:      30,
			ExpectedInflows:   in30,
			ExpectedOutflows:  out30,
			ForecastedBalance: currentCash + in30 - out30,
		},
		Forecast90Day: domain.ForecastIntervalDetail{
			IntervalDays:      90,
			ExpectedInflows:   in90,
			ExpectedOutflows:  out90,
			ForecastedBalance: currentCash + in90 - out90,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── Private Helpers ─────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	} else {
		writeError(w, http.StatusForbidden, "authz_denied", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{
		"error":   code,
		"message": msg,
	})
}
