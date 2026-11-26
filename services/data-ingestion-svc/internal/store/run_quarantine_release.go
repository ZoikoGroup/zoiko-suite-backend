package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/outbox"
)

// Governed release of run-level quarantine (INT-04). See domain/quarantine_release.go for the model and migration
// 000004 for the database's own copy of the rules.

type runItem struct {
	QuarantineID  string
	RunID         string
	SourceID      string
	PayloadRef    string
	QuarantinedBy string
}

func loadRunItem(ctx context.Context, tx pgx.Tx, runID, quarantineID string, forUpdate bool) (*runItem, error) {
	q := `SELECT quarantine_id, run_id, source_id, payload_ref, quarantined_by FROM quarantine_items WHERE quarantine_id = $1 AND run_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var it runItem
	err := tx.QueryRow(ctx, q, quarantineID, runID).Scan(&it.QuarantineID, &it.RunID, &it.SourceID, &it.PayloadRef, &it.QuarantinedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrQuarantineItemNotFound
	}
	return &it, err
}

func loadRunLedger(ctx context.Context, tx pgx.Tx, quarantineID string) ([]domain.RunReleaseEntry, error) {
	rows, err := tx.Query(ctx, `SELECT ledger_id::text, quarantine_id, kind, actor, reason, COALESCE(correction_ref, ''), answers_ledger_id::text,
			COALESCE(landing_id, ''), occurred_at
		FROM quarantine_item_release_ledger WHERE quarantine_id = $1 ORDER BY occurred_at, ledger_id`, quarantineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RunReleaseEntry{}
	for rows.Next() {
		var e domain.RunReleaseEntry
		var kind string
		if err := rows.Scan(&e.LedgerID, &e.QuarantineID, &kind, &e.Actor, &e.Reason, &e.CorrectionRef, &e.AnswersLedgerID, &e.LandingID, &e.OccurredAt); err != nil {
			return nil, err
		}
		e.Kind = domain.RunReleaseKind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}

func lastOf(entries []domain.RunReleaseEntry, kind domain.RunReleaseKind) *domain.RunReleaseEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == kind {
			return &entries[i]
		}
	}
	return nil
}

func insertRunLedger(ctx context.Context, tx pgx.Tx, tenantID, quarantineID string, kind domain.RunReleaseKind, actor, reason, correction string, answers *string, landingID string) error {
	var corr, land *string
	if correction != "" {
		corr = &correction
	}
	if landingID != "" {
		land = &landingID
	}
	_, err := tx.Exec(ctx, `INSERT INTO quarantine_item_release_ledger (tenant_id, quarantine_id, kind, actor, reason, correction_ref, answers_ledger_id, landing_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8)`, tenantID, quarantineID, string(kind), actor, strings.TrimSpace(reason), corr, answers, land)
	if err != nil {
		return fmt.Errorf("record quarantine release %s: %w", kind, err)
	}
	return nil
}

func runReleaseEvent(ctx context.Context, tx pgx.Tx, tenantID, eventType string, it *runItem, actor, reason string, extra map[string]any) error {
	payload := map[string]any{"quarantine_id": it.QuarantineID, "run_id": it.RunID, "source_id": it.SourceID, "by": actor, "reason": reason}
	for k, v := range extra {
		payload[k] = v
	}
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "quarantine_item", AggregateID: it.QuarantineID, EventType: eventType, TenantID: &tenantID, Payload: payload})
}

// RequestRunRelease asks for a quarantined batch to be released, naming the correction made in the owning domain.
func (s *PgStore) RequestRunRelease(ctx context.Context, tenantID, runID, quarantineID, actor string, req domain.RunReleaseRequest, claim domain.IdempotencyClaim) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		it, err := loadRunItem(ctx, tx, runID, quarantineID, true)
		if err != nil {
			return err
		}
		ledger, err := loadRunLedger(ctx, tx, quarantineID)
		if err != nil {
			return err
		}
		switch domain.DeriveRunReleaseState(ledger) {
		case domain.RunReleased:
			return domain.ErrRunReleaseAlreadyReleased
		case domain.RunReleasePending, domain.RunReleaseApprovedS:
			return domain.ErrRunReleaseOpen
		}
		claim.ResourceID = quarantineID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := insertRunLedger(ctx, tx, tenantID, quarantineID, domain.RunReleaseRequested, actor, req.Reason, req.CorrectionRef, nil, ""); err != nil {
			return err
		}
		return runReleaseEvent(ctx, tx, tenantID, "DATA.QuarantineReleaseRequested", it, actor, req.Reason, map[string]any{"correction_ref": req.CorrectionRef})
	})
}

// DecideRunRelease approves or refuses the open request. The decider is never the requester, and an approval is never
// given by whoever quarantined the batch (NP-16).
func (s *PgStore) DecideRunRelease(ctx context.Context, tenantID, runID, quarantineID, actor string, release bool, dec domain.RunReleaseDecision, claim domain.IdempotencyClaim) error {
	if err := dec.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		it, err := loadRunItem(ctx, tx, runID, quarantineID, true)
		if err != nil {
			return err
		}
		ledger, err := loadRunLedger(ctx, tx, quarantineID)
		if err != nil {
			return err
		}
		if domain.DeriveRunReleaseState(ledger) != domain.RunReleasePending {
			return domain.ErrRunNoOpenRelease
		}
		req := lastOf(ledger, domain.RunReleaseRequested)
		// Checked before the idempotency key is claimed, so a refused decision does not consume it.
		if actor == req.Actor || (release && actor == it.QuarantinedBy) {
			return domain.ErrRunReleaseNotIndependent
		}
		claim.ResourceID = quarantineID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		kind, event := domain.RunReleaseRefused, "DATA.QuarantineReleaseRefused"
		if release {
			kind, event = domain.RunReleaseApproved, "DATA.QuarantineReleaseApproved"
		}
		if err := insertRunLedger(ctx, tx, tenantID, quarantineID, kind, actor, dec.Reason, "", &req.LedgerID, ""); err != nil {
			return err
		}
		return runReleaseEvent(ctx, tx, tenantID, event, it, actor, dec.Reason, nil)
	})
}

// RunQuarantineLedger returns an item's release ledger and the state it folds to.
func (s *PgStore) RunQuarantineLedger(ctx context.Context, tenantID, runID, quarantineID string) ([]domain.RunReleaseEntry, domain.RunReleaseState, error) {
	var entries []domain.RunReleaseEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadRunItem(ctx, tx, runID, quarantineID, false); err != nil {
			return err
		}
		var err error
		entries, err = loadRunLedger(ctx, tx, quarantineID)
		return err
	})
	return entries, domain.DeriveRunReleaseState(entries), err
}

// releaseForCommit applies the release rules to a batch about to LAND. It returns the approved ledger entry the batch
// consumes, if it was submitted under one.
//
//   - Submitted under releases_quarantine_id: that item must belong to this tenant and source, have an approved unused
//     release, and the batch must DIFFER from the one quarantined (a correction, not the same data again).
//   - Submitted under none: its content must not be a payload that is still quarantined and unreleased. Re-sending the
//     identical bad batch would walk it around the quarantine.
func releaseForCommit(ctx context.Context, tx pgx.Tx, tenantID string, run *domain.IngestionRun, req domain.CommitBatchRequest) (*domain.RunReleaseEntry, string, error) {
	hash := domain.ComputeContentHash(req.Records)
	if req.ReleasesQuarantineID == "" {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM quarantine_items q
			WHERE q.tenant_id = $1 AND q.source_id = $2 AND q.payload_ref = $3
			  AND NOT EXISTS (SELECT 1 FROM quarantine_item_release_ledger l WHERE l.quarantine_id = q.quarantine_id AND l.kind = 'CONSUMED')`,
			tenantID, run.SourceID, hash).Scan(&n); err != nil {
			return nil, "", err
		}
		if n > 0 {
			return nil, "", domain.ErrRunReleaseRequired
		}
		return nil, "", nil
	}
	var item runItem
	err := tx.QueryRow(ctx, `SELECT quarantine_id, run_id, source_id, payload_ref, quarantined_by FROM quarantine_items
		WHERE quarantine_id = $1 AND tenant_id = $2 FOR UPDATE`, req.ReleasesQuarantineID, tenantID).
		Scan(&item.QuarantineID, &item.RunID, &item.SourceID, &item.PayloadRef, &item.QuarantinedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", domain.ErrQuarantineItemNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if item.SourceID != run.SourceID {
		return nil, "", fmt.Errorf("%w: the quarantined batch belongs to another source", domain.ErrRunReleaseNotApproved)
	}
	ledger, err := loadRunLedger(ctx, tx, item.QuarantineID)
	if err != nil {
		return nil, "", err
	}
	if domain.DeriveRunReleaseState(ledger) != domain.RunReleaseApprovedS {
		return nil, "", domain.ErrRunReleaseNotApproved
	}
	if hash == item.PayloadRef {
		return nil, "", domain.ErrRunReleaseNotACorrection
	}
	return lastOf(ledger, domain.RunReleaseApproved), item.QuarantineID, nil
}

// consumeRelease records that the approved release was used, by the landing the corrected batch became.
func consumeRelease(ctx context.Context, tx pgx.Tx, tenantID, quarantineID string, approved *domain.RunReleaseEntry, landingID, actor string) error {
	if err := insertRunLedger(ctx, tx, tenantID, quarantineID, domain.RunReleaseConsumed, actor,
		"the corrected batch landed under the approved release", "", &approved.LedgerID, landingID); err != nil {
		return err
	}
	return runReleaseEvent(ctx, tx, tenantID, "DATA.QuarantineReleased", &runItem{QuarantineID: quarantineID}, actor,
		"the corrected batch landed under the approved release", map[string]any{"landing_id": landingID})
}
