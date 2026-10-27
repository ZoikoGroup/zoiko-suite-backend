package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/tax-authority-interface-svc/internal/authority"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

var _ authority.CallbackStore = (*PgStore)(nil)

func rank(s string) int {
	switch s {
	case authority.StatusPendingUnknown, authority.StatusPrepared, authority.StatusValidated:
		return 0
	case authority.StatusSubmitted:
		return 1
	case authority.StatusAccepted:
		return 2
	case authority.StatusDelivered, authority.StatusRejected, authority.StatusSuperseded, authority.StatusBlocked:
		return 3
	}
	return 0
}

// QuarantineOwner is the queue that owns an authority callback no durable filing could be found for (INT-CTRL-034). The
// quarantine event names it, so an alert routes somewhere instead of nowhere.
const QuarantineOwner = "tax-operations"

// ApplyCallback applies one authenticated authority callback inside the caller's tenant scope: the filing is found
// (by our id or by the authority's reference), its row locked so concurrent callbacks serialise, duplicates are
// recognised, stale and regressing callbacks are refused, and exactly one receipt records what happened. The state
// change, its outbox event and the receipt commit together.
func (p *PgStore) ApplyCallback(ctx context.Context, in authority.CallbackInput) (*authority.CallbackResult, error) {
	res := &authority.CallbackResult{}
	tenant := middleware.GetTenantID(ctx)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		receipt := func(outcome, detail string, id *string) error {
			res.Outcome, res.Detail = outcome, detail
			if _, err := tx.Exec(ctx, "SAVEPOINT receipt"); err != nil {
				return err
			}
			// an event that correlates to no durable filing is quarantined for a named owner, once per event
			quarantined := outcome == authority.OutcomeUnknownFiling || outcome == authority.OutcomeAmbiguousFiling
			var priorQuarantine int
			if quarantined {
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM tax_authority_callbacks WHERE tenant_id = $1 AND provider_key = $2 AND event_ref = $3
					AND outcome IN ('UNKNOWN_SUBMISSION', 'AMBIGUOUS_SUBMISSION')`, tenant, in.Provider, in.EventRef).Scan(&priorQuarantine); err != nil {
					return err
				}
			}
			ins := func(o, d string) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO tax_authority_callbacks (tenant_id, provider_key, event_ref, submission_ref, submission_id, scheme, reported_status,
						mapped_status, event_time, body_hash, outcome, detail)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
					tenant, in.Provider, in.EventRef, in.SubmissionRef, id, in.Scheme, in.RawStatus, in.Mapped, in.EventTime, in.BodyHash, o, d)
				return err
			}
			err := ins(outcome, detail)
			if err != nil && pgCode(err) == "23505" {
				if _, rb := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT receipt"); rb != nil {
					return rb
				}
				res.Outcome, res.Detail = authority.OutcomeDuplicate, "decided by a concurrent copy"
				err = ins(authority.OutcomeDuplicate, res.Detail)
			}
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT receipt"); err != nil {
				return err
			}
			if quarantined && priorQuarantine == 0 && res.Outcome == outcome {
				return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-callback.quarantined", in.Provider+":"+in.EventRef, tenant, "",
					"authority-callback", "", map[string]any{"provider": in.Provider, "event_ref": in.EventRef, "outcome": outcome,
						"owner": QuarantineOwner, "body_hash": in.BodyHash})
			}
			return nil
		}

		rows, err := tx.Query(ctx, `SELECT submission_id FROM tax_filing_submissions
			WHERE (submission_id = $1 OR authority_ref = $1) ORDER BY submitted_at LIMIT 2 FOR UPDATE`, in.SubmissionRef)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		switch len(ids) {
		case 0:
			return receipt(authority.OutcomeUnknownFiling, "no filing in this tenant has that reference", nil)
		case 1:
		default:
			return receipt(authority.OutcomeAmbiguousFiling, "more than one filing has that reference", nil)
		}
		id := ids[0]
		cur, err := p.loadIn(ctx, tx, id)
		if err != nil {
			return err
		}
		res.Filing = cur

		var already int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tax_authority_callbacks WHERE tenant_id = $1 AND provider_key = $2 AND event_ref = $3
			AND outcome NOT IN ('DUPLICATE', 'UNKNOWN_SUBMISSION', 'AMBIGUOUS_SUBMISSION')`, tenant, in.Provider, in.EventRef).Scan(&already); err != nil {
			return err
		}
		if already > 0 {
			return receipt(authority.OutcomeDuplicate, "this authority event was already decided", &id)
		}

		// what the callback asks for, in the filing's own vocabulary
		var to string
		switch in.Mapped {
		case "ACCEPTED":
			to = authority.StatusAccepted
		case "DELIVERED":
			to = authority.StatusDelivered
		case "REJECTED":
			to = authority.StatusRejected
		default: // PENDING, ACKNOWLEDGED: the authority has it
			to = authority.StatusSubmitted
		}
		var lastAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT GREATEST(sent_at, validated_at) FROM tax_filing_submissions WHERE submission_id = $1`, id).Scan(&lastAt); err != nil {
			return err
		}
		switch {
		case to == authority.StatusDelivered && cur.AdapterKind != authority.KindEInvoice:
			return receipt(authority.OutcomeStaleIgnored, "delivery is not a state of this filing", &id)
		case cur.Status == to:
			return receipt(authority.OutcomeStaleIgnored, "the filing is already in that state", &id)
		case rank(cur.Status) >= 3 || (cur.Status == authority.StatusAccepted && cur.AdapterKind != authority.KindEInvoice):
			return receipt(authority.OutcomeRegressionBlocked, "the filing is final; a callback cannot change it", &id)
		case cur.Status != authority.StatusSubmitted && cur.Status != authority.StatusPendingUnknown && cur.Status != authority.StatusAccepted:
			return receipt(authority.OutcomeStaleIgnored, "the filing was not sent, so the authority cannot be answering for it", &id)
		case rank(to) < rank(cur.Status):
			return receipt(authority.OutcomeStaleIgnored, "the callback reports an earlier stage than the filing has reached", &id)
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT apply_callback"); err != nil {
			return err
		}
		var ack string
		if to == authority.StatusAccepted || to == authority.StatusDelivered {
			ack = in.SubmissionRef
		}
		_, uerr := tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = $2, authority_status = $3, in_flight_since = NULL, last_failure = '',
			next_poll_at = CASE WHEN $5::boolean THEN next_poll_at ELSE NULL END,
			ack_reference = CASE WHEN $4::text <> '' THEN $4::text ELSE ack_reference END WHERE submission_id = $1`, id, to, in.RawStatus, ack, to == authority.StatusSubmitted)
		if uerr != nil {
			// the trigger is the backstop for what the checks above should already have caught
			if pgCode(uerr) == "P0001" {
				if _, rb := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_callback"); rb != nil {
					return rb
				}
				return receipt(authority.OutcomeRegressionBlocked, "refused by the filing's state guard", &id)
			}
			return uerr
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT apply_callback"); err != nil {
			return err
		}
		upd, err := p.loadIn(ctx, tx, id)
		if err != nil {
			return err
		}
		res.Filing = upd
		kind := "submitted"
		switch {
		case to == authority.StatusDelivered:
			kind = "delivered"
		case to != authority.StatusSubmitted && upd.AdapterKind == authority.KindEInvoice:
			kind = strings.ToLower(to)
		case to != authority.StatusSubmitted:
			kind = "response-received"
		}
		if err := p.enqueue(ctx, tx, eventType(upd, kind), id, tenant, upd.LegalEntityID, "authority-callback", in.CorrelationID,
			map[string]any{"submission_id": id, "status": upd.Status, "authority_status": in.RawStatus, "via": in.Provider}); err != nil {
			return err
		}
		return receipt(authority.OutcomeApplied, "", &id)
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return res, nil
}

func (p *PgStore) ListCallbacks(ctx context.Context, submissionID string, limit int) ([]authority.CallbackReceipt, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []authority.CallbackReceipt
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT receipt_id::text, provider_key, event_ref, submission_ref, submission_id, scheme, reported_status, mapped_status, event_time,
				body_hash, outcome, detail, received_at
			FROM tax_authority_callbacks WHERE ($1 = '' OR submission_id = $1) ORDER BY received_at DESC, receipt_id LIMIT $2`, submissionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r authority.CallbackReceipt
			if err := rows.Scan(&r.ReceiptID, &r.Provider, &r.EventRef, &r.SubmissionRef, &r.SubmissionID, &r.Scheme, &r.ReportedStatus,
				&r.MappedStatus, &r.EventTime, &r.BodyHash, &r.Outcome, &r.Detail, &r.ReceivedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %v", authority.ErrStoreUnavailable, err)
	}
	return out, nil
}
