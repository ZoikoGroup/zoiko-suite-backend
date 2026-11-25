// Chunk 6 — Plans, Pricing & Entitlements HTTP handlers.
package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// retiredCatalogWrite answers the doc7 catalog write routes. They published
// a catalog the moment it was created — no review, no maker-checker — and
// stored prices as floats, both of which ZS-SVC-Q-001 forbids (COM-CTRL-002,
// -004; negative path #46). Prices are now created through the COM-01 price
// book. The routes answer 410 rather than 404 so an old client learns why.
// The read routes stay: existing doc7 subscriptions still reference plans.
func retiredCatalogWrite(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, Problem{Status: http.StatusGone, Code: CodeEndpointRetired,
		Detail: "catalog and plan writes moved to the COM-01 price book: POST /v1/commercial/products and /v1/commercial/price-versions"})
}

// RegisterSubscriptionRoutes now serves only the surviving doc7 reads.
//
// Retired (SPEC_DEVIATIONS.md A3, ZS-SVC-Q-001): /v1/subscriptions,
// /v1/subscription-change-requests, /v1/contract-entitlement-overlays and
// /v1/billing-source-transfers, along with CreateSubscription,
// GetSubscription, ResolveEntitlement, CreateEvaluationProgram,
// RecordUsageEvent, SetSubscriptionStatus, ListStatusEvents,
// PreviewSubscriptionChange, ConfirmSubscriptionChange, CreateOverlay and
// TransferBillingSource. ResolveEntitlement was a second runtime entitlement
// decision path reading legacy plan/overlay limits directly — exactly what
// "COM-03 is the sole runtime entitlement decision service" forbids.
// resolveAccountOrg (used only by the removed handlers) is retired with
// them.
//
// No real caller of any of these paths was found (this repo, the frontend,
// or another service) — see the A3 closure note. The underlying tables
// (commercial_subscriptions, evaluation_programs, contract_entitlement_overlays,
// commercial_usage_meter_events, subscription_change_requests,
// billing_source_transfers, subscription_status_events) and their RLS
// policies are left in place, not dropped: migration 000007's
// reject_legacy_subscription_over_com02 trigger still reads
// commercial_subscriptions to keep an account from holding a live
// subscription in both models at once, and internal/store/rls_test.go still
// exercises the tables' RLS policies through store.Store, which keeps the
// read/write methods (CreateSubscription, GetSubscription, etc.). None of
// that requires the HTTP surface, which is what this closes.
func RegisterSubscriptionRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/price-catalogs", func(r chi.Router) {
		r.Post("/", retiredCatalogWrite)
		r.Get("/{id}", h.GetPriceCatalog)
	})
	r.Route("/v1/plans", func(r chi.Router) {
		r.Post("/", retiredCatalogWrite)
		r.Get("/{id}", h.GetPlan)
		r.Put("/{id}/entitlement-limits", retiredCatalogWrite)
	})
}

func (h *Handler) GetPriceCatalog(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetPriceCatalog(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrPriceCatalogNotFound) {
			writeError(w, http.StatusNotFound, "price catalog not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get price catalog")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetPlan(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			writeError(w, http.StatusNotFound, "plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get plan")
		return
	}
	writeJSON(w, http.StatusOK, p)
}
