package handler_test

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/events"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/store"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's fail-closed/expiry semantics, tenant scoping (another
// tenant's rows are "not found", as under RLS), and its all-or-nothing
// idempotency contract (claim + change + event + replay response commit
// together or not at all), not canned responses.
type stubStore struct {
	relationships map[string]*domain.ProcessorRelationship
	subprocessors map[string][]domain.Subprocessor
	mechanisms    map[string]*domain.TransferMechanism
	assessments   map[string][]domain.TransferAssessment // by relationship_id, append order
	decisions     map[string]*domain.TransferDecision
	idempotency   map[string]*domain.IdempotencyRecord // tenant:principal:key

	pub        *stubPublisher // records the events the store "enqueued" (committed only)
	failLookup bool           // GetIdempotency fails
	failCommit bool           // the atomic mutation fails (nothing persisted)
	execs      int            // mutations actually executed
}

func newStubStore() *stubStore {
	return &stubStore{
		relationships: map[string]*domain.ProcessorRelationship{},
		subprocessors: map[string][]domain.Subprocessor{},
		mechanisms:    map[string]*domain.TransferMechanism{},
		assessments:   map[string][]domain.TransferAssessment{},
		decisions:     map[string]*domain.TransferDecision{},
		idempotency:   map[string]*domain.IdempotencyRecord{},
		pub:           &stubPublisher{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func tenantOf(ctx context.Context) string { return middleware.TenantFromContext(ctx) }

func inTenant(ctx context.Context, t *string) bool { return t != nil && *t == tenantOf(ctx) }

// run mirrors PgStore.mutate. pending events are published only when the whole
// operation commits.
func (s *stubStore) run(ctx context.Context, idem *store.Idem, fn func(emit func(events.PublishParams)) (any, error)) (any, *domain.IdempotencyRecord, error) {
	key := ""
	if idem != nil {
		key = tenantOf(ctx) + ":" + idem.PrincipalID + ":" + idem.Key
		if rec, ok := s.idempotency[key]; ok {
			return nil, rec, nil
		}
	}
	if s.failCommit {
		return nil, nil, domain.ErrStoreUnavailable
	}
	var pending []events.PublishParams
	res, err := fn(func(p events.PublishParams) { pending = append(pending, p) })
	if err != nil {
		return nil, nil, err
	}
	if idem != nil {
		code, body, rerr := idem.Render(res)
		if rerr != nil {
			return nil, nil, rerr
		}
		s.idempotency[key] = &domain.IdempotencyRecord{
			IdempotencyKey: idem.Key, TenantID: tenantOf(ctx), PrincipalID: idem.PrincipalID, Endpoint: idem.Endpoint,
			RequestHash: idem.RequestHash, ResponseCode: code, ResponseBody: body,
		}
	}
	s.execs++
	for _, p := range pending {
		_ = s.pub.Publish(ctx, p)
	}
	return res, nil, nil
}

func ev(eventType, entityID, tenantID string, m store.Meta, payload any) events.PublishParams {
	return events.PublishParams{EventType: eventType, EntityID: entityID, TenantID: tenantID, ActorID: m.PrincipalID, CorrelationID: m.CorrelationID, Payload: payload}
}

func (s *stubStore) CreateRelationship(ctx context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, m store.Meta, idem *store.Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		r := &domain.ProcessorRelationship{
			RelationshipID: uuid.New().String(), TenantID: strp(tenantID), ControllerRef: req.ControllerRef,
			ProcessorRef: req.ProcessorRef, Service: req.Service, ProcessingInstructions: req.ProcessingInstructions,
			PurposeActivityRefs: req.PurposeActivityRefs, DataCategories: req.DataCategories, SubjectClasses: req.SubjectClasses,
			ContractEvidenceRef: req.ContractEvidenceRef, Jurisdictions: req.Jurisdictions,
			Status: domain.RelationshipActive, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: m.PrincipalID,
		}
		s.relationships[r.RelationshipID] = r
		emit(ev(store.EventProcessorChanged, r.RelationshipID, tenantID, m, r))
		return r, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.ProcessorRelationship), nil, nil
}

func (s *stubStore) FindRelationship(ctx context.Context, relationshipID string) (*domain.ProcessorRelationship, error) {
	r, ok := s.relationships[relationshipID]
	if !ok || !inTenant(ctx, r.TenantID) {
		return nil, domain.ErrRelationshipNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) ListRelationships(ctx context.Context) ([]domain.ProcessorRelationship, error) {
	var out []domain.ProcessorRelationship
	for _, r := range s.relationships {
		if inTenant(ctx, r.TenantID) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) UpdateRelationshipStatus(ctx context.Context, relationshipID string, status domain.RelationshipStatus, m store.Meta, idem *store.Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		r, ok := s.relationships[relationshipID]
		if !ok || !inTenant(ctx, r.TenantID) {
			return nil, domain.ErrRelationshipNotFound
		}
		r.Status = status
		cp := *r
		emit(ev(store.EventProcessorChanged, r.RelationshipID, tenantOf(ctx), m, &cp))
		return &cp, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.ProcessorRelationship), nil, nil
}

func (s *stubStore) AttachSubprocessor(ctx context.Context, relationshipID string, req domain.AttachSubprocessorRequest, m store.Meta, idem *store.Idem) (*domain.Subprocessor, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		r, ok := s.relationships[relationshipID]
		if !ok || !inTenant(ctx, r.TenantID) {
			return nil, domain.ErrRelationshipNotFound
		}
		sp := domain.Subprocessor{
			SubprocessorID: uuid.New().String(), TenantID: r.TenantID, RelationshipID: relationshipID,
			ProviderIdentity: req.ProviderIdentity, Service: req.Service, Purpose: req.Purpose, DataScope: req.DataScope,
			ProcessingLocations: req.ProcessingLocations, OnwardSubprocessors: req.OnwardSubprocessors,
			NotificationApprovalModel: req.NotificationApprovalModel, ContractEvidenceRef: req.ContractEvidenceRef,
			CreatedAt: time.Now().UTC(), CreatedByPrincipalID: m.PrincipalID,
		}
		s.subprocessors[relationshipID] = append(s.subprocessors[relationshipID], sp)
		emit(ev(store.EventProcessorChanged, sp.SubprocessorID, tenantOf(ctx), m, &sp))
		return &sp, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.Subprocessor), nil, nil
}

func (s *stubStore) ListSubprocessors(ctx context.Context, relationshipID string) ([]domain.Subprocessor, error) {
	r, ok := s.relationships[relationshipID]
	if !ok || !inTenant(ctx, r.TenantID) {
		return nil, nil
	}
	return s.subprocessors[relationshipID], nil
}

func (s *stubStore) CreateMechanism(ctx context.Context, tenantID string, req domain.CreateTransferMechanismRequest, m store.Meta, idem *store.Idem) (*domain.TransferMechanism, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		validFrom := time.Now().UTC()
		if req.ValidFrom != nil {
			validFrom = *req.ValidFrom
		}
		mech := &domain.TransferMechanism{
			MechanismID: uuid.New().String(), TenantID: strp(tenantID), MechanismType: req.MechanismType,
			EvidenceRef: req.EvidenceRef, Conditions: req.Conditions, ValidFrom: validFrom, ValidUntil: req.ValidUntil,
			CreatedAt: time.Now().UTC(), CreatedByPrincipalID: m.PrincipalID,
		}
		s.mechanisms[mech.MechanismID] = mech
		return mech, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.TransferMechanism), nil, nil
}

func (s *stubStore) FindMechanism(ctx context.Context, mechanismID string) (*domain.TransferMechanism, error) {
	m, ok := s.mechanisms[mechanismID]
	if !ok || !inTenant(ctx, m.TenantID) {
		return nil, domain.ErrMechanismNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *stubStore) RecordAssessment(ctx context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, m store.Meta, idem *store.Idem) (*domain.TransferAssessment, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		a := domain.TransferAssessment{
			AssessmentID: uuid.New().String(), TenantID: strp(tenantID), RelationshipID: req.RelationshipID,
			Outcome: req.Outcome, ReviewerPrincipalID: m.PrincipalID, ResidualRisk: req.ResidualRisk,
			EvidenceRef: req.EvidenceRef, GovernmentAccessRisk: req.GovernmentAccessRisk,
			TechnicalMeasures: req.TechnicalMeasures, OrganizationalMeasures: req.OrganizationalMeasures,
			ReviewTriggerAt: req.ReviewTriggerAt, CreatedAt: time.Now().UTC(),
		}
		s.assessments[req.RelationshipID] = append(s.assessments[req.RelationshipID], a)
		if a.ReviewTriggerAt != nil {
			emit(ev(store.EventAssessmentExpiring, a.AssessmentID, tenantID, m, &a))
		}
		return &a, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.TransferAssessment), nil, nil
}

func (s *stubStore) FindLatestAssessment(ctx context.Context, relationshipID string) (*domain.TransferAssessment, error) {
	var latest *domain.TransferAssessment
	for i := range s.assessments[relationshipID] {
		a := s.assessments[relationshipID][i]
		if !inTenant(ctx, a.TenantID) {
			continue
		}
		if latest == nil || a.CreatedAt.After(latest.CreatedAt) {
			cp := a
			latest = &cp
		}
	}
	return latest, nil
}

func (s *stubStore) RecordDecision(ctx context.Context, tenantID string, d *domain.TransferDecision, m store.Meta, idem *store.Idem) (*domain.IdempotencyRecord, error) {
	_, ex, err := s.run(ctx, idem, func(emit func(events.PublishParams)) (any, error) {
		if d.DecisionID == "" {
			d.DecisionID = uuid.New().String()
		}
		if d.AuthorizationID == "" {
			d.AuthorizationID = d.DecisionID
		}
		d.DecidedAt = time.Now().UTC()
		d.TenantID = strp(tenantID)
		cp := *d
		s.decisions[d.DecisionID] = &cp
		emit(ev(store.EventTransferAuthorizationChanged, d.DecisionID, tenantID, m, d))
		return d, nil
	})
	return ex, err
}

func (s *stubStore) FindDecision(ctx context.Context, decisionID string) (*domain.TransferDecision, error) {
	d, ok := s.decisions[decisionID]
	if !ok || !inTenant(ctx, d.TenantID) {
		return nil, domain.ErrDecisionNotFound
	}
	return d, nil
}

func (s *stubStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	if s.failLookup {
		return nil, errors.New("lookup failed")
	}
	rec, ok := s.idempotency[tenantID+":"+principalID+":"+key]
	if !ok {
		return nil, nil
	}
	cp := *rec
	return &cp, nil
}
