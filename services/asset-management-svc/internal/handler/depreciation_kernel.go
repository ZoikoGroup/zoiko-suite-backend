package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
)

// kernelDepreciationPoster is the part of the ledger client that posts depreciation through general-ledger-svc's accounting kernel. A
// client without it, or with it switched off, posts through the legacy command exactly as before.
type kernelDepreciationPoster interface {
	KernelPostingEnabled() bool
	PostDepreciationKernelEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, runID, assetCategory, amount, correlationID string) (string, error)
}

// kernelDepreciationStore is what recording a kernel-posted run needs from the store.
type kernelDepreciationStore interface {
	AssetCategories(ctx context.Context, assetIDs []string) (map[string]string, error)
	MarkDepreciationRunEmittedToKernel(ctx context.Context, runID string, journalIDs []string, at time.Time) error
}

// emitDepreciationThroughKernel posts the run's depreciation as one accounting event per asset category, so the kernel can reconcile the
// register to the ledger by class. The accounts are the tenant's depreciation mappings in the ledger: the account codes stored on the run
// are not used on this path. Each category is idempotent on the run, so a retry after a partial failure posts only what is missing.
func (h *Handler) emitDepreciationThroughKernel(w http.ResponseWriter, r *http.Request, kp kernelDepreciationPoster, run *domain.DepreciationRun, runID, tenantID, principalID string) {
	st, ok := h.store.(kernelDepreciationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "kernel_posting_not_supported", "the store cannot record a run posted through the kernel")
		return
	}
	ids := make([]string, 0, len(run.Lines))
	for _, l := range run.Lines {
		ids = append(ids, l.AssetID)
	}
	categories, err := st.AssetCategories(r.Context(), ids)
	if err != nil {
		h.log.Error("EmitDepreciationAccountingEvent: asset categories unavailable", zap.String("run_id", runID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	cents := map[string]int64{}
	for _, l := range run.Lines {
		if l.PeriodAmount == 0 {
			continue
		}
		cat := categories[l.AssetID]
		if cat == "" {
			writeError(w, http.StatusUnprocessableEntity, "asset_category_missing", "asset "+l.AssetID+" has no category, which the ledger reconciles the register by")
			return
		}
		c := int64(math.Round(l.PeriodAmount * 100))
		if c < 0 {
			writeError(w, http.StatusUnprocessableEntity, "negative_depreciation", "asset "+l.AssetID+" has a negative period amount")
			return
		}
		cents[cat] += c
	}
	cats := make([]string, 0, len(cents))
	for cat, c := range cents {
		if c > 0 {
			cats = append(cats, cat)
		}
	}
	if len(cats) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_depreciation_to_emit", "every line in this run's frozen population depreciated to zero")
		return
	}
	sort.Strings(cats)
	correlationID := getCorrelationID(r)
	journalIDs := make([]string, 0, len(cats))
	for _, cat := range cats {
		amount := fmt.Sprintf("%d.%02d", cents[cat]/100, cents[cat]%100)
		journalID, err := kp.PostDepreciationKernelEvent(r.Context(), tenantID, principalID, run.LegalEntityID, run.FiscalPeriod, runID, cat, amount, correlationID)
		if err != nil {
			h.log.Error("EmitDepreciationAccountingEvent: kernel posting failed", zap.String("run_id", runID), zap.String("asset_category", cat), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
			return
		}
		journalIDs = append(journalIDs, journalID)
	}
	now := time.Now().UTC()
	if err := st.MarkDepreciationRunEmittedToKernel(r.Context(), runID, journalIDs, now); err != nil {
		h.log.Error("depreciation run posted but could not be marked ACCOUNTING_EVENT_EMITTED", zap.String("run_id", runID), zap.Strings("journal_ids", journalIDs), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded", fmt.Sprintf("the journals ARE posted (%v), but the run could not be marked ACCOUNTING_EVENT_EMITTED.", journalIDs))
		return
	}
	h.publisher.PublishDepreciationRunAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, run.LegalEntityID, runID, journalIDs[0])
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": domain.DepreciationRunStatusAccountingEventEmitted, "journal_id": journalIDs[0], "ledger_journal_ids": journalIDs})
}
