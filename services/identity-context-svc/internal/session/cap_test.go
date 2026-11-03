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
		WithRevocationHook(func(_ context.Context, id, principalID, _ string, reason domain.InvalidationReason, corr string) {
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

// TestSessionCap_EvictionEmitsRevokedEvent pins that every cap eviction hands
// the revoked session to the revocation hook with SESSION_CONCURRENCY_CAP as
// the reason, so the hook can publish identity.session.revoked + session.invalidated.
func TestSessionCap_EvictionEmitsRevokedEvent(t *testing.T) {
	store, dc, got := capFixture(2, "01A", "01B")
	issue(t, store, dc, "01C", false, "01A", "01B")

	require.Len(t, *got, 1, "exactly one session evicted")
	ev := (*got)[0]
	assert.Equal(t, domain.InvalidationReasonSessionConcurrencyCap, ev.reason,
		"the revocation hook must record the cap reason on the revoked session")
	assert.Equal(t, "01A", ev.id, "the oldest live session goes first")
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

// With no workload cap configured, a workload session is not capped at all.
func TestCap_MachinePrincipalsAreExemptFromTheHumanCap(t *testing.T) {
	store, dc, got := capFixture(1, "01A", "01B", "01C")
	issue(t, store, dc, "01D", true, "01A", "01B", "01C")
	assert.Empty(t, store.invalidated)
	assert.Empty(t, *got)
}

// TestSessionCap_WorkloadUsesOwnCap: service accounts and API clients are
// capped by SESSION_CAP_WORKLOAD, not by the human cap and not unbounded.
func TestSessionCap_WorkloadUsesOwnCap(t *testing.T) {
	// Human cap 1, workload cap 3: a third workload session revokes nothing,
	// although the human cap alone would have revoked two.
	store, dc, got := capFixture(1, "01W1", "01W2")
	dc.WithWorkloadCap(3)
	issue(t, store, dc, "01W3", true, "01W1", "01W2")
	assert.Empty(t, store.invalidated, "under its own cap a workload keeps its sessions")
	assert.Empty(t, *got)

	// The fourth goes over the workload cap: the oldest is revoked and announced.
	store2, dc2, got2 := capFixture(1, "01W1", "01W2", "01W3")
	dc2.WithWorkloadCap(3)
	issue(t, store2, dc2, "01W4", true, "01W1", "01W2", "01W3")
	assert.Equal(t, []string{"01W1"}, store2.invalidated)
	require.Len(t, *got2, 1)
	assert.Equal(t, domain.InvalidationReasonSessionConcurrencyCap, (*got2)[0].reason)

	// A human session is still capped by the human cap.
	store3, dc3, _ := capFixture(1, "01H1")
	dc3.WithWorkloadCap(3)
	issue(t, store3, dc3, "01H2", false, "01H1")
	assert.Equal(t, []string{"01H1"}, store3.invalidated)
}
