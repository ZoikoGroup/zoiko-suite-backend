package handler_test

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	svcmiddleware "zoiko.io/goods-service-receipt-svc/internal/middleware"
)

// stubStore replicates PgStore's observable semantics: tenant scoping, the
// version check, the state machine, the authoritative tolerance check at
// confirmation, reversal accumulation, and the transactional side effects of a
// confirmation/reversal (a progress push, a GRNI posting request, domain events)
// — each keyed so a replay can never queue the consequence twice.
type stubStore struct {
	receipts  map[string]*domain.GoodsServiceReceipt
	evidence  map[string][]domain.ReceiptEvidence
	reversals []domain.ReceiptReversal
	pushes    []*stubPush
	posting   []*domain.ReceiptAccountingEvent
	events    []string
}

type stubPush struct {
	domain.ProgressPush
	Status string
}

func newStubStore() *stubStore {
	return &stubStore{receipts: map[string]*domain.GoodsServiceReceipt{}, evidence: map[string][]domain.ReceiptEvidence{}}
}

func tenantOf(ctx context.Context) string { return svcmiddleware.TenantFromContext(ctx) }

func (s *stubStore) find(ctx context.Context, id string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[id]
	if !ok || r.TenantID != tenantOf(ctx) {
		return nil, domain.ErrReceiptNotFound
	}
	return r, nil
}

func copyOf(r *domain.GoodsServiceReceipt) *domain.GoodsServiceReceipt {
	c := *r
	return &c
}

func (s *stubStore) pushStatus(r *domain.GoodsServiceReceipt) string {
	st := ""
	for _, p := range s.pushes {
		if p.ReceiptID != r.ReceiptID {
			continue
		}
		switch {
		case p.Status == "FAILED":
			return "FAILED"
		case p.Status == "PENDING":
			st = "PENDING"
		case st == "":
			st = "DELIVERED"
		}
	}
	if st == "" {
		return domain.PushNotApplicable
	}
	return st
}

func (s *stubStore) view(r *domain.GoodsServiceReceipt) *domain.GoodsServiceReceipt {
	c := copyOf(r)
	c.ProgressPushStatus = s.pushStatus(r)
	return c
}

func (s *stubStore) CreateReceipt(ctx context.Context, tenantID string, req domain.CreateReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	now := time.Now().UTC()
	r := &domain.GoodsServiceReceipt{
		ReceiptID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID, PurchaseOrderID: req.PurchaseOrderID,
		ReceiptType: req.ReceiptType, Quantity: req.Quantity, UnitOfMeasure: req.UnitOfMeasure, Amount: req.Amount,
		CurrencyCode: req.CurrencyCode, ReceiptDate: req.ReceiptDate, Location: req.Location, InspectionResult: req.InspectionResult,
		RequiresIndependentAcceptance: req.RequiresIndependentAcceptance, ToleranceExceptionRef: req.ToleranceExceptionRef,
		Status: domain.StatusDraft, ReceiverPrincipalID: cmd.PrincipalID, CreatedByPrincipalID: cmd.PrincipalID,
		Version: 1, CreatedAt: now, UpdatedAt: now, ProgressPushStatus: domain.PushNotApplicable,
	}
	if req.POLineID != "" {
		l := req.POLineID
		r.POLineID = &l
	}
	s.receipts[r.ReceiptID] = r
	s.events = append(s.events, domain.EventReceiptCreated, domain.AliasReceiptCreated)
	return copyOf(r), nil
}

func (s *stubStore) FindReceipt(ctx context.Context, id string) (*domain.GoodsServiceReceipt, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(r), nil
}

func (s *stubStore) ListReceiptsForPO(ctx context.Context, poID string) ([]domain.GoodsServiceReceipt, error) {
	var out []domain.GoodsServiceReceipt
	for _, r := range s.receipts {
		if r.TenantID == tenantOf(ctx) && r.PurchaseOrderID == poID {
			out = append(out, *s.view(r))
		}
	}
	return out, nil
}

func checkVer(r *domain.GoodsServiceReceipt, cmd domain.Command) error {
	if cmd.ExpectedVersion != nil && *cmd.ExpectedVersion != r.Version {
		return domain.ErrStaleVersion
	}
	return nil
}

func (s *stubStore) AmendReceiptDraft(ctx context.Context, id string, req domain.AmendReceiptDraftRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVer(r, cmd); err != nil {
		return nil, err
	}
	if !domain.CanAmendDraft(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if req.Quantity != nil {
		r.Quantity = *req.Quantity
	}
	if req.Amount != nil {
		r.Amount = *req.Amount
	}
	if req.Location != nil {
		r.Location = *req.Location
	}
	r.Version++
	return s.view(r), nil
}

func (s *stubStore) pendingLine(tenant, line string) float64 {
	var q float64
	for _, p := range s.pushes {
		if p.TenantID == tenant && p.POLineID == line && p.Status == "PENDING" {
			q += p.Quantity * float64(p.DeltaSign)
		}
	}
	return q
}

func (s *stubStore) ConfirmReceipt(ctx context.Context, id string, in domain.ConfirmInput, cmd domain.Command) (*domain.ConfirmResult, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVer(r, cmd); err != nil {
		return nil, err
	}
	if !domain.CanConfirm(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	line := r.POLineID
	if in.POLineID != nil && *in.POLineID != "" {
		if line != nil && !strings.EqualFold(*line, *in.POLineID) {
			return nil, domain.ErrPurchaseOrderLineInvalid
		}
		line = in.POLineID
	}
	exception := in.ToleranceExceptionRef
	if exception == "" {
		exception = r.ToleranceExceptionRef
	}
	if exception == "" {
		tol := in.Limits.TolerancePct / 100
		if line != nil {
			ceiling := (in.Limits.LineOpenReceiptQuantity - s.pendingLine(r.TenantID, *line)) + in.Limits.LineOrderedQuantity*tol
			if r.Quantity > ceiling+0.0001 {
				return nil, domain.ErrOverReceiptTolerance
			}
		} else {
			var net float64
			for _, o := range s.receipts {
				if o.TenantID == r.TenantID && o.PurchaseOrderID == r.PurchaseOrderID &&
					(o.Status == domain.StatusConfirmed || o.Status == domain.StatusPartiallyReversed || o.Status == domain.StatusFullyReversed) {
					net += o.Amount - o.ReversedAmount
				}
			}
			if net+r.Amount > in.Limits.POTotalAmount*(1+tol)+0.0001 {
				return nil, domain.ErrOverReceiptTolerance
			}
		}
	}

	now := time.Now().UTC()
	actor := cmd.PrincipalID
	r.Status, r.ConfirmedByPrincipalID, r.ConfirmedAt = domain.StatusConfirmed, &actor, &now
	r.ToleranceExceptionRef = exception
	r.POLineID, r.PORevision = line, in.PORevision
	r.Version++

	if r.POLineID != nil {
		s.pushes = append(s.pushes, &stubPush{Status: "PENDING", ProgressPush: domain.ProgressPush{
			TenantID: r.TenantID, ReceiptID: r.ReceiptID, PurchaseOrderID: r.PurchaseOrderID, POLineID: *r.POLineID,
			Quantity: r.Quantity, Amount: r.Amount, DeltaSign: 1, SourceRef: r.ReceiptID}})
	}
	acct := s.queue(r, domain.DirectionAccrue, r.ReceiptID)
	s.events = append(s.events, domain.EventReceiptConfirmed, domain.AliasReceiptConfirmed)
	return &domain.ConfirmResult{Receipt: s.view(r), Accounting: acct}, nil
}

// queue mirrors the unique (tenant, source_event_id) key: a replay queues nothing.
func (s *stubStore) queue(r *domain.GoodsServiceReceipt, direction, source string) *domain.ReceiptAccountingEvent {
	for _, p := range s.posting {
		if p.TenantID == r.TenantID && *p.SourceEventID == source {
			return nil
		}
	}
	src := source
	e := &domain.ReceiptAccountingEvent{
		EventID: uuid.NewString(), TenantID: r.TenantID, ReceiptID: r.ReceiptID, SourceEventID: &src, Direction: direction,
		Status: domain.AccountingPending, CreatedAt: time.Now().UTC(),
	}
	s.posting = append(s.posting, e)
	return e
}

func (s *stubStore) RejectReceipt(ctx context.Context, id string, req domain.RejectReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVer(r, cmd); err != nil {
		return nil, err
	}
	if !domain.CanReject(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	r.Status, r.RejectionReason = domain.StatusRejected, req.Reason
	r.Version++
	return s.view(r), nil
}

func (s *stubStore) ReverseReceipt(ctx context.Context, id string, req domain.ReverseReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, *domain.ReceiptReversal, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := checkVer(r, cmd); err != nil {
		return nil, nil, err
	}
	if !domain.CanReverse(r.Status) {
		return nil, nil, domain.ErrInvalidTransition
	}
	remAmt, remQty := r.Amount-r.ReversedAmount, r.Quantity-r.ReversedQuantity
	if req.ReversedAmount > remAmt+0.0001 {
		return nil, nil, domain.ErrOverReversal
	}
	full := req.ReversedAmount >= remAmt-0.0001
	var qty float64
	switch {
	case req.ReversedQuantity != nil:
		qty = *req.ReversedQuantity
	case full:
		qty = remQty
	default:
		qty = math.Round(r.Quantity*req.ReversedAmount/r.Amount*10000) / 10000
	}
	if qty > remQty+0.0001 {
		return nil, nil, domain.ErrOverReversal
	}
	r.ReversedAmount += req.ReversedAmount
	r.ReversedQuantity += qty
	r.Status = domain.StatusPartiallyReversed
	if r.ReversedAmount >= r.Amount-0.0001 {
		r.Status = domain.StatusFullyReversed
	}
	r.Version++
	rev := domain.ReceiptReversal{ReversalID: uuid.NewString(), TenantID: r.TenantID, ReceiptID: r.ReceiptID,
		ReversedAmount: req.ReversedAmount, ReversedQuantity: qty, Reason: req.Reason, ReversedByPrincipalID: cmd.PrincipalID}
	s.reversals = append(s.reversals, rev)
	if r.POLineID != nil && qty > 0 {
		s.pushes = append(s.pushes, &stubPush{Status: "PENDING", ProgressPush: domain.ProgressPush{
			TenantID: r.TenantID, ReceiptID: r.ReceiptID, PurchaseOrderID: r.PurchaseOrderID, POLineID: *r.POLineID,
			Quantity: qty, Amount: req.ReversedAmount, DeltaSign: -1, SourceRef: rev.ReversalID}})
	}
	s.queue(r, domain.DirectionReverse, r.ReceiptID+":reversal:"+rev.ReversalID)
	s.events = append(s.events, domain.EventReceiptReversed, domain.AliasReceiptReversed)
	return s.view(r), &rev, nil
}

func (s *stubStore) RecordServiceAcceptance(ctx context.Context, id string, req domain.RecordServiceAcceptanceRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVer(r, cmd); err != nil {
		return nil, err
	}
	if r.Status != domain.StatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = domain.StatusPendingConfirmation
	r.Version++
	if req.EvidenceRef != "" {
		s.evidence[id] = append(s.evidence[id], domain.ReceiptEvidence{EvidenceID: uuid.NewString(), TenantID: r.TenantID,
			ReceiptID: id, EvidenceRef: req.EvidenceRef, Description: req.Notes, RecordedByPrincipalID: cmd.PrincipalID})
	}
	s.events = append(s.events, domain.EventServiceAcceptanceRecorded)
	return s.view(r), nil
}

func (s *stubStore) AttachReceiptEvidence(ctx context.Context, id string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error) {
	r, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	e := domain.ReceiptEvidence{EvidenceID: uuid.NewString(), TenantID: r.TenantID, ReceiptID: id, EvidenceRef: req.EvidenceRef,
		Description: req.Description, RecordedByPrincipalID: principalID}
	s.evidence[id] = append(s.evidence[id], e)
	return &e, nil
}

func (s *stubStore) ListReceiptEvidence(ctx context.Context, id string) ([]domain.ReceiptEvidence, error) {
	return s.evidence[id], nil
}

func (s *stubStore) SumNetConfirmedAmountForPO(ctx context.Context, poID string) (float64, error) {
	var net float64
	for _, o := range s.receipts {
		if o.TenantID == tenantOf(ctx) && o.PurchaseOrderID == poID &&
			(o.Status == domain.StatusConfirmed || o.Status == domain.StatusPartiallyReversed || o.Status == domain.StatusFullyReversed) {
			net += o.Amount - o.ReversedAmount
		}
	}
	return net, nil
}

func (s *stubStore) ReceivedToDate(ctx context.Context, poID string) ([]domain.LineReceived, error) {
	byLine := map[string]*domain.LineReceived{}
	var order []string
	for _, o := range s.receipts {
		if o.TenantID != tenantOf(ctx) || o.PurchaseOrderID != poID || o.POLineID == nil {
			continue
		}
		if o.Status != domain.StatusConfirmed && o.Status != domain.StatusPartiallyReversed && o.Status != domain.StatusFullyReversed {
			continue
		}
		l, ok := byLine[*o.POLineID]
		if !ok {
			l = &domain.LineReceived{POLineID: *o.POLineID}
			byLine[*o.POLineID] = l
			order = append(order, *o.POLineID)
		}
		l.ReceivedQuantity += o.Quantity - o.ReversedQuantity
		l.ReceivedAmount += o.Amount - o.ReversedAmount
	}
	var out []domain.LineReceived
	for _, k := range order {
		out = append(out, *byLine[k])
	}
	return out, nil
}

func (s *stubStore) PendingLineQuantity(ctx context.Context, line string) (float64, error) {
	return s.pendingLine(tenantOf(ctx), line), nil
}

func (s *stubStore) ListAccountingRequests(ctx context.Context, id string) ([]domain.ReceiptAccountingEvent, error) {
	var out []domain.ReceiptAccountingEvent
	for i := len(s.posting) - 1; i >= 0; i-- {
		if p := s.posting[i]; p.ReceiptID == id && p.TenantID == tenantOf(ctx) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubStore) GetLatestAccountingEvent(ctx context.Context, id string) (*domain.ReceiptAccountingEvent, error) {
	list, _ := s.ListAccountingRequests(ctx, id)
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

func (s *stubStore) RequeueAccounting(ctx context.Context, id string) (int64, error) {
	var n int64
	for _, p := range s.posting {
		if p.ReceiptID == id && p.TenantID == tenantOf(ctx) && (p.Status == domain.AccountingFailed || p.Status == domain.AccountingQuarantined) {
			p.Status, p.Attempts = domain.AccountingPending, 0
			n++
		}
	}
	return n, nil
}
