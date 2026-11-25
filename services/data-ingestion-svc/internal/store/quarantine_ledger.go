package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

// The quarantine ledger and governed release (ZS-SVC-Z-001 INV-11/INV-12, TC-07/TC-08, NP-16;
// migration 000003). An item row says where an item is now; the ledger says how it got there and
// who decided what, and it survives the item moving on. The database enforces the rules that
// make a release governed (independence, one answer per request, correction evidence); the code
// here gives callers the reason instead of a constraint violation.

type ledgerRow struct {
	tenantID, batchID string
	rowNo             int
	kind              imports.LedgerKind
	actor, reason     string
	owner             string
	correctionRef     string
	answers           *int64
	outcome           string
	correlationID     string
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func insertLedger(ctx context.Context, tx pgx.Tx, l ledgerRow) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO import_quarantine_ledger (tenant_id, batch_id, row_no, kind, actor, reason, owner, correction_ref,
			answers_ledger_id, revalidation_outcome, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		l.tenantID, l.batchID, l.rowNo, string(l.kind), l.actor, l.reason, nilIfEmpty(l.owner), nilIfEmpty(l.correctionRef),
		l.answers, nilIfEmpty(l.outcome), l.correlationID)
	return err
}

// openRequest returns the item's release request that has not been answered, if any.
func openRequest(ctx context.Context, tx pgx.Tx, batchID string, rowNo int) (id int64, requester string, found bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT r.ledger_id, r.actor FROM import_quarantine_ledger r
		 WHERE r.batch_id = $1 AND r.row_no = $2 AND r.kind = 'RELEASE_REQUESTED'
		   AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger c WHERE c.answers_ledger_id = r.ledger_id)
		 ORDER BY r.ledger_id DESC LIMIT 1`, batchID, rowNo).Scan(&id, &requester)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	return id, requester, err == nil, err
}

// recordRevalidation notes how the owning domain's re-validation of a released item ended.
// It is a no-op for an item that was never released (or whose release was already answered).
func recordRevalidation(ctx context.Context, tx pgx.Tx, tenantID, batchID string, rowNo int, status imports.ItemStatus) error {
	switch status {
	case imports.ItemImported, imports.ItemQuarantined, imports.ItemFailed:
	default:
		return nil // not an outcome of re-validation
	}
	var releasedID int64
	err := tx.QueryRow(ctx, `
		SELECT l.ledger_id FROM import_quarantine_ledger l
		 WHERE l.batch_id = $1 AND l.row_no = $2 AND l.kind = 'RELEASED'
		   AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger c WHERE c.answers_ledger_id = l.ledger_id)
		 ORDER BY l.ledger_id DESC LIMIT 1`, batchID, rowNo).Scan(&releasedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	outcome := string(status)
	return insertLedger(ctx, tx, ledgerRow{tenantID: tenantID, batchID: batchID, rowNo: rowNo, kind: imports.LedgerRevalidated,
		actor: imports.SystemActor, reason: "re-validated by the owning domain after release: " + outcome,
		answers: &releasedID, outcome: outcome})
}

func (s *PgStore) RequestRelease(ctx context.Context, tenantID, batchID string, rowNo int, by, reason, correctionRef, correlationID string,
	events []imports.Event) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		var st string
		err := tx.QueryRow(ctx, `SELECT status FROM import_items WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3 FOR UPDATE`,
			batchID, rowNo, tenantID).Scan(&st)
		if errors.Is(err, pgx.ErrNoRows) {
			return imports.ErrItemNotFound
		}
		if err != nil {
			return err
		}
		if st != "QUARANTINED" {
			return fmt.Errorf("%w: only a quarantined item can have its release requested (%s)", imports.ErrInvalidState, st)
		}
		if _, _, open, err := openRequest(ctx, tx, batchID, rowNo); err != nil {
			return err
		} else if open {
			return imports.ErrReleaseOpen
		}
		if err := insertLedger(ctx, tx, ledgerRow{tenantID: tenantID, batchID: batchID, rowNo: rowNo, kind: imports.LedgerReleaseRequested,
			actor: by, reason: reason, correctionRef: correctionRef, correlationID: correlationID}); err != nil {
			return err
		}
		return emitEvents(ctx, tx, tenantID, batchID, events)
	})
}

func (s *PgStore) DecideRelease(ctx context.Context, tenantID, batchID string, rowNo int, by, reason, correlationID string, release bool, answersRequest int64,
	now time.Time, maxRetries int, events []imports.Event) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		var st string
		var retries int
		err := tx.QueryRow(ctx, `SELECT status, retries FROM import_items WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3 FOR UPDATE`,
			batchID, rowNo, tenantID).Scan(&st, &retries)
		if errors.Is(err, pgx.ErrNoRows) {
			return imports.ErrItemNotFound
		}
		if err != nil {
			return err
		}
		reqID, requester, found, err := openRequest(ctx, tx, batchID, rowNo)
		if err != nil {
			return err
		}
		if !found || st != "QUARANTINED" {
			return imports.ErrNoOpenRelease
		}
		// Optimistic concurrency: the decision was made on a particular request. Checked under the item's
		// row lock, so it cannot race another decision or a new request.
		if answersRequest != 0 && answersRequest != reqID {
			return imports.ErrStaleRelease
		}
		kind := imports.LedgerReleaseRefused
		if release {
			kind = imports.LedgerReleased
			// Independence (NP-16). The database enforces the same rule; checking here gives the
			// caller the reason instead of a constraint violation.
			if by == requester {
				return imports.ErrNotIndependent
			}
			var quarantiner string
			qerr := tx.QueryRow(ctx, `SELECT actor FROM import_quarantine_ledger WHERE batch_id = $1 AND row_no = $2 AND kind = 'QUARANTINED'
				AND ledger_id < $3 ORDER BY ledger_id DESC LIMIT 1`, batchID, rowNo, reqID).Scan(&quarantiner)
			if qerr != nil && !errors.Is(qerr, pgx.ErrNoRows) {
				return qerr
			}
			if qerr == nil && by == quarantiner {
				return imports.ErrNotIndependent
			}
			if retries >= maxRetries {
				return fmt.Errorf("%w: it has been released and retried %d times already", imports.ErrRetryNotAllowed, retries)
			}
		}
		if err := insertLedger(ctx, tx, ledgerRow{tenantID: tenantID, batchID: batchID, rowNo: rowNo, kind: kind, actor: by, reason: reason,
			answers: &reqID, correlationID: correlationID}); err != nil {
			return err
		}
		if release {
			// The item goes back in the queue and the owning domain re-validates it. Its quarantine
			// columns are cleared because the ledger now holds that evidence permanently.
			if _, err := tx.Exec(ctx, `UPDATE import_items SET status = 'PENDING', attempts = 0, retries = retries + 1, release_generation = release_generation + 1,
				next_attempt_at = $4, locked_until = NULL, quarantine_reason = NULL, quarantine_owner = NULL, quarantined_by = NULL, last_error_class = NULL
				WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3`, batchID, rowNo, tenantID, now); err != nil {
				return err
			}
		}
		return emitEvents(ctx, tx, tenantID, batchID, events)
	})
}

func (s *PgStore) QuarantineLedger(ctx context.Context, tenantID, batchID string, rowNo int) ([]imports.LedgerEntry, error) {
	var out []imports.LedgerEntry
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		q := `SELECT ledger_id, batch_id, row_no, kind, actor, reason, COALESCE(owner, ''), COALESCE(correction_ref, ''), answers_ledger_id,
				COALESCE(revalidation_outcome, ''), correlation_id, occurred_at
			FROM import_quarantine_ledger WHERE tenant_id = $1 AND batch_id = $2`
		args := []any{tenantID, batchID}
		if rowNo > 0 {
			q += ` AND row_no = $3`
			args = append(args, rowNo)
		}
		rows, err := tx.Query(ctx, q+` ORDER BY ledger_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e imports.LedgerEntry
			var kind string
			if err := rows.Scan(&e.LedgerID, &e.BatchID, &e.RowNo, &kind, &e.Actor, &e.Reason, &e.Owner, &e.CorrectionRef,
				&e.AnswersLedgerID, &e.RevalidationOutcome, &e.CorrelationID, &e.OccurredAt); err != nil {
				return err
			}
			e.Kind = imports.LedgerKind(kind)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) QuarantineCounts(ctx context.Context, tenantID, batchID string) (quarantined, total, openRequests int, err error) {
	err = s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'QUARANTINED'),
			       count(*) FILTER (WHERE status NOT IN ('INVALID', 'SKIPPED'))
			  FROM import_items WHERE batch_id = $1 AND tenant_id = $2`, batchID, tenantID).Scan(&quarantined, &total); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM import_quarantine_ledger r
			  JOIN import_items i ON i.batch_id = r.batch_id AND i.row_no = r.row_no AND i.status = 'QUARANTINED'
			 WHERE r.batch_id = $1 AND r.tenant_id = $2 AND r.kind = 'RELEASE_REQUESTED'
			   AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger c WHERE c.answers_ledger_id = r.ledger_id)`,
			batchID, tenantID).Scan(&openRequests)
	})
	return quarantined, total, openRequests, err
}
