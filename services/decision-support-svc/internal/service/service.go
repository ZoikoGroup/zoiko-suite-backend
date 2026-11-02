// Package service holds decision-support-svc's business rules: validation,
// authorization, the recommendation state machine, segregation of duties,
// idempotent replay, and assembling the atomic write (row + disposition +
// outbox messages + idempotency record) that the store persists.
//
// The handler package turns HTTP into calls on Service; the store package
// turns Service's write sets into SQL. Neither contains business rules.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/authz"
	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
)

// Authorization action types sent to authorization-svc.
//
// DECISION_SUPPORT_REQUEST and DECISION_SUPPORT_VIEW are already in
// access-control-svc's permission taxonomy; DECISION_SUPPORT_DECIDE is new
// (see ROUTES.md "Gaps / needs clarification").
const (
	ActionRequest = "DECISION_SUPPORT_REQUEST"
	ActionView    = "DECISION_SUPPORT_VIEW"
	ActionDecide  = "DECISION_SUPPORT_DECIDE"
)

// platformScope is authorization-svc's and governance-decision-log-svc's
// literal for "not scoped to one legal entity" — used when a list is not
// filtered by legal entity and so reads across the tenant.
const platformScope = "PLATFORM"

// Store is the persistence the service needs. Every method is tenant-scoped.
type Store interface {
	FindIdempotencyRecord(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error)
	InsertRecommendation(ctx context.Context, write domain.NewRecommendationWrite) error
	GetRecommendation(ctx context.Context, tenantID, recommendationID string) (*domain.Recommendation, error)
	ListRecommendations(ctx context.Context, tenantID string, filter domain.ListFilter) ([]domain.Recommendation, error)
	ApplyTransition(ctx context.Context, write domain.TransitionWrite) error
	ListDispositions(ctx context.Context, tenantID, recommendationID string) ([]domain.Disposition, error)
}

// Authorizer asks authorization-svc. authz.Client satisfies it.
type Authorizer interface {
	CheckAllowed(ctx context.Context, req authz.Request) error
}

// HistorySource lists prior governance decisions. clients.GovernanceLogClient
// satisfies it.
type HistorySource interface {
	ListPriorDecisions(ctx context.Context, q clients.HistoryQuery) ([]clients.PriorDecision, error)
}

// Service implements every decision-support use case.
type Service struct {
	store   Store
	authz   Authorizer
	history HistorySource
	log     *zap.Logger
	now     func() time.Time
}

// New builds a Service.
func New(store Store, authorizer Authorizer, history HistorySource, log *zap.Logger) *Service {
	return &Service{
		store:   store,
		authz:   authorizer,
		history: history,
		log:     log,
		// Microsecond precision is what Postgres stores, so the create
		// response and every later read show the same timestamps.
		now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
	}
}

// WithClock replaces the clock (tests only).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// ── Create ───────────────────────────────────────────────────────────────────

// CreateRecommendation validates the request, authorizes it, computes (or
// records) the advice, and persists it in REVIEW_REQUIRED. It returns the
// stored recommendation and whether this was an idempotent replay.
func (s *Service) CreateRecommendation(ctx context.Context, caller domain.CallerContext, idem domain.IdempotencyInput, req domain.CreateRecommendationRequest) (*domain.Recommendation, bool, error) {
	now := s.now()

	// Step 1: validate the body.
	input, err := validateCreate(caller, req, now)
	if err != nil {
		return nil, false, err
	}

	// Step 2: may this principal ask for advice on this legal entity?
	if err := s.authorize(ctx, caller, input.legalEntityID, ActionRequest); err != nil {
		return nil, false, err
	}

	// Step 3: a retry with the same Idempotency-Key gets the original answer.
	if replayed, found, err := s.findReplay(ctx, caller.TenantID, idem); err != nil || found {
		return replayed, found, err
	}

	// Step 4: build the recommendation, its first history entry and its event.
	rec := s.buildRecommendation(ctx, caller, input, now)
	creation := newDisposition(rec, nil, caller, domain.DecisionRequest{}, now)
	proposedEvent, err := newLifecycleEvent(rec, caller.PrincipalID)
	if err != nil {
		return nil, false, err
	}
	idemRecord, err := newIdempotencyRecord(caller.TenantID, idem, "create_recommendation", rec)
	if err != nil {
		return nil, false, err
	}

	// Step 5: persist all of it in one transaction.
	err = s.store.InsertRecommendation(ctx, domain.NewRecommendationWrite{
		Recommendation: rec,
		Disposition:    creation,
		Outbox:         []domain.OutboxMessage{proposedEvent},
		Idempotency:    idemRecord,
	})
	if errors.Is(err, domain.ErrIdempotencyRace) {
		return s.replayAfterRace(ctx, caller.TenantID, idem)
	}
	if err != nil {
		return nil, false, err
	}
	return &rec, false, nil
}

// buildRecommendation assembles the new row from validated input, using the
// external proposal when one was supplied and the precedent engine otherwise.
func (s *Service) buildRecommendation(ctx context.Context, caller domain.CallerContext, input validatedCreate, now time.Time) domain.Recommendation {
	rec := domain.Recommendation{
		RecommendationID:            uuid.NewString(),
		TenantID:                    caller.TenantID,
		LegalEntityID:               input.legalEntityID,
		SubjectType:                 input.subjectType,
		SubjectReference:            input.subjectReference,
		ActionType:                  input.actionType,
		EvidenceRefs:                input.evidenceRefs,
		AssuranceState:              domain.AssuranceStateAdvisory,
		OversightClass:              input.oversightClass,
		RequiresIndependentReviewer: input.oversightClass.RequiresIndependentReviewer(),
		Status:                      domain.StatusReviewRequired,
		Version:                     1,
		ExpiresAt:                   input.expiresAt,
		DownstreamActionExecuted:    false,
		RequestedByPrincipalID:      caller.PrincipalID,
		CorrelationID:               input.correlationID,
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}

	if input.proposal != nil {
		applyExternalProposal(&rec, *input.proposal)
		return rec
	}

	precedent := s.computePrecedent(ctx, caller, input.legalEntityID, input.actionType)
	rec.Source = domain.SourcePrecedentEngine
	rec.ModelID = domain.PrecedentEngineModelID
	rec.ModelVersion = domain.PrecedentEngineModelVersion
	rec.RecommendedAction = precedent.action
	rec.ConfidenceScore = precedent.confidence
	rec.Rationale = precedent.rationale
	rec.PriorDecisionsSampled = precedent.sampled
	rec.OutcomeDistribution = precedent.distribution
	rec.HistoryStatus = precedent.historyStatus
	return rec
}

func applyExternalProposal(rec *domain.Recommendation, proposal domain.ExternalProposal) {
	rec.Source = domain.SourceExternalModel
	rec.ModelID = proposal.ModelID
	rec.ModelVersion = proposal.ModelVersion
	if proposal.PromptVersion != "" {
		promptVersion := proposal.PromptVersion
		rec.PromptVersion = &promptVersion
	}
	rec.RecommendedAction = proposal.RecommendedAction
	rec.ConfidenceScore = roundConfidence(*proposal.ConfidenceScore)
	rec.Rationale = proposal.Rationale
	rec.OutcomeDistribution = map[string]int{}
	rec.HistoryStatus = domain.HistoryNotApplicable
}

// ── Read ─────────────────────────────────────────────────────────────────────

// GetRecommendation returns one recommendation the caller may view.
func (s *Service) GetRecommendation(ctx context.Context, caller domain.CallerContext, recommendationID string) (*domain.Recommendation, error) {
	rec, err := s.store.GetRecommendation(ctx, caller.TenantID, recommendationID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, caller, rec.LegalEntityID, ActionView); err != nil {
		return nil, err
	}
	return rec, nil
}

// ListRecommendations returns one page of recommendations, newest first.
func (s *Service) ListRecommendations(ctx context.Context, caller domain.CallerContext, filter domain.ListFilter) (*domain.Page, error) {
	authorizationScope := filter.LegalEntityID
	if authorizationScope == "" {
		authorizationScope = platformScope
	}
	if err := s.authorize(ctx, caller, authorizationScope, ActionView); err != nil {
		return nil, err
	}

	// Ask for one extra row: if it comes back, there is a next page.
	requestedLimit := filter.Limit
	filter.Limit = requestedLimit + 1
	rows, err := s.store.ListRecommendations(ctx, caller.TenantID, filter)
	if err != nil {
		return nil, err
	}

	page := &domain.Page{Items: rows}
	if len(rows) > requestedLimit {
		page.Items = rows[:requestedLimit]
		nextCursor := domain.EncodeCursor(page.Items[requestedLimit-1])
		page.NextCursor = &nextCursor
	}
	if page.Items == nil {
		page.Items = []domain.Recommendation{}
	}
	return page, nil
}

// ListDispositions returns a recommendation's append-only decision history.
func (s *Service) ListDispositions(ctx context.Context, caller domain.CallerContext, recommendationID string) ([]domain.Disposition, error) {
	if _, err := s.GetRecommendation(ctx, caller, recommendationID); err != nil {
		return nil, err
	}
	history, err := s.store.ListDispositions(ctx, caller.TenantID, recommendationID)
	if err != nil {
		return nil, err
	}
	if history == nil {
		history = []domain.Disposition{}
	}
	return history, nil
}

// ── helpers shared by create and decide ──────────────────────────────────────

func (s *Service) authorize(ctx context.Context, caller domain.CallerContext, legalEntityID, action string) error {
	return s.authz.CheckAllowed(ctx, authz.Request{
		TenantID:      caller.TenantID,
		PrincipalID:   caller.PrincipalID,
		LegalEntityID: legalEntityID,
		ActionType:    action,
		CorrelationID: caller.CorrelationID,
		RequestID:     caller.RequestID,
	})
}

// findReplay looks up the Idempotency-Key. Same key + same request returns
// the stored response; same key + different request is refused.
func (s *Service) findReplay(ctx context.Context, tenantID string, idem domain.IdempotencyInput) (*domain.Recommendation, bool, error) {
	record, err := s.store.FindIdempotencyRecord(ctx, tenantID, idem.Key)
	if err != nil {
		return nil, false, err
	}
	if record == nil {
		return nil, false, nil
	}
	if record.RequestHash != idem.RequestHash {
		return nil, false, domain.ErrIdempotencyKeyReused
	}

	var stored domain.Recommendation
	if err := json.Unmarshal(record.ResponseBody, &stored); err != nil {
		return nil, false, err
	}
	return &stored, true, nil
}

// replayAfterRace handles the loser of two concurrent requests with the same
// Idempotency-Key: the winner has committed by now, so replay its response.
func (s *Service) replayAfterRace(ctx context.Context, tenantID string, idem domain.IdempotencyInput) (*domain.Recommendation, bool, error) {
	replayed, found, err := s.findReplay(ctx, tenantID, idem)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, domain.ErrIdempotencyKeyReused
	}
	return replayed, true, nil
}

func newIdempotencyRecord(tenantID string, idem domain.IdempotencyInput, operation string, rec domain.Recommendation) (domain.IdempotencyRecord, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return domain.IdempotencyRecord{}, err
	}
	return domain.IdempotencyRecord{
		TenantID:     tenantID,
		Key:          idem.Key,
		Operation:    operation,
		RequestHash:  idem.RequestHash,
		ResourceID:   rec.RecommendationID,
		ResponseBody: body,
	}, nil
}

// newDisposition builds the history entry for rec having just moved from
// fromStatus (nil on creation) to rec.Status.
func newDisposition(rec domain.Recommendation, fromStatus *domain.Status, caller domain.CallerContext, req domain.DecisionRequest, now time.Time) domain.Disposition {
	disposition := domain.Disposition{
		DispositionID:        uuid.NewString(),
		TenantID:             rec.TenantID,
		RecommendationID:     rec.RecommendationID,
		FromStatus:           fromStatus,
		ToStatus:             rec.Status,
		DecidedByPrincipalID: caller.PrincipalID,
		Reason:               optionalText(req.Reason),
		DownstreamActionRef:  optionalText(req.DownstreamActionRef),
		ApprovalReference:    optionalText(req.ApprovalReference),
		ResultingVersion:     rec.Version,
		CorrelationID:        caller.CorrelationID,
		RequestID:            optionalText(caller.RequestID),
		DecidedAt:            now,
	}
	if disposition.CorrelationID == "" {
		disposition.CorrelationID = rec.CorrelationID
	}
	disposition.ReplacementRecommendationID = optionalText(req.ReplacementRecommendationID)
	return disposition
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
