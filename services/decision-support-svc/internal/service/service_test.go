package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/events"
	"zoiko.io/decision-support-svc/internal/service"
	"zoiko.io/decision-support-svc/internal/testsupport"
)

var (
	tenantID    = uuid.NewString()
	legalEntity = uuid.NewString()
	requester   = "principal-requester"
	reviewer    = "principal-reviewer"
	fixedNow    = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
)

type fixture struct {
	store   *testsupport.MemStore
	authz   *testsupport.Authorizer
	history *testsupport.History
	svc     *service.Service
	clock   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		store:   testsupport.NewMemStore(),
		authz:   &testsupport.Authorizer{},
		history: &testsupport.History{},
		clock:   fixedNow,
	}
	f.svc = service.New(f.store, f.authz, f.history, zap.NewNop()).WithClock(func() time.Time { return f.clock })
	return f
}

func caller(principal string) domain.CallerContext {
	return domain.CallerContext{TenantID: tenantID, PrincipalID: principal, LegalEntityID: legalEntity, CorrelationID: "corr-1", RequestID: "req-1"}
}

func newKey() domain.IdempotencyInput {
	return domain.IdempotencyInput{Key: uuid.NewString(), RequestHash: uuid.NewString()}
}

func createRequest() domain.CreateRecommendationRequest {
	return domain.CreateRecommendationRequest{
		LegalEntityID:    legalEntity,
		SubjectType:      "purchase_order",
		SubjectReference: "po-123",
		ActionType:       "PO_ISSUE",
	}
}

func (f *fixture) create(t *testing.T, mutate func(*domain.CreateRecommendationRequest)) *domain.Recommendation {
	t.Helper()
	req := createRequest()
	if mutate != nil {
		mutate(&req)
	}
	rec, replayed, err := f.svc.CreateRecommendation(context.Background(), caller(requester), newKey(), req)
	require.NoError(t, err)
	require.False(t, replayed)
	return rec
}

func (f *fixture) decide(principal string, rec *domain.Recommendation, target domain.Status, req domain.DecisionRequest) (*domain.Recommendation, error) {
	updated, _, err := f.svc.Decide(context.Background(), caller(principal), newKey(), domain.DecisionCommand{
		RecommendationID: rec.RecommendationID,
		TargetStatus:     target,
		ExpectedVersion:  rec.Version,
		Request:          req,
	})
	return updated, err
}

// ── create: precedent engine ─────────────────────────────────────────────────

func TestCreate_MajorityGranted_RecommendsApproveWithProvenance(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("GRANTED", "GRANTED", "GRANTED", "DENIED")

	rec := f.create(t, nil)

	assert.Equal(t, domain.RecommendedActionApprove, rec.RecommendedAction)
	assert.Equal(t, 0.75, rec.ConfidenceScore)
	assert.Equal(t, 4, rec.PriorDecisionsSampled)
	assert.Equal(t, map[string]int{"GRANTED": 3, "DENIED": 1}, rec.OutcomeDistribution)
	assert.Equal(t, domain.HistoryAvailable, rec.HistoryStatus)
	assert.Equal(t, domain.SourcePrecedentEngine, rec.Source)
	assert.Equal(t, domain.PrecedentEngineModelID, rec.ModelID)
	assert.Equal(t, domain.PrecedentEngineModelVersion, rec.ModelVersion)
	assert.Equal(t, domain.AssuranceStateAdvisory, rec.AssuranceState)
	assert.Equal(t, domain.StatusReviewRequired, rec.Status)
	assert.Equal(t, 1, rec.Version)
	assert.Equal(t, domain.OversightQualifiedReviewer, rec.OversightClass, "O2 is the default")
	assert.True(t, rec.RequiresIndependentReviewer)
	assert.Equal(t, fixedNow.Add(72*time.Hour), rec.ExpiresAt)
	assert.Equal(t, requester, rec.RequestedByPrincipalID)
	assert.Equal(t, "corr-1", rec.CorrelationID, "defaults to X-Correlation-ID")
	assert.False(t, rec.DownstreamActionExecuted)

	require.Len(t, f.store.OutboxOfType(events.EventRecommendationProposed), 1)

	require.Len(t, f.history.Queries, 1)
	query := f.history.Queries[0]
	assert.Equal(t, requester, query.PrincipalID, "the caller's principal is forwarded to the decision log")
	assert.Equal(t, legalEntity, query.LegalEntityID)
	assert.Equal(t, "PO_ISSUE", query.ActionType)
}

func TestCreate_MajorityDenied_RecommendsReject(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("DENIED", "DENIED", "GRANTED")
	rec := f.create(t, nil)
	assert.Equal(t, domain.RecommendedActionReject, rec.RecommendedAction)
	assert.Equal(t, 0.667, rec.ConfidenceScore, "rounded to the column's 3 decimals")
}

func TestCreate_TieEscalatesDeterministically(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("GRANTED", "DENIED")
	for i := 0; i < 20; i++ {
		rec := f.create(t, nil)
		require.Equal(t, domain.RecommendedActionEscalate, rec.RecommendedAction)
		require.Equal(t, 0.5, rec.ConfidenceScore)
	}
}

func TestCreate_HistoryUnavailable_DegradesToNoHistory(t *testing.T) {
	f := newFixture(t)
	f.history.Err = errors.New("connection refused")
	rec := f.create(t, nil)
	assert.Equal(t, domain.RecommendedActionNoHistory, rec.RecommendedAction)
	assert.Equal(t, 0.0, rec.ConfidenceScore)
	assert.Equal(t, domain.HistoryUnavailable, rec.HistoryStatus)
}

func TestCreate_NoHistory(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	assert.Equal(t, domain.RecommendedActionNoHistory, rec.RecommendedAction)
	assert.Equal(t, domain.HistoryEmpty, rec.HistoryStatus)
}

// ── create: external proposal ────────────────────────────────────────────────

func TestCreate_ExternalProposalIsRecordedWithModelProvenance(t *testing.T) {
	f := newFixture(t)
	confidence := 0.91234
	rec := f.create(t, func(r *domain.CreateRecommendationRequest) {
		r.Proposal = &domain.ExternalProposal{
			RecommendedAction: domain.RecommendedActionReject,
			ConfidenceScore:   &confidence,
			Rationale:         "supplier on watch list",
			ModelID:           "vendor-risk-llm",
			ModelVersion:      "2026-09-01",
			PromptVersion:     "p-7",
		}
		r.EvidenceRefs = []string{"doc://invoice/1"}
	})
	assert.Equal(t, domain.SourceExternalModel, rec.Source)
	assert.Equal(t, domain.RecommendedActionReject, rec.RecommendedAction)
	assert.Equal(t, 0.912, rec.ConfidenceScore)
	assert.Equal(t, "vendor-risk-llm", rec.ModelID)
	require.NotNil(t, rec.PromptVersion)
	assert.Equal(t, "p-7", *rec.PromptVersion)
	assert.Equal(t, domain.HistoryNotApplicable, rec.HistoryStatus)
	assert.Equal(t, []string{"doc://invoice/1"}, rec.EvidenceRefs)
	assert.Empty(t, f.history.Queries, "the precedent engine is not consulted for an external proposal")
}

func TestCreate_ValidationFailures(t *testing.T) {
	confidence := 1.5
	past := fixedNow.Add(-time.Minute)
	tooFar := fixedNow.Add(91 * 24 * time.Hour)
	cases := map[string]struct {
		mutate func(*domain.CreateRecommendationRequest)
		field  string
	}{
		"missing legal entity":    {func(r *domain.CreateRecommendationRequest) { r.LegalEntityID = "" }, "legal_entity_id"},
		"legal entity not uuid":   {func(r *domain.CreateRecommendationRequest) { r.LegalEntityID = "le-us" }, "legal_entity_id"},
		"legal entity vs header":  {func(r *domain.CreateRecommendationRequest) { r.LegalEntityID = uuid.NewString() }, "legal_entity_id"},
		"missing subject type":    {func(r *domain.CreateRecommendationRequest) { r.SubjectType = " " }, "subject_type"},
		"missing subject ref":     {func(r *domain.CreateRecommendationRequest) { r.SubjectReference = "" }, "subject_reference"},
		"missing action type":     {func(r *domain.CreateRecommendationRequest) { r.ActionType = "" }, "action_type"},
		"unknown oversight class": {func(r *domain.CreateRecommendationRequest) { r.OversightClass = "O9" }, "oversight_class"},
		"expiry in the past":      {func(r *domain.CreateRecommendationRequest) { r.ExpiresAt = &past }, "expires_at"},
		"expiry too far":          {func(r *domain.CreateRecommendationRequest) { r.ExpiresAt = &tooFar }, "expires_at"},
		"empty evidence ref":      {func(r *domain.CreateRecommendationRequest) { r.EvidenceRefs = []string{""} }, "evidence_refs"},
		"proposal confidence > 1": {func(r *domain.CreateRecommendationRequest) {
			r.Proposal = &domain.ExternalProposal{RecommendedAction: "APPROVE", ConfidenceScore: &confidence, Rationale: "x", ModelID: "m", ModelVersion: "1"}
		}, "proposal.confidence_score"},
		"proposal NO_HISTORY": {func(r *domain.CreateRecommendationRequest) {
			r.Proposal = &domain.ExternalProposal{RecommendedAction: "NO_HISTORY"}
		}, "proposal.recommended_action"},
		"proposal without model": {func(r *domain.CreateRecommendationRequest) {
			ok := 0.5
			r.Proposal = &domain.ExternalProposal{RecommendedAction: "APPROVE", ConfidenceScore: &ok, Rationale: "x"}
		}, "proposal.model_id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			req := createRequest()
			tc.mutate(&req)
			_, _, err := f.svc.CreateRecommendation(context.Background(), caller(requester), newKey(), req)
			var fieldErr *domain.FieldError
			require.ErrorAs(t, err, &fieldErr)
			assert.Equal(t, tc.field, fieldErr.Field)
		})
	}
}

func TestCreate_BodyTenantMustMatchHeader(t *testing.T) {
	f := newFixture(t)
	req := createRequest()
	req.TenantID = uuid.NewString()
	_, _, err := f.svc.CreateRecommendation(context.Background(), caller(requester), newKey(), req)
	assert.ErrorIs(t, err, domain.ErrTenantMismatch)
}

func TestCreate_O4IsProhibited(t *testing.T) {
	f := newFixture(t)
	req := createRequest()
	req.OversightClass = "O4"
	_, _, err := f.svc.CreateRecommendation(context.Background(), caller(requester), newKey(), req)
	assert.ErrorIs(t, err, domain.ErrOversightProhibited)
}

func TestCreate_AuthorizationFailsClosed(t *testing.T) {
	for _, authzErr := range []error{domain.ErrAuthorizationDenied, domain.ErrAuthzServiceUnavailable} {
		f := newFixture(t)
		f.authz.Err = authzErr
		_, _, err := f.svc.CreateRecommendation(context.Background(), caller(requester), newKey(), createRequest())
		assert.ErrorIs(t, err, authzErr)
		assert.Empty(t, f.store.Outbox, "nothing is written when authorization fails")
	}
}

func TestCreate_AuthorizesRequestActionOnLegalEntity(t *testing.T) {
	f := newFixture(t)
	f.create(t, nil)
	require.Len(t, f.authz.Requests, 1)
	assert.Equal(t, service.ActionRequest, f.authz.Requests[0].ActionType)
	assert.Equal(t, legalEntity, f.authz.Requests[0].LegalEntityID)
	assert.Equal(t, tenantID, f.authz.Requests[0].TenantID)
}

// ── idempotency ──────────────────────────────────────────────────────────────

func TestCreate_IdempotentReplayReturnsOriginal(t *testing.T) {
	f := newFixture(t)
	key := newKey()

	first, replayed, err := f.svc.CreateRecommendation(context.Background(), caller(requester), key, createRequest())
	require.NoError(t, err)
	require.False(t, replayed)

	second, replayed, err := f.svc.CreateRecommendation(context.Background(), caller(requester), key, createRequest())
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.RecommendationID, second.RecommendationID)
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationProposed), 1, "a replay emits no second event")
}

func TestCreate_SameKeyDifferentRequestIsRefused(t *testing.T) {
	f := newFixture(t)
	key := newKey()
	_, _, err := f.svc.CreateRecommendation(context.Background(), caller(requester), key, createRequest())
	require.NoError(t, err)

	key.RequestHash = "different"
	_, _, err = f.svc.CreateRecommendation(context.Background(), caller(requester), key, createRequest())
	assert.ErrorIs(t, err, domain.ErrIdempotencyKeyReused)
}

func TestDecide_IdempotentReplayAfterTransition(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	key := newKey()
	cmd := domain.DecisionCommand{RecommendationID: rec.RecommendationID, TargetStatus: domain.StatusAccepted, ExpectedVersion: 1}

	first, _, err := f.svc.Decide(context.Background(), caller(reviewer), key, cmd)
	require.NoError(t, err)

	second, replayed, err := f.svc.Decide(context.Background(), caller(reviewer), key, cmd)
	require.NoError(t, err, "the replay is not re-judged against the new state")
	assert.True(t, replayed)
	assert.Equal(t, first.Version, second.Version)
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationAccepted), 1)
}

// ── decisions: lifecycle, SoD, ETag ──────────────────────────────────────────

func TestAccept_ByIndependentReviewer_RecordsDecisionWithoutExecuting(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)

	accepted, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{
		Reason:              "consistent with policy",
		DownstreamActionRef: "po-svc:command/42",
	})
	require.NoError(t, err)

	assert.Equal(t, domain.StatusAccepted, accepted.Status)
	assert.Equal(t, 2, accepted.Version)
	require.NotNil(t, accepted.DecidedByPrincipalID)
	assert.Equal(t, reviewer, *accepted.DecidedByPrincipalID)
	require.NotNil(t, accepted.DecidedAt)
	assert.Equal(t, fixedNow, *accepted.DecidedAt)
	require.NotNil(t, accepted.DecisionReason)
	assert.Equal(t, "consistent with policy", *accepted.DecisionReason)
	assert.False(t, accepted.DownstreamActionExecuted, "accepting never executes the action")

	require.Len(t, f.store.OutboxOfType(events.EventRecommendationAccepted), 1)
	governance := f.store.OutboxOfType("DECISION_SUPPORT_DISPOSITION")
	require.Len(t, governance, 1, "the human decision is queued for the governance decision log")
	var record clients.DecisionRecord
	require.NoError(t, json.Unmarshal(governance[0].Payload, &record))
	assert.Equal(t, "ACCEPTED", record.Outcome)
	assert.Equal(t, reviewer, record.ActorID)
	assert.Equal(t, legalEntity, record.LegalEntityID)

	history, err := f.svc.ListDispositions(context.Background(), caller(reviewer), rec.RecommendationID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Nil(t, history[0].FromStatus)
	assert.Equal(t, domain.StatusReviewRequired, history[0].ToStatus)
	assert.Equal(t, domain.StatusAccepted, history[1].ToStatus)
	assert.Equal(t, reviewer, history[1].DecidedByPrincipalID)
}

func TestAccept_SegregationOfDuties(t *testing.T) {
	for _, class := range []string{"", "O2", "O3"} {
		f := newFixture(t)
		rec := f.create(t, func(r *domain.CreateRecommendationRequest) { r.OversightClass = class })
		_, err := f.decide(requester, rec, domain.StatusAccepted, domain.DecisionRequest{ApprovalReference: "wfc-1"})
		assert.ErrorIs(t, err, domain.ErrSegregationOfDuties, "class %q", class)
	}
}

func TestAccept_O1AllowsSelfAcceptance(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, func(r *domain.CreateRecommendationRequest) { r.OversightClass = "O1" })
	assert.False(t, rec.RequiresIndependentReviewer)
	_, err := f.decide(requester, rec, domain.StatusAccepted, domain.DecisionRequest{})
	assert.NoError(t, err)
}

func TestReject_ByRequesterIsAllowed(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	rejected, err := f.decide(requester, rec, domain.StatusRejected, domain.DecisionRequest{Reason: "withdrawn"})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, rejected.Status)
	require.Len(t, f.store.OutboxOfType("DECISION_SUPPORT_DISPOSITION"), 1)
}

func TestAccept_O3RequiresApprovalReference(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, func(r *domain.CreateRecommendationRequest) { r.OversightClass = "O3" })
	_, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{})
	var fieldErr *domain.FieldError
	require.ErrorAs(t, err, &fieldErr)
	assert.Equal(t, "approval_reference", fieldErr.Field)

	accepted, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{ApprovalReference: "wfc-quorum-9"})
	require.NoError(t, err)
	require.NotNil(t, accepted.ApprovalReference)
	assert.Equal(t, "wfc-quorum-9", *accepted.ApprovalReference)
}

func TestReject_RequiresReason(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	_, err := f.decide(reviewer, rec, domain.StatusRejected, domain.DecisionRequest{})
	var fieldErr *domain.FieldError
	require.ErrorAs(t, err, &fieldErr)
	assert.Equal(t, "reason", fieldErr.Field)
}

func TestDecide_FieldsOnlyAllowedOnTheirOwnDisposition(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	_, err := f.decide(reviewer, rec, domain.StatusRejected, domain.DecisionRequest{Reason: "x", DownstreamActionRef: "y"})
	assert.Error(t, err)
	_, err = f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{ReplacementRecommendationID: uuid.NewString()})
	assert.Error(t, err)
}

func TestDecide_TerminalStatesRefuseEveryTransition(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	accepted, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{})
	require.NoError(t, err)

	for _, target := range []domain.Status{domain.StatusAccepted, domain.StatusRejected, domain.StatusExpired} {
		_, err := f.decide(reviewer, accepted, target, domain.DecisionRequest{Reason: "again"})
		var transitionErr *domain.TransitionError
		require.ErrorAs(t, err, &transitionErr, "target %s", target)
		assert.Equal(t, domain.StatusAccepted, transitionErr.From)
		assert.ErrorIs(t, err, domain.ErrIllegalTransition)
	}
}

func TestDecide_StaleVersionIsPreconditionFailed(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	stale := *rec
	stale.Version = 7
	_, err := f.decide(reviewer, &stale, domain.StatusAccepted, domain.DecisionRequest{})
	assert.ErrorIs(t, err, domain.ErrVersionMismatch)
}

func TestDecide_UnknownAndCrossTenantAreNotFound(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)

	unknown := *rec
	unknown.RecommendationID = uuid.NewString()
	_, err := f.decide(reviewer, &unknown, domain.StatusAccepted, domain.DecisionRequest{})
	assert.ErrorIs(t, err, domain.ErrRecommendationNotFound)

	otherTenant := caller(reviewer)
	otherTenant.TenantID = uuid.NewString()
	_, _, err = f.svc.Decide(context.Background(), otherTenant, newKey(), domain.DecisionCommand{
		RecommendationID: rec.RecommendationID, TargetStatus: domain.StatusAccepted, ExpectedVersion: 1,
	})
	assert.ErrorIs(t, err, domain.ErrRecommendationNotFound)
}

func TestDecide_LegalEntityHeaderMustMatchRecord(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	other := caller(reviewer)
	other.LegalEntityID = uuid.NewString()
	_, _, err := f.svc.Decide(context.Background(), other, newKey(), domain.DecisionCommand{
		RecommendationID: rec.RecommendationID, TargetStatus: domain.StatusAccepted, ExpectedVersion: 1,
	})
	assert.ErrorIs(t, err, domain.ErrLegalEntityScopeMismatch)
}

func TestDecide_AuthorizesDecideAction(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	f.authz.Err = domain.ErrAuthorizationDenied
	_, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{})
	assert.ErrorIs(t, err, domain.ErrAuthorizationDenied)
	last := f.authz.Requests[len(f.authz.Requests)-1]
	assert.Equal(t, service.ActionDecide, last.ActionType)
}

// ── expiry ───────────────────────────────────────────────────────────────────

func TestExpire_BeforeDeadlineNeedsReason_AfterDeadlineDoesNot(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)

	_, err := f.decide(reviewer, rec, domain.StatusExpired, domain.DecisionRequest{})
	var fieldErr *domain.FieldError
	require.ErrorAs(t, err, &fieldErr)
	assert.Equal(t, "reason", fieldErr.Field)

	f.clock = rec.ExpiresAt.Add(time.Second)
	expired, err := f.decide(reviewer, rec, domain.StatusExpired, domain.DecisionRequest{})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusExpired, expired.Status)
	assert.Empty(t, f.store.OutboxOfType("DECISION_SUPPORT_DISPOSITION"), "expiry is not a human judgement")
}

func TestAcceptAndReject_AfterDeadlineAreRefused(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t, nil)
	f.clock = rec.ExpiresAt

	_, err := f.decide(reviewer, rec, domain.StatusAccepted, domain.DecisionRequest{})
	assert.ErrorIs(t, err, domain.ErrReviewDeadlinePassed, "never auto-accept, never late-accept")
	_, err = f.decide(reviewer, rec, domain.StatusRejected, domain.DecisionRequest{Reason: "late"})
	assert.ErrorIs(t, err, domain.ErrReviewDeadlinePassed)
}

// ── supersede ────────────────────────────────────────────────────────────────

func TestSupersede_LinksReplacement(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, nil)
	replacement := f.create(t, nil)

	superseded, err := f.decide(reviewer, original, domain.StatusSuperseded, domain.DecisionRequest{
		ReplacementRecommendationID: replacement.RecommendationID,
		Reason:                      "new evidence",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuperseded, superseded.Status)
	require.NotNil(t, superseded.SupersededByRecommendationID)
	assert.Equal(t, replacement.RecommendationID, *superseded.SupersededByRecommendationID)

	reloaded, err := f.svc.GetRecommendation(context.Background(), caller(reviewer), replacement.RecommendationID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.SupersedesRecommendationID)
	assert.Equal(t, original.RecommendationID, *reloaded.SupersedesRecommendationID)
	assert.Equal(t, 2, reloaded.Version)
}

func TestSupersede_ReplacementRules(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, nil)
	otherSubject := f.create(t, func(r *domain.CreateRecommendationRequest) { r.SubjectReference = "po-999" })
	decided := f.create(t, nil)
	_, err := f.decide(reviewer, decided, domain.StatusRejected, domain.DecisionRequest{Reason: "no"})
	require.NoError(t, err)

	cases := map[string]struct {
		replacementID string
		want          error
	}{
		"self":          {original.RecommendationID, domain.ErrReplacementMismatched},
		"unknown":       {uuid.NewString(), domain.ErrReplacementNotFound},
		"other subject": {otherSubject.RecommendationID, domain.ErrReplacementMismatched},
		"not pending":   {decided.RecommendationID, domain.ErrReplacementNotPending},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.decide(reviewer, original, domain.StatusSuperseded, domain.DecisionRequest{ReplacementRecommendationID: tc.replacementID})
			assert.ErrorIs(t, err, tc.want)
		})
	}

	_, err = f.decide(reviewer, original, domain.StatusSuperseded, domain.DecisionRequest{})
	var fieldErr *domain.FieldError
	require.ErrorAs(t, err, &fieldErr)
	assert.Equal(t, "replacement_recommendation_id", fieldErr.Field)
}

// ── list ─────────────────────────────────────────────────────────────────────

func TestList_PaginatesWithCursor(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.clock = fixedNow.Add(time.Duration(i) * time.Second)
		f.create(t, nil)
	}

	page1, err := f.svc.ListRecommendations(context.Background(), caller(reviewer), domain.ListFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1.Items, 2)
	require.NotNil(t, page1.NextCursor)
	assert.True(t, page1.Items[0].CreatedAt.After(page1.Items[1].CreatedAt), "newest first")

	cursor, ok := domain.DecodeCursor(*page1.NextCursor)
	require.True(t, ok)
	page2, err := f.svc.ListRecommendations(context.Background(), caller(reviewer), domain.ListFilter{Limit: 2, After: cursor})
	require.NoError(t, err)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[1].RecommendationID, page2.Items[0].RecommendationID)

	cursor, _ = domain.DecodeCursor(*page2.NextCursor)
	page3, err := f.svc.ListRecommendations(context.Background(), caller(reviewer), domain.ListFilter{Limit: 2, After: cursor})
	require.NoError(t, err)
	assert.Len(t, page3.Items, 1)
	assert.Nil(t, page3.NextCursor)
}

func TestList_UnfilteredAuthorizesPlatformScope(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.ListRecommendations(context.Background(), caller(reviewer), domain.ListFilter{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, "PLATFORM", f.authz.Requests[0].LegalEntityID)
	assert.Equal(t, service.ActionView, f.authz.Requests[0].ActionType)
}

func TestList_EmptyIsEmptyArrayNotNull(t *testing.T) {
	f := newFixture(t)
	page, err := f.svc.ListRecommendations(context.Background(), caller(reviewer), domain.ListFilter{Limit: 10})
	require.NoError(t, err)
	assert.NotNil(t, page.Items)
	assert.Empty(t, page.Items)
}
