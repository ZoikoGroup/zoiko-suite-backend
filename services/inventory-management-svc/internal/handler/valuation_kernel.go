package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

// kernelInventoryPoster is the part of the ledger client that posts inventory valuation through general-ledger-svc's accounting kernel. A
// client without it, or with it switched off, posts through the legacy command exactly as before.
type kernelInventoryPoster interface {
	KernelPostingEnabled() bool
	PostInventoryKernelEvent(ctx context.Context, tenantID, principalID, legalEntityID, correlationID string, e clients.KernelEvent) (string, error)
}

// kernelValuationStore is what recording a kernel-posted valuation run needs from the store.
type kernelValuationStore interface {
	MarkValuationRunEmittedToKernel(ctx context.Context, runID, principalID string, journalIDs []string, at time.Time) error
	RunInboundByStock(ctx context.Context, runID string) ([]domain.RunInboundLine, error)
}

func (h *Handler) kernelPoster() (kernelInventoryPoster, bool) {
	kp, ok := h.ledger.(kernelInventoryPoster)
	return kp, ok && kp.KernelPostingEnabled()
}

func money(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

// stockDims are the dimensions the kernel reconciles stock by: the code of the location and the type of the item. Neither may be empty:
// stock that cannot be placed in a location and a class cannot be reconciled to the ledger by either.
func (h *Handler) stockDims(ctx context.Context, itemID, locationID string) (map[string]string, error) {
	item, err := h.store.GetItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	loc, err := h.store.GetLocation(ctx, locationID)
	if err != nil {
		return nil, err
	}
	if item.ItemType == "" {
		return nil, fmt.Errorf("item %s has no item_type, which the ledger reconciles stock by", itemID)
	}
	if loc.LocationCode == "" {
		return nil, fmt.Errorf("location %s has no code", locationID)
	}
	return map[string]string{"location": loc.LocationCode, "item_class": item.ItemType}, nil
}

// postWriteDownKernel posts a write-down as an INVENTORY.NRV_WRITE_DOWN event.
func (h *Handler) postWriteDownKernel(ctx context.Context, kp kernelInventoryPoster, tenantID, principalID, correlationID string, w *domain.WriteDown, fiscalPeriod string) (string, error) {
	dims, err := h.stockDims(ctx, w.ItemID, w.LocationID)
	if err != nil {
		return "", err
	}
	return kp.PostInventoryKernelEvent(ctx, tenantID, principalID, w.LegalEntityID, correlationID, clients.KernelEvent{
		EventType: "INVENTORY.NRV_WRITE_DOWN", SourceKey: "inv-wd:" + w.WriteDownID, FiscalPeriod: fiscalPeriod, Dimensions: dims,
		Facts: map[string]string{"amount": money(toCents(w.Amount)), "write_down_ref": w.WriteDownID, "valuation_evidence_ref": w.ValuationEvidenceRef},
	})
}

// postLandedCostKernel posts a landed-cost allocation as an INVENTORY.LANDED_COST_ALLOCATED event: the part on stock still held, the part
// on goods already sold, and the offset.
func (h *Handler) postLandedCostKernel(ctx context.Context, kp kernelInventoryPoster, tenantID, principalID, correlationID string, a *domain.LandedCostAllocation) (string, error) {
	dims, err := h.stockDims(ctx, a.ItemID, a.LocationID)
	if err != nil {
		return "", err
	}
	return kp.PostInventoryKernelEvent(ctx, tenantID, principalID, a.LegalEntityID, correlationID, clients.KernelEvent{
		EventType: "INVENTORY.LANDED_COST_ALLOCATED", SourceKey: "inv-lc:" + a.AllocationID, FiscalPeriod: a.FiscalPeriod, Dimensions: dims,
		Facts: map[string]string{"amount": money(toCents(a.Amount)), "inventory_share": money(toCents(a.InventoryShare)), "cogs_share": money(toCents(a.COGSShare)),
			"allocation_ref": a.AllocationID, "valuation_evidence_ref": a.ValuationEvidenceRef},
	})
}

type stockKey struct{ location, class string }

// emitValuationRunThroughKernel posts a valuation run as accounting events by location and item type: the cost of sales of the run's
// OUTBOUND entries and the reclass of the receipt-linked INBOUND value already expensed through goods-received accruals. The accounts are
// the tenant's mappings in the ledger: the account codes stored on the run are not used on this path. Every event is idempotent on the
// run, location and item type, so a retry after a partial failure posts only what is missing.
func (h *Handler) emitValuationRunThroughKernel(w http.ResponseWriter, r *http.Request, kp kernelInventoryPoster, run *domain.ValuationRun, runID, tenantID, principalID string) {
	st, ok := h.store.(kernelValuationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "kernel_posting_not_supported", "the store cannot record a run posted through the kernel")
		return
	}
	dimsCache := map[[2]string]map[string]string{}
	dimsFor := func(item, loc string) (map[string]string, error) {
		k := [2]string{item, loc}
		if d, ok := dimsCache[k]; ok {
			return d, nil
		}
		d, err := h.stockDims(r.Context(), item, loc)
		if err == nil {
			dimsCache[k] = d
		}
		return d, err
	}
	fail := func(err error) {
		h.log.Error("EmitInventoryAccountingEvent: kernel posting failed", zap.String("run_id", runID), zap.Error(err))
		writeError(w, http.StatusUnprocessableEntity, "kernel_posting_refused", err.Error())
	}

	cogs := map[stockKey]int64{}
	methods := map[stockKey]map[string]bool{}
	for _, e := range run.Entries {
		if e.EntryType != domain.ValuationEntryTypeOutbound || e.Value == 0 {
			continue
		}
		d, err := dimsFor(e.ItemID, e.LocationID)
		if err != nil {
			fail(err)
			return
		}
		k := stockKey{d["location"], d["item_class"]}
		cogs[k] += toCents(e.Value)
		if methods[k] == nil {
			methods[k] = map[string]bool{}
		}
		methods[k][e.ValuationMethod] = true
	}
	inbound, err := st.RunInboundByStock(r.Context(), runID)
	if err != nil {
		h.log.Error("EmitInventoryAccountingEvent: could not split inbound value", zap.String("run_id", runID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	reclass := map[stockKey]int64{}
	for _, l := range inbound {
		if l.ReceiptLinked == 0 {
			continue
		}
		d, err := dimsFor(l.ItemID, l.LocationID)
		if err != nil {
			fail(err)
			return
		}
		reclass[stockKey{d["location"], d["item_class"]}] += toCents(l.ReceiptLinked)
	}
	for _, m := range []map[stockKey]int64{cogs, reclass} {
		for k, c := range m {
			if c < 0 {
				fail(fmt.Errorf("the run has a negative value at %s/%s", k.location, k.class))
				return
			}
		}
	}

	keys := func(m map[stockKey]int64) []stockKey {
		out := make([]stockKey, 0, len(m))
		for k, c := range m {
			if c > 0 {
				out = append(out, k)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].location != out[j].location {
				return out[i].location < out[j].location
			}
			return out[i].class < out[j].class
		})
		return out
	}
	if len(keys(cogs)) == 0 && len(keys(reclass)) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_valuation_to_emit", "this run's frozen population has no OUTBOUND (COGS) value and no AP-receipt-linked INBOUND value to post")
		return
	}
	correlationID := getCorrelationID(r)
	var journalIDs []string
	post := func(eventType, sourceKey string, k stockKey, facts map[string]string) bool {
		id, err := kp.PostInventoryKernelEvent(r.Context(), tenantID, principalID, run.LegalEntityID, correlationID, clients.KernelEvent{
			EventType: eventType, SourceKey: sourceKey, FiscalPeriod: run.FiscalPeriod, Facts: facts, Dimensions: map[string]string{"location": k.location, "item_class": k.class}})
		if err != nil {
			h.log.Error("EmitInventoryAccountingEvent: kernel posting failed", zap.String("run_id", runID), zap.String("event_type", eventType), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
			return false
		}
		journalIDs = append(journalIDs, id)
		return true
	}
	for _, k := range keys(cogs) {
		ms := make([]string, 0, len(methods[k]))
		for m := range methods[k] {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		if !post("INVENTORY.COGS_RECOGNIZED", "inv-run:"+runID+":cogs:"+k.location+":"+k.class, k,
			map[string]string{"amount": money(cogs[k]), "run_ref": runID, "valuation_policy": strings.Join(ms, "+")}) {
			return
		}
	}
	for _, k := range keys(reclass) {
		if !post("INVENTORY.GRNI_EXPENSE_RECLASSIFIED", "inv-run:"+runID+":grni:"+k.location+":"+k.class, k,
			map[string]string{"amount": money(reclass[k]), "run_ref": runID, "receipt_basis": "ap-linked"}) {
			return
		}
	}
	now := time.Now().UTC()
	if err := st.MarkValuationRunEmittedToKernel(r.Context(), runID, principalID, journalIDs, now); err != nil {
		h.log.Error("valuation run posted but could not be marked ACCOUNTING_EVENT_EMITTED", zap.String("run_id", runID), zap.Strings("journal_ids", journalIDs), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded", fmt.Sprintf("the journals ARE posted (%v), but the run could not be marked ACCOUNTING_EVENT_EMITTED.", journalIDs))
		return
	}
	h.publisher.PublishInventoryAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, run.LegalEntityID, runID, journalIDs[0])
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": domain.ValuationRunStatusAccountingEventEmitted, "journal_id": journalIDs[0], "ledger_journal_ids": journalIDs})
}

// postLandedCostJournal posts a landed-cost allocation through the kernel when it is switched on, else as the legacy journal of account codes.
func (h *Handler) postLandedCostJournal(ctx context.Context, tenantID, principalID, correlationID string, a *domain.LandedCostAllocation, lines []clients.LedgerLine) (string, error) {
	if kp, ok := h.kernelPoster(); ok {
		return h.postLandedCostKernel(ctx, kp, tenantID, principalID, correlationID, a)
	}
	return h.ledger.PostInventoryAccountingEvent(ctx, tenantID, principalID, a.LegalEntityID, a.FiscalPeriod, "Inventory landed cost "+a.AllocationID, a.AllocationID, correlationID, lines)
}

// postWriteDownJournal posts a write-down through the kernel when it is switched on, else as the legacy journal of account codes.
func (h *Handler) postWriteDownJournal(ctx context.Context, tenantID, principalID, correlationID string, w *domain.WriteDown, fiscalPeriod string, lines []clients.LedgerLine) (string, error) {
	if kp, ok := h.kernelPoster(); ok {
		return h.postWriteDownKernel(ctx, kp, tenantID, principalID, correlationID, w, fiscalPeriod)
	}
	return h.ledger.PostInventoryAccountingEvent(ctx, tenantID, principalID, w.LegalEntityID, fiscalPeriod, "Inventory write-down "+w.WriteDownID, w.WriteDownID, correlationID, lines)
}
