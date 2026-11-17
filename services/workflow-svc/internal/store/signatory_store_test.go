package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

const sigHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func signatureParams(t *testing.T, req domain.SignatureRequirement, pool []string, required int) domain.CreateWorkflowParams {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	typ, id, ver, fp := "contract", "C-1", 3, sigHash
	return domain.CreateWorkflowParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		WorkflowType: "CONTRACT_SIGNATURE", InitiatedBy: "requester-1",
		SubjectType: &typ, SubjectID: &id, SubjectVersion: &ver, SubjectFingerprint: &fp,
		Stages: []domain.CreateWorkflowStageInput{{StageType: domain.StageTypeQuorum, RequiredApprovals: required, QuorumApprovers: pool}},
		Signatory: &domain.SignatoryBinding{TenantID: testTenantID, DecisionID: uuid.NewString(), PolicyVersionID: uuid.NewString(),
			RuleID: "pv#band:B3", DocumentHash: sigHash, Requirement: raw},
	}
}

func sign(s *store.PgStore, ctx context.Context, id, who, hash string, offices ...string) (*domain.WorkflowInstance, *domain.WorkflowStage, error) {
	inst, st, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: id, ActorPrincipalID: who, Action: "APPROVE", SignerOffices: offices, DocumentHash: hash,
	})
	return inst, st, err
}

// Doc 01 example G-02: CFO then CEO, two distinct persons, on the exact
// document. One person holding both offices is one signer.
func TestPgStore_Signature_OrderedDistinctOfficesOnTheBoundDocument(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	req := domain.SignatureRequirement{Kind: "SIGNATURE_QUORUM", MinDistinctSigners: 2, Sequencing: "SEQUENTIAL",
		Order: []string{"CFO", "CEO"}, RequiredOffices: []domain.OfficeSeat{{Office: "CFO", MinCount: 1}, {Office: "CEO", MinCount: 1}}}
	inst, _, _, err := s.CreateWorkflow(ctx, signatureParams(t, req, []string{"s1", "s2", "s3"}, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := s.FindSignatoryBinding(ctx, inst.WorkflowInstanceID)
	if err != nil || b == nil || b.DocumentHash != sigHash || b.RuleID != "pv#band:B3" {
		t.Fatalf("binding %+v %v", b, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE workflow_signatory_bindings SET document_hash = $1`, "sha256:"+string(make([]byte, 0))); err == nil {
		t.Fatal("a signatory binding must be immutable")
	}

	if _, _, err := sign(s, ctx, inst.WorkflowInstanceID, "s1", "sha256:"+uuidHex(), "CFO"); !errors.Is(err, domain.ErrSignatureDocumentChanged) {
		t.Fatalf("another document: %v", err)
	}
	if _, _, err := sign(s, ctx, inst.WorkflowInstanceID, "s1", sigHash); !errors.Is(err, domain.ErrSignerNotEligible) {
		t.Fatalf("no confirmed office: %v", err)
	}
	if _, _, err := sign(s, ctx, inst.WorkflowInstanceID, "s3", sigHash, "CEO"); !errors.Is(err, domain.ErrSignatureOutOfOrder) {
		t.Fatalf("CEO before CFO: %v", err)
	}
	// s1 holds both offices and signs first: counted as CFO only.
	if _, st, err := sign(s, ctx, inst.WorkflowInstanceID, "s1", sigHash, "CFO", "CEO"); err != nil || st.StageStatus != "PENDING" {
		t.Fatalf("first signature: %+v %v", st, err)
	}
	// s2 is only a CFO: the CFO seat is filled, the CEO is next.
	if _, _, err := sign(s, ctx, inst.WorkflowInstanceID, "s2", sigHash, "CFO"); !errors.Is(err, domain.ErrSignatureOutOfOrder) {
		t.Fatalf("a second CFO before the CEO: %v", err)
	}
	final, st, err := sign(s, ctx, inst.WorkflowInstanceID, "s3", sigHash, "CEO")
	if err != nil || st.StageStatus != "APPROVED" || final.WorkflowStatus != "APPROVED" {
		t.Fatalf("CFO then CEO by distinct people must complete: %+v %+v %v", st, final, err)
	}
	signers, err := s.SignaturesFor(ctx, inst.WorkflowInstanceID)
	if err != nil || len(signers) != 2 || signers[0].PrincipalID != "s1" || len(signers[0].Offices) != 2 || signers[1].Offices[0] != "CEO" {
		t.Fatalf("signatures %+v %v", signers, err)
	}
}

// Enough signatures is not enough: two Director seats need two Directors.
func TestPgStore_Signature_OfficesMustBeFilledByDistinctSigners(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	req := domain.SignatureRequirement{Kind: "SIGNATURE_QUORUM", MinDistinctSigners: 2,
		RequiredOffices: []domain.OfficeSeat{{Office: "DIRECTOR", MinCount: 2}}, EligibleOffices: []string{"DIRECTOR", "FINANCE_SIGNER"}}
	inst, _, _, err := s.CreateWorkflow(ctx, signatureParams(t, req, []string{"a", "b", "c"}, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, st, err := sign(s, ctx, inst.WorkflowInstanceID, "a", sigHash, "DIRECTOR"); err != nil || st.StageStatus != "PENDING" {
		t.Fatalf("a: %+v %v", st, err)
	}
	_, st, err := sign(s, ctx, inst.WorkflowInstanceID, "b", sigHash, "FINANCE_SIGNER")
	if err != nil || st.StageStatus != "PENDING" {
		t.Fatalf("2 signatures but one Director must stay PENDING: %+v %v", st, err)
	}
	final, st, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "c", Action: "REJECT", Rationale: strPtr("not a director")})
	if err != nil || st.StageStatus != "REJECTED" || final.WorkflowStatus != "REJECTED" {
		t.Fatalf("every voter voted and the Director seats cannot be filled: %+v %+v %v", st, final, err)
	}
}

func uuidHex() string {
	u := uuid.New()
	return hexOf(u[:]) + hexOf(u[:])
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func strPtr(s string) *string { return &s }
