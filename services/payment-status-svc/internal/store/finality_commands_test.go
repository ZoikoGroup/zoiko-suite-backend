//go:build integration

package store_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
)

func recordFinalityPayment(t *testing.T, ctx context.Context, tenantID, sourceReference string) *domain.PaymentExecutionState {
	t.Helper()
	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID:     uuid.NewString(),
		ProviderRequestID: "provider-" + uuid.NewString(),
		SourceReference:   sourceReference,
	}, "creator-1", "corr-record-"+sourceReference)
	require.NoError(t, err)
	return p
}

func confirmSettlementRequest() domain.ConfirmSettlementRequest {
	return domain.ConfirmSettlementRequest{
		ProviderReference: "provider-settlement-1",
		BankReference:     "bank-settlement-1",
		Reason:            "bank settlement report reconciled",
		EffectiveDate:     "2026-10-25",
		EvidenceReference: "evidence://bank/settlement/2026-10-25/1",
		EvidenceHash:      strings.Repeat("a", 64),
	}
}

func recordRejectionRequest() domain.RecordRejectionRequest {
	return domain.RecordRejectionRequest{
		ProviderReference: "provider-rejection-1",
		BankReference:     "bank-rejection-1",
		Reason:            "provider rejected the instruction",
		EffectiveDate:     "2026-10-25T12:00:00Z",
		EvidenceReference: "evidence://provider/rejection/1",
		EvidenceHash:      strings.Repeat("b", 64),
	}
}

func TestPgStore_ConfirmSettlement_EvidenceAndOutbox(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-1")

	updated, evidence, replay, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-confirm-1", "corr-confirm-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, domain.StatusSettled, updated.Status)
	require.Equal(t, domain.FinalitySourceManualConfirmation, updated.FinalitySource)
	require.Equal(t, domain.ManualConfirmationMappingVersion, updated.MappingVersion)
	require.NotNil(t, evidence)
	require.Equal(t, domain.CommandConfirmSettlement, evidence.Command)
	require.Equal(t, confirmSettlementRequest().EvidenceReference, evidence.EvidenceReference)
	require.Equal(t, confirmSettlementRequest().EvidenceHash, evidence.EvidenceHash)
	require.Equal(t, "confirmer-1", evidence.ActorPrincipalID)

	var statusEventCount int
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT count(*) FROM status_events WHERE payment_id = $1 AND event_type = $2`,
		p.PaymentID, domain.EventPaymentSettled).Scan(&statusEventCount))
	require.Equal(t, 1, statusEventCount)
	var detail string
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT detail FROM status_events WHERE payment_id = $1 AND event_type = $2`,
		p.PaymentID, domain.EventPaymentSettled).Scan(&detail))
	require.Contains(t, detail, confirmSettlementRequest().EvidenceReference)
	require.Contains(t, detail, confirmSettlementRequest().Reason)

	var evidenceCount, claimCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.Equal(t, 1, evidenceCount)
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, "idem-confirm-1").Scan(&claimCount))
	require.Equal(t, 1, claimCount)

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	var settled int
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.settled" {
			settled++
		}
	}
	require.Equal(t, 1, settled, "expected exactly one PaymentSettled outbox event, got %+v", events)
	payload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, "com.zoikosuite.banking.payment-execution-state.settled")
	require.Contains(t, payload, `"event_type":"PAYMENT_SETTLED"`)
}

func TestPgStore_ConfirmSettlement_DuplicateSameKeyReplaysWithoutSecondEffect(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-2")

	first, firstEvidence, firstReplay, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-confirm-2", "corr-confirm-2")
	require.NoError(t, err)
	require.False(t, firstReplay)
	require.Equal(t, domain.StatusSettled, first.Status)

	second, secondEvidence, secondReplay, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-confirm-2", "corr-confirm-2")
	require.NoError(t, err)
	require.True(t, secondReplay)
	require.Equal(t, first.PaymentID, second.PaymentID)
	require.Equal(t, firstEvidence.EvidenceID, secondEvidence.EvidenceID)

	var statusEventCount, evidenceCount, claimCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM status_events WHERE payment_id = $1 AND event_type = $2`, p.PaymentID, domain.EventPaymentSettled).Scan(&statusEventCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, "idem-confirm-2").Scan(&claimCount))
	require.Equal(t, 1, statusEventCount)
	require.Equal(t, 1, evidenceCount)
	require.Equal(t, 1, claimCount)

	var settled int
	for _, e := range outboxEventsFor(t, ctx, tenantID, p.PaymentID) {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.settled" {
			settled++
		}
	}
	require.Equal(t, 1, settled)
}

func TestPgStore_ConfirmSettlement_AlreadyTerminalWithNewKeyRefused(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-3")

	_, _, _, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-terminal-1", "corr-terminal-1")
	require.NoError(t, err)

	_, _, _, err = testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-terminal-2", "corr-terminal-2")
	require.ErrorIs(t, err, domain.ErrInvalidTransition)

	var evidenceCount, claimCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, "idem-terminal-2").Scan(&claimCount))
	require.Equal(t, 1, evidenceCount)
	require.Equal(t, 0, claimCount, "a refused command must not consume its Idempotency-Key")
}

func TestPgStore_ConfirmSettlement_SameKeyDifferentRequestConflict(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-4")

	_, _, _, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-conflict-1", "corr-conflict-1")
	require.NoError(t, err)

	changed := confirmSettlementRequest()
	changed.Reason = "different evidence under the same key"
	_, _, _, err = testStore.ConfirmSettlement(ctx, p.PaymentID, changed, "confirmer-1", "idem-conflict-1", "corr-conflict-1")
	require.ErrorIs(t, err, domain.ErrIdempotencyKeyConflict)

	var evidenceCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.Equal(t, 1, evidenceCount)
}

func TestPgStore_ConfirmSettlement_SameCreatorForbidden(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-5")

	_, _, _, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "creator-1", "idem-sod-1", "corr-sod-1")
	require.ErrorIs(t, err, domain.ErrSelfResolutionForbidden)

	still, err := testStore.FindPayment(ctx, p.PaymentID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPrepared, still.Status)
	var evidenceCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.Equal(t, 0, evidenceCount)
}

func TestPgStore_ConfirmSettlement_OpenConflictRefused(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-6")

	_, conflict, err := testStore.LinkStatement(ctx, p.PaymentID, domain.LinkStatementRequest{
		StatementReference: "stmt-conflict-1",
		ReportedStatus:     domain.StatusPending,
	}, "creator-1", "corr-link-conflict-1")
	require.NoError(t, err)
	require.True(t, conflict)

	_, _, _, err = testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-conflict-open-1", "corr-conflict-open-1")
	require.ErrorIs(t, err, domain.ErrOpenConflictExists)
}

func TestPgStore_RecordRejection_MissingEvidenceDoesNotConsumeKey(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-reject-1")

	req := recordRejectionRequest()
	req.EvidenceReference = ""
	_, _, _, err := testStore.RecordRejection(ctx, p.PaymentID, req, "confirmer-1", "idem-reject-missing-1", "corr-reject-missing-1")
	require.ErrorIs(t, err, domain.ErrInvalidFinalityRequest)

	still, err := testStore.FindPayment(ctx, p.PaymentID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPrepared, still.Status)
	var evidenceCount, claimCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, "idem-reject-missing-1").Scan(&claimCount))
	require.Equal(t, 0, evidenceCount)
	require.Equal(t, 0, claimCount)
}

func TestPgStore_RecordRejection_EvidenceAndOutbox(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-reject-2")

	updated, evidence, replay, err := testStore.RecordRejection(ctx, p.PaymentID, recordRejectionRequest(), "confirmer-1", "idem-reject-2", "corr-reject-2")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, domain.StatusRejected, updated.Status)
	require.Equal(t, domain.FinalitySourceManualRejection, updated.FinalitySource)
	require.NotNil(t, evidence)
	require.Equal(t, domain.CommandRecordRejection, evidence.Command)

	var rejected int
	for _, e := range outboxEventsFor(t, ctx, tenantID, p.PaymentID) {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.rejected" {
			rejected++
		}
	}
	require.Equal(t, 1, rejected)
	payload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, "com.zoikosuite.banking.payment-execution-state.rejected")
	require.Contains(t, payload, `"event_type":"PAYMENT_REJECTED"`)
}

func TestPgStore_ConfirmSettlement_ConcurrentSameKeyOneEffect(t *testing.T) {
	tenantID := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	p := recordFinalityPayment(t, ctx, tenantID, "src-confirm-concurrent-1")

	var wg sync.WaitGroup
	results := make(chan struct {
		replay bool
		err    error
	}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, replay, err := testStore.ConfirmSettlement(ctx, p.PaymentID, confirmSettlementRequest(), "confirmer-1", "idem-concurrent-1", "corr-concurrent-1")
			results <- struct {
				replay bool
				err    error
			}{replay, err}
		}()
	}
	wg.Wait()
	close(results)

	var replays int
	for result := range results {
		require.NoError(t, result.err)
		if result.replay {
			replays++
		}
	}
	require.Equal(t, 1, replays, "one concurrent first attempt and one replay")

	var statusEventCount, evidenceCount, claimCount int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM status_events WHERE payment_id = $1 AND event_type = $2`, p.PaymentID, domain.EventPaymentSettled).Scan(&statusEventCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_finality_evidence WHERE payment_id = $1`, p.PaymentID).Scan(&evidenceCount))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM payment_command_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, "idem-concurrent-1").Scan(&claimCount))
	require.Equal(t, 1, statusEventCount)
	require.Equal(t, 1, evidenceCount)
	require.Equal(t, 1, claimCount)
}

// TestPgStore_FinalityMigrationDownRefusesWhileRowsDepend proves the down
// migration's guard, without leaving the shared test schema rolled back: the
// migration runs inside a transaction that is rolled back after the expected
// refusal.
func TestPgStore_FinalityEvidence_TenantIsolation(t *testing.T) {
	st := app(t)
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	p := newPayment(t, st, tenantA, "PRV-"+uuid.NewString())

	_, _, _, err := st.ConfirmSettlement(tctx(tenantA), p.PaymentID, confirmSettlementRequest(), "reviewer-1", "idem-rls-"+uuid.NewString(), "corr-rls")
	require.NoError(t, err)

	evidence, err := st.ListFinalityEvidence(tctx(tenantA), p.PaymentID)
	require.NoError(t, err)
	require.Len(t, evidence, 1)

	evidence, err = st.ListFinalityEvidence(tctx(tenantB), p.PaymentID)
	require.NoError(t, err)
	require.Empty(t, evidence)

	_, _, _, err = st.ConfirmSettlement(tctx(tenantB), p.PaymentID, confirmSettlementRequest(), "reviewer-2", "idem-rls-other-"+uuid.NewString(), "corr-rls")
	require.ErrorIs(t, err, domain.ErrPaymentNotFound)
}

func TestPgStore_FinalityMigrationDownRefusesWhileRowsDepend(t *testing.T) {
	downSQL, err := os.ReadFile("../../deployments/migrations/000008_finality_commands.down.sql")
	require.NoError(t, err)
	ctx := context.Background()

	t.Run("evidence row only", func(t *testing.T) {
		paymentID := uuid.NewString()
		tx, err := testPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		// Isolate the evidence guard: no claims and no manual-finality state,
		// just one evidence row on an otherwise ordinary PREPARED payment.
		_, err = tx.Exec(ctx, `TRUNCATE payment_finality_evidence, payment_command_idempotency`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_execution_states (payment_id, legal_entity_id, created_by_principal_id)
			VALUES ($1, $2, 'creator-1')`, paymentID, uuid.NewString())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_finality_evidence (
				tenant_id, payment_id, command, provider_reference, reason, effective_at,
				evidence_reference, mapping_version, finality_source, actor_principal_id, idempotency_key
			) VALUES (NULL, $1, 'CONFIRM_SETTLEMENT', 'provider-x', 'evidence-only', now(),
			          'evidence://down/1', $2, 'MANUAL_CONFIRMATION', 'actor-1', 'idem-down-evidence-only-1')`,
			paymentID, domain.ManualConfirmationMappingVersion)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, string(downSQL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "payment finality evidence exists")
	})

	t.Run("idempotency claim only", func(t *testing.T) {
		paymentID := uuid.NewString()
		tx, err := testPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		// The real schema is shared by the whole integration suite, and the
		// append-only trigger forbids DELETE. TRUNCATE inside this transaction
		// (rolled back at the end) gives this subtest a clean evidence table
		// without disturbing the other tests' committed rows.
		_, err = tx.Exec(ctx, `TRUNCATE payment_finality_evidence, payment_command_idempotency`)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, `
			INSERT INTO payment_execution_states (payment_id, legal_entity_id, created_by_principal_id)
			VALUES ($1, $2, 'creator-1')`, paymentID, uuid.NewString())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_command_idempotency (tenant_id, idempotency_key, command, payment_id, request_hash)
			VALUES (NULL, $1, 'CONFIRM_SETTLEMENT', $2, $3)`,
			"idem-down-claim-only-1", paymentID, strings.Repeat("c", 64))
		require.NoError(t, err)

		_, err = tx.Exec(ctx, string(downSQL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "idempotency claims exist")
	})

	t.Run("manual finality state only", func(t *testing.T) {
		paymentID := uuid.NewString()
		tx, err := testPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		_, err = tx.Exec(ctx, `TRUNCATE payment_finality_evidence, payment_command_idempotency`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_execution_states (
				payment_id, legal_entity_id, finality_source, mapping_version, created_by_principal_id
			) VALUES ($1, $2, 'MANUAL_CONFIRMATION', $3, 'creator-1')`,
			paymentID, uuid.NewString(), domain.ManualConfirmationMappingVersion)
		require.NoError(t, err)

		_, err = tx.Exec(ctx, string(downSQL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "payments depend on manual finality state")
	})
}
