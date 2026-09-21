// BNK-10 FX Exposure's HTTP surface — RecordFXRate, GetFXExposure,
// RunFXScenario. See internal/domain/bnk10.go's own doc comment for the
// honest scope: no real market-data feed exists, and RunFXScenario makes
// no outbound call to any payment/transfer-capable service — structurally,
// not just by policy, since this file imports nothing from
// internal/clients/bnk09_clients.go.
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

// ── FXExposureSnapshot (Wave 15) ────────────────────────────────────────────

func (h *Handler) writeFXExposureErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrFXExposureSnapshotNotFound):
		writeError(w, http.StatusNotFound, "fx_exposure_snapshot_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidFXExposureTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrFXExposureStaleCannotPublish):
		writeError(w, http.StatusConflict, "stale_cannot_publish", err.Error())
	default:
		h.log.Error("fx exposure snapshot operation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
	}
}

// resolveExposureRate is the same rate-resolution GetFXExposure already
// does, factored out so CalculateFXExposure can reuse it without
// duplicating the same-currency/no-rate-recorded/staleness logic.
func (h *Handler) resolveExposureRate(r *http.Request, tenantID, exposureCurrency, functionalCurrency string) (rate float64, rateAsOf time.Time, rateVersion string, rateIsStale bool, notRecorded bool, err error) {
	if exposureCurrency == functionalCurrency {
		return 1.0, time.Now().UTC(), "", false, false, nil
	}
	fxRate, ferr := h.store.GetLatestFXRate(r.Context(), tenantID, fxCurrencyPair(exposureCurrency, functionalCurrency))
	if ferr != nil {
		if errors.Is(ferr, domain.ErrFXRateNotFound) {
			return 0, time.Time{}, "", false, true, nil
		}
		return 0, time.Time{}, "", false, false, ferr
	}
	return fxRate.Rate, fxRate.EffectiveAt, fxRate.RateID, time.Since(fxRate.EffectiveAt) > domain.FXRateStalenessThreshold, false, nil
}

type calculateFXExposureRequest struct {
	LegalEntityID      string `json:"legal_entity_id"`
	ExposureCurrency   string `json:"exposure_currency"`
	FunctionalCurrency string `json:"functional_currency"`
	CorrelationID      string `json:"correlation_id"`
}

// buildFXExposureCalculation composes exposure via the exact same
// buildExposure GetFXExposure/RunFXScenario already use, and derives the
// gross/net figures a persisted snapshot needs.
func (h *Handler) buildFXExposureCalculation(r *http.Request, tenantID, legalEntityID, exposureCurrency, functionalCurrency string) (*domain.FXExposureCalculation, error) {
	rate, rateAsOf, rateVersion, rateIsStale, notRecorded, err := h.resolveExposureRate(r, tenantID, exposureCurrency, functionalCurrency)
	if err != nil {
		return nil, err
	}
	if notRecorded {
		return nil, domain.ErrFXRateNotFound
	}
	resp, err := h.buildExposure(r, tenantID, legalEntityID, exposureCurrency, functionalCurrency, rate, rateAsOf, rateIsStale)
	if err != nil {
		return nil, err
	}
	var gross float64
	for _, b := range resp.Buckets {
		if b.FunctionalCurrencyAmount < 0 {
			gross -= b.FunctionalCurrencyAmount
		} else {
			gross += b.FunctionalCurrencyAmount
		}
	}
	return &domain.FXExposureCalculation{
		RateUsed: rate, RateAsOf: rateAsOf, RateVersion: rateVersion, HasStaleComponent: rateIsStale,
		Buckets: resp.Buckets, GrossExposureAmount: gross, NetExposureAmount: resp.TotalFunctionalAmount,
		AsOfTimestamp: resp.AsOfTimestamp,
	}, nil
}

// CalculateFXExposure handles
// POST /v1/treasury/fx/exposure-snapshot/calculate — the doc's own
// CalculateFXExposure command. netting_scope is always this single
// legal entity — see domain.FXExposureSnapshot's own doc comment on why
// no broader netting is invented here.
func (h *Handler) CalculateFXExposure(w http.ResponseWriter, r *http.Request) {
	var req calculateFXExposureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.ExposureCurrency == "" || req.FunctionalCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, exposure_currency and functional_currency are required")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCalculateFXExposure); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	calc, err := h.buildFXExposureCalculation(r, tenantID, req.LegalEntityID, req.ExposureCurrency, req.FunctionalCurrency)
	if err != nil {
		if errors.Is(err, domain.ErrFXRateNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "fx_rate_not_recorded", err.Error())
			return
		}
		h.log.Error("CalculateFXExposure: composition failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "cannot compose FX exposure: AP/AR/obligations data unavailable")
		return
	}
	snap, err := h.store.CreateFXExposureSnapshot(r.Context(), domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, ExposureCurrency: req.ExposureCurrency, FunctionalCurrency: req.FunctionalCurrency,
		NettingScope: "SINGLE_ENTITY:" + req.LegalEntityID, CorrelationID: req.CorrelationID, ActorPrincipalID: principalID,
	}, *calc)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	h.publishFXExposureCalculated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *snap)
	writeJSON(w, http.StatusCreated, snap)
}

type refreshFXExposureRequest struct {
	calculateFXExposureRequest
	PriorSnapshotID string `json:"prior_snapshot_id"`
}

// RefreshFXExposure handles
// POST /v1/treasury/fx/exposure-snapshot/refresh — recalculates and, if
// prior_snapshot_id is given, supersedes it with the new one, same
// "calculate again, retire the old one" shape as BNK-08's
// RefreshCashPosition.
func (h *Handler) RefreshFXExposure(w http.ResponseWriter, r *http.Request) {
	var req refreshFXExposureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.ExposureCurrency == "" || req.FunctionalCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, exposure_currency and functional_currency are required")
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCalculateFXExposure); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	calc, err := h.buildFXExposureCalculation(r, tenantID, req.LegalEntityID, req.ExposureCurrency, req.FunctionalCurrency)
	if err != nil {
		if errors.Is(err, domain.ErrFXRateNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "fx_rate_not_recorded", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "upstream_dependency_unavailable", "cannot compose FX exposure")
		return
	}
	snap, err := h.store.CreateFXExposureSnapshot(r.Context(), domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, ExposureCurrency: req.ExposureCurrency, FunctionalCurrency: req.FunctionalCurrency,
		NettingScope: "SINGLE_ENTITY:" + req.LegalEntityID, CorrelationID: req.CorrelationID, ActorPrincipalID: principalID,
	}, *calc)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	h.publishFXExposureCalculated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *snap)
	if req.PriorSnapshotID != "" {
		if _, err := h.store.SupersedeFXExposureSnapshot(r.Context(), domain.SupersedeFXExposureParams{
			TenantID: tenantID, SnapshotID: req.PriorSnapshotID, NewSnapshotID: snap.SnapshotID,
		}); err != nil {
			h.writeFXExposureErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, snap)
}

// publishFXExposureCalculated fires the doc's own FXExposureCalculated
// event, plus FXExposureBecameStale when the calculation's own
// HasStaleComponent already flags a stale rate — same already-computed
// trigger mapping as BNK-08's publishCashPositionCalculated.
func (h *Handler) publishFXExposureCalculated(ctx context.Context, correlationID, actorID string, snap domain.FXExposureSnapshot) {
	h.publisher.PublishFXExposureCalculated(ctx, correlationID, actorID, snap)
	if snap.HasStaleComponent {
		h.publisher.PublishFXExposureBecameStale(ctx, correlationID, actorID, snap)
	}
}

// PublishFXExposureSnapshot handles
// POST /v1/treasury/fx/exposure-snapshot/{snapshotID}/publish.
func (h *Handler) PublishFXExposureSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	snap, err := h.store.GetFXExposureSnapshot(r.Context(), tenantID, snapshotID)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionPublishFXExposure); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.PublishFXExposureSnapshot(r.Context(), domain.PublishFXExposureParams{
		TenantID: tenantID, SnapshotID: snapshotID, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	h.publisher.PublishFXExposurePublished(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

type supersedeFXExposureRequest struct {
	NewSnapshotID string `json:"new_snapshot_id"`
}

// SupersedeFXExposureSnapshot handles
// POST /v1/treasury/fx/exposure-snapshot/{snapshotID}/supersede.
func (h *Handler) SupersedeFXExposureSnapshot(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")
	var req supersedeFXExposureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewSnapshotID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_snapshot_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	snap, err := h.store.GetFXExposureSnapshot(r.Context(), tenantID, snapshotID)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionCalculateFXExposure); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.SupersedeFXExposureSnapshot(r.Context(), domain.SupersedeFXExposureParams{
		TenantID: tenantID, SnapshotID: snapshotID, NewSnapshotID: req.NewSnapshotID,
	})
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// GetFXExposureSnapshotLatest handles GET /v1/treasury/fx/exposure-snapshot
// — the most recently calculated snapshot for a legal entity + currency
// pair.
func (h *Handler) GetFXExposureSnapshotLatest(w http.ResponseWriter, r *http.Request) {
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
	snap, err := h.store.GetLatestFXExposure(r.Context(), tenantID, legalEntityID, exposureCurrency, functionalCurrency)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "fx_exposure_snapshot_not_found", "no fx exposure has ever been calculated for this legal entity and currency pair")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// GetFXExposureSnapshotAsOf handles
// GET /v1/treasury/fx/exposure-snapshot/as-of.
func (h *Handler) GetFXExposureSnapshotAsOf(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	exposureCurrency := q.Get("exposure_currency")
	functionalCurrency := q.Get("functional_currency")
	if legalEntityID == "" || exposureCurrency == "" || functionalCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, exposure_currency and functional_currency are required")
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
	snap, err := h.store.GetFXExposureAsOf(r.Context(), tenantID, legalEntityID, exposureCurrency, functionalCurrency, asOf)
	if err != nil {
		h.writeFXExposureErr(w, err)
		return
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "fx_exposure_snapshot_not_found", "no fx exposure snapshot existed as of the given time")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
