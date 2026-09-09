package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

const movementColumns = `
	movement_id, tenant_id, legal_entity_id, movement_type, status, item_id,
	source_location_id, destination_location_id, quantity, uom, lot_number, serial_number,
	source_reference, source_idempotency_key, business_date, fiscal_period,
	reverses_movement_id, supersedes_movement_id, reason,
	created_at, created_by_principal_id, validated_at, committed_at, committed_by_principal_id`

func scanMovement(row pgx.Row) (*domain.InventoryMovement, error) {
	var m domain.InventoryMovement
	if err := row.Scan(
		&m.MovementID, &m.TenantID, &m.LegalEntityID, &m.MovementType, &m.Status, &m.ItemID,
		&m.SourceLocationID, &m.DestinationLocationID, &m.Quantity, &m.UOM, &m.LotNumber, &m.SerialNumber,
		&m.SourceReference, &m.SourceIdempotencyKey, &m.BusinessDate, &m.FiscalPeriod,
		&m.ReversesMovementID, &m.SupersedesMovementID, &m.Reason,
		&m.CreatedAt, &m.CreatedByPrincipalID, &m.ValidatedAt, &m.CommittedAt, &m.CommittedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

// CreateMovement is a real idempotent create — INSERT ... ON CONFLICT
// (tenant_id, source_idempotency_key) DO NOTHING. If the key already
// existed, *m is overwritten with the EXISTING row's data before
// returning — the caller (handler) simply returns m either way, which is
// the spec's own failure semantics verbatim: "Duplicate idempotency key
// returns original result," not an error.
func (s *PgStore) CreateMovement(ctx context.Context, m *domain.InventoryMovement) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO inventory_movements (
				movement_id, tenant_id, legal_entity_id, movement_type, status, item_id,
				source_location_id, destination_location_id, quantity, uom, lot_number, serial_number,
				source_reference, source_idempotency_key, business_date, fiscal_period, reason,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (tenant_id, source_idempotency_key) DO NOTHING
		`, m.MovementID, tenantID, m.LegalEntityID, m.MovementType, m.Status, m.ItemID,
			m.SourceLocationID, m.DestinationLocationID, m.Quantity, m.UOM, nullIfEmptyPtr(m.LotNumber), nullIfEmptyPtr(m.SerialNumber),
			m.SourceReference, m.SourceIdempotencyKey, m.BusinessDate, m.FiscalPeriod, nullIfEmptyPtr(m.Reason),
			m.CreatedAt, m.CreatedByPrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		row := tx.QueryRow(ctx, `SELECT `+movementColumns+` FROM inventory_movements WHERE tenant_id = $1 AND source_idempotency_key = $2`, tenantID, m.SourceIdempotencyKey)
		existing, err := scanMovement(row)
		if err != nil {
			return err
		}
		*m = *existing
		return nil
	})
}

func nullIfEmptyPtr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

func (s *PgStore) GetMovement(ctx context.Context, movementID string) (*domain.InventoryMovement, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var m *domain.InventoryMovement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+movementColumns+` FROM inventory_movements WHERE movement_id = $1 AND tenant_id = $2`, movementID, tenantID)
		var err error
		m, err = scanMovement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMovementNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) ListMovements(ctx context.Context, itemID string) ([]domain.InventoryMovement, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.InventoryMovement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+movementColumns+` FROM inventory_movements WHERE tenant_id = $1 AND item_id = $2 ORDER BY created_at DESC`, tenantID, itemID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMovement(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateMovement moves DRAFT -> VALIDATED. Checks whether this movement
// is well-formed against current master data: the item is ACTIVE, every
// named location is ACTIVE (INV-02's own deferred negative path,
// "Movement enters retired location"), and the item's own current
// TrackingPolicy's lot/serial requirements are satisfied (INV-01's own
// deferred negative path, "Lot-tracked item moved without lot
// identity"). Commit-time-specific checks (idempotency, serial
// duplication, negative stock, period lock) live in CommitMovement
// instead — see migration 000003's doc comment for why that split.
func (s *PgStore) ValidateMovement(ctx context.Context, movementID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var itemID string
		var sourceLoc, destLoc, lotNumber, serialNumber *string
		err := tx.QueryRow(ctx, `
			SELECT item_id, source_location_id, destination_location_id, lot_number, serial_number
			FROM inventory_movements WHERE movement_id = $1 AND status = $2 AND tenant_id = $3
		`, movementID, domain.MovementStatusDraft, tenantID).Scan(&itemID, &sourceLoc, &destLoc, &lotNumber, &serialNumber)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidMovementTransition
		}
		if err != nil {
			return err
		}

		var itemStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM inventory_items WHERE item_id = $1 AND tenant_id = $2`, itemID, tenantID).Scan(&itemStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrItemNotFound
			}
			return err
		}
		if itemStatus != domain.ItemStatusActive {
			return domain.ErrItemNotEligibleForMovement
		}

		for _, locID := range []*string{sourceLoc, destLoc} {
			if locID == nil {
				continue
			}
			var locStatus string
			if err := tx.QueryRow(ctx, `SELECT status FROM inventory_locations WHERE location_id = $1 AND tenant_id = $2`, *locID, tenantID).Scan(&locStatus); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.ErrLocationNotFound
				}
				return err
			}
			if locStatus != domain.LocationStatusActive {
				return domain.ErrLocationNotEligible
			}
		}

		var requiresLot, requiresSerial bool
		err = tx.QueryRow(ctx, `
			SELECT requires_lot_tracking, requires_serial_tracking FROM inventory_tracking_policies
			WHERE tenant_id = $1 AND item_id = $2 AND effective_to IS NULL
		`, tenantID, itemID).Scan(&requiresLot, &requiresSerial)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if requiresLot && (lotNumber == nil || *lotNumber == "") {
			return domain.ErrLotIdentityRequired
		}
		if requiresSerial && (serialNumber == nil || *serialNumber == "") {
			return domain.ErrSerialIdentityRequired
		}

		tag, err := tx.Exec(ctx, `
			UPDATE inventory_movements SET status = $1, validated_at = $2
			WHERE movement_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.MovementStatusValidated, at, movementID, domain.MovementStatusDraft, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidMovementTransition
		}
		return nil
	})
}

// CommitMovement moves VALIDATED -> COMMITTED. Every state-dependent
// negative path lives here, inside one transaction: UOM ambiguity,
// serial duplication (inventory_serial_residency), negative stock (a
// live SUM over already-committed movements), and the hard-closed-period
// check against financial-close-svc (performed by the HANDLER before
// calling this method, since it is an HTTP dependency — see
// internal/handler/movement.go). Once this UPDATE succeeds, the row's
// own reject-mutation trigger makes it permanently immutable.
func (s *PgStore) CommitMovement(ctx context.Context, movementID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		m, err := s.loadValidatedMovementForCommit(ctx, tx, tenantID, movementID)
		if err != nil {
			return err
		}

		var itemBaseUOM string
		if err := tx.QueryRow(ctx, `SELECT base_uom FROM inventory_items WHERE item_id = $1 AND tenant_id = $2`, m.ItemID, tenantID).Scan(&itemBaseUOM); err != nil {
			return err
		}
		if m.UOM != itemBaseUOM {
			return domain.ErrUOMMismatch
		}

		if err := s.checkNegativeStock(ctx, tx, tenantID, m); err != nil {
			return err
		}
		if err := s.applySerialResidency(ctx, tx, tenantID, m, at); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			UPDATE inventory_movements SET status = $1, committed_at = $2, committed_by_principal_id = $3
			WHERE movement_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.MovementStatusCommitted, at, principalID, movementID, domain.MovementStatusValidated, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidMovementTransition
		}
		return nil
	})
}

func (s *PgStore) loadValidatedMovementForCommit(ctx context.Context, tx pgx.Tx, tenantID, movementID string) (*domain.InventoryMovement, error) {
	row := tx.QueryRow(ctx, `SELECT `+movementColumns+` FROM inventory_movements WHERE movement_id = $1 AND tenant_id = $2`, movementID, tenantID)
	m, err := scanMovement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMovementNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.Status != domain.MovementStatusValidated {
		return nil, domain.ErrInvalidMovementTransition
	}
	return m, nil
}

// checkNegativeStock is the spec's own negative path, "Negative stock
// allowed despite policy prohibition" — refused universally in this v1
// (no per-item/location override policy exists yet). Only applies when
// this movement REMOVES quantity from a location (source_location_id
// set) — a pure addition can never go negative.
func (s *PgStore) checkNegativeStock(ctx context.Context, tx pgx.Tx, tenantID string, m *domain.InventoryMovement) error {
	if m.SourceLocationID == nil {
		return nil
	}
	onHand, err := s.liveOnHand(ctx, tx, tenantID, m.ItemID, *m.SourceLocationID, nil)
	if err != nil {
		return err
	}
	if onHand-m.Quantity < 0 {
		return domain.ErrNegativeStockNotAllowed
	}
	return nil
}

// applySerialResidency is the spec's own negative path, "Serial-numbered
// unit appears in two locations." A movement with no serial_number is a
// no-op here. The three shapes below cover every real movement type,
// including reversals (built by swapping source/destination — see
// buildReversal): destination-only = arrival (must not already be
// resident anywhere), source-only = departure (must currently be
// resident exactly at that location), both = transfer (moves the
// residency row atomically).
func (s *PgStore) applySerialResidency(ctx context.Context, tx pgx.Tx, tenantID string, m *domain.InventoryMovement, at time.Time) error {
	if m.SerialNumber == nil || *m.SerialNumber == "" {
		return nil
	}
	switch {
	case m.SourceLocationID == nil && m.DestinationLocationID != nil:
		tag, err := tx.Exec(ctx, `
			INSERT INTO inventory_serial_residency (tenant_id, item_id, serial_number, location_id, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, item_id, serial_number) DO NOTHING
		`, tenantID, m.ItemID, *m.SerialNumber, *m.DestinationLocationID, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrSerialAlreadyResident
		}
		return nil
	case m.SourceLocationID != nil && m.DestinationLocationID == nil:
		tag, err := tx.Exec(ctx, `
			DELETE FROM inventory_serial_residency
			WHERE tenant_id = $1 AND item_id = $2 AND serial_number = $3 AND location_id = $4
		`, tenantID, m.ItemID, *m.SerialNumber, *m.SourceLocationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrSerialNotAtSourceLocation
		}
		return nil
	case m.SourceLocationID != nil && m.DestinationLocationID != nil:
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_serial_residency SET location_id = $1, updated_at = $2
			WHERE tenant_id = $3 AND item_id = $4 AND serial_number = $5 AND location_id = $6
		`, *m.DestinationLocationID, at, tenantID, m.ItemID, *m.SerialNumber, *m.SourceLocationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrSerialNotAtSourceLocation
		}
		return nil
	}
	return nil
}

// liveOnHand computes on-hand quantity for (item, location) directly
// from committed movements — never a stored/editable balance, the real
// structural answer to the spec's own purpose statement, "on-hand
// quantity is derived from events rather than editable balance fields."
// asOf, when non-nil, restricts to movements whose business_date is on
// or before that instant — GetOnHandAsOf's own real implementation.
func (s *PgStore) liveOnHand(ctx context.Context, tx pgx.Tx, tenantID, itemID, locationID string, asOf *time.Time) (float64, error) {
	inQuery := `SELECT COALESCE(SUM(quantity), 0) FROM inventory_movements WHERE tenant_id = $1 AND item_id = $2 AND destination_location_id = $3 AND status = $4`
	outQuery := `SELECT COALESCE(SUM(quantity), 0) FROM inventory_movements WHERE tenant_id = $1 AND item_id = $2 AND source_location_id = $3 AND status = $4`
	args := []any{tenantID, itemID, locationID, domain.MovementStatusCommitted}
	if asOf != nil {
		inQuery += " AND business_date <= $5"
		outQuery += " AND business_date <= $5"
		args = append(args, *asOf)
	}
	var in, out float64
	if err := tx.QueryRow(ctx, inQuery, args...).Scan(&in); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, outQuery, args...).Scan(&out); err != nil {
		return 0, err
	}
	return in - out, nil
}

func (s *PgStore) GetOnHand(ctx context.Context, itemID, locationID string) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	var onHand float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		onHand, err = s.liveOnHand(ctx, tx, tenantID, itemID, locationID, nil)
		return err
	})
	return onHand, err
}

func (s *PgStore) GetOnHandAsOf(ctx context.Context, itemID, locationID string, at time.Time) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	var onHand float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		onHand, err = s.liveOnHand(ctx, tx, tenantID, itemID, locationID, &at)
		return err
	})
	return onHand, err
}

// CreateCorrectionMovement is the shared mechanism behind ReverseMovement
// and SupersedeMovement — see migration 000003's doc comment. It builds
// the mechanical inverse of the original movement (source/destination
// swapped, same item/quantity/UOM/lot/serial), creates it, and commits it
// in one atomic transaction — a correction takes effect immediately, it
// is not left sitting in DRAFT. The two callers differ only in which
// link column they set and which movement_type they record.
func (s *PgStore) CreateCorrectionMovement(ctx context.Context, originalMovementID, principalID, reason string, isSupersede bool, newMovementID string, at time.Time) (*domain.InventoryMovement, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var result *domain.InventoryMovement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+movementColumns+` FROM inventory_movements WHERE movement_id = $1 AND tenant_id = $2`, originalMovementID, tenantID)
		original, err := scanMovement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMovementNotFound
		}
		if err != nil {
			return err
		}
		if original.Status != domain.MovementStatusCommitted {
			return domain.ErrInvalidMovementTransition
		}

		movementType := domain.MovementTypeReversal
		if isSupersede {
			movementType = domain.MovementTypeSupersession
		}
		correction := &domain.InventoryMovement{
			MovementID: newMovementID, LegalEntityID: original.LegalEntityID, MovementType: movementType, Status: domain.MovementStatusValidated,
			ItemID: original.ItemID, SourceLocationID: original.DestinationLocationID, DestinationLocationID: original.SourceLocationID,
			Quantity: original.Quantity, UOM: original.UOM, LotNumber: original.LotNumber, SerialNumber: original.SerialNumber,
			SourceReference: original.SourceReference, SourceIdempotencyKey: newMovementID, BusinessDate: at, FiscalPeriod: original.FiscalPeriod,
			Reason: &reason, CreatedAt: at, CreatedByPrincipalID: principalID, ValidatedAt: &at,
		}
		if isSupersede {
			correction.SupersedesMovementID = &originalMovementID
		} else {
			correction.ReversesMovementID = &originalMovementID
		}

		if err := s.checkNegativeStock(ctx, tx, tenantID, correction); err != nil {
			return err
		}
		if err := s.applySerialResidency(ctx, tx, tenantID, correction, at); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_movements (
				movement_id, tenant_id, legal_entity_id, movement_type, status, item_id,
				source_location_id, destination_location_id, quantity, uom, lot_number, serial_number,
				source_reference, source_idempotency_key, business_date, fiscal_period,
				reverses_movement_id, supersedes_movement_id, reason,
				created_at, created_by_principal_id, validated_at, committed_at, committed_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
		`, correction.MovementID, tenantID, correction.LegalEntityID, correction.MovementType, domain.MovementStatusCommitted, correction.ItemID,
			correction.SourceLocationID, correction.DestinationLocationID, correction.Quantity, correction.UOM, correction.LotNumber, correction.SerialNumber,
			correction.SourceReference, correction.SourceIdempotencyKey, correction.BusinessDate, correction.FiscalPeriod,
			correction.ReversesMovementID, correction.SupersedesMovementID, correction.Reason,
			correction.CreatedAt, correction.CreatedByPrincipalID, correction.ValidatedAt, at, principalID)
		if err != nil {
			return err
		}
		correction.Status, correction.CommittedAt, correction.CommittedByPrincipalID = domain.MovementStatusCommitted, &at, &principalID
		result = correction
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
