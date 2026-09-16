//go:build integration

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

// TestPgStore_ConfirmMatch_RejectsSelfConfirmation is the real proof of
// BNK-05's maker-checker manual-match rule: the principal who proposed a
// match cannot also confirm it.
func TestPgStore_ConfirmMatch_RejectsSelfConfirmation(t *testing.T) {
	f := setupIsolationFixture(t, "confirm-self")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()

	require.NoError(t, testStore.ProposeMatch(ctx, f.tenantID, f.statementLineID, journalID, "maker-1"))

	_, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "maker-1")
	require.ErrorIs(t, err, domain.ErrMatchSelfConfirmation)

	// A different principal confirming must succeed.
	confirmed, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "checker-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusMatched, confirmed.Status)
	require.NotNil(t, confirmed.MatchedJournalID)
	require.Equal(t, journalID, *confirmed.MatchedJournalID)
	require.NotNil(t, confirmed.MatchedByPrincipalID)
	require.Equal(t, "checker-1", *confirmed.MatchedByPrincipalID)
}

// TestPgStore_ConfirmMatch_RequiresPendingConfirmation proves the CAS
// predecessor requirement: confirming a line that was never proposed (or
// already matched) is refused, not silently accepted.
func TestPgStore_ConfirmMatch_RequiresPendingConfirmation(t *testing.T) {
	f := setupIsolationFixture(t, "confirm-cas")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	_, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "checker-1")
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// TestPgStore_RejectProposedMatch_ReturnsLineToException proves a checker
// can send a bad proposal back for correction, clearing the proposed_*
// fields so the line is a normal EXCEPTION queue item again.
func TestPgStore_RejectProposedMatch_ReturnsLineToException(t *testing.T) {
	f := setupIsolationFixture(t, "reject-match")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()

	require.NoError(t, testStore.ProposeMatch(ctx, f.tenantID, f.statementLineID, journalID, "maker-1"))
	require.NoError(t, testStore.RejectProposedMatch(ctx, f.tenantID, f.statementLineID, "wrong journal", "checker-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusException, l.Status)
	require.Nil(t, l.ProposedJournalID)
	require.Nil(t, l.ProposedByPrincipalID)

	// A fresh proposal on the now-EXCEPTION line must succeed — rejection
	// is a real path back into the workflow, not a dead end.
	require.NoError(t, testStore.ProposeMatch(ctx, f.tenantID, f.statementLineID, uuid.New().String(), "maker-2"))
}

// TestPgStore_CertifyStatement_IdempotentOnStatement proves a repeated
// CompleteStatement call (e.g. a client retry) returns the ORIGINAL
// certificate rather than creating a second one for the same statement.
func TestPgStore_CertifyStatement_IdempotentOnStatement(t *testing.T) {
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	bankAccountID := uuid.New().String()
	statementDate := time.Now().UTC().Format("2006-01-02")
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, created1, err := testStore.CertifyStatement(ctx, tenantID, legalEntityID, bankAccountID, statementDate, "certifier-1", "corr-cert-1", 5)
	require.NoError(t, err)
	require.True(t, created1)

	second, created2, err := testStore.CertifyStatement(ctx, tenantID, legalEntityID, bankAccountID, statementDate, "certifier-2", "corr-cert-2", 99)
	require.NoError(t, err)
	require.False(t, created2, "expected created=false on the retried certify — this is a duplicate-certificate bug if true")
	require.Equal(t, first.CertificateID, second.CertificateID)
	require.Equal(t, 5, second.MatchedLineCount, "expected the ORIGINAL certificate's matched_line_count, not the retry's")
}

// TestPgStore_ReconciliationCertificate_IsImmutable is the negative-
// controlled proof of migration 000006's own reject_certificate_mutation
// trigger — a certificate is evidence and can never be edited or deleted.
func TestPgStore_ReconciliationCertificate_IsImmutable(t *testing.T) {
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	bankAccountID := uuid.New().String()
	statementDate := time.Now().UTC().Format("2006-01-02")
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	cert, _, err := testStore.CertifyStatement(ctx, tenantID, legalEntityID, bankAccountID, statementDate, "certifier-1", "corr-immutable", 3)
	require.NoError(t, err)

	// Run over testPool directly (equivalent to superuser) — a BEFORE
	// trigger fires regardless of role.
	_, err = testPool.Exec(ctx, `UPDATE reconciliation_certificates SET matched_line_count = 999 WHERE certificate_id = $1`, cert.CertificateID)
	require.Error(t, err, "expected the trigger to refuse mutating a certificate")

	_, err = testPool.Exec(ctx, `DELETE FROM reconciliation_certificates WHERE certificate_id = $1`, cert.CertificateID)
	require.Error(t, err, "expected the trigger to refuse deleting a certificate")

	_, err = testPool.Exec(ctx, `ALTER TABLE reconciliation_certificates DISABLE TRIGGER trg_reject_certificate_mutation`)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE reconciliation_certificates SET matched_line_count = 999 WHERE certificate_id = $1`, cert.CertificateID)
	require.NoError(t, err, "expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism")
	_, err = testPool.Exec(ctx, `ALTER TABLE reconciliation_certificates ENABLE TRIGGER trg_reject_certificate_mutation`)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE reconciliation_certificates SET matched_line_count = 1 WHERE certificate_id = $1`, cert.CertificateID)
	require.Error(t, err, "expected re-enabling the trigger to restore the refusal")
}

// TestPgStore_TenantIsolation_ConfirmMatch proves tenant B cannot confirm
// (or even see the effect of proposing against) tenant A's statement
// line.
func TestPgStore_TenantIsolation_ConfirmMatch(t *testing.T) {
	a := setupIsolationFixture(t, "confirm-iso-a")
	b := setupIsolationFixture(t, "confirm-iso-b")
	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)

	require.NoError(t, testStore.ProposeMatch(ctxA, a.tenantID, a.statementLineID, uuid.New().String(), "maker-1"))

	// Tenant B attempting to confirm tenant A's line (scoped under B's own
	// tenant) must not succeed.
	_, err := testStore.ConfirmMatch(context.Background(), b.tenantID, a.statementLineID, "checker-1")
	require.Error(t, err, "tenant isolation failure: tenant B was able to confirm tenant A's proposed match")
}
