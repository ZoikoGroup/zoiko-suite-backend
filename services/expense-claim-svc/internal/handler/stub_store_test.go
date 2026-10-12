package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/expense-claim-svc/internal/domain"
	svcmiddleware "zoiko.io/expense-claim-svc/internal/middleware"
)

// stubStore replicates PgStore's observable semantics rather than returning
// canned answers: tenant scoping (another tenant's claim is absent), the
// expected_version check, the state-machine table, the receipt-uniqueness
// invariant, immutable submission snapshots, idempotency saved together with the
// command, and — on approval — the durable payable request and ACC-04 posting
// request written alongside it.
type stubStore struct {
	claims   map[string]*domain.ExpenseClaim
	lines    map[string][]*domain.ExpenseLine
	subs     map[string][]domain.ExpenseClaimSubmission
	events   map[string][]domain.ExpenseClaimEvent
	idem     map[string]*domain.IdemRecord
	payReqs  map[string]*domain.PayableRequest
	postings []*domain.PostingRequest
	outbox   []string
}

func newStubStore() *stubStore {
	return &stubStore{
		claims: map[string]*domain.ExpenseClaim{}, lines: map[string][]*domain.ExpenseLine{},
		subs: map[string][]domain.ExpenseClaimSubmission{}, events: map[string][]domain.ExpenseClaimEvent{},
		idem: map[string]*domain.IdemRecord{}, payReqs: map[string]*domain.PayableRequest{},
	}
}

func tenantOf(ctx context.Context) string { return svcmiddleware.TenantFromContext(ctx) }

func (s *stubStore) visible(ctx context.Context, id string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[id]
	if !ok || c.TenantID == nil || *c.TenantID != tenantOf(ctx) {
		return nil, domain.ErrClaimNotFound
	}
	return c, nil
}

// lock mirrors lockClaim: visible, then expected_version.
func (s *stubStore) lock(ctx context.Context, id string, expected *int) (*domain.ExpenseClaim, error) {
	c, err := s.visible(ctx, id)
	if err != nil {
		return nil, err
	}
	if expected != nil && *expected != c.Version {
		return nil, domain.ErrStaleVersion
	}
	return c, nil
}

func (s *stubStore) emit(c *domain.ExpenseClaim, hist, detail, actor string) {
	s.events[c.ClaimID] = append(s.events[c.ClaimID], domain.ExpenseClaimEvent{
		EventID: uuid.NewString(), TenantID: c.TenantID, ClaimID: c.ClaimID, EventType: hist, Detail: detail,
		ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
	s.outbox = append(s.outbox, domain.OutboxTypes(hist)...)
}

func (s *stubStore) saveIdem(ctx context.Context, idem *domain.IdemKey, body any) error {
	if idem == nil {
		return nil
	}
	k := tenantOf(ctx) + "|" + idem.Key
	if _, exists := s.idem[k]; exists {
		return domain.ErrIdempotencyConflict
	}
	b, _ := json.Marshal(body)
	s.idem[k] = &domain.IdemRecord{Operation: idem.Operation, RequestHash: idem.RequestHash, StatusCode: 200, Body: b}
	return nil
}

func (s *stubStore) GetIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error) {
	return s.idem[tenantOf(ctx)+"|"+key], nil
}

func cp(c *domain.ExpenseClaim) *domain.ExpenseClaim { x := *c; return &x }

func (s *stubStore) CreateClaim(ctx context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID, corr string, idem *domain.IdemKey) (*domain.ExpenseClaim, error) {
	now := time.Now().UTC()
	t := tenantID
	c := &domain.ExpenseClaim{
		ClaimID: uuid.NewString(), TenantID: &t, LegalEntityID: req.LegalEntityID, ClaimantPrincipalID: req.ClaimantPrincipalID,
		Currency: req.Currency, BusinessPurpose: req.BusinessPurpose, ProjectCostCenter: req.ProjectCostCenter,
		PaymentPreferenceRef: req.PaymentPreferenceRef, Status: domain.StatusDraft, Version: 1,
		PolicyAssessmentResult: domain.PolicyNotAssessed, PayableState: domain.PayableNone, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.saveIdem(ctx, idem, c); err != nil {
		return nil, err
	}
	s.claims[c.ClaimID] = c
	s.emit(c, domain.EventClaimCreated, "", principalID)
	return cp(c), nil
}

func (s *stubStore) FindClaim(ctx context.Context, id string) (*domain.ExpenseClaim, error) {
	c, err := s.visible(ctx, id)
	if err != nil {
		return nil, err
	}
	return cp(c), nil
}

func (s *stubStore) AddExpenseLine(ctx context.Context, claimID string, req domain.AddExpenseLineRequest, principalID string, idem *domain.IdemKey) (*domain.ExpenseLine, error) {
	c, err := s.visible(ctx, claimID)
	if err != nil {
		return nil, err
	}
	if !domain.CanAddLine(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if req.ReceiptDocumentID != "" {
		for _, ls := range s.lines {
			for _, l := range ls {
				if l.ReceiptDocumentID == req.ReceiptDocumentID && l.VoidedAt == nil {
					return nil, domain.ErrDuplicateReceipt
				}
			}
		}
	}
	l := &domain.ExpenseLine{
		LineID: uuid.NewString(), TenantID: c.TenantID, ClaimID: claimID, Merchant: req.Merchant, ExpenseDate: req.ExpenseDate,
		Amount: req.Amount, Currency: req.Currency, Category: req.Category, ProjectCostCenter: req.ProjectCostCenter,
		ReceiptDocumentID: req.ReceiptDocumentID, ClaimTaxRecovery: req.ClaimTaxRecovery, Jurisdiction: req.Jurisdiction,
		TaxCategory: req.TaxCategory, CreatedAt: time.Now().UTC(),
	}
	if err := s.saveIdem(ctx, idem, l); err != nil {
		return nil, err
	}
	s.lines[claimID] = append(s.lines[claimID], l)
	x := *l
	return &x, nil
}

func (s *stubStore) VoidExpenseLine(ctx context.Context, claimID, lineID, reason, principalID, corr string, idem *domain.IdemKey) (*domain.ExpenseLine, error) {
	c, err := s.visible(ctx, claimID)
	if err != nil {
		return nil, err
	}
	if !domain.CanAddLine(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	for _, l := range s.lines[claimID] {
		if l.LineID == lineID {
			now := time.Now().UTC()
			l.VoidedAt, l.VoidReason = &now, reason
			s.emit(c, domain.EventLineVoided, reason, principalID)
			x := *l
			return &x, s.saveIdem(ctx, idem, x)
		}
	}
	return nil, domain.ErrLineNotFound
}

func (s *stubStore) ListLines(ctx context.Context, claimID string) ([]domain.ExpenseLine, error) {
	if _, err := s.visible(ctx, claimID); err != nil {
		return nil, err
	}
	var out []domain.ExpenseLine
	for _, l := range s.lines[claimID] {
		out = append(out, *l)
	}
	return out, nil
}

func (s *stubStore) SetLineTaxDetermination(ctx context.Context, lineID, detID string, taxable, calc float64) error {
	for _, ls := range s.lines {
		for _, l := range ls {
			if l.LineID == lineID {
				l.TaxDeterminationID, l.TaxableAmount, l.CalculatedTaxAmount = detID, taxable, calc
				return nil
			}
		}
	}
	return domain.ErrLineNotFound
}

func (s *stubStore) activeLines(id string) []domain.ExpenseLine {
	var all []domain.ExpenseLine
	for _, l := range s.lines[id] {
		all = append(all, *l)
	}
	return domain.ActiveLines(all)
}

func (s *stubStore) SubmitClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	c, err := s.lock(ctx, p.ClaimID, p.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if c.Status == domain.StatusSubmitted {
		return cp(c), nil
	}
	if !domain.CanTransition(c.Status, domain.StatusSubmitted) {
		return nil, domain.ErrInvalidTransition
	}
	lines := s.activeLines(p.ClaimID)
	if len(lines) == 0 {
		return nil, domain.ErrNoLines
	}
	v := c.SubmittedVersion + 1
	snap := domain.BuildSnapshot(c, lines, v, p.PrincipalID, time.Now())
	raw, _ := json.Marshal(snap)
	canonical, hash, _ := domain.SnapshotHash(raw)
	s.subs[p.ClaimID] = append(s.subs[p.ClaimID], domain.ExpenseClaimSubmission{
		SubmissionID: uuid.NewString(), ClaimID: p.ClaimID, VersionNo: v, Snapshot: canonical, SnapshotHash: hash,
		SubmittedBy: p.PrincipalID, SubmittedAt: time.Now().UTC(),
	})
	c.Status, c.SubmittedVersion = domain.StatusSubmitted, v
	c.Version++
	s.emit(c, domain.EventClaimSubmitted, fmt.Sprintf("version %d sha256:%s", v, hash), p.PrincipalID)
	return cp(c), s.saveIdem(ctx, p.Idem, c)
}

func (s *stubStore) RouteForApproval(ctx context.Context, p domain.RoutingParams) (*domain.ExpenseClaim, error) {
	c, err := s.lock(ctx, p.ClaimID, nil)
	if err != nil {
		return nil, err
	}
	if c.Status == domain.StatusPendingApproval {
		return cp(c), s.saveIdem(ctx, p.Idem, c)
	}
	if c.Status != domain.StatusSubmitted {
		return nil, domain.ErrInvalidTransition
	}
	c.Status, c.PolicyAssessmentResult, c.PolicyVersionID = domain.StatusPendingApproval, p.PolicyResult, p.PolicyVersionID
	c.Version++
	s.emit(c, domain.EventClaimRouted, string(p.PolicyResult), p.PrincipalID)
	return cp(c), s.saveIdem(ctx, p.Idem, c)
}

func (s *stubStore) ApproveClaim(ctx context.Context, p domain.ApproveParams) (*domain.ExpenseClaim, error) {
	c, err := s.lock(ctx, p.ClaimID, p.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if !domain.CanDecide(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	lines := s.activeLines(p.ClaimID)
	if len(lines) == 0 {
		return nil, domain.ErrNoLines
	}
	now := time.Now().UTC()
	by := p.PrincipalID
	c.Status, c.ApprovedByPrincipalID, c.ApprovedAt, c.PayableState = domain.StatusApproved, &by, &now, domain.PayablePending
	c.Version++
	s.emit(c, domain.EventClaimApproved, "", p.PrincipalID)

	var total float64
	for _, l := range lines {
		total += l.Amount
	}
	if _, exists := s.payReqs[p.ClaimID]; !exists {
		s.payReqs[p.ClaimID] = &domain.PayableRequest{
			RequestID: uuid.NewString(), TenantID: tenantOf(ctx), LegalEntityID: c.LegalEntityID, ClaimID: c.ClaimID,
			ClaimantPrincipalID: c.ClaimantPrincipalID, PaymentPreferenceRef: c.PaymentPreferenceRef, RequestedBy: p.PrincipalID,
			CorrelationID: p.CorrelationID, Amount: total, Currency: c.Currency, DueDate: p.DueDate, State: domain.PayablePending,
		}
	}
	s.emit(c, domain.EventClaimPayableRequested, "", p.PrincipalID)
	posting := domain.BuildApprovalPosting(c, lines, p.Posting, p.CorrelationID, time.Now())
	payload, _ := json.Marshal(posting)
	dup := false
	for _, q := range s.postings {
		dup = dup || (q.TenantID == tenantOf(ctx) && q.SourceEventID == posting.SourceEventID)
	}
	if !dup {
		s.postings = append(s.postings, &domain.PostingRequest{
			RequestID: uuid.NewString(), TenantID: tenantOf(ctx), LegalEntityID: c.LegalEntityID, AggregateID: c.ClaimID,
			SourceEventID: posting.SourceEventID, Payload: payload, Status: domain.PostingPending, CreatedAt: now,
		})
	}
	s.outbox = append(s.outbox, domain.EventAccountingRequested)
	return cp(c), s.saveIdem(ctx, p.Idem, c)
}

// decision is the shared body of the commands that move a PENDING_APPROVAL
// claim (or, for close, a REIMBURSABLE one) to a terminal/next status.
func (s *stubStore) command(ctx context.Context, p domain.CommandParams, allowed func(domain.ClaimStatus) bool, hist string, apply func(*domain.ExpenseClaim)) (*domain.ExpenseClaim, error) {
	c, err := s.lock(ctx, p.ClaimID, p.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if !allowed(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	apply(c)
	c.Version++
	s.emit(c, hist, p.Reason, p.PrincipalID)
	return cp(c), s.saveIdem(ctx, p.Idem, c)
}

func (s *stubStore) RejectClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.command(ctx, p, domain.CanDecide, domain.EventClaimRejected, func(c *domain.ExpenseClaim) {
		c.Status, c.RejectionReason = domain.StatusRejected, p.Reason
	})
}

func (s *stubStore) ReturnClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.command(ctx, p, domain.CanDecide, domain.EventClaimReturned, func(c *domain.ExpenseClaim) {
		c.Status, c.ReturnReason = domain.StatusReturned, p.Reason
	})
}

func (s *stubStore) CancelClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.command(ctx, p, domain.CanCancel, domain.EventClaimCancelled, func(c *domain.ExpenseClaim) { c.Status = domain.StatusCancelled })
}

func (s *stubStore) RecordPolicyException(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.command(ctx, p, domain.CanDecide, domain.EventPolicyExceptionRecorded, func(c *domain.ExpenseClaim) {
		c.HasPolicyException, c.PolicyExceptionReason = true, p.Reason
	})
}

func (s *stubStore) CloseClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.command(ctx, p, domain.CanClose, domain.EventClaimClosed, func(c *domain.ExpenseClaim) {
		now := time.Now().UTC()
		c.Status, c.ClosedAt, c.CloseReason = domain.StatusClosed, &now, p.Reason
	})
}

func (s *stubStore) IsReceiptInUse(ctx context.Context, docID string) (bool, string, string, error) {
	for id, ls := range s.lines {
		for _, l := range ls {
			if l.ReceiptDocumentID == docID && l.VoidedAt == nil {
				if c := s.claims[id]; c.TenantID != nil && *c.TenantID == tenantOf(ctx) {
					return true, id, l.LineID, nil
				}
			}
		}
	}
	return false, "", "", nil
}

func (s *stubStore) ListClaimEvents(ctx context.Context, id string) ([]domain.ExpenseClaimEvent, error) {
	if _, err := s.visible(ctx, id); err != nil {
		return nil, err
	}
	return s.events[id], nil
}

func (s *stubStore) ListSubmissions(ctx context.Context, id string) ([]domain.ExpenseClaimSubmission, error) {
	if _, err := s.visible(ctx, id); err != nil {
		return nil, err
	}
	return s.subs[id], nil
}

// ── payable relay & postings ─────────────────────────────────────────────────

func (s *stubStore) FindPayableRequest(ctx context.Context, id string) (*domain.PayableRequest, error) {
	if r, ok := s.payReqs[id]; ok && r.TenantID == tenantOf(ctx) {
		x := *r
		return &x, nil
	}
	return nil, nil
}

func (s *stubStore) ListDuePayableRequests(context.Context, int) ([]domain.PayableRequest, error) {
	var out []domain.PayableRequest
	for _, r := range s.payReqs {
		if r.State == domain.PayablePending || r.State == domain.PayableBlocked {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) CompletePayableRequest(ctx context.Context, req domain.PayableRequest, payableID, payeeRef, destID string) error {
	r := s.payReqs[req.ClaimID]
	if r.State == domain.PayableCreated {
		return nil
	}
	r.State = domain.PayableCreated
	c := s.claims[req.ClaimID]
	if c.Status == domain.StatusApproved {
		c.Status, c.PayableState, c.PayableID, c.PayeeDestinationID = domain.StatusReimbursable, domain.PayableCreated, payableID, destID
		c.Version++
	}
	return nil
}

func (s *stubStore) BlockPayableRequest(ctx context.Context, req domain.PayableRequest, reason string, _ time.Time) error {
	s.payReqs[req.ClaimID].State, s.payReqs[req.ClaimID].BlockedReason = domain.PayableBlocked, reason
	s.claims[req.ClaimID].PayableState, s.claims[req.ClaimID].PayableBlockedReason = domain.PayableBlocked, reason
	return nil
}

func (s *stubStore) RecordPayableFailure(ctx context.Context, req domain.PayableRequest, errText string, _ time.Time) error {
	s.payReqs[req.ClaimID].Attempts++
	return nil
}

func (s *stubStore) ListSettlementCandidates(context.Context, int) ([]domain.SettlementCandidate, error) {
	return nil, nil
}

func (s *stubStore) MarkSettlementChecked(context.Context, string, string) error { return nil }

func (s *stubStore) ListPostingRequests(ctx context.Context, claimID string) ([]domain.PostingRequest, error) {
	var out []domain.PostingRequest
	for _, p := range s.postings {
		if p.AggregateID == claimID && p.TenantID == tenantOf(ctx) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubStore) DispatchPostings(context.Context, int, func(domain.PostingRequest) domain.PostingOutcome) (int, error) {
	return 0, nil
}

func (s *stubStore) RequeuePostings(ctx context.Context, claimID string) (int64, error) {
	var n int64
	for _, p := range s.postings {
		if p.AggregateID == claimID && p.TenantID == tenantOf(ctx) && (p.Status == domain.PostingFailed || p.Status == domain.PostingQuarantined) {
			p.Status, p.Attempts = domain.PostingPending, 0
			n++
		}
	}
	return n, nil
}
