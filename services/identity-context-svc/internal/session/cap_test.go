package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/session"
)

// The session-concurrency cap (ZS-IAM-001 §15, MAX_SESSIONS_PER_PRINCIPAL),
// added on 16 Oct 2026 with no test of its own. Fixed on 19 Oct (Group 1
// re-audit): the session just issued always survives, every revocation is
// announced, and machine principals are exempt.

type revocation struct {
	id     string
	reason domain.InvalidationReason
	corr   string
}

func capFixture(max int, existing ...string) (*fakeStore, *session.DurableCache, *[]revocation) {
	hot, store := newFakeHot(), newFakeStore()
	now := time.Now().UTC()
	for _, id := range existing {
		store.contexts[id] = &domain.SessionContext{SessionContextID: id, TenantID: "t-1", PrincipalID: "p-1",
			IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	}
	var got []revocation
	dc := session.NewDurableCache(hot, store, zap.NewNop(), max).
		WithRevocationHook(func(_ context.Context, id, principalID string, reason domain.InvalidationReason, corr string) {
			got = append(got, revocation{id, reason, corr})
		})
	return store, dc, &got
}

func issue(t *testing.T, store *fakeStore, dc *session.DurableCache, id string, exempt bool, live ...string) {
	t.Helper()
	store.livePrincipal["p-1"] = append(live, id)
	sc := domain.SessionContext{SessionContextID: id, TenantID: "t-1", PrincipalID: "p-1", CorrelationID: "corr-new",
		ConcurrencyExempt: exempt, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	require.NoError(t, dc.PersistSessionContextWithEvent(context.Background(), sc, domain.ContextResolvedEvent{
		PrincipalID: "p-1", TenantID: "t-1", SessionContextID: id, CorrelationID: "corr-new"}))
}

func TestCap_RevokesTheOldestAndAnnouncesIt(t *testing.T) {
	store, dc, got := capFixture(2, "01A", "01B")
	issue(t, store, dc, "01C", false, "01A", "01B")

	assert.Equal(t, []string{"01A"}, store.invalidated, "only the oldest goes; two remain")
	require.Len(t, *got, 1)
	assert.Equal(t, revocation{"01A", domain.InvalidationReasonSessionConcurrencyCap, "corr-new"}, (*got)[0])
}

// The new session survives even when its id sorts first, as ULIDs minted by
// another replica in the same millisecond can.
func TestCap_NeverRevokesTheSessionJustIssued(t *testing.T) {
	store, dc, _ := capFixture(2, "01M", "01N")
	issue(t, store, dc, "01A", false, "01M", "01N")

	assert.NotContains(t, store.invalidated, "01A")
	assert.Equal(t, []string{"01M"}, store.invalidated)
}

func TestCap_UnderTheLimitRevokesNothing(t *testing.T) {
	store, dc, got := capFixture(3, "01A")
	issue(t, store, dc, "01B", false, "01A")
	assert.Empty(t, store.invalidated)
	assert.Empty(t, *got)
}

func TestCap_MachinePrincipalsAreExempt(t *testing.T) {
	store, dc, got := capFixture(1, "01A", "01B", "01C")
	issue(t, store, dc, "01D", true, "01A", "01B", "01C")
	assert.Empty(t, store.invalidated)
	assert.Empty(t, *got)
}
