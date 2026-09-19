//go:build integration

// Package store_test — BNK-05 backlog integration tests.
//
// Tests the full run lifecycle, population snapshots, policy immutability,
// and evidence conflict store through an embedded Postgres v16 instance
// with real migrations applied.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTenant() string { return uuid.New().String() }
func newLE() string     { return uuid.New().String() }
func newAcct() string   { return uuid.New().String() }
func newCorr() string   { return "corr-" + uuid.New().String()[:8] }

// seedLine inserts an UNMATCHED statement line for the given tenant and returns
// its StatementLine. The inserted line must satisfy every NOT NULL constraint
// so subsequent operations (e.g. FreezePopulation) can read it.
func seedLine(t *testing.T, ctx context.Context, tenantID, legalEntityID, bankAccountID, statementDate string) *domain.StatementLine {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", statementDate)
	if err != nil {
		t.Fatalf("seedLine: invalid statementDate %q: %v", statementDate, err)
	}
	l := &domain.StatementLine{
		StatementLineID:   uuid.New().String(),
		TenantID:          tenantID,
		LegalEntityID:     legalEntityID,
		BankAccountID:     bankAccountID,
		StatementDate:     parsed,
		Amount:            100.00,
		CurrencyCode:      "USD",
		BankReference:     "REF-" + uuid.New().String()[:6],
		GLCashAccountCode: strPtr("1000"),
		Status:            domain.StatementLineStatusUnmatched,
		CorrelationID:     newCorr(),
		CreatedAt:         time.Now().UTC(),
	}
	created, err := testStore.CreateStatementLine(ctx, l)
	require.NoError(t, err)
	require.True(t, created)
	return l
}

func strPtr(s string) *string { return &s }

// ── Run lifecycle tests ───────────────────────────────────────────────────────

func TestStartRun_NewRun_IsDraft(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()

	req := domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: "2024-01-31",
		CorrelationID: newCorr(),
	}
	run, created, err := testStore.StartRun(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, domain.RunStatusDraft, run.Status)
	require.Equal(t, leID, run.LegalEntityID)
	require.Equal(t, acctID, run.BankAccountID)
}

func TestStartRun_Idempotent_SameCorrelationID(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	corrID := newCorr()
	req := domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: newLE(),
		BankAccountID: newAcct(),
		StatementDate: "2024-02-28",
		CorrelationID: corrID,
	}

	run1, created1, err := testStore.StartRun(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)
	require.True(t, created1)

	run2, created2, err := testStore.StartRun(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)
	require.False(t, created2, "same correlation_id should be idempotent")
	require.Equal(t, run1.RunID, run2.RunID)
}

func TestStartRun_DifferentCorrelationID_SameAccount_Conflicts(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-03-31"

	req1 := domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: date,
		CorrelationID: newCorr(),
	}
	_, _, err := testStore.StartRun(ctx, tenantID, req1, "actor-1")
	require.NoError(t, err)

	req2 := req1
	req2.CorrelationID = newCorr() // different correlation ID
	_, _, err = testStore.StartRun(ctx, tenantID, req2, "actor-1")
	require.ErrorIs(t, err, domain.ErrRunAlreadyExists)
}

func TestGetRun_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testStore.GetRun(ctx, newTenant(), uuid.New().String())
	require.ErrorIs(t, err, domain.ErrRunNotFound)
}

// ── Population snapshot tests ─────────────────────────────────────────────────

func TestFreezePopulation_CreatesImmutableSnapshot(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-04-30"

	// Seed a statement line first so FreezePopulation has rows to snapshot.
	seedLine(t, ctx, tenantID, leID, acctID, date)

	req := domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: date,
		CorrelationID: newCorr(),
	}
	run, _, err := testStore.StartRun(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)

	pop, created, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, pop.PopulationID)
	require.Equal(t, 1, pop.LineCount, "one seeded line must be included")
	require.NotEmpty(t, pop.BankPopulationHash, "hash must be computed")
}

func TestFreezePopulation_Idempotent(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-05-31"

	seedLine(t, ctx, tenantID, leID, acctID, date)
	req := domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}
	run, _, err := testStore.StartRun(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)

	pop1, created1, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)
	require.True(t, created1)

	pop2, created2, err := testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)
	require.False(t, created2, "re-freezing should be idempotent")
	require.Equal(t, pop1.PopulationID, pop2.PopulationID)
	require.Equal(t, pop1.BankPopulationHash, pop2.BankPopulationHash)
}

func TestGetPopulation_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testStore.GetPopulation(ctx, newTenant(), uuid.New().String())
	require.ErrorIs(t, err, domain.ErrPopulationNotFound)
}

// ── Policy tests ──────────────────────────────────────────────────────────────

func TestCreatePolicy_VersionIncrement(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()

	req := domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2024-01-01",
		MaxUnmatchedCount:   10,
		MaxUnmatchedPct:     0.5,
		MaxUnresolvedAmount: 1000.00,
		Currency:            "USD",
		Rationale:           "initial policy",
		CorrelationID:       newCorr(),
	}
	pol1, err := testStore.CreatePolicy(ctx, tenantID, req, "actor-1")
	require.NoError(t, err)
	require.Equal(t, 1, pol1.PolicyVersion)

	req2 := req
	req2.EffectiveFrom = "2024-02-01"
	req2.MaxUnmatchedCount = 5 // narrowing: should be allowed
	req2.CorrelationID = newCorr()
	pol2, err := testStore.CreatePolicy(ctx, tenantID, req2, "actor-1")
	require.NoError(t, err)
	require.Equal(t, 2, pol2.PolicyVersion)
}

func TestCreatePolicy_WideningBlockedWhenRunActive(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-06-30"

	// Create a tight policy first.
	pol, err := testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2024-01-01",
		MaxUnmatchedCount:   5,
		MaxUnmatchedPct:     0.5,
		MaxUnresolvedAmount: 500.00,
		Currency:            "USD",
		Rationale:           "tight policy",
		CorrelationID:       newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	run, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: date,
		CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	// Bind policy to the run so the run is bound to this policy.
	_, err = testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
	require.NoError(t, err)

	// Seed a line and freeze the population (this advances the run to RUNNING).
	seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	// Now try to create a wider policy while the run is active.
	_, err = testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2024-02-01",
		MaxUnmatchedCount:   100,      // wider
		MaxUnmatchedPct:     5.0,      // wider
		MaxUnresolvedAmount: 10000.00, // wider
		Currency:            "USD",
		Rationale:           "attempted widening",
		CorrelationID:       newCorr(),
	}, "actor-2")
	require.ErrorIs(t, err, domain.ErrPolicyWouldWidenActiveRun)
}

func TestGetCurrentPolicy_NotFound_ReturnsError(t *testing.T) {
	ctx := context.Background()
	_, err := testStore.GetCurrentPolicy(ctx, newTenant(), newLE())
	require.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

// ── Reperformance / supersede tests ──────────────────────────────────────────

func TestSupersedeRun_CreatesNewDraftPreservesOld(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-07-31"

	run1, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	newRun, err := testStore.SupersedeRun(ctx, tenantID, run1.RunID, "actor-1", newCorr())
	require.NoError(t, err)
	require.NotEqual(t, run1.RunID, newRun.RunID)
	require.Equal(t, domain.RunStatusDraft, newRun.Status)

	// Original run must now be SUPERSEDED.
	old, err := testStore.GetRun(ctx, tenantID, run1.RunID)
	require.NoError(t, err)
	require.Equal(t, domain.RunStatusSuperseded, old.Status)
}

func TestSupersedeRun_AlreadySuperseded_Errors(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-31"

	run1, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	_, err = testStore.SupersedeRun(ctx, tenantID, run1.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	// Cannot supersede an already-superseded run.
	_, err = testStore.SupersedeRun(ctx, tenantID, run1.RunID, "actor-1", newCorr())
	require.ErrorIs(t, err, domain.ErrRunSuperseded)
}

// ── CertifyRun tests: frozen population evaluation ────────────────────────────

func TestCertifyRun_EvaluatesFrozenPopulationNotLateData(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-15"

	// 1. Create a zero-tolerance policy (max 0 unmatched lines).
	pol, err := testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2024-01-01",
		MaxUnmatchedCount:   0,
		MaxUnmatchedPct:     0.0,
		MaxUnresolvedAmount: 0.0,
		Currency:            "USD",
		Rationale:           "zero tolerance",
		CorrelationID:       newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	// 2. Start a run and bind policy.
	run, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: date,
		CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	_, err = testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
	require.NoError(t, err)

	// 3. Seed line 1 and freeze population (snapshot contains line 1).
	line1 := seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	// 4. Match line 1. Now within the frozen population: unmatched = 0, matched = 1.
	err = testStore.MatchStatementLine(ctx, tenantID, line1.StatementLineID, uuid.New().String(), "matcher-1")
	require.NoError(t, err)

	// 5. Late data arrives: seed line 2 (UNMATCHED) for the same account + date AFTER freeze.
	seedLine(t, ctx, tenantID, leID, acctID, date)

	// 6. CertifyRun MUST evaluate the frozen population, NOT live statement_lines.
	// If it evaluated live statement_lines, unmatched = 1 > 0 -> would fail ErrMaterialResidualBlocked.
	// With frozen population, unmatched = 0 -> certification succeeds!
	cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, cert)
	require.Equal(t, 1, cert.MatchedLineCount)
	require.Equal(t, run.RunID, *cert.RunID)

	// Idempotent second certification returns same cert.
	cert2, created2, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, cert.CertificateID, cert2.CertificateID)
}

func TestCertifyRun_ExceedsThreshold_Blocked(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-16"

	// Zero-tolerance policy.
	pol, err := testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID:            tenantID,
		LegalEntityID:       leID,
		EffectiveFrom:       "2024-01-01",
		MaxUnmatchedCount:   0,
		MaxUnmatchedPct:     0.0,
		MaxUnresolvedAmount: 0.0,
		Currency:            "USD",
		Rationale:           "zero tolerance",
		CorrelationID:       newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	run, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID:      tenantID,
		LegalEntityID: leID,
		BankAccountID: acctID,
		StatementDate: date,
		CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	_, err = testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
	require.NoError(t, err)

	// Seed line and freeze into population without matching it.
	seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	// Line in population is still UNMATCHED -> exceeds MaxUnmatchedCount (0) -> blocked.
	_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.ErrorIs(t, err, domain.ErrMaterialResidualBlocked)
}

// TestCertifyRun_RejectsSelfCertification is the real proof of the
// run-level SoD rule: a principal who matched a line in the frozen
// population cannot also certify the run.
func TestCertifyRun_RejectsSelfCertification(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-15"

	pol, err := testStore.CreatePolicy(ctx, tenantID, domain.CreatePolicyRequest{
		TenantID: tenantID, LegalEntityID: leID, EffectiveFrom: "2024-01-01",
		MaxUnmatchedCount: 0, MaxUnmatchedPct: 0.0, MaxUnresolvedAmount: 0.0,
		Currency: "USD", Rationale: "zero tolerance", CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	run, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)
	_, err = testStore.BindPolicy(ctx, tenantID, run.RunID, pol.PolicyID)
	require.NoError(t, err)

	line1 := seedLine(t, ctx, tenantID, leID, acctID, date)
	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	err = testStore.MatchStatementLine(ctx, tenantID, line1.StatementLineID, uuid.New().String(), "matcher-1")
	require.NoError(t, err)

	// The matcher tries to certify their own work — must be refused.
	_, _, err = testStore.CertifyRun(ctx, tenantID, run.RunID, "matcher-1", newCorr())
	require.ErrorIs(t, err, domain.ErrRunSelfCertificationForbidden)

	// A different principal certifying must succeed.
	cert, created, err := testStore.CertifyRun(ctx, tenantID, run.RunID, "certifier-1", newCorr())
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, cert)
}

// TestListUnmatchedLinesInPopulation_OnlyReturnsFrozenUnmatchedLines is the
// real proof behind RunAutomaticMatching's containment check: it must
// return exactly the lines that are both (a) part of THIS run's frozen
// population snapshot and (b) still UNMATCHED — not a matched line in the
// same population, and not an unrelated UNMATCHED line that arrived after
// the freeze (late data), even though that line is genuinely UNMATCHED
// somewhere in the tenant's register.
func TestListUnmatchedLinesInPopulation_OnlyReturnsFrozenUnmatchedLines(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-20"

	run, _, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}, "actor-1")
	require.NoError(t, err)

	unmatchedLine := seedLine(t, ctx, tenantID, leID, acctID, date)
	matchedLine := seedLine(t, ctx, tenantID, leID, acctID, date)

	_, _, err = testStore.FreezePopulation(ctx, tenantID, run.RunID, "actor-1", newCorr())
	require.NoError(t, err)

	err = testStore.MatchStatementLine(ctx, tenantID, matchedLine.StatementLineID, uuid.New().String(), "matcher-1")
	require.NoError(t, err)

	// Late data: an UNMATCHED line for the same account+date, seeded AFTER
	// the freeze — genuinely UNMATCHED, but never part of this population.
	lateLine := seedLine(t, ctx, tenantID, leID, acctID, date)

	run, err = testStore.GetRun(ctx, tenantID, run.RunID)
	require.NoError(t, err)
	require.NotNil(t, run.PopulationID)

	lines, err := testStore.ListUnmatchedLinesInPopulation(ctx, tenantID, *run.PopulationID)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, unmatchedLine.StatementLineID, lines[0].StatementLineID)

	for _, l := range lines {
		require.NotEqual(t, matchedLine.StatementLineID, l.StatementLineID, "must not include the MATCHED line")
		require.NotEqual(t, lateLine.StatementLineID, l.StatementLineID, "must not include a line outside the frozen population")
	}
}

// TestUnmatchWithReason_MatchedLine_RevertsToException proves the real
// correction path: a MATCHED line reverts to EXCEPTION with the reason
// and actor persisted, and — the negative control — an already-UNMATCHED
// line has no MATCHED state to revert from and is refused.
func TestUnmatchWithReason_MatchedLine_RevertsToException(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-08-25"

	line := seedLine(t, ctx, tenantID, leID, acctID, date)
	err := testStore.MatchStatementLine(ctx, tenantID, line.StatementLineID, uuid.New().String(), "matcher-1")
	require.NoError(t, err)

	err = testStore.UnmatchWithReason(ctx, tenantID, line.StatementLineID, "matched to the wrong journal", "reviewer-1")
	require.NoError(t, err)

	got, err := testStore.GetStatementLine(svcmiddleware.WithTenant(ctx, tenantID), line.StatementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusException, got.Status)
	require.NotNil(t, got.ExceptionReason)
	require.Equal(t, "matched to the wrong journal", *got.ExceptionReason)
	require.NotNil(t, got.FlaggedByPrincipalID)
	require.Equal(t, "reviewer-1", *got.FlaggedByPrincipalID)

	// Negative control: unmatching a line that isn't MATCHED is refused.
	err = testStore.UnmatchWithReason(ctx, tenantID, line.StatementLineID, "already reverted", "reviewer-1")
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// ── Evidence conflict tests ───────────────────────────────────────────────────

func TestRaiseEvidenceConflict_CreatesOpenConflict(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	line := seedLine(t, ctx, tenantID, leID, acctID, "2024-09-01")

	req := domain.RaiseEvidenceConflictRequest{
		TenantID:                tenantID,
		LegalEntityID:           leID,
		StatementLineID:         line.StatementLineID,
		PaymentID:               "pay-" + uuid.New().String()[:8],
		BankRecStatus:           "MATCHED",
		ProviderConfirmedStatus: "SETTLED",
		ConflictReason:          "status_mismatch",
		SourceEventID:           "evt-" + uuid.New().String()[:8],
		CorrelationID:           newCorr(),
	}
	conflict, created, err := testStore.RaiseEvidenceConflict(ctx, tenantID, req)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "OPEN", conflict.ConflictStatus)
	require.Equal(t, req.StatementLineID, conflict.StatementLineID)
}

func TestRaiseEvidenceConflict_Idempotent_SameLinePay(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	line := seedLine(t, ctx, tenantID, leID, acctID, "2024-09-02")
	payID := "pay-" + uuid.New().String()[:8]

	req := domain.RaiseEvidenceConflictRequest{
		TenantID:                tenantID,
		LegalEntityID:           leID,
		StatementLineID:         line.StatementLineID,
		PaymentID:               payID,
		BankRecStatus:           "EXCEPTION",
		ProviderConfirmedStatus: "SETTLED",
		ConflictReason:          "exception_vs_settled",
		SourceEventID:           "evt-" + uuid.New().String()[:8],
		CorrelationID:           newCorr(),
	}
	c1, cr1, err := testStore.RaiseEvidenceConflict(ctx, tenantID, req)
	require.NoError(t, err)
	require.True(t, cr1)

	req.SourceEventID = "evt-" + uuid.New().String()[:8] // different event
	c2, cr2, err := testStore.RaiseEvidenceConflict(ctx, tenantID, req)
	require.NoError(t, err)
	require.False(t, cr2, "idempotent: same line+payment should not create a second conflict")
	require.Equal(t, c1.ConflictID, c2.ConflictID)
}

func TestGetEvidenceConflict_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testStore.GetEvidenceConflict(ctx, newTenant(), uuid.New().String())
	require.ErrorIs(t, err, domain.ErrConflictNotFound)
}

func TestListOpenConflicts_ReturnsOnlyOpenForTenant(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()

	// Raise two conflicts.
	for i := 0; i < 2; i++ {
		line := seedLine(t, ctx, tenantID, leID, acctID, "2024-09-03")
		_, _, err := testStore.RaiseEvidenceConflict(ctx, tenantID, domain.RaiseEvidenceConflictRequest{
			TenantID:                tenantID,
			LegalEntityID:           leID,
			StatementLineID:         line.StatementLineID,
			PaymentID:               "pay-" + uuid.New().String()[:8],
			BankRecStatus:           "MATCHED",
			ProviderConfirmedStatus: "SETTLED",
			ConflictReason:          "mismatch",
			SourceEventID:           "evt-" + uuid.New().String()[:8],
			CorrelationID:           newCorr(),
		})
		require.NoError(t, err)
	}

	conflicts, err := testStore.ListOpenConflicts(ctx, tenantID, 50)
	require.NoError(t, err)
	require.Len(t, conflicts, 2)
}

func TestResolveEvidenceConflict_TransitionsToResolved(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	line := seedLine(t, ctx, tenantID, leID, acctID, "2024-09-04")

	req := domain.RaiseEvidenceConflictRequest{
		TenantID:                tenantID,
		LegalEntityID:           leID,
		StatementLineID:         line.StatementLineID,
		PaymentID:               "pay-" + uuid.New().String()[:8],
		BankRecStatus:           "MATCHED",
		ProviderConfirmedStatus: "SETTLED",
		ConflictReason:          "status_discrepancy",
		SourceEventID:           "evt-" + uuid.New().String()[:8],
		CorrelationID:           newCorr(),
	}
	conflict, _, err := testStore.RaiseEvidenceConflict(ctx, tenantID, req)
	require.NoError(t, err)

	resolved, err := testStore.ResolveEvidenceConflict(ctx, tenantID, conflict.ConflictID, "resolver-1", "verified manually — bank statement confirmed settled")
	require.NoError(t, err)
	require.Equal(t, "RESOLVED", resolved.ConflictStatus)
	require.NotNil(t, resolved.ResolvedAt)
	require.Equal(t, "resolver-1", *resolved.ResolvedByPrincipalID)
}

func TestResolveEvidenceConflict_AlreadyResolved_Errors(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	leID := newLE()
	acctID := newAcct()
	line := seedLine(t, ctx, tenantID, leID, acctID, "2024-09-05")

	req := domain.RaiseEvidenceConflictRequest{
		TenantID:                tenantID,
		LegalEntityID:           leID,
		StatementLineID:         line.StatementLineID,
		PaymentID:               "pay-" + uuid.New().String()[:8],
		BankRecStatus:           "MATCHED",
		ProviderConfirmedStatus: "SETTLED",
		ConflictReason:          "double_resolve_test",
		SourceEventID:           "evt-" + uuid.New().String()[:8],
		CorrelationID:           newCorr(),
	}
	conflict, _, err := testStore.RaiseEvidenceConflict(ctx, tenantID, req)
	require.NoError(t, err)

	_, err = testStore.ResolveEvidenceConflict(ctx, tenantID, conflict.ConflictID, "resolver-1", "first resolution")
	require.NoError(t, err)

	_, err = testStore.ResolveEvidenceConflict(ctx, tenantID, conflict.ConflictID, "resolver-2", "second resolution")
	require.ErrorIs(t, err, domain.ErrConflictAlreadyResolved)
}

func TestIsEventProcessed_MarkEventProcessed_Idempotent(t *testing.T) {
	ctx := context.Background()
	tenantID := newTenant()
	eventID := "evt-" + uuid.New().String()

	processed, err := testStore.IsEventProcessed(ctx, tenantID, eventID)
	require.NoError(t, err)
	require.False(t, processed)

	err = testStore.MarkEventProcessed(ctx, tenantID, eventID)
	require.NoError(t, err)

	processed, err = testStore.IsEventProcessed(ctx, tenantID, eventID)
	require.NoError(t, err)
	require.True(t, processed)

	// Mark again must be idempotent (ON CONFLICT DO NOTHING).
	err = testStore.MarkEventProcessed(ctx, tenantID, eventID)
	require.NoError(t, err)
}

// ── Tenant isolation for run/conflict ────────────────────────────────────────

func TestRun_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	tenantA := newTenant()
	tenantB := newTenant()
	leID := newLE()
	acctID := newAcct()
	date := "2024-09-30"

	runA, _, err := testStore.StartRun(ctx, tenantA, domain.StartRunRequest{
		TenantID: tenantA, LegalEntityID: leID, BankAccountID: acctID, StatementDate: date, CorrelationID: newCorr(),
	}, "actor-A")
	require.NoError(t, err)

	// Tenant B cannot see tenant A's run.
	_, err = testStore.GetRun(ctx, tenantB, runA.RunID)
	require.ErrorIs(t, err, domain.ErrRunNotFound, "tenant B must not see tenant A's run")
}

func TestConflict_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	tenantA := newTenant()
	tenantB := newTenant()
	leID := newLE()
	acctID := newAcct()
	lineA := seedLine(t, ctx, tenantA, leID, acctID, "2024-09-06")

	conflictA, _, err := testStore.RaiseEvidenceConflict(ctx, tenantA, domain.RaiseEvidenceConflictRequest{
		TenantID:                tenantA,
		LegalEntityID:           leID,
		StatementLineID:         lineA.StatementLineID,
		PaymentID:               "pay-" + uuid.New().String()[:8],
		BankRecStatus:           "MATCHED",
		ProviderConfirmedStatus: "SETTLED",
		ConflictReason:          "isolation_test",
		SourceEventID:           "evt-" + uuid.New().String()[:8],
		CorrelationID:           newCorr(),
	})
	require.NoError(t, err)

	// Tenant B cannot see or resolve tenant A's conflict.
	_, err = testStore.GetEvidenceConflict(ctx, tenantB, conflictA.ConflictID)
	require.ErrorIs(t, err, domain.ErrConflictNotFound, "tenant B must not see tenant A's conflict")

	conflictsB, err := testStore.ListOpenConflicts(ctx, tenantB, 50)
	require.NoError(t, err)
	require.Empty(t, conflictsB, "tenant B's list must not include tenant A's conflict")
}
