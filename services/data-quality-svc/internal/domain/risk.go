package domain

import (
	"fmt"
	"strings"
	"time"
)

// Governed risk acceptance (ZS-SVC-Z-001 DQC-05 section 8: "No unresolved BLOCKER exception unless certification
// profile explicitly permits and records governed risk acceptance").
//
// A run whose mandatory rule FAILED is normally uncertifiable: the way back is a clean reperform. A contract whose
// certification profile sets allow_risk_acceptance can be certified over a failed mandatory rule, but only for a
// result with an ACCEPTED risk acceptance:
//
//   - requested by one person, with a reason and evidence references;
//   - approved by TWO other people (RiskApprovalsRequired), each distinct and neither the requester;
//   - and the person who then certifies the run is none of them.
//
// Only a Fail can be accepted. A result that is Indeterminate is an absence of evidence, and accepting it would
// coerce that absence into a pass (INV-04).

const (
	PrefixRiskAcceptance = "dqk_"
	// RiskApprovalsRequired is the number of distinct approvers that make a risk acceptance ACCEPTED.
	RiskApprovalsRequired = 2

	RiskProposed = "PROPOSED"
	RiskAccepted = "ACCEPTED"
)

type RiskApproval struct {
	Approver   string    `json:"approver"`
	Reason     string    `json:"reason"`
	ApprovedAt time.Time `json:"approved_at"`
}

type RiskAcceptance struct {
	AcceptanceID      string         `json:"acceptance_id"`
	TenantID          string         `json:"tenant_id"`
	RunID             string         `json:"run_id"`
	ResultID          string         `json:"result_id"`
	RuleKey           string         `json:"rule_key,omitempty"`
	Severity          *Severity      `json:"severity,omitempty"`
	Reason            string         `json:"reason"`
	EvidenceRefs      []string       `json:"evidence_refs"`
	RequestedBy       string         `json:"requested_by"`
	RequestedAt       time.Time      `json:"requested_at"`
	Approvals         []RiskApproval `json:"approvals"`
	ApprovalsRequired int            `json:"approvals_required"`
	Status            string         `json:"status"`
}

// AcceptedRisk is what a certification records about a risk it was issued over, sealed with it.
type AcceptedRisk struct {
	AcceptanceID string    `json:"acceptance_id"`
	ResultID     string    `json:"result_id"`
	RuleKey      string    `json:"rule_key"`
	Severity     *Severity `json:"severity,omitempty"`
	Reason       string    `json:"reason"`
	RequestedBy  string    `json:"requested_by"`
	Approvers    []string  `json:"approvers"`
}

type RequestRiskAcceptanceRequest struct {
	ResultID     string   `json:"result_id"`
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs"`
}

func (r RequestRiskAcceptanceRequest) Validate() error {
	if strings.TrimSpace(r.ResultID) == "" {
		return fmt.Errorf("%w: result_id is required", ErrRiskInvalid)
	}
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > maxRevocationReasonLen {
		return fmt.Errorf("%w: a reason is required (at most %d characters)", ErrRiskInvalid, maxRevocationReasonLen)
	}
	if len(r.EvidenceRefs) == 0 || len(r.EvidenceRefs) > maxRevocationEvidenceRefs {
		return fmt.Errorf("%w: between 1 and %d evidence references are required", ErrRiskInvalid, maxRevocationEvidenceRefs)
	}
	for _, e := range r.EvidenceRefs {
		if strings.TrimSpace(e) == "" || len(e) > maxRevocationEvidenceRefLen {
			return fmt.Errorf("%w: each evidence reference must be 1-%d characters", ErrRiskInvalid, maxRevocationEvidenceRefLen)
		}
	}
	return nil
}

type ApproveRiskAcceptanceRequest struct {
	Reason string `json:"reason"`
}

func (r ApproveRiskAcceptanceRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > maxRevocationReasonLen {
		return fmt.Errorf("%w: an approval needs a reason (at most %d characters)", ErrRiskInvalid, maxRevocationReasonLen)
	}
	return nil
}

var (
	ErrRiskInvalid                 = errorString("invalid risk acceptance")
	ErrRiskNotFound                = errorString("risk acceptance not found")
	ErrRiskNotPermitted            = errorString("the run's contract does not permit risk acceptance")
	ErrRiskNotAcceptable           = errorString("only a failed, mandatory result of a run with exceptions open can be risk-accepted")
	ErrRiskAlreadyRequested        = errorString("a risk acceptance already exists for this result")
	ErrRiskApproverIsRequester     = errorString("the person who requested a risk acceptance cannot approve it")
	ErrRiskAlreadyApproved         = errorString("you have already approved this risk acceptance")
	ErrRiskAlreadyAccepted         = errorString("this risk acceptance already has the approvals it needs")
	ErrRiskCertifierNotIndependent = errorString("the certifier must be someone other than the requester and every approver of the risks the certification rests on")
)
