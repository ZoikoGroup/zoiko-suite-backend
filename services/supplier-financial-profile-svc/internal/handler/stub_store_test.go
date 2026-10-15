package handler_test

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
)

// stubStore is a working in-memory store.Store: it reuses the domain package's
// pure transition logic and mirrors PgStore's semantics (version bump,
// revision per change, evidence rows, outbox event names, idempotency
// results, non-overlapping payment terms). The SQL-level behaviour itself is
// covered by the real-Postgres tests in internal/store.
type stubStore struct {
	clock          time.Time
	profiles       map[string]*domain.SupplierFinancialProfile
	paymentTerms   map[string][]domain.PaymentTermsPeriod
	changeRequests map[string]*domain.HighRiskChangeRequest
	events         map[string][]domain.ProfileChangeEvent
	revisions      map[string][]domain.ProfileRevision
	outbox         []string // event types, in order
	idem           map[string]domain.IdemRecord
}

func newStubStore() *stubStore {
	return &stubStore{
		clock:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		profiles:       map[string]*domain.SupplierFinancialProfile{},
		paymentTerms:   map[string][]domain.PaymentTermsPeriod{},
		changeRequests: map[string]*domain.HighRiskChangeRequest{},
		events:         map[string][]domain.ProfileChangeEvent{},
		revisions:      map[string][]domain.ProfileRevision{},
		idem:           map[string]domain.IdemRecord{},
	}
}

// tick advances the stub clock so successive changes have distinct instants.
func (s *stubStore) tick() time.Time {
	s.clock = s.clock.Add(time.Hour)
	return s.clock
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(p *domain.SupplierFinancialProfile, eventType, prior, new, reason, actor string) {
	s.events[p.ProfileID] = append(s.events[p.ProfileID], domain.ProfileChangeEvent{
		EventID: uuid.New().String(), TenantID: p.TenantID, ProfileID: p.ProfileID,
		EventType: eventType, PriorValue: prior, NewValue: new, Reason: reason, ActorPrincipalID: actor, CreatedAt: s.clock,
	})
}

func (s *stubStore) finish(ctx context.Context, idem *domain.IdemScope, status int, body any) {
	if idem == nil {
		return
	}
	b, _ := json.Marshal(body)
	s.idem[middleware.TenantFromContext(ctx)+"/"+idem.Key] = domain.IdemRecord{
		Operation: idem.Operation, RequestHash: idem.RequestHash, StatusCode: status, Response: b,
	}
}

func (s *stubStore) reserve(ctx context.Context, idem *domain.IdemScope) error {
	if idem == nil {
		return nil
	}
	if _, ok := s.idem[middleware.TenantFromContext(ctx)+"/"+idem.Key]; ok {
		return domain.ErrIdempotencyRace
	}
	return nil
}

// apply mirrors PgStore.applyChange.
func (s *stubStore) apply(cur *domain.SupplierFinancialProfile, next domain.SupplierFinancialProfile, o domain.Outcome, actor, approver string) *domain.SupplierFinancialProfile {
	prior := *cur
	next.Version = cur.Version + 1
	next.UpdatedAt = s.tick()
	*cur = next
	rev := domain.ProfileRevision{
		RevisionID: uuid.New().String(), ProfileID: cur.ProfileID, Version: cur.Version, ChangeType: o.ChangeType,
		Snapshot: *cur, PriorSnapshot: &prior, ActorPrincipalID: actor, ApproverPrincipalID: approver,
		Reason: o.Reason, PayeeRelated: o.PayeeRelated, EffectiveFrom: cur.UpdatedAt,
	}
	s.revisions[cur.ProfileID] = append(s.revisions[cur.ProfileID], rev)
	s.recordEvent(cur, o.EventType, o.Prior, o.New, o.Reason, actor)
	s.outbox = append(s.outbox, domain.OutboxEventTypes(o.ChangeType)...)
	out := *cur
	return &out
}

func (s *stubStore) CreateProfile(ctx context.Context, tenantID string, req domain.CreateProfileRequest, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error) {
	if err := s.reserve(ctx, idem); err != nil {
		return nil, err
	}
	for _, p := range s.profiles {
		if p.LegalEntityID == req.LegalEntityID && p.SupplierRef == req.SupplierRef && p.Status != domain.StatusRetired &&
			p.TenantID != nil && *p.TenantID == tenantID {
			return nil, domain.ErrDuplicateProfile
		}
	}
	now := s.tick()
	p := &domain.SupplierFinancialProfile{
		ProfileID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID, SupplierRef: req.SupplierRef,
		Status: domain.StatusDraft, Version: 1, Category: req.Category, InvoiceChannel: req.InvoiceChannel,
		ProcurementCategoryRefs: req.ProcurementCategoryRefs, CreatedAt: now, CreatedByPrincipalID: principalID, UpdatedAt: now,
	}
	s.profiles[p.ProfileID] = p
	s.revisions[p.ProfileID] = append(s.revisions[p.ProfileID], domain.ProfileRevision{
		RevisionID: uuid.New().String(), ProfileID: p.ProfileID, Version: 1, ChangeType: domain.ChangeCreated,
		Snapshot: *p, ActorPrincipalID: principalID, EffectiveFrom: now,
	})
	s.recordEvent(p, domain.EventProfileCreated, "", req.SupplierRef, "", principalID)
	s.outbox = append(s.outbox, domain.OutboxEventTypes(domain.ChangeCreated)...)
	out := *p
	s.finish(ctx, idem, 201, out)
	return &out, nil
}

func (s *stubStore) FindProfile(_ context.Context, profileID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	out := *p
	return &out, nil
}

func (s *stubStore) FindProfileBySupplier(_ context.Context, legalEntityID, supplierRef string) (*domain.SupplierFinancialProfile, error) {
	var best *domain.SupplierFinancialProfile
	for _, p := range s.profiles {
		if p.LegalEntityID != legalEntityID || p.SupplierRef != supplierRef {
			continue
		}
		if best == nil || (best.Status == domain.StatusRetired && p.Status != domain.StatusRetired) {
			best = p
		}
	}
	if best == nil {
		return nil, domain.ErrProfileNotFound
	}
	out := *best
	return &out, nil
}

func (s *stubStore) ListProfiles(_ context.Context) ([]domain.SupplierFinancialProfile, error) {
	var out []domain.SupplierFinancialProfile
	for _, p := range s.profiles {
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) FindProfileAsOf(_ context.Context, profileID string, at time.Time) (*domain.ProfileAsOf, error) {
	revs := s.revisions[profileID]
	for i := len(revs) - 1; i >= 0; i-- {
		if !revs[i].EffectiveFrom.After(at) {
			res := &domain.ProfileAsOf{Profile: revs[i].Snapshot, AsOf: at, RevisionVersion: revs[i].Version, EffectiveFrom: revs[i].EffectiveFrom}
			for _, t := range s.paymentTerms[profileID] {
				if !at.Before(t.EffectiveFrom) && (t.EffectiveTo == nil || at.Before(*t.EffectiveTo)) {
					t := t
					res.PaymentTerms = &t
				}
			}
			return res, nil
		}
	}
	return nil, domain.ErrNoRevisionAsOf
}

func (s *stubStore) ListProfileHistory(_ context.Context, profileID string) ([]domain.ProfileRevision, error) {
	revs := append([]domain.ProfileRevision(nil), s.revisions[profileID]...)
	sort.Slice(revs, func(i, j int) bool { return revs[i].Version < revs[j].Version })
	for i := 0; i+1 < len(revs); i++ {
		t := revs[i+1].EffectiveFrom
		revs[i].EffectiveTo = &t
	}
	return revs, nil
}

func (s *stubStore) LastPayeeChange(_ context.Context, profileID string) (*domain.LastPayeeChange, error) {
	revs := s.revisions[profileID]
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].PayeeRelated {
			return &domain.LastPayeeChange{
				ProfileID: profileID, PrincipalID: revs[i].ActorPrincipalID, ApproverPrincipalID: revs[i].ApproverPrincipalID,
				ChangedAt: revs[i].EffectiveFrom, Version: revs[i].Version, ChangeType: revs[i].ChangeType,
			}, nil
		}
	}
	return nil, domain.ErrNoPayeeChange
}

func (s *stubStore) Transition(ctx context.Context, profileID string, cmd domain.Command, expectedVersion *int, reason, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error) {
	if err := s.reserve(ctx, idem); err != nil {
		return nil, err
	}
	cur, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if expectedVersion != nil && *expectedVersion != cur.Version {
		return nil, domain.ErrStaleVersion
	}
	next, o, err := domain.ApplyCommand(*cur, cmd, reason)
	if err != nil {
		return nil, err
	}
	out := s.apply(cur, next, o, principalID, "")
	s.finish(ctx, idem, 200, out)
	return out, nil
}

func (s *stubStore) AmendProfile(ctx context.Context, profileID string, req domain.AmendProfileRequest, principalID string, idem *domain.IdemScope) (*domain.AmendResult, error) {
	proposals := req.HighRiskProposals()
	if !req.LowRiskPresent() && len(proposals) == 0 {
		return nil, domain.ErrNoChanges
	}
	if err := s.reserve(ctx, idem); err != nil {
		return nil, err
	}
	cur, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != cur.Version {
		return nil, domain.ErrStaleVersion
	}
	if cur.Status == domain.StatusRetired {
		return nil, domain.ErrInvalidTransition
	}
	snapshotBefore := *cur
	res := &domain.AmendResult{Profile: *cur}
	if req.LowRiskPresent() {
		next, o, err := domain.ApplyAmendLowRisk(*cur, req)
		if err != nil {
			return nil, err
		}
		res.Profile = *s.apply(cur, next, o, principalID, "")
	}
	for _, pr := range proposals {
		c := s.newChangeRequest(&snapshotBefore, pr, principalID)
		res.PendingChanges = append(res.PendingChanges, *c)
	}
	s.finish(ctx, idem, res.HTTPStatus(), res.Body())
	return res, nil
}

func (s *stubStore) newChangeRequest(p *domain.SupplierFinancialProfile, req domain.ProposeHighRiskChangeRequest, principalID string) *domain.HighRiskChangeRequest {
	old := domain.HighRiskFieldValue(*p, req.Field)
	c := &domain.HighRiskChangeRequest{
		ChangeRequestID: uuid.New().String(), TenantID: p.TenantID, ProfileID: p.ProfileID, Field: req.Field,
		OldValue: old, NewValue: req.NewValue, Reason: req.Reason, Status: domain.ChangeRequestPending,
		ProposedByPrincipalID: principalID, ProposedAt: s.clock,
	}
	s.changeRequests[c.ChangeRequestID] = c
	s.recordEvent(p, domain.EventHighRiskProposed, old, req.NewValue, req.Reason, principalID)
	return c
}

// overlaps replicates the Postgres EXCLUDE constraint's [from, to) semantics.
func overlaps(aFrom time.Time, aTo *time.Time, bFrom time.Time, bTo *time.Time) bool {
	aEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if aTo != nil {
		aEnd = *aTo
	}
	bEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if bTo != nil {
		bEnd = *bTo
	}
	return aFrom.Before(bEnd) && bFrom.Before(aEnd)
}

func (s *stubStore) ChangePaymentTerms(ctx context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string, idem *domain.IdemScope) (*domain.PaymentTermsPeriod, error) {
	if err := s.reserve(ctx, idem); err != nil {
		return nil, err
	}
	cur, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != cur.Version {
		return nil, domain.ErrStaleVersion
	}
	if cur.Status == domain.StatusRetired {
		return nil, domain.ErrInvalidTransition
	}
	for _, existing := range s.paymentTerms[profileID] {
		if overlaps(req.EffectiveFrom, req.EffectiveTo, existing.EffectiveFrom, existing.EffectiveTo) {
			return nil, domain.ErrOverlappingPaymentTerms
		}
	}
	t := domain.PaymentTermsPeriod{
		PaymentTermsID: uuid.New().String(), TenantID: cur.TenantID, ProfileID: profileID, TermsCode: req.TermsCode,
		EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo, CreatedAt: s.clock, CreatedByPrincipalID: principalID,
	}
	s.paymentTerms[profileID] = append(s.paymentTerms[profileID], t)
	s.apply(cur, *cur, domain.Outcome{ChangeType: domain.ChangePaymentTermsChanged, EventType: domain.EventPaymentTermsChanged, New: req.TermsCode}, principalID, "")
	s.finish(ctx, idem, 201, t)
	return &t, nil
}

func (s *stubStore) ListPaymentTerms(_ context.Context, profileID string) ([]domain.PaymentTermsPeriod, error) {
	return s.paymentTerms[profileID], nil
}

func (s *stubStore) ProposeHighRiskChange(ctx context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, error) {
	if err := s.reserve(ctx, idem); err != nil {
		return nil, err
	}
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != p.Version {
		return nil, domain.ErrStaleVersion
	}
	if p.Status == domain.StatusRetired {
		return nil, domain.ErrInvalidTransition
	}
	c := s.newChangeRequest(p, req, principalID)
	s.finish(ctx, idem, 201, c)
	return c, nil
}

func (s *stubStore) FindChangeRequest(_ context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error) {
	c, ok := s.changeRequests[changeRequestID]
	if !ok {
		return nil, domain.ErrChangeRequestNotFound
	}
	out := *c
	return &out, nil
}

func (s *stubStore) DecideHighRiskChange(ctx context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error) {
	if err := s.reserve(ctx, idem); err != nil {
		return nil, nil, err
	}
	c, ok := s.changeRequests[changeRequestID]
	if !ok || c.Status != domain.ChangeRequestPending {
		return nil, nil, domain.ErrChangeRequestNotPending
	}
	if c.ProposedByPrincipalID == principalID {
		return nil, nil, domain.ErrSoDConflict
	}
	cur := s.profiles[c.ProfileID]
	if req.ExpectedVersion != nil && *req.ExpectedVersion != cur.Version {
		return nil, nil, domain.ErrStaleVersion
	}
	now := s.tick()
	var p *domain.SupplierFinancialProfile
	if req.Approve {
		if domain.HighRiskFieldValue(*cur, c.Field) != c.OldValue {
			return nil, nil, domain.ErrStaleVersion
		}
		next := *cur
		if err := domain.ApplyHighRiskField(&next, c.Field, c.NewValue); err != nil {
			return nil, nil, err
		}
		p = s.apply(cur, next, domain.Outcome{
			ChangeType: domain.ChangeHighRiskApplied, EventType: domain.EventHighRiskApplied, Prior: c.OldValue, New: c.NewValue,
			Reason: req.Reason, PayeeRelated: c.Field.IsPayeeRelated(),
		}, c.ProposedByPrincipalID, principalID)
		c.Status = domain.ChangeRequestApproved
	} else {
		out := *cur
		p = &out
		c.Status = domain.ChangeRequestRejected
		s.recordEvent(cur, domain.EventHighRiskRejected, c.OldValue, c.NewValue, req.Reason, principalID)
	}
	c.DecidedByPrincipalID = &principalID
	c.DecidedAt = &now
	c.DecisionReason = req.Reason
	s.outbox = append(s.outbox, domain.LegacyHighRiskDecided)
	out := *c
	s.finish(ctx, idem, 200, map[string]any{"change_request": out, "profile": p})
	return &out, p, nil
}

func (s *stubStore) ListChangeEvents(_ context.Context, profileID string) ([]domain.ProfileChangeEvent, error) {
	return s.events[profileID], nil
}

func (s *stubStore) LookupIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error) {
	rec, ok := s.idem[middleware.TenantFromContext(ctx)+"/"+key]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}
