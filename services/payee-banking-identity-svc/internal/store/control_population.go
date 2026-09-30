package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

// destinationChangesScope is the destination-changes control population.
//
// A payee destination's bank details are immutable (trigger
// reject_payee_destination_mutation), so a "change of bank details" is always a
// NEW payee_destinations row, recorded by exactly one PAYEE_DESTINATION_PROPOSED
// event. The change date is that event's created_at (UTC date).
//
// The destination is in scope when it reached an approved state:
//   - an APPROVED or ACTIVATED event exists, or
//   - approved_at is set, or
//   - status is APPROVAL_PENDING, ACTIVE or SUSPENDED (SUSPENDED is only
//     reachable from ACTIVE).
//
// Deliberately not gated on a non-blank approver: an ACTIVE destination with no
// approver is precisely the NO_APPROVER exception the control looks for.
//
// STRICT PRIVACY: only opaque ids and principal ids are selected. No account
// number, IBAN, sort code, institution, payee name, country or last4.
//
// $1 tenant_id, $2 legal_entity_id, $3 changed_from, $4 changed_to (dates).
const destinationChangesScope = `
	SELECT d.destination_id::text                           AS record_id,
	       d.destination_id::text                           AS reference,
	       '0'::text                                        AS amount,
	       'XXX'::text                                      AS currency,
	       to_char((pe.created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS txn_date,
	       d.party_ref                                      AS party_ref,
	       d.status                                         AS status,
	       d.proposed_by_principal_id                       AS proposed_by,
	       COALESCE(d.verified_by_principal_id, '')         AS verified_by,
	       COALESCE(d.approved_by_principal_id, '')         AS approved_by,
	       CASE
	           WHEN NULLIF(BTRIM(d.approved_by_principal_id), '') IS NULL THEN 'NO_APPROVER'
	           WHEN BTRIM(d.approved_by_principal_id) = BTRIM(d.proposed_by_principal_id) THEN 'SELF_APPROVED'
	           WHEN NULLIF(BTRIM(d.verified_by_principal_id), '') IS NOT NULL
	                AND BTRIM(d.verified_by_principal_id) = BTRIM(d.proposed_by_principal_id) THEN 'SELF_VERIFIED'
	           WHEN NULLIF(BTRIM(d.verified_by_principal_id), '') IS NULL THEN 'NO_VERIFIER'
	           ELSE '' END                                  AS sod_gap
	FROM payee_destinations d
	JOIN (SELECT ev.destination_id, MIN(ev.created_at) AS created_at
	        FROM payee_destination_events ev
	       WHERE ev.tenant_id = $1::uuid
	         AND ev.event_type = 'PAYEE_DESTINATION_PROPOSED'
	       GROUP BY ev.destination_id) pe
	  ON pe.destination_id = d.destination_id
	WHERE d.tenant_id = $1::uuid
	  AND d.legal_entity_id = $2::text
	  AND (pe.created_at AT TIME ZONE 'UTC')::date BETWEEN $3::date AND $4::date
	  AND (d.approved_at IS NOT NULL
	       OR d.status IN ('APPROVAL_PENDING', 'ACTIVE', 'SUSPENDED')
	       OR EXISTS (SELECT 1 FROM payee_destination_events ax
	                   WHERE ax.tenant_id = $1::uuid
	                     AND ax.destination_id = d.destination_id
	                     AND ax.event_type IN ('PAYEE_DESTINATION_APPROVED', 'PAYEE_DESTINATION_ACTIVATED')))`

// destinationChangesDigestExpr covers every wire field and attribute.
const destinationChangesDigestExpr = `record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' ||
	party_ref || '|' || status || '|' || proposed_by || '|' || verified_by || '|' || approved_by || '|' || sod_gap`

// QueryDestinationChanges returns one keyset page (ordered by destination_id)
// of the destination-changes population with the whole-set watermark and
// declared totals, all read in ONE REPEATABLE READ read-only transaction.
func (s *PgStore) QueryDestinationChanges(ctx context.Context, tenantID string, q domain.DestinationChangesQuery) (*domain.ControlPopulationPage, error) {
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	args := []any{tenantID, q.LegalEntityID, q.ChangedFrom, q.ChangedTo}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{"XXX": "0"}},
	}

	var digest string
	err = tx.QueryRow(ctx, `
		WITH scope AS (`+destinationChangesScope+`)
		SELECT COUNT(*),
		       md5(COALESCE(string_agg(`+destinationChangesDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, args...).Scan(&page.DeclaredTotals.RowCount, &digest)
	if err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("pb1:n=%d;md5=%s", page.DeclaredTotals.RowCount, digest)

	rows, err := tx.Query(ctx, `
		WITH scope AS (`+destinationChangesScope+`)
		SELECT record_id, reference, amount, currency, txn_date, party_ref, status, proposed_by, verified_by, approved_by, sod_gap
		FROM scope
		WHERE ($5::uuid IS NULL OR record_id::uuid > $5::uuid)
		ORDER BY record_id::uuid ASC
		LIMIT $6`, append(args, after, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		var partyRef, status, proposedBy, verifiedBy, approvedBy, sodGap string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
			&partyRef, &status, &proposedBy, &verifiedBy, &approvedBy, &sodGap); err != nil {
			return nil, err
		}
		r.Attributes = map[string]string{
			"party_ref":   partyRef,
			"status":      status,
			"proposed_by": proposedBy,
			"verified_by": verifiedBy,
			"approved_by": approvedBy,
			"sod_gap":     sodGap,
		}
		page.Records = append(page.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextRecordID = page.Records[limit-1].RecordID
	}
	return page, nil
}
