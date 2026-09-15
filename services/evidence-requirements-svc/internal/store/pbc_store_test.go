package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/store"
)

func createSentRequest(t *testing.T, s *store.PgStore, ctx context.Context, tenantID string, correlationPrefix string) *domain.EvidenceRequest {
	t.Helper()
	requestID := uuid.NewString()
	legalEntityID := uuid.New().String()
	req, created, err := s.CreateEvidenceRequest(ctx, requestID, domain.CreateEvidenceRequestParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, Title: "Q1 confirmations", Description: "AR confirmation letters",
		AssignedToPrincipalID: "client-contact-1", CreatedByPrincipalID: "auditor-1", CorrelationID: correlationPrefix + "-create",
		DueAt: time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil || !created {
		t.Fatalf("create request: created=%v err=%v", created, err)
	}
	sent, err := s.SendEvidenceRequest(ctx, domain.SendEvidenceRequestParams{RequestID: req.RequestID, TenantID: tenantID, CorrelationID: correlationPrefix + "-send"})
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return sent
}

// TestPgStore_MarkSatisfied_RefusesSelfEvaluation is the real proof of
// AUD-NEG-016 "PBC submitter marks own file sufficient evidence": the
// principal who submitted the latest response may not be the one who
// marks the request SATISFIED, and this is a CAS WHERE clause, not just
// an app-layer check.
func TestPgStore_MarkSatisfied_RefusesSelfEvaluation(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()
	tenantID := uuid.New().String()
	s := store.New(pool, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := createSentRequest(t, s, ctx, tenantID, "selfeval")
	resp, created, err := s.SubmitResponse(ctx, domain.SubmitResponseParams{RequestID: req.RequestID, TenantID: tenantID, SubmittedByPrincipalID: "client-contact-1", CorrelationID: "selfeval-resp-1"})
	if err != nil || !created {
		t.Fatalf("submit response: created=%v err=%v", created, err)
	}
	if _, err := s.RecordScanResult(ctx, domain.ScanResultParams{ResponseID: resp.ResponseID, TenantID: tenantID, Result: domain.MalwareScanClean}); err != nil {
		t.Fatalf("record scan result: %v", err)
	}

	if _, err := s.MarkSatisfied(ctx, domain.MarkSatisfiedParams{RequestID: req.RequestID, TenantID: tenantID, ActorPrincipalID: "client-contact-1"}); err != domain.ErrSelfEvaluationNotAllowed {
		t.Fatalf("expected ErrSelfEvaluationNotAllowed when the submitter marks their own response satisfied, got %v", err)
	}

	satisfied, err := s.MarkSatisfied(ctx, domain.MarkSatisfiedParams{RequestID: req.RequestID, TenantID: tenantID, ActorPrincipalID: "auditor-1"})
	if err != nil || satisfied.Status != domain.EvidenceRequestSatisfied {
		t.Fatalf("expected a different evaluator to succeed: status=%v err=%v", satisfied, err)
	}
}

// TestPgStore_MarkSatisfied_RequiresCleanScan proves the malware-scan
// gate: a response whose artifact hasn't been confirmed CLEAN cannot
// satisfy the request, even by a legitimate third-party evaluator.
func TestPgStore_MarkSatisfied_RequiresCleanScan(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()
	tenantID := uuid.New().String()
	s := store.New(pool, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := createSentRequest(t, s, ctx, tenantID, "scangate")
	if _, _, err := s.SubmitResponse(ctx, domain.SubmitResponseParams{RequestID: req.RequestID, TenantID: tenantID, SubmittedByPrincipalID: "client-contact-1", CorrelationID: "scangate-resp-1"}); err != nil {
		t.Fatalf("submit response: %v", err)
	}

	if _, err := s.MarkSatisfied(ctx, domain.MarkSatisfiedParams{RequestID: req.RequestID, TenantID: tenantID, ActorPrincipalID: "auditor-1"}); err != domain.ErrArtifactNotScanned {
		t.Fatalf("expected ErrArtifactNotScanned before the scan completes, got %v", err)
	}
}

// TestPgStore_SubmitResponse_AlwaysCreatesNewVersion is the real proof of
// AUD-NEG-017 "new PBC response overwrites prior version": a second
// SubmitResponse call creates version 2, and version 1 remains readable
// and untouched — a raw UPDATE attempt on it is refused outright.
func TestPgStore_SubmitResponse_AlwaysCreatesNewVersion(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()
	tenantID := uuid.New().String()
	s := store.New(pool, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := createSentRequest(t, s, ctx, tenantID, "version")
	first, created1, err := s.SubmitResponse(ctx, domain.SubmitResponseParams{RequestID: req.RequestID, TenantID: tenantID, SubmittedByPrincipalID: "client-contact-1", CorrelationID: "version-resp-1"})
	if err != nil || !created1 || first.Version != 1 {
		t.Fatalf("first response: version=%d created=%v err=%v", first.Version, created1, err)
	}

	second, created2, err := s.SubmitResponse(ctx, domain.SubmitResponseParams{RequestID: req.RequestID, TenantID: tenantID, SubmittedByPrincipalID: "client-contact-1", CorrelationID: "version-resp-2"})
	if err != nil || !created2 || second.Version != 2 {
		t.Fatalf("second response: version=%d created=%v err=%v", second.Version, created2, err)
	}
	if second.ResponseID == first.ResponseID {
		t.Fatal("expected a genuinely new response row, not a mutation of the first")
	}

	if _, err := pool.Exec(ctx, `UPDATE evidence_request_responses SET submitted_by_principal_id='tampered' WHERE response_id=$1`, first.ResponseID); err == nil {
		t.Fatal("expected the reject_evidence_response_mutation trigger to refuse a raw UPDATE on an existing response's content")
	}
}
