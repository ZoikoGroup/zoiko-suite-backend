// BNK-10 FX Exposure's HTTP surface — RecordFXRate, GetFXExposure,
// RunFXScenario. See internal/domain/bnk10.go's own doc comment for the
// honest scope: no real market-data feed exists, and RunFXScenario makes
// no outbound call to any payment/transfer-capable service — structurally,
// not just by policy, since this file imports nothing from
// internal/clients/bnk09_clients.go.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

const actionRecordFXRate = "TREASURY_FX_RATE_RECORD"

func fxCurrencyPair(exposureCurrency, functionalCurrency string) string {
	return exposureCurrency + "/" + functionalCurrency
}

// ── POST /v1/treasury/fx/rates ────────────────────────────────────────────

type recordFXRateRequest struct {
	CurrencyPair string    `json:"currency_pair"`
	Rate         float64   `json:"rate"`
	EffectiveAt  time.Time `json:"effective_at"`
}

func (h *Handler) RecordFXRate(w http.ResponseWriter, r *http.Request) {
	var req recordFXRateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.CurrencyPair == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "currency_pair")
		return
	}
	if req.Rate <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_rate", string(domain.ErrInvalidFXRate))
		return
	}
	if req.EffectiveAt.IsZero() {
		req.EffectiveAt = time.Now().UTC()
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Platform-level action (no legal_entity_id): an FX rate isn't scoped
	// to one legal entity, it's a market observation shared across every
	// entity that reports in that currency pair.
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", actionRecordFXRate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	rate, err := h.store.RecordFXRate(r.Context(), domain.RecordFXRateParams{
		TenantID: tenantID, CurrencyPair: req.CurrencyPair, Rate: req.Rate,
		EffectiveAt: req.EffectiveAt, RecordedByPrincipalID: principalID,
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rate)
}

// buildExposure composes AR/AP/obligations for legalEntityID (reusing the
// exact same client calls EffectiveCashResponse already makes), buckets
// each flow into a maturity window by days-to-due, and converts to the
// functional currency at the given rate. Shared by GetFXExposure (the
// real recorded rate) and RunFXScenario (a caller-supplied hypothetical
// one) so both use identical bucketing logic.
func (h *Handler) buildExposure(r *http.Request, tenantID, legalEntityID, exposureCurrency, functionalCurrency string, rate float64, rateAsOf time.Time, rateIsStale bool) (*domain.FXExposureResponse, error) {
	inflows, outflows, err := h.clients.GetLiquidityForecastData(r.Context(), tenantID, legalEntityID, exposureCurrency)
	if err != nil {
		return nil, err
	}

	type bucketKey struct{ bucket, category string }
	sums := map[bucketKey]float64{}
	bucketOf := func(due time.Time) string {
		days := int(time.Until(due).Hours() / 24)
		switch {
		case days <= 30:
			return "0-30D"
		case days <= 90:
			return "31-90D"
		case days <= 180:
			return "91-180D"
		case days <= 365:
			return "181-365D"
		default:
			return "365D+"
		}
	}
	for _, f := range inflows {
		sums[bucketKey{bucketOf(f.DueDate), f.Category}] += f.Amount
	}
	for _, f := range outflows {
		sums[bucketKey{bucketOf(f.DueDate), f.Category}] -= f.Amount
	}

	resp := &domain.FXExposureResponse{
		TenantID: tenantID, LegalEntityID: legalEntityID,
		ExposureCurrency: exposureCurrency, FunctionalCurrency: functionalCurrency,
		RateUsed: rate, RateAsOf: rateAsOf, RateIsStale: rateIsStale,
		AsOfTimestamp: time.Now().UTC(),
	}
	for k, amount := range sums {
		functionalAmount := amount * rate
		resp.Buckets = append(resp.Buckets, domain.FXExposureBucket{
			MaturityBucket: k.bucket, Category: k.category,
			ExposureCurrencyAmount: amount, FunctionalCurrencyAmount: functionalAmount,
		})
		resp.TotalExposureAmount += amount
		resp.TotalFunctionalAmount += functionalAmount
	}
	return resp, nil
}

// ── GET /v1/treasury/fx/exposure ──────────────────────────────────────────

func (h *Handler) GetFXExposure(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	exposureCurrency := q.Get("exposure_currency")
	functionalCurrency := q.Get("functional_currency")
	if legalEntityID == "" || exposureCurrency == "" || functionalCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, exposure_currency and functional_currency are required")
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

	rate := 1.0
	var rateAsOf time.Time
	var rateIsStale bool
	if exposureCurrency != functionalCurrency {
		fxRate, err := h.store.GetLatestFXRate(r.Context(), tenantID, fxCurrencyPair(exposureCurrency, functionalCurrency))
		if err != nil {
			if errors.Is(err, domain.ErrFXRateNotFound) {
				writeError(w, http.StatusUnprocessableEntity, "fx_rate_not_recorded", err.Error())
				return
			}
			writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
			return
		}
		rate = fxRate.Rate
		rateAsOf = fxRate.EffectiveAt
		rateIsStale = time.Since(rateAsOf) > domain.FXRateStalenessThreshold
	} else {
		rateAsOf = time.Now().UTC()
	}

	resp, err := h.buildExposure(r, tenantID, legalEntityID, exposureCurrency, functionalCurrency, rate, rateAsOf, rateIsStale)
	if err != nil {
		h.log.Error("GetFXExposure: composition failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "cannot compose FX exposure: AP/AR/obligations data unavailable")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/treasury/fx/scenario ─────────────────────────────────────────
//
// Pure computation: reuses buildExposure with the caller-supplied
// hypothetical rate, and separately with the real latest recorded rate for
// comparison. Neither path calls anything beyond the existing AP/AR/
// obligations clients — no payment, ledger or intercompany client is
// imported by this file, so this handler cannot move money even if asked
// to; that is a structural property of the package, not a runtime check.
func (h *Handler) RunFXScenario(w http.ResponseWriter, r *http.Request) {
	var req domain.RunFXScenarioRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.ExposureCurrency == "" || req.FunctionalCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, exposure_currency and functional_currency are required")
		return
	}
	if req.HypotheticalRate <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_rate", string(domain.ErrInvalidFXRate))
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	currentRate := 1.0
	var currentRateAsOf time.Time
	var currentRateIsStale bool
	if req.ExposureCurrency != req.FunctionalCurrency {
		fxRate, err := h.store.GetLatestFXRate(r.Context(), tenantID, fxCurrencyPair(req.ExposureCurrency, req.FunctionalCurrency))
		if err != nil && !errors.Is(err, domain.ErrFXRateNotFound) {
			writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
			return
		}
		if fxRate != nil {
			currentRate = fxRate.Rate
			currentRateAsOf = fxRate.EffectiveAt
			currentRateIsStale = time.Since(currentRateAsOf) > domain.FXRateStalenessThreshold
		}
	} else {
		currentRateAsOf = time.Now().UTC()
	}

	current, err := h.buildExposure(r, tenantID, req.LegalEntityID, req.ExposureCurrency, req.FunctionalCurrency, currentRate, currentRateAsOf, currentRateIsStale)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "cannot compose current FX exposure")
		return
	}
	scenario, err := h.buildExposure(r, tenantID, req.LegalEntityID, req.ExposureCurrency, req.FunctionalCurrency, req.HypotheticalRate, time.Now().UTC(), false)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "cannot compose scenario FX exposure")
		return
	}

	writeJSON(w, http.StatusOK, domain.FXScenarioResponse{
		Current: *current, Scenario: *scenario,
		FunctionalAmountDelta: scenario.TotalFunctionalAmount - current.TotalFunctionalAmount,
	})
}
