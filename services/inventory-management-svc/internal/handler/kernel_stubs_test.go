package handler_test

import (
	"context"
	"time"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── kernel posting stubs ─────────────────────────────────────────────────────

func (s *stubStore) MarkValuationRunEmittedToKernel(_ context.Context, runID, principalID string, journalIDs []string, at time.Time) error {
	r, ok := s.valuationRuns[runID]
	if !ok || r.Status != domain.ValuationRunStatusPopulationFrozen || len(journalIDs) == 0 {
		return domain.ErrInvalidRunTransition
	}
	r.Status, r.EmittedAt, r.ApprovedAt, r.ApprovedByPrincipalID, r.JournalID, r.LedgerJournalIDs = domain.ValuationRunStatusAccountingEventEmitted, &at, &at, &principalID, &journalIDs[0], journalIDs
	return nil
}

func (s *stubStore) RunInboundByStock(_ context.Context, runID string) ([]domain.RunInboundLine, error) {
	var out []domain.RunInboundLine
	for _, e := range s.valuationEntries {
		if e.RunID == nil || *e.RunID != runID || e.EntryType != domain.ValuationEntryTypeInbound {
			continue
		}
		if _, linked := s.receiptLinks[e.MovementID]; linked {
			out = append(out, domain.RunInboundLine{ItemID: e.ItemID, LocationID: e.LocationID, ReceiptLinked: e.Value})
		}
	}
	return out, nil
}

func (l *stubLedger) KernelPostingEnabled() bool { return l.kernel }

func (l *stubLedger) PostInventoryKernelEvent(_ context.Context, _, _, _, _ string, e clients.KernelEvent) (string, error) {
	if l.kernelErr != nil {
		return "", l.kernelErr
	}
	l.kernelEvents = append(l.kernelEvents, e)
	return "kj-" + e.EventType, nil
}
