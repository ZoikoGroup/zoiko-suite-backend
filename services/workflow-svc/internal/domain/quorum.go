package domain

import "time"

// Quorum / N-of-M dual-control approval (ZS-SVC-R-001 §5.1 WFC-03,
// §8.3). Confirmed via repo-wide search before this was built: no
// service anywhere in this platform expressed N-of-M approval —
// every prior "approval chain" concept (this service's own
// WorkflowStage included) was a strictly sequential, single-named-
// approver model. A QUORUM stage adds an eligible pool and a
// threshold alongside the existing SINGLE shape, without changing it.

const (
	StageTypeSingle = "SINGLE"
	StageTypeQuorum = "QUORUM"
)

const (
	QuorumDecisionApprove = "APPROVE"
	QuorumDecisionReject  = "REJECT"
)

// QuorumDecision is one eligible approver's vote on a QUORUM stage.
// Each eligible approver may cast exactly one decision, ever — the
// migration's composite UNIQUE/FK constraints are the real
// enforcement (NP-09's "concurrency-safe double-count protection");
// this struct is just the read shape.
type QuorumDecision struct {
	WorkflowStageQuorumDecisionID string    `json:"workflow_stage_quorum_decision_id"`
	WorkflowStageID               string    `json:"workflow_stage_id"`
	ApproverPrincipalID           string    `json:"approver_principal_id"`
	Decision                      string    `json:"decision"`
	Rationale                     *string   `json:"rationale,omitempty"`
	ActedAt                       time.Time `json:"acted_at"`
}

// SubmitQuorumVoteParams is one eligible approver's APPROVE/REJECT
// vote against the workflow's current QUORUM stage.
type SubmitQuorumVoteParams struct {
	WorkflowInstanceID string
	ActorPrincipalID   string
	// Action: APPROVE | REJECT.
	Action      string
	Rationale   *string
	CausationID *string
}

// QuorumStageStatus evaluates a QUORUM stage's current vote tally
// against its threshold and pool size, per §8.3's join semantics:
// reached once approvals >= required; failed once it becomes
// mathematically impossible to still reach required (not merely on
// the first dissenting vote) — a quorum is about reaching enough
// support, not unanimous agreement.
type QuorumStageStatus struct {
	PoolSize          int
	RequiredApprovals int
	ApprovalCount     int
	RejectionCount    int
	VotesCast         int
}

// Resolved reports the stage's outcome once it can be determined, or
// ("", false) while still PENDING more votes.
func (q QuorumStageStatus) Resolved() (outcome string, resolved bool) {
	if q.ApprovalCount >= q.RequiredApprovals {
		return "APPROVED", true
	}
	remainingVoters := q.PoolSize - q.VotesCast
	if q.ApprovalCount+remainingVoters < q.RequiredApprovals {
		// Even if every remaining eligible approver votes to approve,
		// the threshold can no longer be reached.
		return "REJECTED", true
	}
	return "", false
}
