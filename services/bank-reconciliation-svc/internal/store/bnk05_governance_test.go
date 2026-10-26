//go:build integration

// Package store_test — BNK-05 governance tests added by the spec survey:
// complete certifier independence, every non-matched line counted as
// unresolved, the pinned unresolved-amount tolerance, policy widening across
// every active run, and serialised certification.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

func mustPolicy(t *testing.T, tenantID, legalEntityID string, maxCount int, maxPct, maxAmount float64, currency string) *domain.ReconciliationPolicy {
	t.Helper()
	pol, err := testStore.CreatePolicy(context.Background(), tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       legalEntityID,
		EffectiveFrom:       "2024-01-01",
		MaxUnmatchedCount:   maxCount,
		MaxUnmatchedPct:     maxPct,
		MaxUnresolvedAmount: maxAmount,
		Currency:            currency,
		Rationale:           "test policy",
		CorrelationID:       newCorr(),
	}, "policy-author")
	require.NoError(t, err)
	return pol
}

func mustRun(t *testing.T, tenantID, legalEntityID, bankAccountID, statementDate, creator string) *domain.ReconciliationRun {
	t.Helper()
	run, _, err := testStore.StartRun(context.Background(), tenantID, domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		BankAccountID: bankAccountID,
		StatementDate: statementDate,
		CorrelationID: newCorr(),
	}, creator)
	require.NoError(t, err)
	return run
}

func seedLineInCurrency(t *testing.T, tenantID, legalEntityID, bankAccountID, statementDate, currency string, amount float64) *domain.StatementLine {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", statementDate)
	require.NoError(t, err)
	l := &domain.StatementLine{
		StatementLineID:   uuid.New().String(),
		TenantID:          tenantID,
		LegalEntityID:     legalEntityID,
		BankAccountID:     bankAccountID,
		StatementDate:     parsed,
		Amount:            amount,
		CurrencyCode:      currency,
		BankReference:     "REF-" + uuid.New().String()[:6],
		GLCashAccountCode: strPtr("1000"),
		Status:            domain.StatementLineStatusUnmatched,
		CorrelationID:     newCorr(),
		CreatedAt:         time.Now().UTC(),
	}
	created, err := testStore.CreateStatementLine(context.Background(), l, "ingest-fixture")
	require.NoError(t, err)
	require.True(t, created)
	return l
}

// TestCertifyRun_RunCreatorCannotCertify proves the run-level SoD rule covers
// the principal who started the run, not only the last matcher.
func TestCertifyRun_RunCreatorCannotCertify(t *testing.T) {
	ctx := context.Background()
	tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-01-31"

	run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
	line := seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
	require.NoError(t, err)
	require.NoError(t, testStore.MatchStatementLine(ctx, tenantID, line.StatementLineID, uuid.New().String(), "matcher-1"))

	_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "creator-1", newCorr())
	require.ErrorIs(t, err, domain.ErrRunSelfCertificationForbidden)

	cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, cert)
}

// TestCertifyRun_MatchProposerCannotCertify proves a maker who proposed a
// match is a preparer even after a different checker confirmed it.
func TestCertifyRun_MatchProposerCannotCertify(t *testing.T) {
	ctx := context.Background()
	tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-02-28"

	run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
	line := seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
	require.NoError(t, err)

	require.NoError(t, testStore.ProposeMatch(ctx, tenantID, line.StatementLineID, uuid.New().String(), "maker-1"))
	_, err = testStore.ConfirmMatch(ctx, tenantID, line.StatementLineID, "checker-1")
	require.NoError(t, err)

	_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "maker-1", newCorr())
	require.ErrorIs(t, err, domain.ErrRunSelfCertificationForbidden)

	cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, cert)
}

// TestCertifyRun_CountsExceptionAndPendingAsUnresolved proves certification
// no longer treats only UNMATCHED as outstanding. The policy tolerates zero
// items and 50 per cent, so a single unresolved item of one must block; the
// old UNMATCHED-only count saw zero and certified.
func TestCertifyRun_CountsExceptionAndPendingAsUnresolved(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, tenantID, leID, acctID, date, lineID string)
	}{
		{
			name: "exception",
			setup: func(t *testing.T, tenantID, leID, acctID, date, lineID string) {
				require.NoError(t, testStore.FlagException(ctx, tenantID, lineID, "unidentified bank fee", "flagger-1"))
			},
		},
		{
			name: "pending_confirmation",
			setup: func(t *testing.T, tenantID, leID, acctID, date, lineID string) {
				require.NoError(t, testStore.ProposeMatch(ctx, tenantID, lineID, uuid.New().String(), "maker-1"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-03-31"
			pol := mustPolicy(t, tenantID, leID, 0, 0.5, 1000, "USD")
			run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
			_, err := testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
			require.NoError(t, err)

			line := seedLine(t, ctx, tenantID, leID, acctID, date)
			_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
			require.NoError(t, err)
			tc.setup(t, tenantID, leID, acctID, date, line.StatementLineID)

			_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
			require.ErrorIs(t, err, domain.ErrMaterialResidualBlocked)
		})
	}
}

// TestCertifyRun_EnforcesMaxUnresolvedAmount proves the amount tolerance the
// policy always had is now pinned on the run and applied. Count and per-cent
// tolerances are deliberately permissive, so only the amount can block.
func TestCertifyRun_EnforcesMaxUnresolvedAmount(t *testing.T) {
	ctx := context.Background()

	t.Run("blocked", func(t *testing.T) {
		tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-04-30"
		pol := mustPolicy(t, tenantID, leID, 10, 1.0, 50, "USD")
		run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
		_, err := testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
		require.NoError(t, err)
		seedLineInCurrency(t, tenantID, leID, acctID, date, "USD", 100)
		_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
		require.NoError(t, err)

		_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
		require.ErrorIs(t, err, domain.ErrMaterialResidualBlocked)
	})

	t.Run("within", func(t *testing.T) {
		tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-05-31"
		pol := mustPolicy(t, tenantID, leID, 10, 1.0, 150, "USD")
		run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
		_, err := testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
		require.NoError(t, err)
		seedLineInCurrency(t, tenantID, leID, acctID, date, "USD", 100)
		_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
		require.NoError(t, err)

		cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
		require.NoError(t, err)
		require.True(t, created)
		require.NotNil(t, cert)
	})

	t.Run("other_currency", func(t *testing.T) {
		tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-06-30"
		pol := mustPolicy(t, tenantID, leID, 10, 1.0, 150, "USD")
		run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
		_, err := testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
		require.NoError(t, err)
		seedLineInCurrency(t, tenantID, leID, acctID, date, "EUR", 100)
		_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
		require.NoError(t, err)

		_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
		require.ErrorIs(t, err, domain.ErrUnresolvedExposureUnassessable)
	})
}

// TestCreatePolicy_WideningCheckedAgainstEveryActiveRun proves the widening
// guard is a constraint from every active run, not only the newest policy a
// run happens to be bound to. A new policy looser than an older bound policy
// must be refused even when a newer, looser policy also exists.
func TestCreatePolicy_WideningCheckedAgainstEveryActiveRun(t *testing.T) {
	ctx := context.Background()
	tenantID, leID := newTenant(), newLE()

	strict := mustPolicy(t, tenantID, leID, 5, 0.5, 500, "USD")
	loose := mustPolicy(t, tenantID, leID, 100, 5.0, 10000, "USD")

	run1 := mustRun(t, tenantID, leID, newAcct(), "2025-07-31", "actor-1")
	_, err := testStore.BindPolicy(ctx, tenantID, run1.RunID, strict.PolicyID)
	require.NoError(t, err)
	seedLine(t, ctx, tenantID, leID, run1.BankAccountID, run1.StatementDate)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run1.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	run2 := mustRun(t, tenantID, leID, newAcct(), "2025-07-31", "actor-1")
	_, err = testStore.BindPolicy(ctx, tenantID, run2.RunID, loose.PolicyID)
	require.NoError(t, err)
	seedLine(t, ctx, tenantID, leID, run2.BankAccountID, run2.StatementDate)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run2.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	// 50 / 3.0 / 5000 is tighter than the newest bound policy (100 / 5.0 /
	// 10000) but wider than the older one (5 / 0.5 / 500).
	_, err = testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2025-08-01",
		MaxUnmatchedCount:   50,
		MaxUnmatchedPct:     3.0,
		MaxUnresolvedAmount: 5000,
		Currency:            "USD",
		Rationale:           "must be refused because run1 is still bound to the strict policy",
		CorrelationID:       newCorr(),
	}, "policy-author")
	require.ErrorIs(t, err, domain.ErrPolicyWouldWidenActiveRun)

	// The same policy as the strict one is not a widening and must pass.
	_, err = testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2025-08-01",
		MaxUnmatchedCount:   5,
		MaxUnmatchedPct:     0.5,
		MaxUnresolvedAmount: 500,
		Currency:            "USD",
		Rationale:           "equal to the strictest bound policy",
		CorrelationID:       newCorr(),
	}, "policy-author")
	require.NoError(t, err)
}

// TestCreatePolicy_WideningChecksUnresolvedAmount isolates the amount from the
// count and per-cent checks: only max_unresolved_amount grows.
func TestCreatePolicy_WideningChecksUnresolvedAmount(t *testing.T) {
	ctx := context.Background()
	tenantID, leID := newTenant(), newLE()

	pol := mustPolicy(t, tenantID, leID, 10, 1.0, 500, "USD")
	run := mustRun(t, tenantID, leID, newAcct(), "2025-08-31", "actor-1")
	_, err := testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
	require.NoError(t, err)
	seedLine(t, ctx, tenantID, leID, run.BankAccountID, run.StatementDate)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	_, err = testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2025-09-01",
		MaxUnmatchedCount:   10,
		MaxUnmatchedPct:     1.0,
		MaxUnresolvedAmount: 501,
		Currency:            "USD",
		Rationale:           "amount-only widening",
		CorrelationID:       newCorr(),
	}, "policy-author")
	require.ErrorIs(t, err, domain.ErrPolicyWouldWidenActiveRun)

	_, err = testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2025-09-01",
		MaxUnmatchedCount:   10,
		MaxUnmatchedPct:     1.0,
		MaxUnresolvedAmount: 100,
		Currency:            "USD",
		Rationale:           "amount-only narrowing",
		CorrelationID:       newCorr(),
	}, "policy-author")
	require.NoError(t, err)
}

// TestCertifyRun_ConcurrentCallsProduceOneCertificate proves the run row is
// locked during certification: racing certifiers serialise and all retries
// return the same certificate instead of one losing on the unique index.
func TestCertifyRun_ConcurrentCallsProduceOneCertificate(t *testing.T) {
	ctx := context.Background()
	tenantID, leID, acctID, date := newTenant(), newLE(), newAcct(), "2025-10-31"

	run := mustRun(t, tenantID, leID, acctID, date, "creator-1")
	line := seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "freezer-1", newCorr())
	require.NoError(t, err)
	require.NoError(t, testStore.MatchStatementLine(ctx, tenantID, line.StatementLineID, uuid.New().String(), "matcher-1"))

	type outcome struct {
		cert    *domain.ReconciliationCertificate
		created bool
		err     error
	}
	const callers = 4
	start := make(chan struct{})
	out := make(chan outcome, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
			out <- outcome{cert: cert, created: created, err: err}
		}()
	}
	close(start)

	created := 0
	var certificateID string
	for i := 0; i < callers; i++ {
		o := <-out
		require.NoError(t, o.err)
		require.NotNil(t, o.cert)
		if o.created {
			created++
		}
		if certificateID == "" {
			certificateID = o.cert.CertificateID
		} else {
			require.Equal(t, certificateID, o.cert.CertificateID)
		}
	}
	require.Equal(t, 1, created, "exactly one certifier must create the certificate; the rest must replay it")
}
