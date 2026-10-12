package context_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/sod"
)

// GOV-01's AttachSupportContext, and negative path #3 of its minimum
// acceptance set: "support-session context expires automatically".
//
// None of this existed before — there was no support context, so the negative
// path had nothing to assert against.
//
// The spec's shared invariant is the bar these tests hold the implementation
// to: "Emergency elevation is scoped, time-limited, independently approved,
// fully evidenced and followed by reconciliation/review." There is one test
// per clause.

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeSupportStore struct {
	contexts  map[string]*domain.SupportContext
	events    []outbox.Record
	insertErr error
}

func newFakeSupportStore() *fakeSupportStore {
	return &fakeSupportStore{contexts: map[string]*domain.SupportContext{}}
}

func (f *fakeSupportStore) InsertSupportContextWithEvent(_ context.Context, sc domain.SupportContext, rec outbox.Record) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	// Mirrors the schema CHECK, so a test cannot pass here and fail in Postgres.
	if sc.ApproverPrincipalID == sc.SupportPrincipalID {
		return domain.ErrSupportSelfApproval
	}
	cp := sc
	f.contexts[sc.SupportContextID] = &cp
	if rec.EventID != "" {
		f.events = append(f.events, rec)
	}
	return nil
}

// Mirrors the store: only a pending, unrevoked request naming this approver.
func (f *fakeSupportStore) ApproveSupportContextWithEvent(_ context.Context, id, tenantID, approver string, grantedAt, expiresAt time.Time, rec outbox.Record) (bool, error) {
	sc, ok := f.contexts[id]
	if !ok || sc.TenantID != tenantID || sc.ApproverPrincipalID != approver ||
		sc.ApprovalStatus != domain.SupportPendingApproval || sc.RevokedAt != nil {
		return false, nil
	}
	sc.ApprovalStatus, sc.ApprovedAt, sc.GrantedAt, sc.ExpiresAt = domain.SupportApproved, &grantedAt, grantedAt, expiresAt
	f.events = append(f.events, rec)
	return true, nil
}

func (f *fakeSupportStore) FindSupportContext(_ context.Context, id, tenantID string) (*domain.SupportContext, error) {
	sc, ok := f.contexts[id]
	if !ok || sc.TenantID != tenantID {
		return nil, nil
	}
	return sc, nil
}

func (f *fakeSupportStore) FindLiveSupportContext(_ context.Context, supportPrincipalID, tenantID string, at time.Time) (*domain.SupportContext, error) {
	for _, sc := range f.contexts {
		if sc.SupportPrincipalID == supportPrincipalID && sc.TenantID == tenantID && sc.Live(at) {
			return sc, nil
		}
	}
	return nil, nil
}

func (f *fakeSupportStore) RevokeSupportContextWithEvent(_ context.Context, id, tenantID, reason string, at time.Time, rec outbox.Record) (bool, error) {
	sc, ok := f.contexts[id]
	if !ok || sc.TenantID != tenantID || sc.RevokedAt != nil {
		return false, nil
	}
	sc.RevokedAt = &at
	sc.RevocationReason = &reason
	f.events = append(f.events, rec)
	return true, nil
}

// FindUnreviewedExpiredSupportContextsAllTenants is the cross-tenant sibling
// the background reconciler uses. Deliberately ignores tenant, which is the
// whole point: a sweep that needed a tenant could only ever cover tenants
// somebody thought to ask about.
func (f *fakeSupportStore) FindUnreviewedExpiredSupportContextsAllTenants(_ context.Context, before time.Time, limit int) ([]domain.SupportContext, error) {
	var out []domain.SupportContext
	for _, sc := range f.contexts {
		if sc.ReviewedAt != nil {
			continue
		}
		if !sc.ExpiresAt.After(before) || sc.RevokedAt != nil {
			out = append(out, *sc)
		}
	}
	return out, nil
}

func (f *fakeSupportStore) FindUnreviewedExpiredSupportContexts(_ context.Context, tenantID string, before time.Time, limit int) ([]domain.SupportContext, error) {
	var out []domain.SupportContext
	for _, sc := range f.contexts {
		if sc.TenantID != tenantID || sc.ReviewedAt != nil {
			continue
		}
		if !sc.ExpiresAt.After(before) || sc.RevokedAt != nil {
			out = append(out, *sc)
		}
	}
	return out, nil
}

// Mirrors the store: append-only, and the event is recorded only when the
// review is, so a repeat cannot add a second event.
func (f *fakeSupportStore) MarkSupportContextReviewedWithEvent(_ context.Context, id, tenantID, reviewer string, at time.Time, rec outbox.Record) (bool, error) {
	sc, ok := f.contexts[id]
	if !ok || sc.TenantID != tenantID || sc.ReviewedAt != nil {
		return false, nil
	}
	sc.ReviewedAt = &at
	sc.ReviewedBy = &reviewer
	f.events = append(f.events, rec)
	return true, nil
}

// conflictingSoD always reports a conflict.
type conflictingSoD struct{}

func (conflictingSoD) CheckConflict(_ context.Context, _ sod.Request) (*sod.Decision, error) {
	return &sod.Decision{Result: "CONFLICT", Reason: "maker and checker share a role"}, sod.ErrConflict
}

// unavailableSoD cannot reach GOV-04.
type unavailableSoD struct{}

func (unavailableSoD) CheckConflict(_ context.Context, _ sod.Request) (*sod.Decision, error) {
	return nil, sod.ErrUnavailable
}

// clearSoD reports no conflict.
type clearSoD struct{ calls []sod.Request }

func (c *clearSoD) CheckConflict(_ context.Context, r sod.Request) (*sod.Decision, error) {
	c.calls = append(c.calls, r)
	return &sod.Decision{Result: "NO_CONFLICT", RuleVersion: "v3"}, nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

type supportFixture struct {
	store *fakeSupportStore
	sod   sod.Checker
}

func newSupportFixture() *supportFixture {
	return &supportFixture{store: newFakeSupportStore(), sod: &clearSoD{}}
}

func (f *supportFixture) build() *identityctx.SupportService {
	return identityctx.NewSupportService(
		f.store,
		events.NewPublisherWithSink(zap.NewNop(), "test-topic", noopSink{}),
		f.sod,
		siem.New("", "identity-context-svc", zap.NewNop()),
		identityctx.DefaultSupportPolicy(),
		zap.NewNop(),
	)
}

type noopSink struct{}

func (noopSink) Emit(_ context.Context, _ outbox.Record) error { return nil }

// requesterID makes support requests in these tests. It is never the
// approver: the request names "support-lead-9", who approves it themselves.
const requesterID = "support-requester-3"

// attachApproved is the full two-party flow: a request, then the named
// approver's own approval. The tests below used to attach with the caller set
// to the approver, which is exactly the hole S1-1 / R-2 recorded.
func attachApproved(svc *identityctx.SupportService, req domain.AttachSupportContextRequest) (*domain.SupportContext, error) {
	sc, err := svc.Attach(context.Background(), req, requesterID)
	if err != nil {
		return nil, err
	}
	return svc.Approve(context.Background(), sc.SupportContextID, req.TenantID, req.ApproverPrincipalID, "corr-approve")
}

func validAttachRequest() domain.AttachSupportContextRequest {
	return domain.AttachSupportContextRequest{
		TenantID:            "tenant-a",
		SupportPrincipalID:  "support-eng-1",
		ReasonCode:          domain.SupportReasonIncident,
		Justification:       "Customer reports envelopes rejected estate-wide since 09:15; need session evidence.",
		TicketRef:           "INC-4471",
		ApproverPrincipalID: "support-lead-9",
		TTLSeconds:          1800,
		CorrelationID:       "corr-1",
	}
}

// ── "scoped" ─────────────────────────────────────────────────────────────────

func TestAttachSupportContext_GrantsScopedToOneTenant(t *testing.T) {
	f := newSupportFixture()
	sc, err := attachApproved(f.build(), validAttachRequest())

	require.NoError(t, err)
	assert.Equal(t, "tenant-a", sc.TenantID)
	assert.NotEmpty(t, sc.EvidenceID)
	assert.Nil(t, sc.SubjectPrincipalID, "an unnarrowed grant is tenant-wide")
}

func TestAttachSupportContext_NarrowedGrantCoversOnlyItsSubject(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	subject := "principal-77"
	req.SubjectPrincipalID = &subject

	sc, err := attachApproved(f.build(), req)
	require.NoError(t, err)

	assert.True(t, sc.Covers("principal-77"))
	assert.False(t, sc.Covers("principal-88"),
		"a grant narrowed to one principal must not silently widen")
}

// ── "time-limited" — negative path #3 ────────────────────────────────────────

func TestSupportContext_ExpiresAutomatically(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.TTLSeconds = 60

	sc, err := attachApproved(f.build(), req)
	require.NoError(t, err)

	// Live at grant time...
	assert.True(t, sc.Live(sc.GrantedAt.Add(30*time.Second)))
	// ...and NOT live one instant past expiry, with no revocation, no sweep
	// and nothing else having to run. That is what "expires automatically"
	// means: expiry is a property of the record, not an event somebody has to
	// remember to fire.
	assert.False(t, sc.Live(sc.ExpiresAt),
		"expiry is inclusive of the boundary — a grant is dead at expires_at, not after it")
	assert.False(t, sc.Live(sc.ExpiresAt.Add(time.Nanosecond)))
}

func TestVerifySupportContext_ReportsExpiryDistinctlyFromAbsence(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	req := validAttachRequest()
	req.TTLSeconds = 60

	sc, err := attachApproved(svc, req)
	require.NoError(t, err)

	// Force it past its window.
	stored := f.store.contexts[sc.SupportContextID]
	stored.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	stored.GrantedAt = time.Now().UTC().Add(-2 * time.Minute)

	_, err = svc.Verify(context.Background(), sc.SupportContextID, "tenant-a", "support-eng-1", "")
	require.ErrorIs(t, err, domain.ErrSupportContextExpired,
		"an operator debugging a failed support session needs expired apart from never-existed")

	_, err = svc.Verify(context.Background(), "sup-does-not-exist", "tenant-a", "support-eng-1", "")
	require.ErrorIs(t, err, domain.ErrSupportContextNotFound)
}

func TestAttachSupportContext_RefusesWindowBeyondMaximum(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.TTLSeconds = int((30 * 24 * time.Hour).Seconds())

	_, err := attachApproved(f.build(), req)

	// REFUSED, not clamped. Clamping would issue a grant with a window nobody
	// requested and nobody reviewed, and the requester would believe they held
	// it far longer than they do.
	require.ErrorIs(t, err, domain.ErrSupportTTLExceeded)
}

func TestVerifySupportContext_RevokedGrantIsUnusableImmediately(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	require.NoError(t, svc.Revoke(context.Background(), sc.SupportContextID, "tenant-a", "incident closed", "support-lead-9", "corr-2"))

	_, err = svc.Verify(context.Background(), sc.SupportContextID, "tenant-a", "support-eng-1", "")
	require.Error(t, err, "a revoked grant must stop working now, not at expiry")
}

// ── "independently approved" ─────────────────────────────────────────────────

func TestAttachSupportContext_RefusesSelfApproval(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.ApproverPrincipalID = req.SupportPrincipalID

	_, err := f.build().Attach(context.Background(), req, req.SupportPrincipalID)

	require.ErrorIs(t, err, domain.ErrSupportSelfApproval,
		"self-approval defeats maker-checker entirely")
}

func TestAttachSupportContext_RequiresAnApprover(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.ApproverPrincipalID = ""

	_, err := attachApproved(f.build(), req)
	require.Error(t, err, "there is no single-party form of this command")
}

// ── Segregation of duties (GOV-04) ───────────────────────────────────────────

func TestAttachSupportContext_RefusedOnSoDConflict(t *testing.T) {
	f := newSupportFixture()
	f.sod = conflictingSoD{}

	_, err := attachApproved(f.build(), validAttachRequest())

	require.ErrorIs(t, err, sod.ErrConflict)
	assert.Empty(t, f.store.contexts, "a conflicting grant must not be written")
}

// TestAttachSupportContext_FailsClosedWhenSoDUnavailable is the one that
// matters most.
//
// A conflict check that did not run is not a check that passed. Treating "we
// could not ask" as "no conflict" means a GOV-04 outage silently disables the
// control rather than blocking on it, and nobody finds out until an audit.
func TestAttachSupportContext_FailsClosedWhenSoDUnavailable(t *testing.T) {
	f := newSupportFixture()
	f.sod = unavailableSoD{}

	_, err := attachApproved(f.build(), validAttachRequest())

	require.ErrorIs(t, err, sod.ErrUnavailable)
	assert.Empty(t, f.store.contexts)
}

// TestAttachSupportContext_AsksSoDAboutTheRightPair pins WHAT is asked.
//
// GOV-03 answers "may this principal attach support contexts"; GOV-04 answers
// "may THIS principal approve an elevation for THAT one". Asking GOV-04 the
// first question would make it a duplicate authorization check and no
// segregation control at all.
func TestAttachSupportContext_AsksSoDAboutTheRightPair(t *testing.T) {
	f := newSupportFixture()
	checker := &clearSoD{}
	f.sod = checker

	_, err := attachApproved(f.build(), validAttachRequest())
	require.NoError(t, err)

	// Asked at the request, and again at the approval with the approver who
	// actually acted. The maker is the requester, never the approver: the
	// old single call passed the caller as both.
	require.Len(t, checker.calls, 2)
	for _, call := range checker.calls {
		assert.Equal(t, requesterID, call.MakerPrincipalID)
		assert.Equal(t, "support-lead-9", call.CheckerPrincipalID)
		assert.Equal(t, "support-eng-1", call.SubjectPrincipalID)
		assert.Equal(t, identityctx.ActionAttachSupportContext, call.ActionType)
		assert.Equal(t, "tenant-a", call.TenantID)
	}
}

// ── "fully evidenced" ────────────────────────────────────────────────────────

func TestAttachSupportContext_RequiresAUsableJustification(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.Justification = "asdf"

	_, err := attachApproved(f.build(), req)

	// A justification nobody can act on is not evidence, it is a checkbox.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "justification")
}

func TestAttachSupportContext_RequiresARecognisedReason(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.ReasonCode = "BECAUSE_I_SAID_SO"

	_, err := attachApproved(f.build(), req)
	require.Error(t, err, "a reason nobody can group a report by is not a reason")
}

func TestAttachSupportContext_RequiresATicketReference(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.TicketRef = "   "

	_, err := attachApproved(f.build(), req)
	require.Error(t, err)
}

func TestAttachSupportContext_EmitsItsEventAtomically(t *testing.T) {
	f := newSupportFixture()
	sc, err := attachApproved(f.build(), validAttachRequest())
	require.NoError(t, err)

	// A privileged elevation the security team was never told about is the
	// scenario the control exists to prevent, so the event travels in the same
	// transaction as the grant.
	require.Len(t, f.store.events, 1)
	assert.Equal(t, events.EventSupportContextAttached, f.store.events[0].EventType)
	assert.Equal(t, sc.SupportContextID, f.store.events[0].PartitionKey)

	// The justification is on the wire: a security team watching this topic
	// should not have to query this service to learn why somebody was granted
	// access to a customer tenant.
	assert.Contains(t, string(f.store.events[0].Payload), "envelopes rejected estate-wide")
}

// ── "followed by reconciliation/review" ──────────────────────────────────────

func TestReconcile_ReportsExpiredGrantsThatWereNeverReviewed(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	f.store.contexts[sc.SupportContextID].ExpiresAt = time.Now().UTC().Add(-time.Hour)

	pending, err := svc.Reconcile(context.Background(), "tenant-a", 10)
	require.NoError(t, err)

	// This is the half that is normally missing: the grant expires, nobody
	// looks, and the control is decorative.
	require.Len(t, pending, 1)
	assert.Equal(t, sc.SupportContextID, pending[0].SupportContextID)
}

func TestReconcile_DoesNotAutoApprove(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)
	f.store.contexts[sc.SupportContextID].ExpiresAt = time.Now().UTC().Add(-time.Hour)

	_, err = svc.Reconcile(context.Background(), "tenant-a", 10)
	require.NoError(t, err)

	// An automatic review is not a review. Reconcile reports; a human marks.
	assert.Nil(t, f.store.contexts[sc.SupportContextID].ReviewedAt)

	require.NoError(t, svc.MarkReviewed(context.Background(), sc.SupportContextID, "tenant-a", "security-lead-3", "corr-review"))
	require.NotNil(t, f.store.contexts[sc.SupportContextID].ReviewedAt)
	assert.Equal(t, "security-lead-3", *f.store.contexts[sc.SupportContextID].ReviewedBy)

	// And once reviewed it drops out of the pending report.
	pending, err := svc.Reconcile(context.Background(), "tenant-a", 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// ── Tenant isolation ─────────────────────────────────────────────────────────

func TestVerifySupportContext_ForeignTenantIsNotFound(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	_, err = svc.Verify(context.Background(), sc.SupportContextID, "tenant-b", "support-eng-1", "")
	require.ErrorIs(t, err, domain.ErrSupportContextNotFound)
}

func TestVerifySupportContext_AnotherEngineersGrantIsNotFound(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	// Holding the id of somebody else's grant must not be enough to use it.
	_, err = svc.Verify(context.Background(), sc.SupportContextID, "tenant-a", "some-other-engineer", "")
	require.ErrorIs(t, err, domain.ErrSupportContextNotFound)
}

func TestRevokeSupportContext_IsIdempotent(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	require.NoError(t, svc.Revoke(context.Background(), sc.SupportContextID, "tenant-a", "done", "lead", "c1"))
	before := len(f.store.events)

	// Re-revoking reports success — the caller's intended end state holds —
	// but must not emit a second event.
	require.NoError(t, svc.Revoke(context.Background(), sc.SupportContextID, "tenant-a", "done again", "lead", "c2"))
	assert.Equal(t, before, len(f.store.events))

	// And the first reason stands: append-only, same doctrine as session
	// invalidation, because the reason is the part an investigation reads.
	assert.Equal(t, "done", *f.store.contexts[sc.SupportContextID].RevocationReason)
}

func TestAttachSupportContext_DefaultTTLAppliesWhenNoneRequested(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.TTLSeconds = 0

	sc, err := attachApproved(f.build(), req)
	require.NoError(t, err)

	window := sc.ExpiresAt.Sub(sc.GrantedAt)
	assert.Equal(t, identityctx.DefaultSupportPolicy().DefaultTTL, window)
	assert.True(t, window <= identityctx.DefaultSupportPolicy().MaxTTL)
}

func TestSupportJustificationIsTrimmedNotPadded(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.Justification = "   " + strings.Repeat("x", 5) + "   "

	// Whitespace must not be able to satisfy the minimum length.
	_, err := attachApproved(f.build(), req)
	require.Error(t, err)
}


// ── The review is evidenced (2026-09-28) ─────────────────────────────────────
//
// Attach and revoke always emitted events; the review — the step that closes
// the break-glass loop — wrote two columns and nothing else.

func TestMarkReviewed_EmitsExactlyOneReviewedEvent(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)
	before := len(f.store.events)

	require.NoError(t, svc.MarkReviewed(context.Background(), sc.SupportContextID, "tenant-a", "security-lead-3", "corr-review"))

	require.Len(t, f.store.events, before+1, "a review must leave an event in the governance stream")
	rec := f.store.events[len(f.store.events)-1]
	assert.Equal(t, events.EventSupportContextReviewed, rec.EventType)
	assert.Equal(t, "tenant-a", rec.TenantID)

	var env struct {
		ActorID       string         `json:"actor_id"`
		CorrelationID string         `json:"correlation_id"`
		Payload       map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(rec.Payload, &env))
	assert.Equal(t, "security-lead-3", env.ActorID)
	assert.Equal(t, "corr-review", env.CorrelationID)
	assert.Equal(t, sc.SupportContextID, env.Payload["support_context_id"])
	assert.Equal(t, "security-lead-3", env.Payload["reviewed_by"])
	assert.Equal(t, sc.SupportPrincipalID, env.Payload["support_principal_id"])
	assert.Equal(t, sc.ApproverPrincipalID, env.Payload["approver_principal_id"])
}

// Append-only: the first reviewer stands, and a repeat writes no second event.
func TestMarkReviewed_RepeatIsANoOpWithNoSecondEvent(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	require.NoError(t, svc.MarkReviewed(context.Background(), sc.SupportContextID, "tenant-a", "security-lead-3", "c1"))
	after := len(f.store.events)
	require.NoError(t, svc.MarkReviewed(context.Background(), sc.SupportContextID, "tenant-a", "someone-else", "c2"))

	assert.Len(t, f.store.events, after, "a repeat review must not add a second event")
	assert.Equal(t, "security-lead-3", *f.store.contexts[sc.SupportContextID].ReviewedBy)
}

func TestMarkReviewed_UnknownOrForeignGrantIsNotFound(t *testing.T) {
	f := newSupportFixture()
	svc := f.build()
	sc, err := attachApproved(svc, validAttachRequest())
	require.NoError(t, err)

	assert.ErrorIs(t, svc.MarkReviewed(context.Background(), "no-such-grant", "tenant-a", "r", "c"), domain.ErrSupportContextNotFound)
	assert.ErrorIs(t, svc.MarkReviewed(context.Background(), sc.SupportContextID, "tenant-b", "r", "c"), domain.ErrSupportContextNotFound)
}
