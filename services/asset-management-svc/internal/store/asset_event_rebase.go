package store

// AST-03 book-state effects for IMPAIRMENT, REVALUATION and ADDITION events.
//
// A "re-base" = end-date the asset's current ACTIVE depreciation schedule
// version for the event's book and insert a NEW version whose
//
//	cost_basis          IS the new CARRYING AMOUNT, and
//	useful_life_months  IS the REMAINING months of the old version
//
// depreciation_lines are keyed by schedule_version_id, so the new version
// starts at accumulated = 0 and GetNetBookValueTotal
// (cost_basis - latest accumulated of the current version) stays correct.
// The old version and all of its depreciation_lines are never touched.
//
//	carrying  = old.cost_basis - SUM(old-version depreciation_lines.period_amount)
//	remaining = old.useful_life_months - COUNT(old-version depreciation_lines)
//
// Amount semantics (also documented in migration 000004):
//
//	IMPAIRMENT   amount = impairment loss;   new_carrying = carrying - amount
//	ADDITION     amount = capitalised cost;  new_carrying = carrying + amount
//	REVALUATION  amount = the REVALUED CARRYING AMOUNT itself (not a delta);
//	             new_carrying = amount
//
// Event types deliberately NOT handled here (record-only, unchanged):
//
//	COMPONENT_REPLACEMENT  components are not modeled per depreciation
//	                       schedule (a schedule is asset+book level), so there
//	                       is no schedule figure a replacement could change.
//	CAPITALIZATION         is AST-01's RequestCapitalization command; applying
//	                       the event here must not second-guess that.
//	TRANSFER               intra-entity only (cross-entity is blocked by a
//	                       CHECK constraint); an intra-entity custody/location
//	                       move has no book effect.
//
// Every re-base runs in the same transaction as the event's status
// transition, and is refused (never silently skipped) when it cannot be
// performed.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/asset-management-svc/internal/domain"
)

const (
	effectKindApply   = "APPLY"
	effectKindReverse = "REVERSE"
)

func isBookEventType(eventType string) bool {
	switch eventType {
	case domain.AssetEventTypeImpairment, domain.AssetEventTypeRevaluation, domain.AssetEventTypeAddition:
		return true
	}
	return false
}

type currentSchedule struct {
	versionID, scheduleID, legalEntityID, assetID, bookID, method string
	version, usefulLifeMonths                                      int
	costBasis, residualValue                                       float64
	inServiceDate                                                  time.Time
}

// lockCurrentSchedule returns the asset's current ACTIVE schedule for the
// book, locked FOR UPDATE so a concurrent re-base serialises behind us.
func lockCurrentSchedule(ctx context.Context, tx pgx.Tx, tenantID, assetID, bookID string) (*currentSchedule, error) {
	var c currentSchedule
	err := tx.QueryRow(ctx, `
		SELECT schedule_version_id, schedule_id, legal_entity_id, asset_id, book_id, method,
		       version, useful_life_months, cost_basis, residual_value, in_service_date
		FROM depreciation_schedules
		WHERE tenant_id = $1 AND asset_id = $2 AND book_id = $3
		  AND effective_to IS NULL AND status = $4
		FOR UPDATE
	`, tenantID, assetID, bookID, domain.DepreciationScheduleStatusActive).Scan(
		&c.versionID, &c.scheduleID, &c.legalEntityID, &c.assetID, &c.bookID, &c.method,
		&c.version, &c.usefulLifeMonths, &c.costBasis, &c.residualValue, &c.inServiceDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoActiveScheduleForBook
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// carryingAndRemaining derives the live carrying amount and remaining
// months of a schedule version from its own depreciation_lines.
func carryingAndRemaining(ctx context.Context, tx pgx.Tx, tenantID string, c *currentSchedule) (carrying float64, remaining int, depreciated float64, err error) {
	var count int
	if err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(period_amount), 0), COUNT(*) FROM depreciation_lines
		WHERE tenant_id = $1 AND schedule_version_id = $2
	`, tenantID, c.versionID).Scan(&depreciated, &count); err != nil {
		return 0, 0, 0, err
	}
	return roundCents(c.costBasis - depreciated), c.usefulLifeMonths - count, depreciated, nil
}

// assertNoRunInFlight refuses when a not-yet-emitted depreciation run holds
// this exact schedule version in its frozen population: policy/life/residual
// changes after approval invalidate the run, so we block rather than let the
// run and the schedule silently diverge.
func assertNoRunInFlight(ctx context.Context, tx pgx.Tx, tenantID, versionID string) error {
	var inFlight bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM depreciation_run_population rp
			JOIN depreciation_runs dr ON dr.run_id = rp.run_id
			WHERE dr.tenant_id = $1 AND rp.schedule_version_id = $2
			  AND dr.status IN ($3, $4, $5)
		)
	`, tenantID, versionID,
		domain.DepreciationRunStatusPopulationFrozen, domain.DepreciationRunStatusValidated, domain.DepreciationRunStatusApproved,
	).Scan(&inFlight); err != nil {
		return err
	}
	if inFlight {
		return domain.ErrDepreciationRunInFlight
	}
	return nil
}

// rebase end-dates cur and inserts the new version, then records the
// effects link row. Returns nothing; all failures abort the transaction.
func rebase(ctx context.Context, tx pgx.Tx, tenantID, eventID, kind string, cur *currentSchedule,
	oldCarrying, newCarrying float64, remaining int, principalID string, at time.Time) error {

	newResidual := cur.residualValue
	if newCarrying < newResidual {
		newResidual = newCarrying
	}
	if _, err := tx.Exec(ctx, `
		UPDATE depreciation_schedules SET status = $1, effective_to = $2
		WHERE schedule_version_id = $3 AND tenant_id = $4 AND effective_to IS NULL
	`, domain.DepreciationScheduleStatusSuperseded, at, cur.versionID, tenantID); err != nil {
		return err
	}
	newVersionID := newUUID()
	if _, err := tx.Exec(ctx, `
		INSERT INTO depreciation_schedules (
			schedule_version_id, schedule_id, version, tenant_id, legal_entity_id, asset_id, book_id,
			method, cost_basis, residual_value, useful_life_months, in_service_date, status,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, newVersionID, cur.scheduleID, cur.version+1, tenantID, cur.legalEntityID, cur.assetID, cur.bookID,
		cur.method, newCarrying, newResidual, remaining, cur.inServiceDate, domain.DepreciationScheduleStatusActive,
		at, principalID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO asset_event_schedule_effects (
			effect_id, event_id, effect_kind, tenant_id, old_schedule_version_id, new_schedule_version_id,
			old_carrying, new_carrying, remaining_months, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, newUUID(), eventID, kind, tenantID, cur.versionID, newVersionID, oldCarrying, newCarrying, remaining, at)
	return err
}

// applyBookEffect is called by ApplyAssetEvent inside its transaction.
func applyBookEffect(ctx context.Context, tx pgx.Tx, tenantID, eventID, assetID, eventType string, at time.Time) error {
	if !isBookEventType(eventType) {
		return nil // CAPITALIZATION / COMPONENT_REPLACEMENT / TRANSFER / DISPOSAL: see file comment
	}
	var bookID, approver *string
	var createdBy string
	var amount *float64
	if err := tx.QueryRow(ctx, `
		SELECT book_id, amount, approved_by_principal_id, created_by_principal_id
		FROM asset_events WHERE event_id = $1 AND tenant_id = $2
	`, eventID, tenantID).Scan(&bookID, &amount, &approver, &createdBy); err != nil {
		return err
	}
	if bookID == nil || *bookID == "" {
		return domain.ErrBookRequiredForBookEvent
	}
	if amount == nil || *amount <= 0 {
		return domain.ErrRebaseAmountInvalid
	}
	principal := createdBy
	if approver != nil && *approver != "" {
		principal = *approver
	}

	cur, err := lockCurrentSchedule(ctx, tx, tenantID, assetID, *bookID)
	if err != nil {
		return err
	}
	if err := assertNoRunInFlight(ctx, tx, tenantID, cur.versionID); err != nil {
		return err
	}
	carrying, remaining, _, err := carryingAndRemaining(ctx, tx, tenantID, cur)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		return domain.ErrScheduleFullyDepreciated
	}

	var newCarrying float64
	switch eventType {
	case domain.AssetEventTypeImpairment:
		if *amount >= carrying {
			return domain.ErrImpairmentExceedsCarrying
		}
		newCarrying = roundCents(carrying - *amount)
	case domain.AssetEventTypeAddition:
		newCarrying = roundCents(carrying + *amount)
	case domain.AssetEventTypeRevaluation:
		newCarrying = roundCents(*amount) // amount IS the revalued carrying amount
	}
	return rebase(ctx, tx, tenantID, eventID, effectKindApply, cur, carrying, newCarrying, remaining, principal, at)
}

// reverseBookEffect is called by correctAssetEvent (Reverse/Supersede)
// inside its transaction. It undoes the re-base the APPLY effects row
// recorded:
//
//	IMPAIRMENT   new_carrying = current carrying + amount
//	ADDITION     new_carrying = current carrying - amount (refused if <= 0)
//	REVALUATION  new_carrying = effects.old_carrying - SUM(depreciation
//	             charged on the post-event version); refused unless that
//	             post-event version is still the current one (LIFO).
//
// Remaining life is the current remaining life. Events applied before
// migration 000004 have no APPLY effects row; their reversal remains
// record-only exactly as before.
func reverseBookEffect(ctx context.Context, tx pgx.Tx, tenantID, eventID, assetID, eventType, principalID string, at time.Time) error {
	if !isBookEventType(eventType) {
		return nil
	}
	var applyOldCarrying float64
	var applyNewVersionID string
	err := tx.QueryRow(ctx, `
		SELECT old_carrying, new_schedule_version_id FROM asset_event_schedule_effects
		WHERE tenant_id = $1 AND event_id = $2 AND effect_kind = $3
	`, tenantID, eventID, effectKindApply).Scan(&applyOldCarrying, &applyNewVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var bookID *string
	var amount *float64
	if err := tx.QueryRow(ctx, `SELECT book_id, amount FROM asset_events WHERE event_id = $1 AND tenant_id = $2`,
		eventID, tenantID).Scan(&bookID, &amount); err != nil {
		return err
	}
	if bookID == nil || amount == nil {
		return domain.ErrRebaseAmountInvalid
	}

	cur, err := lockCurrentSchedule(ctx, tx, tenantID, assetID, *bookID)
	if err != nil {
		return err
	}
	if err := assertNoRunInFlight(ctx, tx, tenantID, cur.versionID); err != nil {
		return err
	}
	carrying, remaining, depreciated, err := carryingAndRemaining(ctx, tx, tenantID, cur)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		return domain.ErrScheduleFullyDepreciated
	}

	var newCarrying float64
	switch eventType {
	case domain.AssetEventTypeImpairment:
		newCarrying = roundCents(carrying + *amount)
	case domain.AssetEventTypeAddition:
		newCarrying = roundCents(carrying - *amount)
	case domain.AssetEventTypeRevaluation:
		if cur.versionID != applyNewVersionID {
			return domain.ErrReversalOutOfOrder
		}
		newCarrying = roundCents(applyOldCarrying - depreciated)
	}
	if newCarrying <= 0 {
		return domain.ErrReversalWouldZeroCarrying
	}
	return rebase(ctx, tx, tenantID, eventID, effectKindReverse, cur, carrying, newCarrying, remaining, principalID, at)
}
