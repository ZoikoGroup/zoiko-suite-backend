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
	CreateBankAccount(ctx context.Context, acct *domain.BankAccount) (created bool, err error)
	GetBankAccount(ctx context.Context, bankAccountID string) (*domain.BankAccount, error)
	ListBankAccounts(ctx context.Context, legalEntityID string) ([]domain.BankAccount, error)
	UpdateBankAccountStatus(ctx context.Context, bankAccountID, status string) error
	CreateCashBalance(ctx context.Context, bal *domain.CashBalance) error
	GetLatestCashBalance(ctx context.Context, bankAccountID string) (*domain.CashBalance, error)
	SetLiquidityThreshold(ctx context.Context, threshold *domain.LiquidityThreshold) error
	GetLiquidityThreshold(ctx context.Context, legalEntityID, currencyCode string) (*domain.LiquidityThreshold, error)

	// BNK-09 — see internal/store/bnk09_store.go's own doc comments. The
	// old ExecuteTransfer (two internal cash_balances rows, no external
	// consequence) is removed and replaced wholesale by this real
	// maker-checker flow.
	CreateTreasuryTransfer(ctx context.Context, p domain.CreateTreasuryTransferParams) (*domain.TreasuryTransfer, bool, error)
	GetTreasuryTransfer(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error)
	ApproveTreasuryTransfer(ctx context.Context, p domain.ApproveTreasuryTransferParams) (*domain.TreasuryTransfer, error)
	RejectTreasuryTransfer(ctx context.Context, p domain.RejectTreasuryTransferParams) (*domain.TreasuryTransfer, error)
	MarkTransferSubmitted(ctx context.Context, tenantID, transferID, paymentAttemptID string) (*domain.TreasuryTransfer, error)
	MarkTransferLedgerPosted(ctx context.Context, tenantID, transferID, sourceJournalID string) (*domain.TreasuryTransfer, error)
	MarkTransferIntercompanyPaired(ctx context.Context, tenantID, transferID, intercompanyEntryID string) (*domain.TreasuryTransfer, error)
	MarkTransferCompleted(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error)
	AmendTreasuryTransfer(ctx context.Context, p domain.AmendTreasuryTransferParams) (*domain.TreasuryTransfer, error)
	SubmitTransferForApproval(ctx context.Context, p domain.SubmitTransferForApprovalParams) (*domain.TreasuryTransfer, error)
	CancelBeforeSubmission(ctx context.Context, p domain.CancelBeforeSubmissionParams) (*domain.TreasuryTransfer, error)
	MarkTransferReturned(ctx context.Context, p domain.MarkTransferReturnedParams) (*domain.TreasuryTransfer, error)
	ResolveTreasuryTransfer(ctx context.Context, p domain.ResolveTreasuryTransferParams) (*domain.TreasuryTransfer, error)

	// BNK-10 — see internal/store/bnk10_store.go's own doc comments.
	RecordFXRate(ctx context.Context, p domain.RecordFXRateParams) (*domain.FXRate, error)
	GetLatestFXRate(ctx context.Context, tenantID, currencyPair string) (*domain.FXRate, error)

	// BNK-08 — see internal/store/bnk08_store.go's own doc comments.
	CreateCashPositionSnapshot(ctx context.Context, p domain.CalculateCashPositionParams, calc domain.CashPositionCalculation) (*domain.CashPositionSnapshot, error)
	GetCashPositionSnapshot(ctx context.Context, tenantID, snapshotID string) (*domain.CashPositionSnapshot, error)
	GetLatestCashPosition(ctx context.Context, tenantID, legalEntityID, reportingCurrency string) (*domain.CashPositionSnapshot, error)
	GetCashPositionAsOf(ctx context.Context, tenantID, legalEntityID, reportingCurrency string, asOf time.Time) (*domain.CashPositionSnapshot, error)
	ListCurrencyBreakdown(ctx context.Context, tenantID, legalEntityID string) ([]domain.CashPositionSnapshot, error)
	PublishCashPositionSnapshot(ctx context.Context, p domain.PublishCashPositionParams) (*domain.CashPositionSnapshot, error)
	SupersedeCashPositionSnapshot(ctx context.Context, p domain.SupersedeCashPositionParams) (*domain.CashPositionSnapshot, error)

	// BNK-01 — see internal/store/bnk01_store.go's own doc comments.
	VerifyBankAccountOwnership(ctx context.Context, p domain.VerifyOwnershipParams) (*domain.OwnershipEvidence, error)
	ListOwnershipEvidence(ctx context.Context, tenantID, bankAccountID string) ([]domain.OwnershipEvidence, error)
	IsOwnershipVerified(ctx context.Context, tenantID, bankAccountID string) (bool, error)
	AmendBankAccountMetadata(ctx context.Context, p domain.AmendBankAccountMetadataParams) (*domain.BankAccount, error)
	ChangeOperationalUse(ctx context.Context, p domain.ChangeOperationalUseParams) (*domain.BankAccount, error)
	SuspendBankAccount(ctx context.Context, p domain.SuspendAccountParams) (*domain.BankAccount, error)
	ReactivateBankAccount(ctx context.Context, p domain.ReactivateAccountParams) (*domain.BankAccount, error)
	CloseBankAccount(ctx context.Context, p domain.CloseAccountParams) (*domain.BankAccount, error)
	RotateAccountIdentifierToken(ctx context.Context, p domain.RotateAccountTokenParams) (*domain.BankAccount, error)
	GetBankAccountAsOf(ctx context.Context, tenantID, bankAccountID string, asOf time.Time) (*domain.AccountHistoryEntry, error)
}

// Publisher defines Kafka event publication contract.
type Publisher interface {
	PublishCashPositionUpdated(ctx context.Context, correlationID, legalEntityID, actorID string, balance domain.CashBalance)
	PublishEffectiveCashUpdated(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse)
	PublishLiquidityThresholdBreached(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse)

	// BNK-01 domain events — see internal/events/publisher.go's own doc
	// comments. Previously this service published nothing at all for the
	// bank-account lifecycle.
	PublishBankAccountCreated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountOwnershipVerified(ctx context.Context, correlationID, actorID string, acct domain.BankAccount, evidence domain.OwnershipEvidence)
	PublishBankAccountMetadataAmended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountOperationalUseChanged(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountSuspended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountReactivated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountClosed(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)
	PublishBankAccountTokenRotated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount)

	// BNK-09 events for the new Wave 12 transitions — see this file's own
	// package doc: BNK-09 published nothing at all before this. Only the
	// two new commands' outcomes are wired here; retroactively covering
	// Create/Approve/Reject/Submitted/Completed is a separate, deferred
	// gap, not part of this wave.
	PublishTreasuryTransferReturned(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer)
	PublishTreasuryTransferCancelled(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer)
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

// TransferClients defines BNK-09's outbound calls to
// payment-initiation-adapter-svc, general-ledger-svc and
// intercompany-accounting-svc — see internal/clients/bnk09_clients.go.
type TransferClients interface {
	SubmitTreasuryPayment(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, transferID, payerAccountRef, payeeRef string, amount float64, currency string) (string, error)
	PostTreasuryTransferJournal(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, fiscalPeriod, transferID string, amount float64) (string, error)
	PairTreasuryTransferIntercompany(ctx context.Context, tenantID, principalID, correlationID, sourceLegalEntityID, targetLegalEntityID, sourceJournalID string, amount float64, currencyCode string) (string, error)
}

// BankingConnector is BNK-01's read-only dependency on banking-connector-svc
// for ListConnectionOptions (Wave 10d) — optional (nil is a valid value,
// same convention as bank-reconciliation-svc's own optional banking
// client): a deployment that hasn't wired banking-connector-svc's URL yet
// simply can't serve this one query, not a startup failure.
type BankingConnector interface {
	ListConnectionOptions(ctx context.Context, tenantID, legalEntityID, bankAccountID, correlationID string) ([]domain.ConnectionOption, error)
}

const (
	actionRegisterAccount  = "TREASURY_ACCOUNT_REGISTER"
	actionSetThreshold     = "TREASURY_THRESHOLD_SET"
	actionInitiateTransfer = "TREASURY_TRANSFER_INITIATE"
	actionApproveTransfer  = "TREASURY_TRANSFER_APPROVE"
	actionExecuteTransfer  = "TREASURY_TRANSFER_EXECUTE"
	// actionModifyTransfer gates Amend/SubmitForApproval/CancelBeforeSubmission
	// — the maker-only commands over a not-yet-approved transfer.
	// actionResolveTransfer gates MarkTransferReturned/ResolveTreasuryTransfer
	// — operator-facing, post-submission recovery actions.
	actionModifyTransfer  = "TREASURY_TRANSFER_MODIFY"
	actionResolveTransfer = "TREASURY_TRANSFER_RESOLVE"
	actionViewPositions    = "TREASURY_POSITIONS_VIEW"
	// actionCalculateCashPosition gates Calculate/RefreshCashPosition —
	// the doc's own "cash.position.calculate"-equivalent permission.
	// actionPublishCashPosition gates PublishCashPositionSnapshot only —
	// the doc lists cash.position.publish as a distinct permission from
	// calculate/read.
	actionCalculateCashPosition = "TREASURY_CASH_POSITION_CALCULATE"
	actionPublishCashPosition   = "TREASURY_CASH_POSITION_PUBLISH"
)

type Handler struct {
	store           Store
	publisher       Publisher
	authz           AuthZClient
	clients         Clients
	transferClients TransferClients
	banking         BankingConnector
	log             *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, transferClients TransferClients, banking BankingConnector, log *zap.Logger) *Handler {
	return &Handler{
		store:           store,
		publisher:       publisher,
		authz:           authz,
		clients:         clients,
		transferClients: transferClients,
		banking:         banking,
		log:             log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/treasury", func(r chi.Router) {
		r.Post("/accounts", h.RegisterBankAccount)
		r.Get("/accounts", h.ListBankAccounts)
		r.Get("/accounts/{accountID}", h.GetBankAccountByID)
		r.Get("/positions", h.GetCashPositions)
		r.Post("/thresholds", h.SetLiquidityThreshold)
		r.Get("/effective-cash", h.GetEffectiveCash)
		r.Get("/forecasts", h.GetForecasts)

		// BNK-09 — see internal/handler/bnk09_handler.go.
		r.Post("/transfers", h.CreateTreasuryTransfer)
		r.Get("/transfers/{transferID}", h.GetTreasuryTransfer)
		r.Post("/transfers/{transferID}/approve", h.ApproveTreasuryTransfer)
		r.Post("/transfers/{transferID}/reject", h.RejectTreasuryTransfer)
		r.Post("/transfers/{transferID}/execute", h.ExecuteTreasuryTransfer)
		r.Post("/transfers/{transferID}/amend", h.AmendTreasuryTransfer)
		r.Post("/transfers/{transferID}/submit-for-approval", h.SubmitTransferForApproval)
		r.Post("/transfers/{transferID}/cancel", h.CancelBeforeSubmission)
		r.Post("/transfers/{transferID}/mark-returned", h.MarkTransferReturned)
		r.Post("/transfers/{transferID}/resolve", h.ResolveTreasuryTransfer)

		// BNK-10 — see internal/handler/bnk10_handler.go.
		r.Post("/fx/rates", h.RecordFXRate)
		r.Get("/fx/exposure", h.GetFXExposure)
		r.Post("/fx/scenario", h.RunFXScenario)

		// BNK-01 — see internal/handler/bnk01_handler.go.
		r.Post("/accounts/{accountID}/verify-ownership", h.VerifyBankAccountOwnership)
		r.Get("/accounts/{accountID}/ownership-evidence", h.GetOwnershipEvidence)
		r.Post("/accounts/{accountID}/amend-metadata", h.AmendBankAccountMetadata)
		r.Post("/accounts/{accountID}/change-operational-use", h.ChangeOperationalUse)
		r.Post("/accounts/{accountID}/suspend", h.SuspendBankAccount)
		r.Post("/accounts/{accountID}/reactivate", h.ReactivateBankAccount)
		r.Post("/accounts/{accountID}/close", h.CloseBankAccount)
		r.Post("/accounts/{accountID}/rotate-token", h.RotateAccountIdentifierToken)
		r.Get("/accounts/{accountID}/as-of", h.GetBankAccountAsOf)
		r.Get("/accounts/{accountID}/masked", h.GetBankAccountMasked)
		r.Get("/accounts/{accountID}/available-actions", h.GetAvailableActions)
		r.Get("/accounts/{accountID}/connection-options", h.ListConnectionOptions)

		// BNK-08 — see internal/handler/bnk08_handler.go.
		r.Post("/cash-position/calculate", h.CalculateCashPosition)
		r.Post("/cash-position/refresh", h.RefreshCashPosition)
		r.Post("/cash-position/{snapshotID}/publish", h.PublishCashPositionSnapshot)
		r.Post("/cash-position/{snapshotID}/supersede", h.SupersedeCashPositionSnapshot)
		r.Get("/cash-position", h.GetCashPosition)
		r.Get("/cash-position/as-of", h.GetCashPositionAsOf)
		r.Get("/cash-position/{snapshotID}/freshness", h.GetSourceFreshness)
		r.Get("/cash-position/{snapshotID}/drilldown", h.GetAccountDrilldown)
		r.Get("/cash-position/currency-breakdown", h.GetCurrencyBreakdown)
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
		BankAccountID:           uuid.New().String(),
		TenantID:                tenantID,
		LegalEntityID:           req.LegalEntityID,
		AccountName:             req.AccountName,
		MaskedAccountNumber:     req.MaskedAccountNumber,
		BankIdentifier:          req.BankIdentifier,
		CurrencyCode:            req.CurrencyCode,
		AccountStatus:           "ACTIVE",
		BranchRef:               req.BranchRef,
		Country:                 req.Country,
		AccountType:             req.AccountType,
		RequestedOperationalUse: req.RequestedOperationalUse,
		CorrelationID:           req.CorrelationID,
		CreatedByPrincipalID:    principalID,
	}

	created, err := h.store.CreateBankAccount(r.Context(), acct)
	if err != nil {
		h.log.Error("failed to create bank account", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original account, don't re-initialize its balance trace.
		writeJSON(w, http.StatusOK, acct)
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

	h.publisher.PublishBankAccountCreated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *acct)
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

	// oldestBankBalanceAsOf tracks the LEAST recent as_of_timestamp across
	// every account composed into bankSum — the position as a whole is
	// only as fresh as its stalest contributing account, so a single
	// out-of-date account is enough to flag the composed figure.
	var bankSum float64
	var oldestBankBalanceAsOf time.Time
	var haveBankBalance bool
	for _, acct := range accts {
		if acct.CurrencyCode == currencyCode && acct.AccountStatus == "ACTIVE" {
			bal, err := h.store.GetLatestCashBalance(r.Context(), acct.BankAccountID)
			if err == nil && bal != nil {
				bankSum += bal.AvailableBalance
				if !haveBankBalance || bal.AsOfTimestamp.Before(oldestBankBalanceAsOf) {
					oldestBankBalanceAsOf = bal.AsOfTimestamp
				}
				haveBankBalance = true
			}
		}
	}
	// Flagged, not blocked: unlike AP/obligations being fully unreachable
	// (nothing to show at all, so this handler fails closed), a stale
	// bank balance is real data that's merely old — the doc's rule is
	// "never show it AS CURRENT," which HasStaleComponent/IsStale below
	// satisfies by labeling it, not by withholding the whole response.
	bankBalanceStale := haveBankBalance && time.Since(oldestBankBalanceAsOf) > domain.BankBalanceStalenessThreshold
	if bankBalanceStale {
		h.log.Warn("effective cash: bank balance component is stale",
			zap.String("legal_entity_id", legalEntityID), zap.Time("oldest_as_of", oldestBankBalanceAsOf))
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

	now := time.Now().UTC()
	components := []domain.ComponentFreshness{
		{Component: "current_bank_balance", AsOfTimestamp: oldestBankBalanceAsOf, StalenessThresholdSeconds: int(domain.BankBalanceStalenessThreshold.Seconds()), IsStale: bankBalanceStale},
		// AP/obligations are synchronous live queries — see
		// domain.ComponentFreshness's own doc comment on why they are
		// always as-of-now and never stale.
		{Component: "pending_ap_commitments", AsOfTimestamp: now, StalenessThresholdSeconds: 0, IsStale: false},
		{Component: "payroll_obligations", AsOfTimestamp: now, StalenessThresholdSeconds: 0, IsStale: false},
		{Component: "tax_liabilities", AsOfTimestamp: now, StalenessThresholdSeconds: 0, IsStale: false},
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
		AsOfTimestamp:            now,
		ThresholdDetails:         details,
		Components:               components,
		HasStaleComponent:        bankBalanceStale,
	}

	if details != nil && details.IsBreached {
		h.publisher.PublishLiquidityThresholdBreached(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, resp)
	}

	h.publisher.PublishEffectiveCashUpdated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, resp)

	writeJSON(w, http.StatusOK, resp)
}

// BNK-09 treasury transfer handlers (CreateTreasuryTransfer,
// GetTreasuryTransfer, ApproveTreasuryTransfer, RejectTreasuryTransfer,
// ExecuteTreasuryTransfer) live in internal/handler/bnk09_handler.go —
// see that file for the real maker-checker flow that replaces the old
// InitiateTransfer wholesale.

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
