package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// S1-1 / R-2 (000011): a pending support request grants nothing until the
// named approver approves it, and the schema refuses requester = approver.
func TestPgStore_SupportRequestIsLiveOnlyAfterTheNamedApproverApproves(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()
	sc := domain.SupportContext{
		SupportContextID: "sup-1", TenantID: "t-1", SupportPrincipalID: "eng-1",
		ReasonCode: domain.SupportReasonIncident, Justification: "a long enough justification", TicketRef: "INC-1",
		ApproverPrincipalID: "lead-9", RequestedByPrincipalID: "req-3", ApprovalStatus: domain.SupportPendingApproval,
		RequestedTTLSeconds: 1800, GrantedAt: now, ExpiresAt: now.Add(30 * time.Minute), EvidenceID: "ev-1", CorrelationID: "c-1",
	}
	require.NoError(t, s.InsertSupportContextWithEvent(ctx, sc, outbox.Record{}))

	live, err := s.FindLiveSupportContext(ctx, "eng-1", "t-1", now.Add(time.Minute))
	require.NoError(t, err)
	require.Nil(t, live, "a pending request is not a live grant")

	changed, err := s.ApproveSupportContextWithEvent(ctx, "sup-1", "t-1", "someone-else", now, now.Add(time.Hour), outbox.Record{})
	require.NoError(t, err)
	require.False(t, changed, "only the named approver's approval applies")

	at := time.Now().UTC()
	changed, err = s.ApproveSupportContextWithEvent(ctx, "sup-1", "t-1", "lead-9", at, at.Add(30*time.Minute), outbox.Record{})
	require.NoError(t, err)
	require.True(t, changed)
	live, err = s.FindLiveSupportContext(ctx, "eng-1", "t-1", at.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Equal(t, domain.SupportApproved, live.ApprovalStatus)
	require.Equal(t, "req-3", live.RequestedByPrincipalID)
	require.NotNil(t, live.ApprovedAt)

	changed, err = s.ApproveSupportContextWithEvent(ctx, "sup-1", "t-1", "lead-9", at, at.Add(time.Hour), outbox.Record{})
	require.NoError(t, err)
	require.False(t, changed, "a second approval changes nothing")

	// The schema refuses the requester as approver even if the code did not.
	bad := sc
	bad.SupportContextID, bad.RequestedByPrincipalID = "sup-2", "lead-9"
	require.Error(t, s.InsertSupportContextWithEvent(ctx, bad, outbox.Record{}))
}
