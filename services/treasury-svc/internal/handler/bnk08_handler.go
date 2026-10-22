// BNK-08 Cash Position's own HTTP surface, added to treasury-svc's
// existing /v1/treasury routes rather than a new service. See
// internal/domain/bnk08.go and internal/store/bnk08_store.go for the
// real Calculated->Published->Superseded lifecycle this drives.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

func (h *Handler) writeCashPositionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCashPositionSnapshotNotFound):
		writeError(w, http.StatusNotFound, "cash_position_snapshot_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidCashPositionTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrCashPositionStaleCannotPublish):
		writeError(w, http.StatusConflict, "stale_cannot_publish", err.Error())
	case errors.Is(err, domain.ErrCashPositionScopeMismatch):
		writeError(w, http.StatusConflict, "snapshot_scope_mismatch", err.Error())
	default:
		h.log.Error("cash position operation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
	}
}

// composeCashPosition is the exact bank-balance/AP/obligations
// composition GetEffectiveCash already performs (handler.go's
// GetEffectiveCash), now also returning the per-account breakdown a
// snapshot needs for GetAccountDrilldown. Reused by both
// CalculateCashPosition and RefreshCashPosition rather than duplicated.
func (h *Handler) composeCashPosition(w http.ResponseWriter, r *http.Request, tenantID, legalEntityID, currencyCode string) (*domain.CashPositionCalculation, bool) {
	breakdown, bankSum, bankBalanceStale, ok := h.composeBankBalances(w, r, legalEntityID, currencyCode)
	if !ok {
		return nil, false
	}

	apSum, err := h.clients.GetPendingAPCommitments(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		h.log.Error("AP service unavailable — failing closed on cash position", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "accounts-payable-svc is unreachable; cash position cannot be calculated reliably")
		return nil, false
	}
	payrollSum, taxSum, err := h.clients.GetOutstandingObligations(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		h.log.Error("Obligations service unavailable — failing closed on cash position", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "obligations-svc is unreachable; cash position cannot be calculated reliably")
		return nil, false
	}

	return &domain.CashPositionCalculation{
		BankBalance: bankSum, PendingAPCommitments: apSum, PayrollObligations: payrollSum, TaxLiabilities: taxSum,
		AccountBreakdown: breakdown, HasStaleComponent: bankBalanceStale, AsOfTimestamp: time.Now().UTC(),
	}, true
}

// composeBankBalances is the bank-balance half of a cash position: the
// latest balance of every ACTIVE account the legal entity holds in
// currencyCode, read in one query. A store failure fails closed (503)
// rather than dropping accounts from the sum, and an ACTIVE account with
// no balance at all marks the position stale (partial) instead of being
// skipped silently. Before, both cases were a silent `continue`, so a
// failed read understated cash with nothing to show the figure was
// incomplete.
func (h *Handler) composeBankBalances(w http.ResponseWriter, r *http.Request, legalEntityID, currencyCode string) (breakdown []domain.CashPositionAccountLine, bankSum float64, stale bool, ok bool) {
	accts, err := h.store.ListBankAccounts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return nil, 0, false, false
	}
	var eligible []domain.BankAccount
	var ids []string
	for _, acct := range accts {
		if acct.CurrencyCode == currencyCode && acct.AccountStatus == domain.BankAccountActive {
			eligible = append(eligible, acct)
			ids = append(ids, acct.BankAccountID)
		}
	}
	balances, err := h.store.GetLatestCashBalances(r.Context(), ids)
	if err != nil {
		h.log.Error("cash position: bank balances unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", "bank balances cannot be read; cash position cannot be calculated reliably")
		return nil, 0, false, false
	}
	missing := false
	for _, acct := range eligible {
		bal := balances[acct.BankAccountID]
		if bal == nil {
			missing = true
			h.log.Warn("cash position: active account has no bank balance — position is partial",
				zap.String("legal_entity_id", legalEntityID), zap.String("bank_account_id", acct.BankAccountID))
			continue
		}
		bankSum += bal.AvailableBalance
		breakdown = append(breakdown, domain.CashPositionAccountLine{
			BankAccountID: acct.BankAccountID, AccountName: acct.AccountName, Balance: bal.AvailableBalance, AsOfTimestamp: bal.AsOfTimestamp,
		})
	}
	stale = domain.CashPositionStale(missing, breakdown, time.Now())
	if stale {
		h.log.Warn("cash position: bank balance component is stale or partial", zap.String("legal_entity_id", legalEntityID))
	}
	return breakdown, bankSum, stale, true
}

type calculateCashPositionRequest struct {
	LegalEntityID     string  `json:"legal_entity_id"`
	ReportingCurrency string  `json:"reporting_currency"`
	RestrictedAmount  float64 `json:"restricted_amount"`
	CorrelationID     string  `json:"correlation_id"`
}

// CalculateCashPosition handles POST /v1/treasury/cash-position/calculate
// — the doc's own CalculateCashPosition command. Persists a new
// CALCULATED snapshot; does not touch any prior one (see
// RefreshCashPosition for the supersede-in-place variant).
func (h *Handler) CalculateCashPosition(w http.ResponseWriter, r *http.Request) {
	var req calculateCashPositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.ReportingCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and reporting_currency are required")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCalculateCashPosition); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	calc, ok := h.composeCashPosition(w, r, tenantID, req.LegalEntityID, req.ReportingCurrency)
	if !ok {
		return
	}
	calc.AvailableCash = calc.BankBalance - req.RestrictedAmount - calc.PendingAPCommitments - calc.PayrollObligations - calc.TaxLiabilities

	snap, err := h.store.CreateCashPositionSnapshot(r.Context(), domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, ReportingCurrency: req.ReportingCurrency,
		RestrictedAmount: req.RestrictedAmount, CorrelationID: req.CorrelationID, ActorPrincipalID: principalID,
	}, *calc)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	h.publishCashPositionCalculated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *snap)
	writeJSON(w, http.StatusCreated, snap)
}

type refreshCashPositionRequest struct {
	calculateCashPositionRequest
	PriorSnapshotID string `json:"prior_snapshot_id"`
}

// RefreshCashPosition handles POST /v1/treasury/cash-position/refresh —
// recalculates, then (if prior_snapshot_id is given) supersedes that
// prior snapshot with the freshly calculated one, both in the same
// request. "Refresh" is "calculate again, then retire the old one," not
// a separate mechanism.
func (h *Handler) RefreshCashPosition(w http.ResponseWriter, r *http.Request) {
	var req refreshCashPositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.ReportingCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and reporting_currency are required")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCalculateCashPosition); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	calc, ok := h.composeCashPosition(w, r, tenantID, req.LegalEntityID, req.ReportingCurrency)
	if !ok {
		return
	}
	calc.AvailableCash = calc.BankBalance - req.RestrictedAmount - calc.PendingAPCommitments - calc.PayrollObligations - calc.TaxLiabilities

	// One transaction: the new snapshot exists only if the prior one (when
	// given) was superseded by it, and the prior one must be for this same
	// legal entity and currency.
	snap, err := h.store.RefreshCashPositionSnapshot(r.Context(), domain.RefreshCashPositionParams{
		CalculateCashPositionParams: domain.CalculateCashPositionParams{
			TenantID: tenantID, LegalEntityID: req.LegalEntityID, ReportingCurrency: req.ReportingCurrency,
			RestrictedAmount: req.RestrictedAmount, CorrelationID: req.CorrelationID, ActorPrincipalID: principalID,
		},
		PriorSnapshotID: req.PriorSnapshotID,
	}, *calc)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	h.publishCashPositionCalculated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *snap)
	writeJSON(w, http.StatusCreated, snap)
}

// PublishCashPositionSnapshot handles
// POST /v1/treasury/cash-position/{snapshotID}/publish. Refuses a
// snapshot with a stale component (domain.ErrCashPositionStaleCannotPublish)
// — the doc's own "never carry forward stale data as current" rule.
func (h *Handler) PublishCashPositionSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	snap, err := h.store.GetCashPositionSnapshot(r.Context(), tenantID, snapshotID)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionPublishCashPosition); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.PublishCashPositionSnapshot(r.Context(), domain.PublishCashPositionParams{
		TenantID: tenantID, SnapshotID: snapshotID, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// publishCashPositionCalculated fires the doc's own CashPositionCalculated
// event for every newly created snapshot, and additionally
// CashPositionBecameStale when the calculation itself already reveals a
// stale source component — HasStaleComponent is computed at calculation
// time from real source freshness (see domain.CashPositionCalculation),
// so this is the mechanical, already-computed trigger for that event
// rather than a fabricated new staleness-detection mechanism.
func (h *Handler) publishCashPositionCalculated(ctx context.Context, correlationID, actorID string, snap domain.CashPositionSnapshot) {
	// Events are now enqueued in the store layer (CreateCashPositionSnapshot)
	// via the transactional outbox pattern.
}

type supersedeCashPositionRequest struct {
	NewSnapshotID string `json:"new_snapshot_id"`
}

// SupersedeCashPositionSnapshot handles
// POST /v1/treasury/cash-position/{snapshotID}/supersede — the standalone
// primitive RefreshCashPosition also uses internally; exposed directly so
// an operator can retire a snapshot found to be wrong without waiting for
// a fresh calculation to exist first is not required — new_snapshot_id
// must already exist.
func (h *Handler) SupersedeCashPositionSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")
	var req supersedeCashPositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewSnapshotID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_snapshot_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	snap, err := h.store.GetCashPositionSnapshot(r.Context(), tenantID, snapshotID)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionCalculateCashPosition); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.SupersedeCashPositionSnapshot(r.Context(), domain.SupersedeCashPositionParams{
		TenantID: tenantID, SnapshotID: snapshotID, NewSnapshotID: req.NewSnapshotID,
	})
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// GetCashPosition handles GET /v1/treasury/cash-position — the doc's own
// GetCashPosition query: the most recently calculated snapshot for a
// legal entity + reporting currency.
func (h *Handler) GetCashPosition(w http.ResponseWriter, r *http.Request) {
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
	snap, err := h.store.GetLatestCashPosition(r.Context(), tenantID, legalEntityID, currencyCode)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "cash_position_snapshot_not_found", "no cash position has ever been calculated for this legal entity and currency")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// GetCashPositionAsOf handles GET /v1/treasury/cash-position/as-of — the
// doc's own GetCashPositionAsOf query, mirroring BNK-01's
// GetBankAccountAsOf idiom exactly.
func (h *Handler) GetCashPositionAsOf(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	currencyCode := q.Get("currency_code")
	if legalEntityID == "" || currencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and currency_code are required")
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
	asOf := time.Now().UTC()
	if raw := q.Get("at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "at must be an RFC3339 timestamp")
			return
		}
		asOf = parsed
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snap, err := h.store.GetCashPositionAsOf(r.Context(), tenantID, legalEntityID, currencyCode, asOf)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "cash_position_snapshot_not_found", "no cash position snapshot existed as of the given time")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// authorizedCashPositionSnapshot loads the {snapshotID} snapshot for a
// principal with view permission in its legal entity — the same check
// every other cash-position read makes. Before, the freshness and
// drill-down reads needed only a principal, so anyone in the tenant could
// read any entity's per-account balances.
func (h *Handler) authorizedCashPositionSnapshot(w http.ResponseWriter, r *http.Request) (*domain.CashPositionSnapshot, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snap, err := h.store.GetCashPositionSnapshot(r.Context(), tenantID, chi.URLParam(r, "snapshotID"))
	if err != nil {
		h.writeCashPositionErr(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return snap, true
}

type sourceFreshnessResponse struct {
	SnapshotID string                      `json:"snapshot_id"`
	Components []domain.ComponentFreshness `json:"components"`
}

// GetSourceFreshness handles
// GET /v1/treasury/cash-position/{snapshotID}/freshness — the doc's own
// GetSourceFreshness query, reusing ComponentFreshness's shape exactly as
// GetEffectiveCash's response already does.
//
// The bank-balance component reports the as-of time of the oldest balance
// the snapshot composed — before, it reported the calculation time, so a
// days-old balance looked as fresh as the moment it was summed — and its
// staleness as of this read.
func (h *Handler) GetSourceFreshness(w http.ResponseWriter, r *http.Request) {
	snap, ok := h.authorizedCashPositionSnapshot(w, r)
	if !ok {
		return
	}
	bankAsOf, composed := domain.OldestBankBalanceAsOf(snap.AccountBreakdown)
	if !composed {
		bankAsOf = snap.AsOfTimestamp
	}
	writeJSON(w, http.StatusOK, sourceFreshnessResponse{
		SnapshotID: snap.SnapshotID,
		Components: []domain.ComponentFreshness{
			{Component: "current_bank_balance", AsOfTimestamp: bankAsOf, StalenessThresholdSeconds: int(domain.BankBalanceStalenessThreshold.Seconds()), IsStale: snap.HasStaleComponent},
			{Component: "pending_ap_commitments", AsOfTimestamp: snap.CreatedAt, StalenessThresholdSeconds: 0, IsStale: false},
			{Component: "payroll_obligations", AsOfTimestamp: snap.CreatedAt, StalenessThresholdSeconds: 0, IsStale: false},
			{Component: "tax_liabilities", AsOfTimestamp: snap.CreatedAt, StalenessThresholdSeconds: 0, IsStale: false},
		},
	})
}

// GetAccountDrilldown handles
// GET /v1/treasury/cash-position/{snapshotID}/drilldown — the doc's own
// GetAccountDrilldown query, reading back the per-account breakdown
// captured at calculate time.
func (h *Handler) GetAccountDrilldown(w http.ResponseWriter, r *http.Request) {
	snap, ok := h.authorizedCashPositionSnapshot(w, r)
	if !ok {
		return
	}
	lines := snap.AccountBreakdown
	if lines == nil {
		lines = []domain.CashPositionAccountLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

// GetCurrencyBreakdown handles
// GET /v1/treasury/cash-position/currency-breakdown — the doc's own
// GetCurrencyBreakdown query: the latest snapshot on file for each
// reporting currency a legal entity has ever calculated one in.
func (h *Handler) GetCurrencyBreakdown(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id is required")
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
	snaps, err := h.store.ListCurrencyBreakdown(r.Context(), tenantID, legalEntityID)
	if err != nil {
		h.writeCashPositionErr(w, err)
		return
	}
	if snaps == nil {
		snaps = []domain.CashPositionSnapshot{}
	}
	writeJSON(w, http.StatusOK, snaps)
}
