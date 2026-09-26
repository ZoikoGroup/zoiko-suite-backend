// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, legal_entity_id, actor_id, correlation_id). tenant_id
// is correctly omitted: RecordAccessDecision never persists a tenant
// scope on domain.AccessDecisionLog.
package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublishAuthorizationGranted_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	err := p.PublishAuthorizationGranted(context.Background(), domain.AccessDecisionLog{
		AccessDecisionID: "dec-1", PrincipalID: "principal-1", LegalEntityID: "entity-1",
		ActionType: "PAYMENT_INITIATE", DecisionOutcome: "GRANTED", DecisionBasis: "rbac:role=FINANCE_APPROVER",
		CorrelationID: "corr-1", DecidedAt: time.Now(),
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "authorization.granted", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "authorization-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "principal-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishAuthorizationDenied_RepeatEventsOnSameDecision_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishAuthorizationDenied(context.Background(), domain.AccessDecisionLog{
			AccessDecisionID: "dec-1", PrincipalID: "principal-1", LegalEntityID: "entity-1",
			ActionType: "PAYMENT_INITIATE", DecisionOutcome: "DENIED", DecisionBasis: "no_grant",
			CorrelationID: "corr-x", DecidedAt: time.Now(),
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestPublisher_PublishAccessReviewStarted(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	review := domain.AccessReview{
		ReviewID:            "rev-1",
		TenantID:            "tenant-1",
		CampaignID:          "camp-1",
		CampaignName:        "Q3 Privileged Access Review",
		ReviewerPrincipalID: "reviewer-1",
		TargetPrincipalID:   "target-1",
		RoleID:              "role-treasury-lead",
		LegalEntityID:       "entity-1",
		ReviewType:          domain.ReviewTypePrivileged,
		Status:              domain.ReviewStatusOpen,
		DueAt:               time.Now().Add(7 * 24 * time.Hour),
	}

	err := p.PublishAccessReviewStarted(context.Background(), review)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env1 := decode(t, w.msgs[0])
	assert.Equal(t, "iam.access_review.started", env1.EventType)
	assert.Equal(t, "reviewer-1", env1.ActorID)
	assert.Equal(t, "entity-1", env1.LegalEntityID)
}

func TestPublisher_PublishAccessReviewCompleted(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	decision := domain.ReviewDecisionRevoke
	reason := "Role no longer needed"
	review := domain.AccessReview{
		ReviewID:            "rev-1",
		TenantID:            "tenant-1",
		CampaignID:          "camp-1",
		CampaignName:        "Q3 Privileged Access Review",
		ReviewerPrincipalID: "reviewer-1",
		TargetPrincipalID:   "target-1",
		RoleID:              "role-treasury-lead",
		LegalEntityID:       "entity-1",
		ReviewType:          domain.ReviewTypePrivileged,
		Status:              domain.ReviewStatusCompleted,
		Decision:            &decision,
		DecisionReason:      &reason,
		DueAt:               time.Now().Add(7 * 24 * time.Hour),
	}

	err := p.PublishAccessReviewCompleted(context.Background(), review)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "iam.access_review.completed", env.EventType)
}

func TestPublisher_PublishAccessReviewLifecycle(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	review := domain.AccessReview{
		ReviewID:            "rev-1",
		TenantID:            "tenant-1",
		CampaignID:          "camp-1",
		CampaignName:        "Q3 Privileged Access Review",
		ReviewerPrincipalID: "reviewer-1",
		TargetPrincipalID:   "target-1",
		RoleID:              "role-treasury-lead",
		LegalEntityID:       "entity-1",
		ReviewType:          domain.ReviewTypePrivileged,
		Status:              domain.ReviewStatusOpen,
		DueAt:               time.Now().Add(7 * 24 * time.Hour),
	}

	err := p.PublishAccessReviewStarted(context.Background(), review)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env1 := decode(t, w.msgs[0])
	assert.Equal(t, "iam.access_review.started", env1.EventType)
	assert.Equal(t, "reviewer-1", env1.ActorID)
	assert.Equal(t, "entity-1", env1.LegalEntityID)

	decision := domain.ReviewDecisionRevoke
	reason := "Role no longer needed"
	review.Status = domain.ReviewStatusCompleted
	review.Decision = &decision
	review.DecisionReason = &reason

	err = p.PublishAccessReviewCompleted(context.Background(), review)
	require.NoError(t, err)
	require.Len(t, w.msgs, 2)

	env2 := decode(t, w.msgs[1])
	assert.Equal(t, "iam.access_review.completed", env2.EventType)
}

func TestPublisher_PublishPrivilegedSessionLifecycle(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	session := domain.PrivilegedSession{
		SessionID:        "pam-1",
		TenantID:         "tenant-1",
		PrincipalID:      "admin-1",
		TicketRef:        "CHG-1001",
		Reason:           "Emergency hotfix",
		RequestedActions: []string{"payment.release"},
		Status:           domain.PrivilegedSessionStatusActive,
		ExpiresAt:        time.Now().Add(time.Hour),
	}

	err := p.PublishPrivilegedSessionStarted(context.Background(), session)
	require.NoError(t, err)

	session.Status = domain.PrivilegedSessionStatusRevoked
	err = p.PublishPrivilegedSessionEnded(context.Background(), session)
	require.NoError(t, err)

	require.Len(t, w.msgs, 2)
	assert.Equal(t, "security.privileged_session.started", decode(t, w.msgs[0]).EventType)
	assert.Equal(t, "security.privileged_session.ended", decode(t, w.msgs[1]).EventType)
}

func TestPublisher_PublishAuthorityLimitChanged(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	tenantID := "tenant-1"
	principalID := "user-1"
	legalEntityID := "entity-1"
	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-1",
		TenantID:         tenantID,
		PrincipalID:      &principalID,
		LegalEntityID:    &legalEntityID,
		AuthorityType:    "payment_release",
		UpperLimit:       "50000",
		Currency:         "GBP",
	}

	err := p.PublishAuthorityLimitChanged(context.Background(), limit, "CREATED")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "iam.authority_limit.changed", decode(t, w.msgs[0]).EventType)
}

func TestPublisher_PublishSODPolicyPublished(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	tenantID := "tenant-1"
	rule := domain.SoDRule{
		SoDRuleID:    "sod-1",
		TenantID:     &tenantID,
		DomainCode:   "FIN",
		ActionA:      "payment.prepare",
		ActionB:      "payment.release",
		ConflictType: "MUTUALLY_EXCLUSIVE",
		ActiveFlag:   true,
	}

	err := p.PublishSoDPolicyPublished(context.Background(), rule)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "iam.sod_policy.published", decode(t, w.msgs[0]).EventType)
}

func TestPublisher_PublishPolicySetPublished(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.authorization.events", w)

	err := p.PublishPolicySetPublished(context.Background(), "2026.08.24.4", "tenant-1", "admin-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "iam.policy_set.published", decode(t, w.msgs[0]).EventType)
}
