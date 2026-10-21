package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/testdb"
)

func TestPgStore_TenantIsolation_RLS(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-000000000001"
	tenantB := "00000000-0000-0000-0000-000000000002"

	decA := &domain.PrivacyDecision{
		DecisionID:           uuid.New().String(),
		SubjectRef:           "user-a",
		ProcessingActivityID: "act-1",
		PurposeID:            "purp-1",
		ProposedOperation:    domain.OperationUse,
		Result:               domain.ResultPermit,
		ReasonCodes:          []string{},
		PolicyRefs:           []string{"CONSENT"},
		ActorPrincipalID:     "principal-a",
		CorrelationID:        "corr-a",
		InputFingerprint:     "fingerprint-a",
	}

	// 1. Record decision for Tenant A
	if err := st.RecordDecision(ctx, tenantA, decA); err != nil {
		t.Fatalf("record decision tenant A: %v", err)
	}

	// 2. Tenant A can read its own decision
	foundA, err := st.FindDecision(ctx, tenantA, decA.DecisionID)
	if err != nil {
		t.Fatalf("find decision tenant A: %v", err)
	}
	if foundA.DecisionID != decA.DecisionID {
		t.Fatalf("expected decision ID %s, got %s", decA.DecisionID, foundA.DecisionID)
	}
	if foundA.TenantID == nil || *foundA.TenantID != tenantA {
		t.Fatalf("expected tenant ID %s, got %v", tenantA, foundA.TenantID)
	}
	if len(foundA.PolicyRefs) != 1 || foundA.PolicyRefs[0] != "CONSENT" {
		t.Fatalf("expected policy_refs [CONSENT], got %v", foundA.PolicyRefs)
	}
	if foundA.EvidenceRef == "" {
		t.Fatalf("expected evidence_ref to be populated, got empty")
	}

	// 3. Tenant B CANNOT read Tenant A's decision (returns ErrDecisionNotFound under RLS)
	foundB, err := st.FindDecision(ctx, tenantB, decA.DecisionID)
	if err == nil || foundB != nil {
		t.Fatalf("CRITICAL SECURITY VIOLATION: Tenant B could read Tenant A's decision under RLS!")
	}
	if err != domain.ErrDecisionNotFound {
		t.Fatalf("expected ErrDecisionNotFound for cross-tenant read, got %v", err)
	}
}

func TestPgStore_NullTenantRejected(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	dec := &domain.PrivacyDecision{
		DecisionID:           uuid.New().String(),
		SubjectRef:           "user-null",
		ProcessingActivityID: "act-1",
		PurposeID:            "purp-1",
		ProposedOperation:    domain.OperationUse,
		Result:               domain.ResultPermit,
		ActorPrincipalID:     "principal-1",
	}

	// Calling RecordDecision with empty tenantID must fail
	if err := st.RecordDecision(ctx, "", dec); err == nil {
		t.Fatalf("expected error when recording decision with empty tenantID, got nil")
	}

	// Direct INSERT with NULL tenant_id on restricted role must fail due to NOT NULL constraint and RLS
	_, err := db.App.Exec(ctx, `
		INSERT INTO privacy_decisions (decision_id, tenant_id, subject_ref, processing_activity_id, purpose_id, proposed_operation, result, actor_principal_id)
		VALUES ($1, NULL, 'subj', 'act', 'purp', 'USE', 'PERMIT', 'actor')`,
		uuid.New().String(),
	)
	if err == nil {
		t.Fatalf("CRITICAL SECURITY VIOLATION: NULL tenant_id insert succeeded!")
	}
}

func TestPgStore_AppendOnly_ImmutabilityTrigger(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000003"
	dec := &domain.PrivacyDecision{
		DecisionID:           uuid.New().String(),
		SubjectRef:           "user-immut",
		ProcessingActivityID: "act-1",
		PurposeID:            "purp-1",
		ProposedOperation:    domain.OperationUse,
		Result:               domain.ResultPermit,
		ActorPrincipalID:     "principal-1",
	}

	if err := st.RecordDecision(ctx, tenantID, dec); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	// Attempt to UPDATE decision must be rejected by reject_decision_mutation trigger
	_, err := db.Admin.Exec(ctx, `
		UPDATE privacy_decisions SET result = 'BLOCK' WHERE decision_id = $1`,
		dec.DecisionID,
	)
	if err == nil {
		t.Fatalf("expected UPDATE to fail on append-only privacy_decisions evidence table, but succeeded")
	}

	// Attempt to DELETE decision must be rejected by reject_decision_mutation trigger
	_, err = db.Admin.Exec(ctx, `
		DELETE FROM privacy_decisions WHERE decision_id = $1`,
		dec.DecisionID,
	)
	if err == nil {
		t.Fatalf("expected DELETE to fail on append-only privacy_decisions evidence table, but succeeded")
	}
}

func TestPgStore_Idempotency_TenantScoped(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	tenantA := "00000000-0000-0000-0000-000000000001"
	tenantB := "00000000-0000-0000-0000-000000000002"
	key := "idem-test-key-1"

	dec := &domain.PrivacyDecision{
		SubjectRef: "u", ProcessingActivityID: "a", PurposeID: "p", ProposedOperation: domain.OperationUse,
		Result: domain.ResultPermit, ActorPrincipalID: "principal-1", InputFingerprint: "v2:x",
	}
	claim := domain.IdempotencyRecord{Key: key, TenantID: tenantA, PrincipalID: "principal-1", Endpoint: "EvaluateDecision", RequestHash: "hash-a"}
	existing, err := st.CommitDecision(ctx, claim, dec, func(d *domain.PrivacyDecision) (int, []byte, error) {
		return 200, []byte(`{"result":"PERMIT"}`), nil
	})
	if err != nil || existing != nil {
		t.Fatalf("commit: existing=%v err=%v", existing, err)
	}

	// Tenant A / same principal finds its idempotency record
	gotA, err := st.GetIdempotency(ctx, tenantA, "principal-1", key)
	if err != nil {
		t.Fatalf("get idempotency tenant A: %v", err)
	}
	if gotA == nil || gotA.RequestHash != "hash-a" || gotA.ResponseCode != 200 {
		t.Fatalf("expected idempotency record for tenant A, got %v", gotA)
	}

	// Tenant B cannot see Tenant A's idempotency record
	gotB, err := st.GetIdempotency(ctx, tenantB, "principal-1", key)
	if err != nil {
		t.Fatalf("get idempotency tenant B: %v", err)
	}
	if gotB != nil {
		t.Fatalf("CRITICAL SECURITY VIOLATION: Tenant B saw Tenant A's idempotency record: %v", gotB)
	}
}

func TestPgStore_DurableDecisionFields(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	tenantID := "00000000-0000-0000-0000-000000000005"
	exp := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	receiptID := "receipt-xyz"
	noticeVerID := "nv-1.0"
	holdID := "hold-999"
	transferID := "td-456"

	dec := &domain.PrivacyDecision{
		DecisionID:           uuid.New().String(),
		SubjectRef:           "user-fields",
		SubjectContext:       &domain.SubjectContext{SubjectRef: "user-fields", SubjectClass: "CUSTOMER", Jurisdiction: "GB"},
		DataContext:          &domain.DataContext{DataCategories: []string{"CONTACT"}, Classification: "CONFIDENTIAL"},
		ProcessingActivityID: "act-1",
		PurposeID:            "purp-1",
		ProposedOperation:    domain.OperationUse,
		Result:               domain.ResultPermit,
		ReasonCodes:          []string{},
		Constraints:          []domain.DecisionConstraint{{Type: "RECIPIENT_LIMITATION", Description: "Internal only"}},
		PolicyRefs:           []string{"GDPR_ART_6_1_A", "POLICY_HR_V1"},
		EvidenceRef:          "prv-dec:custom",
		ExpiresAt:            &exp,
		ConsentReceiptID:     &receiptID,
		NoticeVersionID:      &noticeVerID,
		LegalHoldID:          &holdID,
		TransferDecisionID:   &transferID,
		ActorPrincipalID:     "principal-actor-01",
		CorrelationID:        "corr-id-007",
		InputFingerprint:     "fingerprint-sha256-abc",
	}

	if err := st.RecordDecision(ctx, tenantID, dec); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	got, err := st.FindDecision(ctx, tenantID, dec.DecisionID)
	if err != nil {
		t.Fatalf("find decision: %v", err)
	}

	if len(got.PolicyRefs) != 2 || got.PolicyRefs[0] != "GDPR_ART_6_1_A" {
		t.Errorf("policy_refs mismatch: %v", got.PolicyRefs)
	}
	if got.EvidenceRef != "prv-dec:custom" {
		t.Errorf("evidence_ref mismatch: %s", got.EvidenceRef)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at mismatch: got %v, expected %v", got.ExpiresAt, exp)
	}
	if got.ConsentReceiptID == nil || *got.ConsentReceiptID != receiptID {
		t.Errorf("consent_receipt_id mismatch: %v", got.ConsentReceiptID)
	}
	if got.NoticeVersionID == nil || *got.NoticeVersionID != noticeVerID {
		t.Errorf("notice_version_id mismatch: %v", got.NoticeVersionID)
	}
	if got.LegalHoldID == nil || *got.LegalHoldID != holdID {
		t.Errorf("legal_hold_id mismatch: %v", got.LegalHoldID)
	}
	if got.TransferDecisionID == nil || *got.TransferDecisionID != transferID {
		t.Errorf("transfer_decision_id mismatch: %v", got.TransferDecisionID)
	}
	if len(got.Constraints) != 1 || got.Constraints[0].Type != "RECIPIENT_LIMITATION" {
		t.Errorf("constraints mismatch: %v", got.Constraints)
	}
}
