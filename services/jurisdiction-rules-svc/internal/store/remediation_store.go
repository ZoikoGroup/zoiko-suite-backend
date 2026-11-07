package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 s23, s31 and JUR-NEG-19: the remediation register (migration 000026). The rules live in the database triggers; this is the access to it.

// RemediationProgress says how far a case has got.
type RemediationProgress struct {
	Total         int            `json:"total"`
	Dispositioned int            `json:"dispositioned"`
	Pending       int            `json:"pending"`
	ByDisposition map[string]int `json:"by_disposition"`
}

// RemediationCase is one remediation case.
type RemediationCase struct {
	CaseID         string               `json:"case_id"`
	PackRef        string               `json:"pack_ref"`
	Version        string               `json:"version"`
	Trigger        string               `json:"trigger"`
	RingCode       *string              `json:"ring_code"`
	RegionCode     *string              `json:"region_code"`
	Reason         string               `json:"reason"`
	EvidenceRef    *string              `json:"evidence_ref"`
	Status         string               `json:"status"`
	DecisionsTotal int                  `json:"decisions_total"`
	OpenedBy       string               `json:"opened_by"`
	OpenedAt       time.Time            `json:"opened_at"`
	ClosedBy       *string              `json:"closed_by"`
	ClosedAt       *time.Time           `json:"closed_at"`
	ClosureNote    *string              `json:"closure_note"`
	Progress       *RemediationProgress `json:"progress,omitempty"`
	// Opened is true when this call created the case, false when it attached to one already open. Not stored.
	Opened bool `json:"-"`
}

// RemediationDisposition is what was done about one decision.
type RemediationDisposition struct {
	Disposition   string    `json:"disposition"`
	EvidenceRef   string    `json:"evidence_ref"`
	Note          *string   `json:"note"`
	NewDecisionID *string   `json:"new_decision_id"`
	RecordedBy    string    `json:"recorded_by"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// RemediationDecision is one decision in a case, with its disposition when it has one.
type RemediationDecision struct {
	DecisionID   string                  `json:"decision_id"`
	DecisionKind string                  `json:"decision_kind"`
	Outcome      string                  `json:"outcome"`
	EffectiveAt  time.Time               `json:"effective_at"`
	DecidedAt    time.Time               `json:"decided_at"`
	RequestedBy  string                  `json:"requested_by"`
	Ring         string                  `json:"resolver_ring"`
	Region       string                  `json:"resolver_region"`
	Disposition  *RemediationDisposition `json:"disposition"`
}

const remediationCols = `c.case_id::text, p.pack_ref, v.version, c.trigger, c.ring_code, c.region_code, c.reason, c.evidence_ref, c.status, c.decisions_total,
	c.opened_by, c.opened_at, c.closed_by, c.closed_at, c.closure_note`
const remediationFrom = ` FROM remediation_cases c JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)`

func scanRemediation(r pgx.Row) (*RemediationCase, error) {
	var c RemediationCase
	err := r.Scan(&c.CaseID, &c.PackRef, &c.Version, &c.Trigger, &c.RingCode, &c.RegionCode, &c.Reason, &c.EvidenceRef, &c.Status, &c.DecisionsTotal,
		&c.OpenedBy, &c.OpenedAt, &c.ClosedBy, &c.ClosedAt, &c.ClosureNote)
	return &c, err
}

func refusal(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23514" || pgErr.Code == "23503") {
		return fmt.Errorf("%w: %s", domain.ErrRemediationRefused, pgErr.Message)
	}
	return nil
}

// scopeWhere builds the filter for "decisions made with this version in this scope". Empty ring or region means every one; since, when set,
// leaves out decisions older than the deployment that was rolled back.
func scopeWhere(versionID, ring, region string, since *time.Time) (string, []any) {
	where, args := `pack_version_id::text=$1`, []any{versionID}
	if ring != "" {
		args = append(args, ring)
		where += fmt.Sprintf(` AND resolver_ring=$%d`, len(args))
	}
	if region != "" {
		args = append(args, region)
		where += fmt.Sprintf(` AND resolver_region=$%d`, len(args))
	}
	if since != nil {
		args = append(args, *since)
		where += fmt.Sprintf(` AND created_at >= $%d`, len(args))
	}
	return where, args
}

// openRemediationTx snapshots the decisions made with a version in a scope into a case, inside the caller's transaction. It returns nil when there
// are none (nothing to remediate) and the case already open for the same version, trigger and scope when there is one (Opened false).
func (s *PgStore) openRemediationTx(ctx context.Context, tx pgx.Tx, versionID, trigger, ring, region string, since *time.Time, reason, evidence, actor string) (*RemediationCase, error) {
	var existing string
	err := tx.QueryRow(ctx, `SELECT case_id::text FROM remediation_cases WHERE pack_version_id::text=$1 AND trigger=$2
		AND COALESCE(ring_code,'')=$3 AND COALESCE(region_code,'')=$4 AND status='OPEN'`, versionID, trigger, ring, region).Scan(&existing)
	if err == nil {
		return &RemediationCase{CaseID: existing}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, s.registryFail("openRemediation existing", err, nil)
	}
	where, args := scopeWhere(versionID, ring, region, since)
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE `+where, args...).Scan(&n); err != nil {
		return nil, s.registryFail("openRemediation count", err, nil)
	}
	if n == 0 {
		return nil, nil
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO remediation_cases (pack_version_id, trigger, ring_code, region_code, reason, evidence_ref, decisions_total, opened_by)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8) RETURNING case_id::text`, versionID, trigger, nullIfEmpty(ring), nullIfEmpty(region), reason, nullIfEmpty(evidence), n, actor).Scan(&id); err != nil {
		return nil, s.registryFail("openRemediation insert", err, nil)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO remediation_case_decisions (case_id, decision_id) SELECT $`+fmt.Sprint(len(args)+1)+`::uuid, decision_id FROM rule_decision_evidence WHERE `+where,
		append(args, id)...); err != nil {
		return nil, s.registryFail("openRemediation decisions", err, nil)
	}
	return &RemediationCase{CaseID: id, DecisionsTotal: n, Opened: true}, nil
}

// OpenRemediationCase opens a case by hand for a version and scope (for an incident the automatic triggers do not cover). It is refused when
// no decision was made with the version in that scope, and returns the case already open for the same scope rather than opening a second.
func (s *PgStore) OpenRemediationCase(ctx context.Context, ref, version, ring, region, reason, evidence, actor string) (*RemediationCase, error) {
	tx, err := s.begin(ctx, "OpenRemediationCase")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, _, _, err := s.versionForUpdate(ctx, tx, ref, version, false)
	if err != nil {
		return nil, err
	}
	c, err := s.openRemediationTx(ctx, tx, id, domain.RemediationManual, ring, region, nil, reason, evidence, actor)
	if err != nil {
		if r := refusal(err); r != nil {
			return nil, r
		}
		return nil, err
	}
	if c == nil {
		return nil, domain.ErrNothingToRemediate
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("OpenRemediationCase commit", err, nil)
	}
	out, err := s.GetRemediationCase(ctx, c.CaseID)
	if err != nil {
		return nil, err
	}
	out.Opened = c.Opened
	return out, nil
}

func (s *PgStore) remediationProgress(ctx context.Context, q querier, caseID string) (*RemediationProgress, error) {
	p := &RemediationProgress{ByDisposition: map[string]int{}}
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM remediation_case_decisions WHERE case_id::text=$1`, caseID).Scan(&p.Total); err != nil {
		return nil, s.registryFail("remediationProgress total", err, nil)
	}
	rows, err := q.Query(ctx, `SELECT disposition, COUNT(*) FROM remediation_dispositions WHERE case_id::text=$1 GROUP BY 1`, caseID)
	if err != nil {
		return nil, s.registryFail("remediationProgress", err, nil)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			return nil, s.registryFail("remediationProgress scan", err, nil)
		}
		p.ByDisposition[d] = n
		p.Dispositioned += n
	}
	p.Pending = p.Total - p.Dispositioned
	return p, s.registryFail("remediationProgress rows", rows.Err(), nil)
}

// GetRemediationCase reads one case with its progress.
func (s *PgStore) GetRemediationCase(ctx context.Context, id string) (*RemediationCase, error) {
	c, err := scanRemediation(s.pool.QueryRow(ctx, `SELECT `+remediationCols+remediationFrom+` WHERE c.case_id::text=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetRemediationCase", err, domain.ErrRemediationNotFound)
	}
	if c.Progress, err = s.remediationProgress(ctx, s.pool, id); err != nil {
		return nil, err
	}
	return c, nil
}

// ListRemediationCases lists cases, newest first, optionally by status (OPEN or CLOSED).
func (s *PgStore) ListRemediationCases(ctx context.Context, status string, limit, offset int) ([]*RemediationCase, error) {
	limit = pageLimit(limit, 50, 200)
	rows, err := s.pool.Query(ctx, `SELECT `+remediationCols+remediationFrom+` WHERE ($1='' OR c.status=$1) ORDER BY c.opened_at DESC, c.case_id LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, s.registryFail("ListRemediationCases", err, nil)
	}
	defer rows.Close()
	out := []*RemediationCase{}
	for rows.Next() {
		c, err := scanRemediation(rows)
		if err != nil {
			return nil, s.registryFail("ListRemediationCases scan", err, nil)
		}
		out = append(out, c)
	}
	return out, s.registryFail("ListRemediationCases rows", rows.Err(), nil)
}

// ListRemediationDecisions lists the decisions of a case with their dispositions, optionally only those still pending.
func (s *PgStore) ListRemediationDecisions(ctx context.Context, caseID string, pendingOnly bool, limit, offset int) ([]*RemediationDecision, error) {
	limit = pageLimit(limit, 100, 500)
	if _, err := s.GetRemediationCase(ctx, caseID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT e.decision_id::text, e.decision_kind, e.outcome, e.effective_at, e.created_at, e.requested_by, COALESCE(e.resolver_ring,''), COALESCE(e.resolver_region,''),
			x.disposition, x.evidence_ref, x.note, x.new_decision_id::text, x.recorded_by, x.recorded_at
		FROM remediation_case_decisions d JOIN rule_decision_evidence e ON e.decision_id = d.decision_id
		LEFT JOIN remediation_dispositions x ON x.case_id = d.case_id AND x.decision_id = d.decision_id
		WHERE d.case_id::text=$1 AND ($2 = FALSE OR x.disposition IS NULL) ORDER BY e.created_at, e.decision_id LIMIT $3 OFFSET $4`, caseID, pendingOnly, limit, offset)
	if err != nil {
		return nil, s.registryFail("ListRemediationDecisions", err, nil)
	}
	defer rows.Close()
	out := []*RemediationDecision{}
	for rows.Next() {
		var d RemediationDecision
		var disp, evid, by *string
		var note, newID *string
		var at *time.Time
		if err := rows.Scan(&d.DecisionID, &d.DecisionKind, &d.Outcome, &d.EffectiveAt, &d.DecidedAt, &d.RequestedBy, &d.Ring, &d.Region, &disp, &evid, &note, &newID, &by, &at); err != nil {
			return nil, s.registryFail("ListRemediationDecisions scan", err, nil)
		}
		if disp != nil {
			d.Disposition = &RemediationDisposition{Disposition: *disp, EvidenceRef: *evid, Note: note, NewDecisionID: newID, RecordedBy: *by, RecordedAt: *at}
		}
		out = append(out, &d)
	}
	return out, s.registryFail("ListRemediationDecisions rows", rows.Err(), nil)
}

// RecordDispositionsResult counts what a batch did.
type RecordDispositionsResult struct {
	Recorded int `json:"recorded"`
	Replayed int `json:"replayed"`
}

// RecordDispositions records what was done about decisions in an OPEN case, all or nothing. Repeating an identical disposition is a replay;
// a different one for the same decision is refused, because dispositions are final.
func (s *PgStore) RecordDispositions(ctx context.Context, caseID string, in []domain.DispositionInput, actor string) (*RecordDispositionsResult, error) {
	tx, err := s.begin(ctx, "RecordDispositions")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM remediation_cases WHERE case_id::text=$1 FOR UPDATE`, caseID).Scan(&status); err != nil {
		return nil, s.registryFail("RecordDispositions case", err, domain.ErrRemediationNotFound)
	}
	if status != "OPEN" {
		return nil, fmt.Errorf("%w: the case is %s and takes no more dispositions", errInvalidLifecycle, status)
	}
	res := &RecordDispositionsResult{}
	for _, d := range in {
		var inCase bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM remediation_case_decisions WHERE case_id::text=$1 AND decision_id::text=$2)`, caseID, d.DecisionID).Scan(&inCase); err != nil {
			return nil, s.registryFail("RecordDispositions membership", err, nil)
		}
		if !inCase {
			return nil, fmt.Errorf("%w: %s", domain.ErrDecisionNotInCase, d.DecisionID)
		}
		var disp, evid string
		var newID *string
		err := tx.QueryRow(ctx, `SELECT disposition, evidence_ref, new_decision_id::text FROM remediation_dispositions WHERE case_id::text=$1 AND decision_id::text=$2`, caseID, d.DecisionID).Scan(&disp, &evid, &newID)
		if err == nil {
			same := disp == d.Disposition && evid == d.EvidenceRef && (newID == nil) == (d.NewDecisionID == nil) && (newID == nil || *newID == *d.NewDecisionID)
			if !same {
				return nil, fmt.Errorf("%w: %s", domain.ErrDispositionDiffers, d.DecisionID)
			}
			res.Replayed++
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, s.registryFail("RecordDispositions existing", err, nil)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO remediation_dispositions (case_id, decision_id, disposition, evidence_ref, note, new_decision_id, recorded_by)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6::uuid,$7)`, caseID, d.DecisionID, d.Disposition, d.EvidenceRef, nullIfEmpty(d.Note), d.NewDecisionID, actor); err != nil {
			if r := refusal(err); r != nil {
				return nil, r
			}
			return nil, s.registryFail("RecordDispositions insert", err, nil)
		}
		res.Recorded++
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("RecordDispositions commit", err, nil)
	}
	return res, nil
}

// CloseRemediationCase closes a case once every decision has a disposition, by someone other than the person who opened it. Closing a closed
// case changes nothing.
func (s *PgStore) CloseRemediationCase(ctx context.Context, id, note, actor string) (*RemediationCase, bool, error) {
	tx, err := s.begin(ctx, "CloseRemediationCase")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, openedBy string
	if err := tx.QueryRow(ctx, `SELECT status, opened_by FROM remediation_cases WHERE case_id::text=$1 FOR UPDATE`, id).Scan(&status, &openedBy); err != nil {
		return nil, false, s.registryFail("CloseRemediationCase lock", err, domain.ErrRemediationNotFound)
	}
	if status == "CLOSED" {
		c, err := s.GetRemediationCase(ctx, id)
		return c, false, err
	}
	var reasons []string
	p, err := s.remediationProgress(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if p.Pending > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d decision(s) have no disposition", p.Pending, p.Total))
	}
	if len(reasons) > 0 {
		return nil, false, &OpsBlocked{Reasons: reasons}
	}
	if openedBy == actor {
		return nil, false, domain.ErrNotIndependent
	}
	if _, err := tx.Exec(ctx, `UPDATE remediation_cases SET status='CLOSED', closed_by=$2, closed_at=NOW(), closure_note=$3 WHERE case_id::text=$1`, id, actor, note); err != nil {
		if r := refusal(err); r != nil {
			return nil, false, r
		}
		return nil, false, s.registryFail("CloseRemediationCase update", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("CloseRemediationCase commit", err, nil)
	}
	c, err := s.GetRemediationCase(ctx, id)
	return c, true, err
}

func rcID(c *RemediationCase) string {
	if c == nil {
		return "(none)"
	}
	return c.CaseID
}

// pageLimit applies a default when the caller gave none (0) and a ceiling so one request cannot read an unbounded list.
func pageLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
