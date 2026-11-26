package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// One-time, read-only diagnostic for SPEC_DEVIATIONS.md A1/B12 of consolidation-svc and general-ledger-svc.
//
// Between 2026-09-07 (b5a2b984, the posting path added) and 2026-09-30 (ddfa4015, general-ledger-svc started
// requiring transaction_currency/document_date on PostAccountingEvent) consolidation-svc's adjustment-posting path
// called general-ledger-svc's generic system-originated posting endpoint (POST /v1/postings/events) directly,
// with no book identifier. A journal posted that way lands as an ordinary journal of the group entity's own
// (statutory) book: general-ledger-svc's PostAccountingEvent handler never sets accounting_book_id or
// journal_purpose, and forwards the caller's source_event_id onto journal_headers.source_event_id unchanged.
//
// That path was replaced (05c75479, "fix(consolidation): post adjustments to the group book, never an entity
// ledger") by one that posts into the group's own accounting book via the kernel's consolidation-adjustments
// subsystem (consolidation_adjustments, consolidation_groups.group_book_id): every journal the kernel now
// produces for a group adjustment carries a real accounting_book_id (the group's book, never NULL) and
// journal_purpose = 'GROUP_ADJUSTMENT', and never sets source_event_id at all (consolidation_adjustments.source_ref
// is the only place the submitting service's reference is kept, via the group-adjustment proposal, not the
// journal header).
//
// ScanHistoricalGroupBookContamination finds journals that structurally match what that OLD, now-replaced path
// would have produced: an ordinary journal (no book) of a legal entity that is (or was) a consolidation group's
// reporting/parent entity, carrying a caller-declared source_event_id (the generic path's idempotency anchor —
// the kernel's own journals never set one), created inside the exposed window, and with no corresponding row in
// consolidation_adjustments (so it was not, in fact, accounted for by the safe group-book path under that same
// reference).
//
// This is a CANDIDATE list for manual review, not a final verdict: consolidation-svc's own adjustment ids are
// plain UUIDs with no distinguishing prefix, so "shape" alone cannot prove origin. The combination of signals
// above (no book, a caller-declared source_event_id, the legal entity being a real group's reporting entity, the
// exact historical window, and no matching consolidation_adjustments.source_ref) is what the data can actually
// support; an operator should still read each candidate's lines and description before treating it as confirmed
// contamination.
var ErrContaminationScanNeedsAllRows = errors.New("consol-adjustment-contamination-scan: database role must be SUPERUSER or BYPASSRLS to see every tenant's rows")

// DefaultContaminationWindow is the exposure window SPEC_DEVIATIONS.md names: from the day the vulnerable posting
// path shipped (b5a2b984) up to and including the day before general-ledger-svc started rejecting its request
// shape (ddfa4015).
func DefaultContaminationWindow() (start, end time.Time) {
	start = time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	end = time.Date(2026, time.September, 30, 24, 0, 0, 0, time.UTC) // exclusive upper bound: through end of Sep 30
	return start, end
}

// ContaminationCandidate is one journal that matches the historical vulnerable path's shape.
type ContaminationCandidate struct {
	TenantID      string
	JournalID     string
	JournalNumber string
	LegalEntityID string
	GroupID       string
	SourceEventID string
	FiscalPeriod  string
	Status        string
	Description   string
	CreatedAt     time.Time
}

// ContaminationReport is what one scan found.
type ContaminationReport struct {
	WindowStart, WindowEnd time.Time
	GroupEntityCount       int
	Candidates             []ContaminationCandidate
}

// ScanHistoricalGroupBookContamination runs the read-only query described above. It writes nothing. pool must be a
// role that bypasses row-level security (SUPERUSER or BYPASSRLS) — never the runtime zoiko_app role — or it would
// silently see only (or none of) one tenant's rows and under-report.
func ScanHistoricalGroupBookContamination(ctx context.Context, pool *pgxpool.Pool, windowStart, windowEnd time.Time) (*ContaminationReport, error) {
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		return nil, fmt.Errorf("consol-adjustment-contamination-scan: read database role: %w", err)
	}
	if !super && !bypass {
		return nil, ErrContaminationScanNeedsAllRows
	}

	report := &ContaminationReport{WindowStart: windowStart, WindowEnd: windowEnd}
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT (tenant_id, parent_entity_id)) FROM consolidation_groups`).Scan(&report.GroupEntityCount); err != nil {
		return nil, fmt.Errorf("consol-adjustment-contamination-scan: count group entities: %w", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT jh.tenant_id::text, jh.journal_id::text, COALESCE(jh.journal_number, ''), jh.legal_entity_id::text, ge.group_id::text,
		       jh.source_event_id, jh.fiscal_period, jh.status, jh.description, jh.created_at
		FROM journal_headers jh
		JOIN (SELECT DISTINCT tenant_id, parent_entity_id, group_id FROM consolidation_groups) ge
		  ON ge.tenant_id = jh.tenant_id AND ge.parent_entity_id = jh.legal_entity_id
		LEFT JOIN consolidation_adjustments ca
		  ON ca.tenant_id = jh.tenant_id AND ca.source_ref = jh.source_event_id
		WHERE jh.source_event_id IS NOT NULL
		  AND jh.accounting_book_id IS NULL
		  AND (jh.journal_purpose IS NULL OR jh.journal_purpose NOT IN ('CONSOLIDATION', 'ELIMINATION', 'GROUP_ADJUSTMENT'))
		  AND jh.created_at >= $1 AND jh.created_at < $2
		  AND ca.adjustment_id IS NULL
		ORDER BY jh.created_at`, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("consol-adjustment-contamination-scan: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c ContaminationCandidate
		if err := rows.Scan(&c.TenantID, &c.JournalID, &c.JournalNumber, &c.LegalEntityID, &c.GroupID, &c.SourceEventID, &c.FiscalPeriod, &c.Status, &c.Description, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("consol-adjustment-contamination-scan: scan row: %w", err)
		}
		report.Candidates = append(report.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consol-adjustment-contamination-scan: iterate rows: %w", err)
	}
	return report, nil
}
