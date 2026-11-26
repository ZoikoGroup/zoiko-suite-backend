package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/outbox"
)

// Governed risk acceptance (ZS-SVC-Z-001 DQC-05 section 8). See domain/risk.go for the rules and migration 000010
// for the database's own copy of them.

const riskCols = `k.acceptance_id, k.tenant_id, k.run_id, k.result_id, r.rule_key, r.severity, k.reason, k.evidence_refs, k.requested_by, k.requested_at`

func scanRisk(row pgx.Row) (*domain.RiskAcceptance, error) {
	var k domain.RiskAcceptance
	var sev *string
	if err := row.Scan(&k.AcceptanceID, &k.TenantID, &k.RunID, &k.ResultID, &k.RuleKey, &sev, &k.Reason, &k.EvidenceRefs, &k.RequestedBy, &k.RequestedAt); err != nil {
		return nil, err
	}
	if sev != nil {
		s := domain.Severity(*sev)
		k.Severity = &s
	}
	k.Approvals = []domain.RiskApproval{}
	k.ApprovalsRequired = domain.RiskApprovalsRequired
	k.Status = domain.RiskProposed
	return &k, nil
}

func loadApprovals(ctx context.Context, tx pgx.Tx, k *domain.RiskAcceptance) error {
	rows, err := tx.Query(ctx, `SELECT approver, reason, approved_at FROM dq_risk_acceptance_approvals WHERE acceptance_id = $1 ORDER BY approved_at, approver`, k.AcceptanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a domain.RiskApproval
		if err := rows.Scan(&a.Approver, &a.Reason, &a.ApprovedAt); err != nil {
			return err
		}
		k.Approvals = append(k.Approvals, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(k.Approvals) >= domain.RiskApprovalsRequired {
		k.Status = domain.RiskAccepted
	}
	return nil
}

func loadRisk(ctx context.Context, tx pgx.Tx, where string, forUpdate bool, args ...any) (*domain.RiskAcceptance, error) {
	q := `SELECT ` + riskCols + ` FROM dq_risk_acceptances k JOIN dq_results r ON r.result_id = k.result_id WHERE ` + where
	if forUpdate {
		q += ` FOR UPDATE OF k`
	}
	k, err := scanRisk(tx.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRiskNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, loadApprovals(ctx, tx, k)
}

func riskEvent(ctx context.Context, tx pgx.Tx, tenantID, eventType string, k *domain.RiskAcceptance, extra map[string]any) error {
	payload := map[string]any{
		"acceptance_id": k.AcceptanceID, "run_id": k.RunID, "result_id": k.ResultID, "rule_key": k.RuleKey, "severity": k.Severity,
		"requested_by": k.RequestedBy, "reason": k.Reason, "evidence_refs": k.EvidenceRefs,
		"approvals": len(k.Approvals), "approvals_required": domain.RiskApprovalsRequired,
	}
	for key, v := range extra {
		payload[key] = v
	}
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_risk_acceptance", AggregateID: k.AcceptanceID,
		EventType: eventType, TenantID: &tenantID, Payload: payload})
}

// RequestRiskAcceptance proposes accepting the risk of one failed mandatory result. It needs no approval yet.
func (s *PgStore) RequestRiskAcceptance(ctx context.Context, tenantID, runID, actor string, req domain.RequestRiskAcceptanceRequest, claim domain.IdempotencyClaim) (*domain.RiskAcceptance, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.RiskAcceptance
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, runID, false)
		if err != nil {
			return err
		}
		if run.ContractID == nil {
			return fmt.Errorf("%w: a rule-set run has no certification profile", domain.ErrRiskNotPermitted)
		}
		contract, err := loadContractByID(ctx, tx, *run.ContractID)
		if err != nil {
			return err
		}
		if !contract.Body.CertificationProfile.AllowRiskAcceptance {
			return domain.ErrRiskNotPermitted
		}
		if run.Status != domain.RunExceptionsOpen {
			return fmt.Errorf("%w: the run is %s", domain.ErrRiskNotAcceptable, run.Status)
		}
		result, err := loadResult(ctx, tx, req.ResultID)
		if err != nil {
			return err
		}
		if result.RunID != runID {
			return domain.ErrResultNotFound
		}
		if result.Status != domain.ResultFail {
			return fmt.Errorf("%w: the result is %s; only a Fail is a risk, a missing result is an absence of evidence", domain.ErrRiskNotAcceptable, result.Status)
		}
		if result.Mandatory == nil || !*result.Mandatory {
			return fmt.Errorf("%w: the rule is not mandatory for certification, so nothing needs accepting", domain.ErrRiskNotAcceptable)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dq_risk_acceptances WHERE result_id = $1`, req.ResultID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return domain.ErrRiskAlreadyRequested
		}
		id := newID(domain.PrefixRiskAcceptance)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dq_risk_acceptances (acceptance_id, tenant_id, run_id, result_id, reason, evidence_refs, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, tenantID, runID, req.ResultID, strings.TrimSpace(req.Reason), req.EvidenceRefs, actor); err != nil {
			return fmt.Errorf("insert risk acceptance: %w", err)
		}
		k, err := loadRisk(ctx, tx, `k.acceptance_id = $1`, false, id)
		if err != nil {
			return err
		}
		out = k
		return riskEvent(ctx, tx, tenantID, "dqc.risk.requested", k, nil)
	})
	return out, err
}

// ApproveRiskAcceptance adds one approval. The requester cannot approve, nobody approves twice, and once the
// required number is reached the acceptance is ACCEPTED and takes no more.
func (s *PgStore) ApproveRiskAcceptance(ctx context.Context, tenantID, acceptanceID, actor string, req domain.ApproveRiskAcceptanceRequest, claim domain.IdempotencyClaim) (*domain.RiskAcceptance, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.RiskAcceptance
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		k, err := loadRisk(ctx, tx, `k.acceptance_id = $1`, true, acceptanceID)
		if err != nil {
			return err
		}
		// Checked before the idempotency key is claimed, so a refused approval does not consume it.
		if actor == k.RequestedBy {
			return domain.ErrRiskApproverIsRequester
		}
		for _, a := range k.Approvals {
			if a.Approver == actor {
				return domain.ErrRiskAlreadyApproved
			}
		}
		if k.Status == domain.RiskAccepted {
			return domain.ErrRiskAlreadyAccepted
		}
		run, err := loadRun(ctx, tx, k.RunID, false)
		if err != nil {
			return err
		}
		if run.Status != domain.RunExceptionsOpen {
			return fmt.Errorf("%w: the run is now %s", domain.ErrRiskNotAcceptable, run.Status)
		}
		claim.ResourceID = k.AcceptanceID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dq_risk_acceptance_approvals (tenant_id, acceptance_id, approver, reason) VALUES ($1, $2, $3, $4)`,
			tenantID, k.AcceptanceID, actor, strings.TrimSpace(req.Reason)); err != nil {
			return fmt.Errorf("insert risk approval: %w", err)
		}
		k, err = loadRisk(ctx, tx, `k.acceptance_id = $1`, false, acceptanceID)
		if err != nil {
			return err
		}
		out = k
		if err := riskEvent(ctx, tx, tenantID, "dqc.risk.approved", k, map[string]any{"approver": actor}); err != nil {
			return err
		}
		if k.Status == domain.RiskAccepted {
			approvers := make([]string, 0, len(k.Approvals))
			for _, a := range k.Approvals {
				approvers = append(approvers, a.Approver)
			}
			return riskEvent(ctx, tx, tenantID, "dqc.risk.accepted", k, map[string]any{"approvers": approvers})
		}
		return nil
	})
	return out, err
}

// ListRiskAcceptances lists a run's risk acceptances with their approvals.
func (s *PgStore) ListRiskAcceptances(ctx context.Context, tenantID, runID string) ([]domain.RiskAcceptance, error) {
	out := []domain.RiskAcceptance{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadRun(ctx, tx, runID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+riskCols+` FROM dq_risk_acceptances k JOIN dq_results r ON r.result_id = k.result_id
			WHERE k.run_id = $1 ORDER BY k.requested_at`, runID)
		if err != nil {
			return err
		}
		var ks []*domain.RiskAcceptance
		for rows.Next() {
			k, err := scanRisk(rows)
			if err != nil {
				rows.Close()
				return err
			}
			ks = append(ks, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range ks {
			if err := loadApprovals(ctx, tx, k); err != nil {
				return err
			}
			out = append(out, *k)
		}
		return nil
	})
	return out, err
}

// acceptedRisksFor decides whether a run with exceptions open can be certified over governed risk acceptance, and
// returns the risks the certification would rest on. Every mandatory result that is not a Pass must be a Fail with an
// ACCEPTED risk acceptance, and the certifier must be independent of all of them. Anything else is the same refusal a
// run with open exceptions always got.
func (s *PgStore) acceptedRisksFor(ctx context.Context, tx pgx.Tx, run *domain.DQRun, src *ruleSource, results []domain.DQResult, actor string) ([]domain.AcceptedRisk, error) {
	refuse := func(why string) error {
		return fmt.Errorf("%w: run must be Running (clean, no failing results) to certify; it is %s (%s)", domain.ErrRunHasOpenIssues, run.Status, why)
	}
	if !src.isContract() || !src.AllowRiskAcceptance {
		return nil, refuse("its contract does not permit certifying over a failed rule")
	}
	byRule := map[string]domain.DQResult{}
	for _, r := range results {
		byRule[r.RuleKey] = r
	}
	var out []domain.AcceptedRisk
	for _, sp := range src.Specs {
		if !sp.Mandatory {
			continue
		}
		res, ok := byRule[sp.Key]
		if !ok || res.Status == domain.ResultPass {
			continue
		}
		if res.Status != domain.ResultFail {
			return nil, refuse(fmt.Sprintf("rule %q is %s, an absence of evidence that cannot be risk-accepted", sp.Key, res.Status))
		}
		k, err := loadRisk(ctx, tx, `k.result_id = $1`, false, res.ResultID)
		if errors.Is(err, domain.ErrRiskNotFound) {
			return nil, refuse(fmt.Sprintf("rule %q failed and has no risk acceptance", sp.Key))
		}
		if err != nil {
			return nil, err
		}
		if k.Status != domain.RiskAccepted {
			return nil, refuse(fmt.Sprintf("the risk acceptance for rule %q has %d of %d approvals", sp.Key, len(k.Approvals), domain.RiskApprovalsRequired))
		}
		approvers := make([]string, 0, len(k.Approvals))
		if actor == k.RequestedBy {
			return nil, domain.ErrRiskCertifierNotIndependent
		}
		for _, a := range k.Approvals {
			if actor == a.Approver {
				return nil, domain.ErrRiskCertifierNotIndependent
			}
			approvers = append(approvers, a.Approver)
		}
		sort.Strings(approvers)
		out = append(out, domain.AcceptedRisk{AcceptanceID: k.AcceptanceID, ResultID: res.ResultID, RuleKey: sp.Key, Severity: res.Severity,
			Reason: k.Reason, RequestedBy: k.RequestedBy, Approvers: approvers})
	}
	if len(out) == 0 {
		return nil, refuse("no mandatory result carries an accepted risk")
	}
	return out, nil
}
