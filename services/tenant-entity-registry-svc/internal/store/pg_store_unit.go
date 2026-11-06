package store

// REF-09 units of measure on Postgres (migration 000025). See registry.UnitStore for the contract.
//
// A quantity dimension has one base unit; every other unit converts to it by an exact rational factor held as two whole
// numbers. A conversion is a version: effective-dated, never edited, and the one a result applied is named so the calling
// domain can pin it. Every command takes the unit's row lock and checks expected_version under it. Converting between two
// units checks their dimension first, then multiplies and divides exactly, rounding only the final figure.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.UnitStore = (*PgStore)(nil)

const unitSelect = `SELECT u.unit_id, u.tenant_id, d.dimension_code, u.unit_code, u.name, u.symbol, u.is_base, u.status, u.decimals,
	COALESCE(u.source_standard,''), u.record_version, u.created_by, u.created_at, u.retired_on, COALESCE(u.retire_reason,''), COALESCE(u.retired_by,'')
	FROM units u JOIN quantity_dimensions d ON d.tenant_id = u.tenant_id AND d.dimension_id = u.dimension_id`

func scanUnit(row pgx.Row) (*domain.Unit, error) {
	var u domain.Unit
	var retiredOn *time.Time
	var decimals int16
	if err := row.Scan(&u.UnitID, &u.TenantID, &u.DimensionCode, &u.UnitCode, &u.Name, &u.Symbol, &u.IsBase, &u.Status, &decimals,
		&u.SourceStandard, &u.RecordVersion, &u.CreatedBy, &u.CreatedAt, &retiredOn, &u.RetireReason, &u.RetiredBy); err != nil {
		return nil, err
	}
	u.Decimals = int(decimals)
	u.RetiredOn = fmtDatePtr(retiredOn)
	return &u, nil
}

const convSelect = `SELECT version_no, factor_num::text, factor_den::text, source_class, source_ref, status, valid_from, valid_to, supersedes_version,
	COALESCE(reason,''), evidence_ref, proposed_by, proposed_at, COALESCE(approved_by,''), approved_at FROM conversion_versions`

func scanConv(row pgx.Row) (*domain.ConversionVersion, error) {
	var c domain.ConversionVersion
	var num, den string
	var from time.Time
	var to *time.Time
	var supersedes *int
	if err := row.Scan(&c.VersionNo, &num, &den, &c.SourceClass, &c.SourceRef, &c.Status, &from, &to, &supersedes, &c.Reason, &c.EvidenceRef,
		&c.ProposedBy, &c.ProposedAt, &c.ApprovedBy, &c.ApprovedAt); err != nil {
		return nil, err
	}
	r, ok := new(big.Rat).SetString(num + "/" + den)
	if !ok {
		return nil, fmt.Errorf("conversion %d holds an unreadable factor %s/%s", c.VersionNo, num, den)
	}
	c.Factor, c.FactorDecimal = domain.FractionString(r), decimalOf(r)
	c.ValidFrom, c.ValidTo = fmtDate(from), fmtDatePtr(to)
	if supersedes != nil {
		c.SupersedesVersion = *supersedes
	}
	return &c, nil
}

// decimalOf renders a factor to 18 decimals without trailing zeros; the exact fraction is carried beside it.
func decimalOf(r *big.Rat) string {
	s := domain.RoundHalfUp(r, 18)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func lockUnit(ctx context.Context, tx pgx.Tx, tid, code string) (*domain.Unit, error) {
	u, err := scanUnit(tx.QueryRow(ctx, unitSelect+` WHERE u.tenant_id = $1 AND u.unit_code = $2 FOR UPDATE OF u`, tid, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return u, err
}

func unitByCode(ctx context.Context, tx pgx.Tx, tid, code string) (*domain.Unit, error) {
	u, err := scanUnit(tx.QueryRow(ctx, unitSelect+` WHERE u.tenant_id = $1 AND u.unit_code = $2`, tid, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func convInForce(ctx context.Context, tx pgx.Tx, unitID string, day time.Time) (*domain.ConversionVersion, error) {
	c, err := scanConv(tx.QueryRow(ctx, convSelect+` WHERE unit_id = $1 AND status IN ('ACTIVE','SUPERSEDED') AND valid_from <= $2
		AND (valid_to IS NULL OR valid_to > $2) ORDER BY version_no DESC LIMIT 1`, unitID, day))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func convWithStatus(ctx context.Context, tx pgx.Tx, unitID, status string) (*domain.ConversionVersion, error) {
	c, err := scanConv(tx.QueryRow(ctx, convSelect+` WHERE unit_id = $1 AND status = $2`, unitID, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func convByNo(ctx context.Context, tx pgx.Tx, unitID string, no int) (*domain.ConversionVersion, error) {
	c, err := scanConv(tx.QueryRow(ctx, convSelect+` WHERE unit_id = $1 AND version_no = $2`, unitID, no))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// bumpUnit advances the unit's record version and, when newStatus is not empty, its status; it returns the unit as it now is.
func bumpUnit(ctx context.Context, tx pgx.Tx, tid, unitID, newStatus string) (*domain.Unit, error) {
	if _, err := tx.Exec(ctx, `UPDATE units SET record_version = record_version + 1, status = COALESCE(NULLIF($3,''), status) WHERE tenant_id = $1 AND unit_id = $2`,
		tid, unitID, newStatus); err != nil {
		return nil, err
	}
	return scanUnit(tx.QueryRow(ctx, unitSelect+` WHERE u.tenant_id = $1 AND u.unit_id = $2`, tid, unitID))
}

func insertUnit(ctx context.Context, tx pgx.Tx, tid, dimensionID string, f domain.UnitFields, isBase bool, status, actor, corr string) (*domain.Unit, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `INSERT INTO units (unit_id, tenant_id, dimension_id, unit_code, name, symbol, is_base, status, decimals, source_standard, created_by, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,NULLIF($12,''))`,
		id, tid, dimensionID, f.UnitCode, strings.TrimSpace(f.Name), strings.TrimSpace(f.Symbol), isBase, status, f.DecimalsOrDefault(),
		strings.TrimSpace(f.SourceStandard), actor, corr)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: a unit %s already exists", registry.ErrConflict, f.UnitCode)
		}
		return nil, err
	}
	return scanUnit(tx.QueryRow(ctx, unitSelect+` WHERE u.tenant_id = $1 AND u.unit_id = $2`, tid, id))
}

func insertConversion(ctx context.Context, tx pgx.Tx, tid, unitID string, no int, factor *big.Rat, req domain.ProposeConversionRequest, status string,
	from time.Time, supersedes *int, actor string) (*domain.ConversionVersion, error) {
	var approvedBy, approvedAt any
	if status == domain.ConvActive {
		approvedBy, approvedAt = actor, time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO conversion_versions (conversion_id, tenant_id, unit_id, version_no, factor_num, factor_den, source_class, source_ref, status,
		valid_from, supersedes_version, reason, evidence_ref, proposed_by, approved_by, approved_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$15,$16,NULLIF($17,''))`,
		uuid.NewString(), tid, unitID, no, factor.Num().String(), factor.Denom().String(), req.SourceClass, strings.TrimSpace(req.SourceRef), status,
		from, supersedes, strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), actor, approvedBy, approvedAt, req.CorrelationID)
	if err != nil {
		return nil, err
	}
	return convByNo(ctx, tx, unitID, no)
}

// ── dimensions and units ────────────────────────────────────────────────────

// CreateQuantityDimension creates a dimension with its base unit.
func (s *PgStore) CreateQuantityDimension(ctx context.Context, req domain.CreateQuantityDimensionRequest, actor string) (*registry.DimensionWithBase, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.DimensionWithBase
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		id := uuid.NewString()
		var d domain.QuantityDimension
		err := tx.QueryRow(ctx, `INSERT INTO quantity_dimensions (dimension_id, tenant_id, dimension_code, name, description, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,'')) RETURNING dimension_id, tenant_id, dimension_code, name, COALESCE(description,''), created_by, created_at`,
			id, tid, req.DimensionCode, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), actor, req.CorrelationID).
			Scan(&d.DimensionID, &d.TenantID, &d.DimensionCode, &d.Name, &d.Description, &d.CreatedBy, &d.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a quantity dimension %s already exists", registry.ErrConflict, req.DimensionCode)
			}
			return err
		}
		base, err := insertUnit(ctx, tx, tid, id, req.BaseUnit, true, domain.UnitActive, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		d.BaseUnitCode = base.UnitCode
		out = &registry.DimensionWithBase{Dimension: d, BaseUnit: *base}
		rec, err := events.QuantityDimensionCreatedRecord(d, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		rec, err = events.UnitCreatedRecord(*base, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListQuantityDimensions lists the tenant's dimensions with their base units.
func (s *PgStore) ListQuantityDimensions(ctx context.Context) ([]domain.QuantityDimension, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.QuantityDimension{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT d.dimension_id, d.tenant_id, d.dimension_code, d.name, COALESCE(d.description,''), COALESCE(b.unit_code,''), d.created_by, d.created_at
			FROM quantity_dimensions d LEFT JOIN units b ON b.tenant_id = d.tenant_id AND b.dimension_id = d.dimension_id AND b.is_base
			WHERE d.tenant_id = $1 ORDER BY d.dimension_code`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.QuantityDimension
			if err := rows.Scan(&d.DimensionID, &d.TenantID, &d.DimensionCode, &d.Name, &d.Description, &d.BaseUnitCode, &d.CreatedBy, &d.CreatedAt); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// CreateUnit opens a DRAFT unit of an existing dimension.
func (s *PgStore) CreateUnit(ctx context.Context, req domain.CreateUnitRequest, actor string) (*registry.UnitOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.UnitOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var dimensionID string
		err := tx.QueryRow(ctx, `SELECT dimension_id FROM quantity_dimensions WHERE tenant_id = $1 AND dimension_code = $2`, tid, req.DimensionCode).Scan(&dimensionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: dimension_code %s names no quantity dimension of this tenant", registry.ErrInvalidInput, req.DimensionCode)
		}
		if err != nil {
			return err
		}
		u, err := insertUnit(ctx, tx, tid, dimensionID, req.UnitFields, false, domain.UnitDraft, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.UnitOutcome{Unit: *u}
		rec, err := events.UnitCreatedRecord(*u, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── conversions ─────────────────────────────────────────────────────────────

// ProposeConversion records a unit's next conversion version.
func (s *PgStore) ProposeConversion(ctx context.Context, unitCode string, supersede bool, req domain.ProposeConversionRequest, actor string, allowUnversioned bool) (*registry.UnitOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.UnitOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		u, err := lockUnit(ctx, tx, tid, unitCode)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, u.RecordVersion, allowUnversioned, "unit"); err != nil {
			return err
		}
		if u.IsBase {
			return fmt.Errorf("%w: %s is the base unit of %s and converts to itself", registry.ErrInvalidInput, u.UnitCode, u.DimensionCode)
		}
		if u.Status == domain.UnitRetired {
			return fmt.Errorf("%w: unit %s is retired", registry.ErrStateConflict, u.UnitCode)
		}
		factor, err := domain.ParseFactor(req.Factor)
		if err != nil {
			return fmt.Errorf("%w: %v", registry.ErrInvalidInput, err)
		}
		pending, err := convWithStatus(ctx, tx, u.UnitID, domain.ConvPending)
		if err != nil {
			return err
		}
		if pending != nil {
			return fmt.Errorf("%w: conversion version %d of %s is already awaiting approval", registry.ErrStateConflict, pending.VersionNo, u.UnitCode)
		}
		active, err := convWithStatus(ctx, tx, u.UnitID, domain.ConvActive)
		if err != nil {
			return err
		}
		today := asOfDate(time.Now().UTC())
		from := today
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		var supersedes *int
		switch {
		case !supersede && active != nil:
			return fmt.Errorf("%w: %s already has an active conversion; supersede it", registry.ErrStateConflict, u.UnitCode)
		case supersede && active == nil:
			return fmt.Errorf("%w: %s has no active conversion to supersede", registry.ErrStateConflict, u.UnitCode)
		case supersede:
			// A change is prospective: it never rewrites a conversion already applied to a quantity.
			activeFrom, _ := domain.ParseDate(active.ValidFrom)
			if from.Before(today) || !from.After(activeFrom) {
				return fmt.Errorf("%w: a replacement takes effect today or later, and after the active conversion began on %s", registry.ErrInvalidInput, active.ValidFrom)
			}
			if old, _ := new(big.Rat).SetString(active.Factor); old != nil && old.Cmp(factor) == 0 && active.SourceClass == req.SourceClass {
				return fmt.Errorf("%w: the factor is unchanged", registry.ErrInvalidInput)
			}
			n := active.VersionNo
			supersedes = &n
		}
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_no),0) + 1 FROM conversion_versions WHERE unit_id = $1`, u.UnitID).Scan(&next); err != nil {
			return err
		}
		status := domain.ConvPending
		if !domain.NonStandard(req.SourceClass) {
			status = domain.ConvActive
		}
		if status == domain.ConvActive && active != nil {
			// The live conversion ends where the new one begins, before the new one can be the live one.
			if _, err := tx.Exec(ctx, `UPDATE conversion_versions SET status = 'SUPERSEDED', valid_to = $3 WHERE unit_id = $1 AND version_no = $2`, u.UnitID, active.VersionNo, from); err != nil {
				return err
			}
		}
		c, err := insertConversion(ctx, tx, tid, u.UnitID, next, factor, req, status, from, supersedes, actor)
		if err != nil {
			return err
		}
		newStatus := ""
		if status == domain.ConvActive && u.Status == domain.UnitDraft {
			newStatus = domain.UnitActive
		}
		nu, err := bumpUnit(ctx, tx, tid, u.UnitID, newStatus)
		if err != nil {
			return err
		}
		out = &registry.UnitOutcome{Unit: *nu, Conversion: c}
		if status == domain.ConvPending {
			rec, err := events.ConversionRecord(events.EventConversionProposed, *nu, *c, active, actor, req.CorrelationID)
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, rec)
		}
		return s.announceActivation(ctx, tx, nu, c, active, actor, req.CorrelationID, out)
	})
	return out, err
}

// announceActivation emits the activation of c and, when it replaced a conversion, the supersession of that one.
func (s *PgStore) announceActivation(ctx context.Context, tx pgx.Tx, u *domain.Unit, c, replaced *domain.ConversionVersion, actor, corr string, out *registry.UnitOutcome) error {
	if replaced != nil {
		old, err := convByNo(ctx, tx, u.UnitID, replaced.VersionNo)
		if err != nil {
			return err
		}
		out.Previous = old
		rec, err := events.ConversionRecord(events.EventConversionSuperseded, *u, *old, c, actor, corr)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
	}
	rec, err := events.ConversionRecord(events.EventConversionActivated, *u, *c, replaced, actor, corr)
	if err != nil {
		return err
	}
	return s.enqueue(ctx, tx, rec)
}

// ApproveConversion takes a pending conversion into effect.
func (s *PgStore) ApproveConversion(ctx context.Context, unitCode string, versionNo int, req domain.ApproveConversionRequest, actor string, allowUnversioned bool) (*registry.UnitOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.UnitOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		u, err := lockUnit(ctx, tx, tid, unitCode)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, u.RecordVersion, allowUnversioned, "unit"); err != nil {
			return err
		}
		pend, err := convByNo(ctx, tx, u.UnitID, versionNo)
		if err != nil {
			return err
		}
		if pend == nil {
			return registry.ErrNotFound
		}
		if pend.Status != domain.ConvPending {
			return fmt.Errorf("%w: conversion version %d is %s, not pending", registry.ErrStateConflict, versionNo, pend.Status)
		}
		// s4.19: a manual or non-standard conversion is not self-approved.
		if pend.ProposedBy == actor {
			return fmt.Errorf("%w: a conversion is approved by someone other than the principal who proposed it", registry.ErrSelfApproval)
		}
		if u.Status == domain.UnitRetired {
			return fmt.Errorf("%w: unit %s is retired", registry.ErrStateConflict, u.UnitCode)
		}
		active, err := convWithStatus(ctx, tx, u.UnitID, domain.ConvActive)
		if err != nil {
			return err
		}
		from, _ := domain.ParseDate(pend.ValidFrom)
		switch {
		case pend.SupersedesVersion != 0:
			if active == nil || active.VersionNo != pend.SupersedesVersion {
				return fmt.Errorf("%w: the conversion it replaces is no longer the active one; propose it again", registry.ErrStateConflict)
			}
			if from.Before(asOfDate(time.Now().UTC())) {
				return fmt.Errorf("%w: the replacement was to take effect on %s, which has passed; propose it again", registry.ErrStateConflict, pend.ValidFrom)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversion_versions SET status = 'SUPERSEDED', valid_to = $3 WHERE unit_id = $1 AND version_no = $2`, u.UnitID, active.VersionNo, from); err != nil {
				return err
			}
		case active != nil:
			return fmt.Errorf("%w: %s already has an active conversion", registry.ErrStateConflict, u.UnitCode)
		}
		if _, err := tx.Exec(ctx, `UPDATE conversion_versions SET status = 'ACTIVE', approved_by = $3, approved_at = now() WHERE unit_id = $1 AND version_no = $2`,
			u.UnitID, versionNo, actor); err != nil {
			return err
		}
		newStatus := ""
		if u.Status == domain.UnitDraft {
			newStatus = domain.UnitActive
		}
		nu, err := bumpUnit(ctx, tx, tid, u.UnitID, newStatus)
		if err != nil {
			return err
		}
		c, err := convByNo(ctx, tx, u.UnitID, versionNo)
		if err != nil {
			return err
		}
		out = &registry.UnitOutcome{Unit: *nu, Conversion: c}
		return s.announceActivation(ctx, tx, nu, c, active, actor, req.CorrelationID, out)
	})
	return out, err
}

// RetireUnit rejects new use of a unit from today.
func (s *PgStore) RetireUnit(ctx context.Context, unitCode string, req domain.RetireUnitRequest, actor string, allowUnversioned bool) (*registry.UnitOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.UnitOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		u, err := lockUnit(ctx, tx, tid, unitCode)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, u.RecordVersion, allowUnversioned, "unit"); err != nil {
			return err
		}
		if u.Status == domain.UnitRetired {
			return fmt.Errorf("%w: unit %s is already retired", registry.ErrStateConflict, u.UnitCode)
		}
		if u.IsBase {
			// Every other unit converts through the base: retiring it first would strand them.
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM units o JOIN units b ON b.tenant_id = o.tenant_id AND b.dimension_id = o.dimension_id
				WHERE b.unit_id = $1 AND o.unit_id <> b.unit_id AND o.status <> 'RETIRED'`, u.UnitID).Scan(&live); err != nil {
				return err
			}
			if live > 0 {
				return fmt.Errorf("%w: %s is the base unit of %s and %d other unit(s) still convert through it; retire them first", registry.ErrStateConflict, u.UnitCode, u.DimensionCode, live)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE units SET status = 'RETIRED', retired_on = $3, retired_by = $4, retired_at = now(), retire_reason = $5, record_version = record_version + 1
			WHERE tenant_id = $1 AND unit_id = $2`, tid, u.UnitID, asOfDate(time.Now().UTC()), actor, strings.TrimSpace(req.Reason)); err != nil {
			return err
		}
		nu, err := scanUnit(tx.QueryRow(ctx, unitSelect+` WHERE u.tenant_id = $1 AND u.unit_id = $2`, tid, u.UnitID))
		if err != nil {
			return err
		}
		out = &registry.UnitOutcome{Unit: *nu}
		rec, err := events.UnitRetiredRecord(*nu, strings.TrimSpace(req.EvidenceRef), req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── reads ───────────────────────────────────────────────────────────────────

// retiredOn reports whether the unit is retired on day.
func retiredOn(u *domain.Unit, day time.Time) bool {
	if u.RetiredOn == "" {
		return false
	}
	r, _ := domain.ParseDate(u.RetiredOn)
	return !r.After(day)
}

// GetUnit returns the unit as it was on asOf.
func (s *PgStore) GetUnit(ctx context.Context, unitCode string, asOf time.Time, history bool) (*domain.UnitView, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.UnitView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		u, err := unitByCode(ctx, tx, tid, unitCode)
		if err != nil || u == nil {
			return err
		}
		day := asOfDate(asOf)
		v := &domain.UnitView{Unit: *u, AsOf: fmtDate(day)}
		switch {
		case retiredOn(u, day):
			v.Reason = "the unit was retired on " + u.RetiredOn
		case u.IsBase:
			v.Usable = true
		default:
			c, err := convInForce(ctx, tx, u.UnitID, day)
			if err != nil {
				return err
			}
			v.Conversion = c
			if c == nil {
				v.Reason = "no approved conversion is in force on that date"
			} else {
				v.Usable = true
			}
		}
		if v.Pending, err = convWithStatus(ctx, tx, u.UnitID, domain.ConvPending); err != nil {
			return err
		}
		if history {
			rows, err := tx.Query(ctx, convSelect+` WHERE unit_id = $1 ORDER BY version_no`, u.UnitID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				c, err := scanConv(rows)
				if err != nil {
					return err
				}
				v.Versions = append(v.Versions, *c)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out = v
		return nil
	})
	return out, err
}

// GetConversionVersion returns one conversion version of a unit.
func (s *PgStore) GetConversionVersion(ctx context.Context, unitCode string, versionNo int) (*domain.ConversionVersion, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ConversionVersion
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		u, err := unitByCode(ctx, tx, tid, unitCode)
		if err != nil || u == nil {
			return err
		}
		out, err = convByNo(ctx, tx, u.UnitID, versionNo)
		return err
	})
	return out, err
}

// ListUnitsByDimension lists a dimension's units, the base first.
func (s *PgStore) ListUnitsByDimension(ctx context.Context, dimensionCode string) ([]domain.Unit, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.Unit{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM quantity_dimensions WHERE tenant_id = $1 AND dimension_code = $2`, tid, dimensionCode).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return registry.ErrNotFound
		}
		rows, err := tx.Query(ctx, unitSelect+` WHERE u.tenant_id = $1 AND d.dimension_code = $2 ORDER BY u.is_base DESC, u.unit_code`, tid, dimensionCode)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUnit(rows)
			if err != nil {
				return err
			}
			out = append(out, *u)
		}
		return rows.Err()
	})
	return out, err
}

// pinFor is the factor a unit takes to its base on day, with the version that supplied it. The base is its own factor, 1.
func pinFor(ctx context.Context, tx pgx.Tx, u *domain.Unit, day time.Time) (*domain.ConversionPin, *big.Rat, error) {
	if retiredOn(u, day) {
		return nil, nil, fmt.Errorf("%w: unit %s was retired on %s", registry.ErrReferenceInvalid, u.UnitCode, u.RetiredOn)
	}
	if u.IsBase {
		return &domain.ConversionPin{UnitCode: u.UnitCode, IsBase: true, Factor: "1", FactorDecimal: "1"}, big.NewRat(1, 1), nil
	}
	c, err := convInForce(ctx, tx, u.UnitID, day)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, fmt.Errorf("%w: unit %s on %s", registry.ErrConversionMissing, u.UnitCode, fmtDate(day))
	}
	r, ok := new(big.Rat).SetString(c.Factor)
	if !ok {
		return nil, nil, fmt.Errorf("conversion %d of %s holds an unreadable factor", c.VersionNo, u.UnitCode)
	}
	return &domain.ConversionPin{UnitCode: u.UnitCode, VersionNo: c.VersionNo, Factor: c.Factor, FactorDecimal: c.FactorDecimal,
		SourceClass: c.SourceClass, SourceRef: c.SourceRef, ValidFrom: c.ValidFrom}, r, nil
}

// ConvertQuantity converts a quantity exactly between two units of one dimension, on asOf.
func (s *PgStore) ConvertQuantity(ctx context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ConversionResult
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		qty, err := domain.ParseQuantity(q.Quantity)
		if err != nil {
			return fmt.Errorf("%w: %v", registry.ErrInvalidInput, err)
		}
		from, err := unitByCode(ctx, tx, tid, q.From)
		if err != nil {
			return err
		}
		if from == nil {
			return fmt.Errorf("%w: unit %s", registry.ErrNotFound, q.From)
		}
		to, err := unitByCode(ctx, tx, tid, q.To)
		if err != nil {
			return err
		}
		if to == nil {
			return fmt.Errorf("%w: unit %s", registry.ErrNotFound, q.To)
		}
		// Dimensionality first: no arithmetic is attempted across dimensions.
		if from.DimensionCode != to.DimensionCode {
			return fmt.Errorf("%w: %s is %s and %s is %s", registry.ErrDimensionMismatch, from.UnitCode, from.DimensionCode, to.UnitCode, to.DimensionCode)
		}
		day := asOfDate(q.AsOf)
		fp, fr, err := pinFor(ctx, tx, from, day)
		if err != nil {
			return err
		}
		tp, trat, err := pinFor(ctx, tx, to, day)
		if err != nil {
			return err
		}
		exact := domain.ConvertExact(qty, fr, trat)
		out = &domain.ConversionResult{
			Dimension: from.DimensionCode, AsOf: fmtDate(day), Input: strings.TrimSpace(q.Quantity), FromUnit: from.UnitCode, ToUnit: to.UnitCode,
			Value: domain.RoundHalfUp(exact, to.Decimals), Exact: domain.FractionString(exact), Decimals: to.Decimals, Rounding: domain.UnitRounding,
			From: *fp, To: *tp,
		}
		return nil
	})
	return out, err
}
