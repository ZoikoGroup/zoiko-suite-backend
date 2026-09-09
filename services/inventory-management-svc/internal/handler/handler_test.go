package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/handler"
	"zoiko.io/inventory-management-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	items map[string]*domain.InventoryItem

	trackingPolicies  map[string]*domain.TrackingPolicy // current version, keyed by item_id
	valuationPolicies map[string]*domain.ValuationPolicy

	createErr error

	locations map[string]*domain.InventoryLocation
	parents   map[string]*string // location_id -> current parent_location_id

	movements       map[string]*domain.InventoryMovement
	movementsByKey  map[string]string // source_idempotency_key -> movement_id
	serialResidency map[string]string // "item_id|serial_number" -> location_id

	costLayers        []*domain.CostLayer
	valuationEntries  map[string]*domain.ValuationEntry // entry_id -> entry
	entriesByMovement map[string]string                 // movement_id -> entry_id
	valuationRuns     map[string]*domain.ValuationRun
	writeDowns        map[string]*domain.WriteDown

	stockCounts         map[string]*domain.StockCount
	stockCountLocations map[string][]string // count_id -> location_ids
	countLines          map[string]*domain.StockCountLine
}

func newStubStore() *stubStore {
	return &stubStore{
		items:               make(map[string]*domain.InventoryItem),
		trackingPolicies:    make(map[string]*domain.TrackingPolicy),
		valuationPolicies:   make(map[string]*domain.ValuationPolicy),
		locations:           make(map[string]*domain.InventoryLocation),
		parents:             make(map[string]*string),
		movements:           make(map[string]*domain.InventoryMovement),
		stockCounts:         make(map[string]*domain.StockCount),
		stockCountLocations: make(map[string][]string),
		countLines:          make(map[string]*domain.StockCountLine),
		movementsByKey:      make(map[string]string),
		serialResidency:     make(map[string]string),
		valuationEntries:    make(map[string]*domain.ValuationEntry),
		entriesByMovement:   make(map[string]string),
		valuationRuns:       make(map[string]*domain.ValuationRun),
		writeDowns:          make(map[string]*domain.WriteDown),
	}
}

// ── INV-04 (Inventory Valuation) ─────────────────────────────────────────────

func (s *stubStore) ValueMovement(_ context.Context, movementID, principalID string, unitCost *float64, at time.Time) (*domain.ValuationEntry, error) {
	if _, exists := s.entriesByMovement[movementID]; exists {
		return nil, domain.ErrMovementAlreadyValued
	}
	m, ok := s.movements[movementID]
	if !ok {
		return nil, domain.ErrMovementNotFound
	}
	if m.Status != domain.MovementStatusCommitted {
		return nil, domain.ErrMovementNotCommitted
	}
	vp, ok := s.valuationPolicies[m.ItemID]
	if !ok {
		return nil, domain.ErrValuationPolicyRequiredForActivation
	}

	entryID := "entry-" + movementID
	entry := &domain.ValuationEntry{
		EntryID: entryID, LegalEntityID: m.LegalEntityID, ItemID: m.ItemID, MovementID: movementID,
		ValuationMethod: vp.ValuationMethod, FiscalPeriod: m.FiscalPeriod, Quantity: m.Quantity,
		CreatedAt: at, CreatedByPrincipalID: principalID,
	}

	if m.DestinationLocationID != nil {
		if unitCost == nil {
			return nil, domain.ErrUnitCostRequired
		}
		entry.LocationID = *m.DestinationLocationID
		entry.EntryType = domain.ValuationEntryTypeInbound
		entry.Value = m.Quantity * *unitCost
		s.costLayers = append(s.costLayers, &domain.CostLayer{
			LayerID: "layer-" + movementID, ItemID: m.ItemID, LocationID: *m.DestinationLocationID, SourceMovementID: movementID,
			OriginalQuantity: m.Quantity, RemainingQuantity: m.Quantity, UnitCost: *unitCost, CreatedAt: at,
		})
	} else if m.SourceLocationID != nil {
		entry.LocationID = *m.SourceLocationID
		entry.EntryType = domain.ValuationEntryTypeOutbound
		if vp.ValuationMethod == domain.ValuationMethodStandardCost {
			if unitCost == nil {
				return nil, domain.ErrUnitCostRequired
			}
			entry.Value = m.Quantity * *unitCost
		} else {
			var totalRemaining, totalValue float64
			for _, l := range s.costLayers {
				if l.ItemID == m.ItemID && l.LocationID == *m.SourceLocationID {
					totalRemaining += l.RemainingQuantity
					totalValue += l.RemainingQuantity * l.UnitCost
				}
			}
			if totalRemaining < m.Quantity {
				return nil, domain.ErrInsufficientCostLayers
			}
			averageRate := totalValue / totalRemaining
			remaining := m.Quantity
			var consumedValue float64
			for _, l := range s.costLayers {
				if remaining <= 0 {
					break
				}
				if l.ItemID != m.ItemID || l.LocationID != *m.SourceLocationID || l.RemainingQuantity <= 0 {
					continue
				}
				take := l.RemainingQuantity
				if take > remaining {
					take = remaining
				}
				rate := l.UnitCost
				if vp.ValuationMethod == domain.ValuationMethodWeightedAverage {
					rate = averageRate
				}
				consumedValue += take * rate
				l.RemainingQuantity -= take
				remaining -= take
			}
			entry.Value = consumedValue
		}
	} else {
		return nil, domain.ErrMovementNotFound
	}

	s.valuationEntries[entryID] = entry
	s.entriesByMovement[movementID] = entryID
	cp := *entry
	return &cp, nil
}

func (s *stubStore) GetValuationEntry(_ context.Context, entryID string) (*domain.ValuationEntry, error) {
	e, ok := s.valuationEntries[entryID]
	if !ok {
		return nil, domain.ErrValuationEntryNotFound
	}
	cp := *e
	return &cp, nil
}

func (s *stubStore) GetInventoryValue(_ context.Context, itemID, locationID string) (float64, error) {
	var value float64
	for _, l := range s.costLayers {
		if l.ItemID == itemID && l.LocationID == locationID {
			value += l.RemainingQuantity * l.UnitCost
		}
	}
	return value, nil
}

func (s *stubStore) GetCostLayers(_ context.Context, itemID, locationID string) ([]domain.CostLayer, error) {
	var out []domain.CostLayer
	for _, l := range s.costLayers {
		if l.ItemID == itemID && l.LocationID == locationID {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (s *stubStore) CreateValuationRun(_ context.Context, r *domain.ValuationRun) (int, error) {
	for _, existing := range s.valuationRuns {
		if existing.LegalEntityID == r.LegalEntityID && existing.FiscalPeriod == r.FiscalPeriod && existing.Status != domain.ValuationRunStatusAccountingEventEmitted {
			return 0, domain.ErrValuationRunAlreadyExistsForPeriod
		}
	}
	frozen := 0
	cp := *r
	cp.Status = domain.ValuationRunStatusPopulationFrozen
	for _, e := range s.valuationEntries {
		if e.LegalEntityID == r.LegalEntityID && e.FiscalPeriod == r.FiscalPeriod && e.RunID == nil {
			runID := r.RunID
			e.RunID = &runID
			cp.Entries = append(cp.Entries, *e)
			frozen++
		}
	}
	s.valuationRuns[r.RunID] = &cp
	return frozen, nil
}

func (s *stubStore) GetValuationRun(_ context.Context, runID string) (*domain.ValuationRun, error) {
	r, ok := s.valuationRuns[runID]
	if !ok {
		return nil, domain.ErrValuationRunNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) MarkValuationRunEmitted(_ context.Context, runID, principalID, journalID string, at time.Time) error {
	r, ok := s.valuationRuns[runID]
	if !ok || r.Status != domain.ValuationRunStatusPopulationFrozen {
		return domain.ErrInvalidRunTransition
	}
	r.Status, r.EmittedAt, r.ApprovedAt, r.ApprovedByPrincipalID, r.JournalID = domain.ValuationRunStatusAccountingEventEmitted, &at, &at, &principalID, &journalID
	return nil
}

func (s *stubStore) CreateWriteDown(_ context.Context, w *domain.WriteDown, journalID string) error {
	cp := *w
	cp.JournalID = &journalID
	cp.Status = domain.WriteDownStatusAccountingEventEmitted
	s.writeDowns[w.WriteDownID] = &cp
	return nil
}

func (s *stubStore) GetWriteDown(_ context.Context, writeDownID string) (*domain.WriteDown, error) {
	w, ok := s.writeDowns[writeDownID]
	if !ok {
		return nil, domain.ErrWriteDownNotFound
	}
	cp := *w
	return &cp, nil
}

func (s *stubStore) ReverseWriteDown(_ context.Context, writeDownID, principalID, reason string, at time.Time) error {
	w, ok := s.writeDowns[writeDownID]
	if !ok || w.Status != domain.WriteDownStatusAccountingEventEmitted {
		return domain.ErrWriteDownAlreadyReversed
	}
	w.Status, w.ReversedAt, w.ReversedByPrincipalID, w.ReversalReason = domain.WriteDownStatusReversed, &at, &principalID, &reason
	return nil
}

// ── INV-05 (Stock Count) ──────────────────────────────────────────────────────

func (s *stubStore) CreateStockCount(_ context.Context, sc *domain.StockCount, locationIDs []string) error {
	cp := *sc
	s.stockCounts[sc.CountID] = &cp
	s.stockCountLocations[sc.CountID] = locationIDs
	return nil
}

func (s *stubStore) GetStockCount(_ context.Context, countID string) (*domain.StockCount, error) {
	sc, ok := s.stockCounts[countID]
	if !ok {
		return nil, domain.ErrStockCountNotFound
	}
	cp := *sc
	cp.LocationIDs = s.stockCountLocations[countID]
	for _, l := range s.countLines {
		if l.CountID == countID {
			cp.Lines = append(cp.Lines, *l)
		}
	}
	return &cp, nil
}

func (s *stubStore) FreezeCountPopulation(_ context.Context, countID string, at time.Time) (int, error) {
	sc, ok := s.stockCounts[countID]
	if !ok || sc.Status != domain.StockCountStatusPlanned {
		return 0, domain.ErrInvalidCountTransition
	}
	sc.Status, sc.FrozenAt, sc.CutoffAt = domain.StockCountStatusPopulationFrozen, &at, &at

	type pair struct{ itemID, locationID string }
	seen := map[pair]bool{}
	frozen := 0
	for _, locID := range s.stockCountLocations[countID] {
		for _, m := range s.movements {
			if m.Status != domain.MovementStatusCommitted {
				continue
			}
			var itemID string
			if m.SourceLocationID != nil && *m.SourceLocationID == locID {
				itemID = m.ItemID
			} else if m.DestinationLocationID != nil && *m.DestinationLocationID == locID {
				itemID = m.ItemID
			} else {
				continue
			}
			p := pair{itemID, locID}
			if seen[p] {
				continue
			}
			seen[p] = true

			var onHand float64
			for _, other := range s.movements {
				if other.Status != domain.MovementStatusCommitted || other.ItemID != itemID {
					continue
				}
				if other.DestinationLocationID != nil && *other.DestinationLocationID == locID {
					onHand += other.Quantity
				}
				if other.SourceLocationID != nil && *other.SourceLocationID == locID {
					onHand -= other.Quantity
				}
			}

			lineID := "line-" + countID + "-" + itemID + "-" + locID
			s.countLines[lineID] = &domain.StockCountLine{
				LineID: lineID, CountID: countID, ItemID: itemID, LocationID: locID,
				SystemQuantity: onHand, Status: domain.CountLineStatusPending, CreatedAt: at,
			}
			frozen++
		}
	}
	return frozen, nil
}

func (s *stubStore) AssignCounter(_ context.Context, lineID, counterPrincipalID string) error {
	l, ok := s.countLines[lineID]
	if !ok {
		return domain.ErrCountLineNotFound
	}
	l.AssignedCounterPrincipalID = &counterPrincipalID
	return nil
}

func (s *stubStore) RecordBlindCount(_ context.Context, lineID, principalID string, observedQuantity float64, at time.Time) (*domain.StockCountLine, error) {
	l, ok := s.countLines[lineID]
	if !ok || (l.Status != domain.CountLineStatusPending && l.Status != domain.CountLineStatusNeedsRecount) {
		return nil, domain.ErrInvalidCountLineTransition
	}
	l.ObservedQuantity, l.ObservedAt, l.ObservedByPrincipalID, l.Status = &observedQuantity, &at, &principalID, domain.CountLineStatusCounted
	cp := *l
	return &cp, nil
}

func (s *stubStore) RequestRecount(_ context.Context, lineID string) error {
	l, ok := s.countLines[lineID]
	if !ok || l.Status != domain.CountLineStatusCounted {
		return domain.ErrInvalidCountLineTransition
	}
	l.Status = domain.CountLineStatusNeedsRecount
	return nil
}

func (s *stubStore) ApproveCountVariance(_ context.Context, lineID, principalID string, at time.Time) error {
	l, ok := s.countLines[lineID]
	if !ok || l.Status != domain.CountLineStatusCounted {
		return domain.ErrInvalidCountLineTransition
	}
	if l.ObservedByPrincipalID != nil && *l.ObservedByPrincipalID == principalID {
		return domain.ErrSelfVarianceApprovalNotPermitted
	}
	l.Status, l.VarianceApprovedAt, l.VarianceApprovedByPrincipalID = domain.CountLineStatusVarianceApproved, &at, &principalID
	return nil
}

func (s *stubStore) GetCountLine(_ context.Context, lineID string) (*domain.StockCountLine, error) {
	l, ok := s.countLines[lineID]
	if !ok {
		return nil, domain.ErrCountLineNotFound
	}
	cp := *l
	return &cp, nil
}

func (s *stubStore) LinkCountLineAdjustment(_ context.Context, lineID, movementID string) error {
	l, ok := s.countLines[lineID]
	if !ok || l.Status != domain.CountLineStatusVarianceApproved {
		return domain.ErrInvalidCountLineTransition
	}
	l.Status, l.AdjustmentMovementID = domain.CountLineStatusAdjustmentGenerated, &movementID
	return nil
}

func (s *stubStore) MarkCountAdjustmentsGenerated(_ context.Context, countID string) error {
	sc, ok := s.stockCounts[countID]
	if !ok {
		return domain.ErrInvalidCountTransition
	}
	switch sc.Status {
	case domain.StockCountStatusPopulationFrozen, domain.StockCountStatusAdjustmentsGenerated:
	default:
		return domain.ErrInvalidCountTransition
	}
	sc.Status = domain.StockCountStatusAdjustmentsGenerated
	return nil
}

func (s *stubStore) CertifyStockCount(_ context.Context, countID, principalID string, at time.Time) error {
	sc, ok := s.stockCounts[countID]
	if !ok || sc.Status != domain.StockCountStatusAdjustmentsGenerated {
		return domain.ErrInvalidCountTransition
	}
	sc.Status, sc.CertifiedAt, sc.CertifiedByPrincipalID = domain.StockCountStatusCertified, &at, &principalID
	return nil
}

func (s *stubStore) CancelStockCount(_ context.Context, countID, principalID, reason string, at time.Time) error {
	sc, ok := s.stockCounts[countID]
	if !ok || sc.Status == domain.StockCountStatusCertified || sc.Status == domain.StockCountStatusCancelled {
		return domain.ErrInvalidCountTransition
	}
	sc.Status, sc.CancelledAt, sc.CancelledByPrincipalID, sc.CancelReason = domain.StockCountStatusCancelled, &at, &principalID, &reason
	return nil
}

// ── INV-02 (Inventory Location) ──────────────────────────────────────────────

func (s *stubStore) CreateLocation(_ context.Context, l *domain.InventoryLocation, parentLocationID *string) error {
	for _, existing := range s.locations {
		if existing.LegalEntityID == l.LegalEntityID && existing.LocationCode == l.LocationCode {
			return domain.ErrDuplicateLocationCode
		}
	}
	cp := *l
	s.locations[l.LocationID] = &cp
	s.parents[l.LocationID] = parentLocationID
	return nil
}

func (s *stubStore) GetLocation(_ context.Context, locationID string) (*domain.InventoryLocation, error) {
	l, ok := s.locations[locationID]
	if !ok {
		return nil, domain.ErrLocationNotFound
	}
	cp := *l
	return &cp, nil
}

func (s *stubStore) ListLocations(_ context.Context, legalEntityID string, eligibleOnly bool) ([]domain.InventoryLocation, error) {
	var out []domain.InventoryLocation
	for _, l := range s.locations {
		if l.LegalEntityID != legalEntityID {
			continue
		}
		if eligibleOnly && l.Status != domain.LocationStatusActive {
			continue
		}
		out = append(out, *l)
	}
	return out, nil
}

func (s *stubStore) ActivateLocation(_ context.Context, locationID, principalID string, at time.Time) error {
	l, ok := s.locations[locationID]
	if !ok || l.Status != domain.LocationStatusDraft {
		return domain.ErrInvalidLocationTransition
	}
	l.Status, l.ActivatedAt, l.ActivatedByPrincipalID = domain.LocationStatusActive, &at, &principalID
	return nil
}

func (s *stubStore) SuspendLocation(_ context.Context, locationID, principalID, reason string, at time.Time) error {
	l, ok := s.locations[locationID]
	if !ok || l.Status != domain.LocationStatusActive {
		return domain.ErrInvalidLocationTransition
	}
	l.Status, l.SuspendedAt, l.SuspendedByPrincipalID, l.SuspensionReason = domain.LocationStatusSuspended, &at, &principalID, &reason
	return nil
}

func (s *stubStore) SetQuarantine(_ context.Context, locationID, principalID, reason string, quarantine bool, at time.Time) error {
	l, ok := s.locations[locationID]
	if !ok {
		return domain.ErrLocationNotFound
	}
	if quarantine {
		if l.Status != domain.LocationStatusActive {
			return domain.ErrInvalidLocationTransition
		}
		l.Status, l.QuarantinedAt, l.QuarantinedByPrincipalID, l.QuarantineReason = domain.LocationStatusQuarantine, &at, &principalID, &reason
		return nil
	}
	if l.Status != domain.LocationStatusQuarantine {
		return domain.ErrLocationNotQuarantined
	}
	if l.QuarantinedByPrincipalID != nil && *l.QuarantinedByPrincipalID == principalID {
		return domain.ErrSelfQuarantineReleaseNotPermitted
	}
	l.Status, l.ReleasedAt, l.ReleasedByPrincipalID = domain.LocationStatusActive, &at, &principalID
	return nil
}

func (s *stubStore) RetireLocation(_ context.Context, locationID, principalID, reason string, at time.Time) error {
	l, ok := s.locations[locationID]
	if !ok {
		return domain.ErrLocationNotFound
	}
	switch l.Status {
	case domain.LocationStatusActive, domain.LocationStatusSuspended, domain.LocationStatusQuarantine:
	default:
		return domain.ErrInvalidLocationTransition
	}
	l.Status, l.RetiredAt, l.RetiredByPrincipalID, l.RetirementReason = domain.LocationStatusRetired, &at, &principalID, &reason
	return nil
}

func (s *stubStore) AmendLocationMetadata(_ context.Context, locationID string, description, custodianEntity *string) error {
	l, ok := s.locations[locationID]
	if !ok {
		return domain.ErrLocationNotFound
	}
	if description != nil {
		l.Description = *description
	}
	if custodianEntity != nil {
		l.CustodianEntity = *custodianEntity
	}
	return nil
}

func (s *stubStore) GetCurrentParent(_ context.Context, locationID string) (*string, error) {
	return s.parents[locationID], nil
}

func (s *stubStore) GetAncestorChain(_ context.Context, locationID string) ([]string, error) {
	var chain []string
	current := locationID
	for i := 0; i < 1000; i++ {
		parent := s.parents[current]
		if parent == nil {
			return chain, nil
		}
		chain = append(chain, *parent)
		current = *parent
	}
	return chain, nil
}

func (s *stubStore) ReparentLocation(_ context.Context, locationID, newParentLocationID, principalID string, at time.Time) error {
	child, ok := s.locations[locationID]
	if !ok {
		return domain.ErrLocationNotFound
	}
	newParent, ok := s.locations[newParentLocationID]
	if !ok {
		return domain.ErrLocationNotFound
	}
	if child.LegalEntityID != newParent.LegalEntityID {
		return domain.ErrReparentAcrossLegalEntities
	}
	current := newParentLocationID
	for i := 0; i < 1000; i++ {
		if current == locationID {
			return domain.ErrCircularLocationHierarchy
		}
		parent := s.parents[current]
		if parent == nil {
			break
		}
		current = *parent
	}
	s.parents[locationID] = &newParentLocationID
	return nil
}

func (s *stubStore) GetParentAsOf(_ context.Context, locationID string, _ time.Time) (*string, error) {
	return s.parents[locationID], nil
}

func (s *stubStore) CreateItem(_ context.Context, it *domain.InventoryItem) error {
	if s.createErr != nil {
		return s.createErr
	}
	for _, existing := range s.items {
		if existing.LegalEntityID == it.LegalEntityID && existing.SKU == it.SKU {
			return domain.ErrDuplicateSKU
		}
	}
	cp := *it
	s.items[it.ItemID] = &cp
	return nil
}

func (s *stubStore) GetItem(_ context.Context, itemID string) (*domain.InventoryItem, error) {
	it, ok := s.items[itemID]
	if !ok {
		return nil, domain.ErrItemNotFound
	}
	cp := *it
	return &cp, nil
}

func (s *stubStore) ListItems(_ context.Context, legalEntityID string) ([]domain.InventoryItem, error) {
	var out []domain.InventoryItem
	for _, it := range s.items {
		if it.LegalEntityID == legalEntityID {
			out = append(out, *it)
		}
	}
	return out, nil
}

func (s *stubStore) ActivateItem(_ context.Context, itemID, principalID string, at time.Time) error {
	it, ok := s.items[itemID]
	if !ok || it.Status != domain.ItemStatusDraft {
		return domain.ErrInvalidItemTransition
	}
	it.Status, it.ActivatedAt, it.ActivatedByPrincipalID = domain.ItemStatusActive, &at, &principalID
	return nil
}

func (s *stubStore) RetireItem(_ context.Context, itemID, principalID, reason string, at time.Time) error {
	it, ok := s.items[itemID]
	if !ok || it.Status != domain.ItemStatusActive {
		return domain.ErrInvalidItemTransition
	}
	it.Status, it.RetiredAt, it.RetiredByPrincipalID, it.RetirementReason = domain.ItemStatusRetired, &at, &principalID, &reason
	return nil
}

func (s *stubStore) AmendProfile(_ context.Context, itemID string, description, itemType, physicalCharacteristics *string) error {
	it, ok := s.items[itemID]
	if !ok {
		return domain.ErrItemNotFound
	}
	if description != nil {
		it.Description = *description
	}
	if itemType != nil {
		it.ItemType = *itemType
	}
	if physicalCharacteristics != nil {
		it.PhysicalCharacteristics = *physicalCharacteristics
	}
	return nil
}

func (s *stubStore) LinkCatalogItem(_ context.Context, itemID, catalogItemID string) error {
	it, ok := s.items[itemID]
	if !ok {
		return domain.ErrItemNotFound
	}
	it.CatalogItemID = &catalogItemID
	return nil
}

func (s *stubStore) SetTrackingPolicy(_ context.Context, p *domain.TrackingPolicy, _ time.Time) error {
	cp := *p
	s.trackingPolicies[p.ItemID] = &cp
	return nil
}

func (s *stubStore) GetCurrentTrackingPolicy(_ context.Context, itemID string) (*domain.TrackingPolicy, error) {
	p, ok := s.trackingPolicies[itemID]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (s *stubStore) SetValuationPolicy(_ context.Context, p *domain.ValuationPolicy) error {
	cp := *p
	s.valuationPolicies[p.ItemID] = &cp
	return nil
}

func (s *stubStore) GetCurrentValuationPolicy(_ context.Context, itemID string) (*domain.ValuationPolicy, error) {
	p, ok := s.valuationPolicies[itemID]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (s *stubStore) GetProfileAsOf(_ context.Context, itemID string, _ time.Time) (*domain.TrackingPolicy, *domain.ValuationPolicy, error) {
	return s.trackingPolicies[itemID], s.valuationPolicies[itemID], nil
}

// ── INV-03 (Inventory Movement) ──────────────────────────────────────────────

func (s *stubStore) CreateMovement(_ context.Context, m *domain.InventoryMovement) error {
	if existingID, ok := s.movementsByKey[m.SourceIdempotencyKey]; ok {
		*m = *s.movements[existingID]
		return nil
	}
	cp := *m
	s.movements[m.MovementID] = &cp
	s.movementsByKey[m.SourceIdempotencyKey] = m.MovementID
	return nil
}

func (s *stubStore) GetMovement(_ context.Context, movementID string) (*domain.InventoryMovement, error) {
	m, ok := s.movements[movementID]
	if !ok {
		return nil, domain.ErrMovementNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *stubStore) ListMovements(_ context.Context, itemID string) ([]domain.InventoryMovement, error) {
	var out []domain.InventoryMovement
	for _, m := range s.movements {
		if m.ItemID == itemID {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *stubStore) ValidateMovement(_ context.Context, movementID string, at time.Time) error {
	m, ok := s.movements[movementID]
	if !ok || m.Status != domain.MovementStatusDraft {
		return domain.ErrInvalidMovementTransition
	}
	item, ok := s.items[m.ItemID]
	if !ok {
		return domain.ErrItemNotFound
	}
	if item.Status != domain.ItemStatusActive {
		return domain.ErrItemNotEligibleForMovement
	}
	for _, locID := range []*string{m.SourceLocationID, m.DestinationLocationID} {
		if locID == nil {
			continue
		}
		loc, ok := s.locations[*locID]
		if !ok {
			return domain.ErrLocationNotFound
		}
		if loc.Status != domain.LocationStatusActive {
			return domain.ErrLocationNotEligible
		}
	}
	if tp, ok := s.trackingPolicies[m.ItemID]; ok {
		if tp.RequiresLotTracking && (m.LotNumber == nil || *m.LotNumber == "") {
			return domain.ErrLotIdentityRequired
		}
		if tp.RequiresSerialTracking && (m.SerialNumber == nil || *m.SerialNumber == "") {
			return domain.ErrSerialIdentityRequired
		}
	}
	m.Status, m.ValidatedAt = domain.MovementStatusValidated, &at
	return nil
}

func serialKey(itemID string, serial *string) string {
	if serial == nil {
		return ""
	}
	return itemID + "|" + *serial
}

func (s *stubStore) checkNegativeStock(m *domain.InventoryMovement) error {
	if m.SourceLocationID == nil {
		return nil
	}
	var onHand float64
	for _, other := range s.movements {
		if other.Status != domain.MovementStatusCommitted || other.ItemID != m.ItemID {
			continue
		}
		if other.DestinationLocationID != nil && *other.DestinationLocationID == *m.SourceLocationID {
			onHand += other.Quantity
		}
		if other.SourceLocationID != nil && *other.SourceLocationID == *m.SourceLocationID {
			onHand -= other.Quantity
		}
	}
	if onHand-m.Quantity < 0 {
		return domain.ErrNegativeStockNotAllowed
	}
	return nil
}

func (s *stubStore) applySerialResidency(m *domain.InventoryMovement) error {
	if m.SerialNumber == nil || *m.SerialNumber == "" {
		return nil
	}
	key := serialKey(m.ItemID, m.SerialNumber)
	switch {
	case m.SourceLocationID == nil && m.DestinationLocationID != nil:
		if _, exists := s.serialResidency[key]; exists {
			return domain.ErrSerialAlreadyResident
		}
		s.serialResidency[key] = *m.DestinationLocationID
	case m.SourceLocationID != nil && m.DestinationLocationID == nil:
		if s.serialResidency[key] != *m.SourceLocationID {
			return domain.ErrSerialNotAtSourceLocation
		}
		delete(s.serialResidency, key)
	case m.SourceLocationID != nil && m.DestinationLocationID != nil:
		if s.serialResidency[key] != *m.SourceLocationID {
			return domain.ErrSerialNotAtSourceLocation
		}
		s.serialResidency[key] = *m.DestinationLocationID
	}
	return nil
}

func (s *stubStore) CommitMovement(_ context.Context, movementID, principalID string, at time.Time) error {
	m, ok := s.movements[movementID]
	if !ok || m.Status != domain.MovementStatusValidated {
		return domain.ErrInvalidMovementTransition
	}
	item := s.items[m.ItemID]
	if item != nil && m.UOM != item.BaseUOM {
		return domain.ErrUOMMismatch
	}
	if err := s.checkNegativeStock(m); err != nil {
		return err
	}
	if err := s.applySerialResidency(m); err != nil {
		return err
	}
	m.Status, m.CommittedAt, m.CommittedByPrincipalID = domain.MovementStatusCommitted, &at, &principalID
	return nil
}

func (s *stubStore) GetOnHand(_ context.Context, itemID, locationID string) (float64, error) {
	var onHand float64
	for _, m := range s.movements {
		if m.Status != domain.MovementStatusCommitted || m.ItemID != itemID {
			continue
		}
		if m.DestinationLocationID != nil && *m.DestinationLocationID == locationID {
			onHand += m.Quantity
		}
		if m.SourceLocationID != nil && *m.SourceLocationID == locationID {
			onHand -= m.Quantity
		}
	}
	return onHand, nil
}

func (s *stubStore) GetOnHandAsOf(ctx context.Context, itemID, locationID string, _ time.Time) (float64, error) {
	return s.GetOnHand(ctx, itemID, locationID)
}

func (s *stubStore) CreateCorrectionMovement(_ context.Context, originalMovementID, principalID, reason string, isSupersede bool, newMovementID string, at time.Time) (*domain.InventoryMovement, error) {
	original, ok := s.movements[originalMovementID]
	if !ok {
		return nil, domain.ErrMovementNotFound
	}
	if original.Status != domain.MovementStatusCommitted {
		return nil, domain.ErrInvalidMovementTransition
	}
	movementType := domain.MovementTypeReversal
	if isSupersede {
		movementType = domain.MovementTypeSupersession
	}
	correction := &domain.InventoryMovement{
		MovementID: newMovementID, TenantID: original.TenantID, LegalEntityID: original.LegalEntityID, MovementType: movementType, Status: domain.MovementStatusValidated,
		ItemID: original.ItemID, SourceLocationID: original.DestinationLocationID, DestinationLocationID: original.SourceLocationID,
		Quantity: original.Quantity, UOM: original.UOM, LotNumber: original.LotNumber, SerialNumber: original.SerialNumber,
		SourceReference: original.SourceReference, SourceIdempotencyKey: newMovementID, BusinessDate: at, FiscalPeriod: original.FiscalPeriod,
		Reason: &reason, CreatedAt: at, CreatedByPrincipalID: principalID, ValidatedAt: &at,
	}
	if isSupersede {
		correction.SupersedesMovementID = &originalMovementID
	} else {
		correction.ReversesMovementID = &originalMovementID
	}
	if err := s.checkNegativeStock(correction); err != nil {
		return nil, err
	}
	if err := s.applySerialResidency(correction); err != nil {
		return nil, err
	}
	correction.Status, correction.CommittedAt, correction.CommittedByPrincipalID = domain.MovementStatusCommitted, &at, &principalID
	cp := *correction
	s.movements[correction.MovementID] = &cp
	s.movementsByKey[correction.SourceIdempotencyKey] = correction.MovementID
	return correction, nil
}

var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishInventoryItemCreated(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryItemActivated(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryPolicyChanged(_ context.Context, _, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryItemRetired(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryLocationCreated(_ context.Context, _, _ string, _ domain.InventoryLocation) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryLocationActivated(_ context.Context, _, _ string, _ domain.InventoryLocation) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryLocationQuarantined(_ context.Context, _, _ string, _ domain.InventoryLocation) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryLocationChanged(_ context.Context, _, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryLocationRetired(_ context.Context, _, _ string, _ domain.InventoryLocation) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryMovementCommitted(_ context.Context, _, _ string, _ domain.InventoryMovement) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryMovementReversed(_ context.Context, _, _ string, _ domain.InventoryMovement) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryTransferred(_ context.Context, _, _ string, _ domain.InventoryMovement) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryReceived(_ context.Context, _, _ string, _ domain.InventoryMovement) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryIssued(_ context.Context, _, _ string, _ domain.InventoryMovement) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryValued(_ context.Context, _, _, _ string, _ domain.ValuationEntry) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryWriteDownRecorded(_ context.Context, _, _, _ string, _ domain.WriteDown) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryWriteDownReversed(_ context.Context, _, _, _ string, _ domain.WriteDown) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryAccountingEventEmitted(_ context.Context, _, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishStockCountStarted(_ context.Context, _, _, _ string, _ domain.StockCount) {
	p.calls++
}
func (p *stubPublisher) PublishStockCountPopulationFrozen(_ context.Context, _, _, _, _ string, _ int) {
	p.calls++
}
func (p *stubPublisher) PublishStockCountVarianceApproved(_ context.Context, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishStockCountAdjustmentRequested(_ context.Context, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishStockCountCertified(_ context.Context, _, _, _ string, _ domain.StockCount) {
	p.calls++
}

var _ handler.Publisher = (*stubPublisher)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubPeriodChecker struct{ err error }

func (c *stubPeriodChecker) CheckPeriodOpen(_ context.Context, _, _, _ string) error { return c.err }

var _ handler.PeriodChecker = (*stubPeriodChecker)(nil)

type stubLedger struct {
	postJournalID string
	postErr       error
	postCalls     int
	reverseErr    error
	reverseCalls  int
}

func (l *stubLedger) PostInventoryAccountingEvent(_ context.Context, _, _, _, _, _, sourceEventID, _ string, _ []clients.LedgerLine) (string, error) {
	l.postCalls++
	if l.postErr != nil {
		return "", l.postErr
	}
	if l.postJournalID != "" {
		return l.postJournalID, nil
	}
	return "journal-" + sourceEventID, nil
}

func (l *stubLedger) ReverseInventoryJournal(_ context.Context, _, _, _, _ string) error {
	l.reverseCalls++
	return l.reverseErr
}

var _ handler.InventoryLedgerClient = (*stubLedger)(nil)

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	return newRouterWithPeriodChecker(s, pub, authz, &stubPeriodChecker{})
}

func newRouterWithPeriodChecker(s *stubStore, pub *stubPublisher, authz *stubAuthZ, pc *stubPeriodChecker) chi.Router {
	return newRouterFull(s, pub, authz, pc, &stubLedger{})
}

func newRouterWithLedger(s *stubStore, pub *stubPublisher, authz *stubAuthZ, ledger *stubLedger) chi.Router {
	return newRouterFull(s, pub, authz, &stubPeriodChecker{}, ledger)
}

func newRouterFull(s *stubStore, pub *stubPublisher, authz *stubAuthZ, pc *stubPeriodChecker, ledger *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop()).WithPeriodChecker(pc).WithLedgerClient(ledger)
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftItem(t *testing.T, r chi.Router, legalEntityID, sku string) domain.InventoryItem {
	t.Helper()
	req := domain.CreateInventoryItemRequest{LegalEntityID: legalEntityID, SKU: sku, Description: "Widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var it domain.InventoryItem
	_ = json.NewDecoder(rr.Body).Decode(&it)
	return it
}

func setValuationPolicy(t *testing.T, r chi.Router, itemID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: domain.ValuationMethodFIFO, EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/items/"+itemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("set valuation policy failed: %d %s", rr.Code, rr.Body.String())
	}
}

func createActiveItem(t *testing.T, s *stubStore, r chi.Router, legalEntityID, sku string) string {
	t.Helper()
	_ = s
	it := createDraftItem(t, r, legalEntityID, sku)
	setValuationPolicy(t, r, it.ItemID)
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/activate", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("activate failed: %d %s", rr.Code, rr.Body.String())
	}
	return it.ItemID
}

// ── CreateInventoryItem ──────────────────────────────────────────────────────

func TestCreateInventoryItem_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/items/", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateInventoryItem_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-1")
	if it.Status != domain.ItemStatusDraft {
		t.Fatalf("expected DRAFT, got %q", it.Status)
	}
}

func TestCreateInventoryItem_DuplicateSKU_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createDraftItem(t, r, "le-1", "SKU-DUP")

	req := domain.CreateInventoryItemRequest{LegalEntityID: "le-1", SKU: "SKU-DUP", Description: "Another widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ActivateInventoryItem ("Missing valuation policy blocks ... valuation") ──

func TestActivateInventoryItem_NoValuationPolicy_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-2")

	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/activate", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActivateInventoryItem_WithValuationPolicy_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-3")
	_ = id
	if s.items[id].Status != domain.ItemStatusActive {
		t.Fatalf("expected ACTIVE, got %q", s.items[id].Status)
	}
}

// ── SetValuationPolicyFutureEffective ("Retroactive valuation-policy change") ─

func TestSetValuationPolicy_PastEffectiveDate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-4")

	past := time.Now().UTC().Add(-time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: domain.ValuationMethodFIFO, EffectiveFrom: &past}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retroactive valuation policy, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetValuationPolicy_FutureEffectiveDate_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-5")
	setValuationPolicy(t, r, it.ItemID)
	if s.valuationPolicies[it.ItemID] == nil {
		t.Fatalf("expected a valuation policy to be recorded")
	}
}

func TestSetValuationPolicy_InvalidMethod_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-6")

	future := time.Now().UTC().Add(time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: "MADE_UP_METHOD", EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── RetireInventoryItem ──────────────────────────────────────────────────────

func TestRetireInventoryItem_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-7")

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/retire", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRetireInventoryItem_FromDraft_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-8")

	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/retire", domain.RetireInventoryItemRequest{Reason: "x"}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retiring a DRAFT item, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRetireInventoryItem_FromActive_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-9")

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/retire", domain.RetireInventoryItemRequest{Reason: "discontinued"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.items[id].Status != domain.ItemStatusRetired {
		t.Fatalf("expected RETIRED, got %q", s.items[id].Status)
	}
}

// ── LinkCommercialCatalogItem (never touches valuation) ──────────────────────

func TestLinkCommercialCatalogItem_NeverChangesValuationPolicy(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-10")
	before := s.valuationPolicies[id].ValuationMethod

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/link-catalog", domain.LinkCommercialCatalogItemRequest{CatalogItemID: "cat-1"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.valuationPolicies[id].ValuationMethod != before {
		t.Fatalf("expected valuation method unchanged by catalog link, got %q (was %q)", s.valuationPolicies[id].ValuationMethod, before)
	}
	if s.items[id].CatalogItemID == nil || *s.items[id].CatalogItemID != "cat-1" {
		t.Fatalf("expected catalog_item_id linked, got %v", s.items[id].CatalogItemID)
	}
}

// ── AmendInventoryProfile (never touches base_uom/sku) ───────────────────────

func TestAmendInventoryProfile_NeverChangesBaseUOM(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-11")

	newDesc := "Updated description"
	req := domain.AmendInventoryProfileRequest{Description: &newDesc}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/amend", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.items[it.ItemID].BaseUOM != "EACH" {
		t.Fatalf("expected base_uom unchanged (EACH), got %q", s.items[it.ItemID].BaseUOM)
	}
	if s.items[it.ItemID].Description != newDesc {
		t.Fatalf("expected description updated, got %q", s.items[it.ItemID].Description)
	}
}

// ── Authorization ────────────────────────────────────────────────────────────

func TestCreateInventoryItem_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateInventoryItemRequest{LegalEntityID: "le-1", SKU: "SKU-12", Description: "Widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
