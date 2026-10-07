//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

// TestMain provisions a real, throwaway embedded Postgres instance and
// applies every migration in filename order — this service had no
// real-Postgres test harness before this file, only a stub/mock store
// exercised by internal/handler/handler_test.go, which cannot prove a
// database-level trigger actually fires.
func TestMain(m *testing.M) {
	dbPort := uint32(15901 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(dbPort).
			Database("payment_adapter_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf("host=localhost port=%d dbname=payment_adapter_test user=postgres password=postgres sslmode=disable", dbPort)
	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}
	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_immutability.up.sql",
		"000003_add_rls.up.sql",
		"000004_add_authorization_verification.up.sql",
	} {
		sql, err := os.ReadFile("../../deployments/migrations/" + migration)
		if err != nil {
			fmt.Printf("read migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
		if _, err := testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("apply migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
	}

	testStore = store.NewPgStore(testPool, zap.NewNop())

	code := m.Run()
	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// TestPgStore_PreparedAttempt_AuthorizedFieldsAreImmutable is the real,
// negative-controlled proof of migration 000002's reject_attempt_mutation
// trigger — the literal fix for the spec's own negative path "payment
// amount changed after authorization." This mechanism existed before
// this test file but had no test exercising it at all.
func TestPgStore_PreparedAttempt_AuthorizedFieldsAreImmutable(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	attempt, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 100.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-immutable-1",
	}, "maker-1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	if attempt.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", attempt.Status)
	}

	// Application-layer: no store command exposes a way to edit amount —
	// only a raw UPDATE can even attempt it, which is exactly what the
	// trigger exists to refuse.
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET amount = 999999 WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected the trigger to refuse changing amount on a PREPARED attempt")
	}
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET payee_ref = 'acct-attacker' WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected the trigger to refuse changing payee_ref on a PREPARED attempt")
	}

	// Disable the trigger, confirm the same UPDATE now succeeds (proving
	// the trigger — not something else — was refusing it), then re-enable
	// and confirm refusal returns.
	if _, err := testPool.Exec(ctx, `ALTER TABLE payment_initiation_attempts DISABLE TRIGGER trg_reject_attempt_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET amount = 999999 WHERE attempt_id = $1`, attempt.AttemptID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE payment_initiation_attempts ENABLE TRIGGER trg_reject_attempt_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET amount = 1 WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_PreparedAttempt_AuthorizationFieldsPersistAndAreImmutable
// proves Wave 11a's new authorization_id/authorization_source columns
// round-trip through PrepareAttempt correctly and are covered by the same
// protected-field trigger as amount/payee_ref — a caller cannot swap in a
// different authorization_id after the fact to make a stale fingerprint
// verification look current.
func TestPgStore_PreparedAttempt_AuthorizationFieldsPersistAndAreImmutable(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	attempt, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 250.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-auth-fields-1",
		AuthorizationFingerprint: "sha256:real-fingerprint", AuthorizationID: "auth-real-1",
		AuthorizationSource: domain.AuthorizationSourcePaymentAuthorization,
	}, "maker-1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	if attempt.AuthorizationID != "auth-real-1" || attempt.AuthorizationSource != domain.AuthorizationSourcePaymentAuthorization {
		t.Fatalf("expected authorization_id/authorization_source to round-trip, got %+v", attempt)
	}

	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET authorization_id = 'auth-swapped' WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected the trigger to refuse changing authorization_id on a PREPARED attempt")
	}

	// Negative control: disable the trigger, confirm the same UPDATE now
	// succeeds (proving the trigger — not something else — was refusing
	// it), then re-enable and confirm refusal returns.
	if _, err := testPool.Exec(ctx, `ALTER TABLE payment_initiation_attempts DISABLE TRIGGER trg_reject_attempt_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET authorization_id = 'auth-swapped' WHERE attempt_id = $1`, attempt.AttemptID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE payment_initiation_attempts ENABLE TRIGGER trg_reject_attempt_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET authorization_id = 'auth-real-1' WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_SubmittedAttempt_IsFullyTerminal proves SUBMITTED blocks
// ALL mutation, not just the authorized fields — status itself can never
// be reverted once submitted, from this service's own perspective (BNK-07
// owns everything past that point).
func TestPgStore_SubmittedAttempt_IsFullyTerminal(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	attempt, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 50.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-immutable-2",
	}, "maker-1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	if _, err := testStore.MarkSubmitted(ctx, attempt.AttemptID, "provider-req-1", "provider-resp-1", "maker-1"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}

	if _, err := testPool.Exec(ctx, `UPDATE payment_initiation_attempts SET status = 'PREPARED' WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected the trigger to refuse reverting a SUBMITTED attempt back to PREPARED")
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM payment_initiation_attempts WHERE attempt_id = $1`, attempt.AttemptID); err == nil {
		t.Fatal("expected the trigger to refuse deleting any attempt row")
	}
}

// TestPgStore_PrepareAttempt_RejectsSecondUnresolvedAttempt is the real
// proof of Invariant #16: a retry with a BRAND-NEW idempotency_key for
// the same source_reference must not create a second, concurrent live
// attempt while the first is still unresolved — the idempotency_key
// unique index alone doesn't catch this, since the key differs.
func TestPgStore_PrepareAttempt_RejectsSecondUnresolvedAttempt(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	sourceRef := "instruction-" + uuid.New().String()

	first, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), SourceReference: sourceRef,
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-unresolved-1",
	}, "maker-1")
	if err != nil {
		t.Fatalf("first PrepareAttempt: %v", err)
	}

	// Same source_reference, a DIFFERENT idempotency_key — the exact retry
	// shape Invariant #16 exists to catch.
	second, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), SourceReference: sourceRef,
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-unresolved-2",
	}, "maker-1")
	if !errors.Is(err, domain.ErrUnresolvedAttemptExists) {
		t.Fatalf("expected ErrUnresolvedAttemptExists, got err=%v", err)
	}
	if second == nil || second.AttemptID != first.AttemptID {
		t.Fatalf("expected the ORIGINAL attempt id %s to be returned, got %+v", first.AttemptID, second)
	}

	var count int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM payment_initiation_attempts WHERE source_reference = $1`, sourceRef).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 attempt row for this source_reference, got %d — this is a duplicate-attempt bug if not 1", count)
	}

	// Once the first attempt is SUBMITTED, a second attempt for the SAME
	// source_reference must still be refused — SUBMITTED means a payment
	// was already sent to the provider for this instruction, and this
	// service never transitions an attempt OUT of SUBMITTED (finality
	// tracking is BNK-07/payment-status-svc's concern), so a duplicate
	// send must never be allowed regardless of how the first one later
	// resolves downstream.
	if _, err := testStore.MarkSubmitted(ctx, first.AttemptID, "provider-req-1", "provider-resp-1", "maker-1"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	third, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), SourceReference: sourceRef,
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-unresolved-3",
	}, "maker-1")
	if !errors.Is(err, domain.ErrUnresolvedAttemptExists) {
		t.Fatalf("expected ErrUnresolvedAttemptExists once the prior attempt is SUBMITTED, got %v", err)
	}
	if third == nil || third.AttemptID != first.AttemptID {
		t.Fatalf("expected the ORIGINAL (SUBMITTED) attempt id %s to be returned, got %+v", first.AttemptID, third)
	}

	// A genuinely new instruction — a different source_reference — is
	// completely unaffected, proving this isn't a global lockout.
	fourth, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), SourceReference: "instruction-" + uuid.New().String(),
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-unresolved-4",
	}, "maker-1")
	if err != nil {
		t.Fatalf("expected a genuinely new source_reference to be unaffected, got %v", err)
	}
	if fourth.AttemptID == first.AttemptID {
		t.Fatal("expected a genuinely new attempt row for a different source_reference")
	}
}

// TestPgStore_PrepareAttempt_EmptySourceReference_NeverDeduped proves an
// empty source_reference (the field is optional) is never treated as a
// dedup key — otherwise every caller who omits it would collide with
// every other such caller's unresolved attempts.
func TestPgStore_PrepareAttempt_EmptySourceReference_NeverDeduped(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	first, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 10.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-empty-ref-1",
	}, "maker-1")
	if err != nil {
		t.Fatalf("first PrepareAttempt: %v", err)
	}
	second, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 20.00, Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: "corr-empty-ref-2",
	}, "maker-1")
	if err != nil {
		t.Fatalf("expected two unrelated attempts with no source_reference to both succeed, got %v", err)
	}
	if second.AttemptID == first.AttemptID {
		t.Fatal("expected two genuinely distinct attempts")
	}
}
