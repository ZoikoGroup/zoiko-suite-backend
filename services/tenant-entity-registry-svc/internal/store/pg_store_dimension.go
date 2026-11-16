package store

// REF-08 financial dimensions on Postgres (migration 000024). See registry.DimensionStore for the contract.
//
// A value's identity never changes; its label and parent are effective-dated versions, its status a recorded transition
// per date. Every command takes the value's row lock and checks expected_version under it. A hierarchy never loops: a
// proposed parent is checked on the date it takes effect and on every later date a version of the type's hierarchy begins.
// Historical questions (the roll-up, the label, whether the value could post) are answered from the versions and the
// transitions, never from the current state.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.DimensionStore = (*PgStore)(nil)

const dimTypeColumns = `type_id, tenant_id, type_code, name, COALESCE(description,''), hierarchical, controlled, COALESCE(source_domain,''), created_by, created_at`

func scanDimType(row pgx.Row) (*domain.DimensionType, error) {
	var t domain.DimensionType
	if err := row.Scan(&t.TypeID, &t.TenantID, &t.TypeCode, &t.Name, &t.Description, &t.Hierarchical, &t.Controlled, &t.SourceDomain, &t.CreatedBy, &t.CreatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

const dimValueSelect = `SELECT v.value_id, v.tenant_id, v.type_id, t.type_code, COALESCE(v.legal_entity_id::text,''), v.code, v.status, v.current_version,
	v.record_version, v.created_by, v.created_at FROM dimension_values v JOIN dimension_types t ON t.tenant_id = v.tenant_id AND t.type_id = v.type_id`

func scanDimValue(row pgx.Row) (*domain.DimensionValue, error) {
	var v domain.DimensionValue
	if err := row.Scan(&v.ValueID, &v.TenantID, &v.TypeID, &v.TypeCode, &v.LegalEntityID, &v.Code, &v.Status, &v.CurrentVersion, &v.RecordVersion, &v.CreatedBy, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

const dimVersionSelect = `SELECT ver.version_no, COALESCE(ver.parent_value_id::text,''), COALESCE(p.code,''), ver.label, ver.status, ver.valid_from, ver.valid_to,
	COALESCE(ver.reason,''), ver.evidence_ref, ver.proposed_by, ver.proposed_at, COALESCE(ver.approved_by,''), ver.approved_at
	FROM dimension_value_versions ver LEFT JOIN dimension_values p ON p.tenant_id = ver.tenant_id AND p.value_id = ver.parent_value_id`

func scanDimVersion(row pgx.Row) (*domain.HierarchyVersion, error) {
	var h domain.HierarchyVersion
	var from time.Time
	var to *time.Time
	if err := row.Scan(&h.VersionNo, &h.ParentValueID, &h.ParentCode, &h.Label, &h.Status, &from, &to, &h.Reason, &h.EvidenceRef, &h.ProposedBy,
		&h.ProposedAt, &h.ApprovedBy, &h.ApprovedAt); err != nil {
		return nil, err
	}
	h.ValidFrom, h.ValidTo = fmtDate(from), fmtDatePtr(to)
	return &h, nil
}

const dimTransitionColumns = `seq, from_status, to_status, effective_date, command, COALESCE(reason,''), evidence_ref, exception_until, actor,
	COALESCE(version_before,0), version_after, occurred_at`

func scanDimTransition(row pgx.Row) (*domain.DimensionTransition, error) {
	var t domain.DimensionTransition
	var from *string
	var eff time.Time
	var exc *time.Time
	if err := row.Scan(&t.Seq, &from, &t.To, &eff, &t.Command, &t.Reason, &t.EvidenceRef, &exc, &t.Actor, &t.VersionBefore, &t.VersionAfter, &t.OccurredAt); err != nil {
		return nil, err
	}
	if from != nil {
		t.From = *from
	}
	t.EffectiveDate, t.ExceptionUntil = fmtDate(eff), fmtDatePtr(exc)
	return &t, nil
}

// dimType finds a dimension type by code; ErrInvalidInput when the tenant has none.
func dimType(ctx context.Context, tx pgx.Tx, tid, code string) (*domain.DimensionType, error) {
	t, err := scanDimType(tx.QueryRow(ctx, `SELECT `+dimTypeColumns+` FROM dimension_types WHERE tenant_id = $1 AND type_code = $2`, tid, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: type_code %s names no dimension type of this tenant", registry.ErrInvalidInput, code)
	}
	return t, err
}

func lockDimValue(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.DimensionValue, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	v, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1 AND v.tenant_id = $2 FOR UPDATE OF v`, id, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return v, err
}

func dimVersionInForce(ctx context.Context, tx pgx.Tx, valueID string, day time.Time) (*domain.HierarchyVersion, error) {
	h, err := scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.status = 'APPROVED' AND ver.valid_from <= $2
		AND (ver.valid_to IS NULL OR ver.valid_to > $2) ORDER BY ver.version_no DESC LIMIT 1`, valueID, day))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

func dimLatestApproved(ctx context.Context, tx pgx.Tx, valueID string) (*domain.HierarchyVersion, error) {
	return scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.status = 'APPROVED' ORDER BY ver.version_no DESC LIMIT 1`, valueID))
}

// parentMapAsOf is value id to parent id for every value of the type, as the approved hierarchy stood on day.
func parentMapAsOf(ctx context.Context, tx pgx.Tx, tid, typeID string, day time.Time) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT ver.value_id::text, COALESCE(ver.parent_value_id::text,'') FROM dimension_value_versions ver
		JOIN dimension_values v ON v.tenant_id = ver.tenant_id AND v.value_id = ver.value_id
		WHERE ver.tenant_id = $1 AND v.type_id = $2 AND ver.status = 'APPROVED' AND ver.valid_from <= $3 AND (ver.valid_to IS NULL OR ver.valid_to > $3)`, tid, typeID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		if parent != "" {
			m[id] = parent
		}
	}
	return m, rows.Err()
}

// checkNoLoop refuses a parent that would close a loop on the date it takes effect or on any later date a version of the
// type's hierarchy begins: a loop needs only one such date to exist.
func checkNoLoop(ctx context.Context, tx pgx.Tx, tid, typeID, valueID, newParent string, from time.Time) error {
	if newParent == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ver.valid_from FROM dimension_value_versions ver JOIN dimension_values v ON v.tenant_id = ver.tenant_id AND v.value_id = ver.value_id
		WHERE ver.tenant_id = $1 AND v.type_id = $2 AND ver.status = 'APPROVED' AND ver.valid_from > $3 ORDER BY ver.valid_from`, tid, typeID, from)
	if err != nil {
		return err
	}
	dates := []time.Time{from}
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		dates = append(dates, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range dates {
		m, err := parentMapAsOf(ctx, tx, tid, typeID, d)
		if err != nil {
			return err
		}
		if domain.HierarchyLoop(m, valueID, newParent) {
			return fmt.Errorf("%w: that parent would make a loop in the hierarchy as it stands on %s", registry.ErrInvalidInput, fmtDate(d))
		}
	}
	return nil
}

func insertDimVersion(ctx context.Context, tx pgx.Tx, tid, valueID string, no int, parent, label, status string, from time.Time, reason, evidence, actor string, approve bool, corr string) error {
	var approvedBy any
	var approvedAt any
	if approve {
		approvedBy, approvedAt = actor, time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO dimension_value_versions (version_id, tenant_id, value_id, version_no, parent_value_id, label, status, valid_from, reason,
		evidence_ref, proposed_by, approved_by, approved_at, correlation_id) VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,NULLIF($14,''))`,
		uuid.NewString(), tid, valueID, no, parent, strings.TrimSpace(label), status, from, reason, strings.TrimSpace(evidence), actor, approvedBy, approvedAt, corr)
	return err
}

func insertDimTransition(ctx context.Context, tx pgx.Tx, tid, valueID string, seq int, from *string, to string, eff time.Time, command, reason, evidence string, exc *time.Time,
	actor string, before *int64, after int64, corr string) (*domain.DimensionTransition, error) {
	return scanDimTransition(tx.QueryRow(ctx, `INSERT INTO dimension_value_transitions (transition_id, tenant_id, value_id, seq, from_status, to_status, effective_date,
		command, reason, evidence_ref, exception_until, actor, version_before, version_after, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,$14,NULLIF($15,'')) RETURNING `+dimTransitionColumns,
		uuid.NewString(), tid, valueID, seq, from, to, eff, command, reason, evidence, exc, actor, before, after, corr))
}

// ── types ───────────────────────────────────────────────────────────────────

// CreateDimensionType creates a dimension type.
func (s *PgStore) CreateDimensionType(ctx context.Context, req domain.CreateDimensionTypeRequest, actor string) (*domain.DimensionType, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.DimensionType
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		t, err := scanDimType(tx.QueryRow(ctx, `INSERT INTO dimension_types (type_id, tenant_id, type_code, name, description, hierarchical, controlled, source_domain, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,NULLIF($8,''),$9,NULLIF($10,'')) RETURNING `+dimTypeColumns,
			uuid.NewString(), tid, req.TypeCode, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), req.Hierarchical, req.Controlled,
			strings.TrimSpace(req.SourceDomain), actor, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a dimension type %s already exists", registry.ErrConflict, req.TypeCode)
			}
			return err
		}
		out = t
		rec, err := events.DimensionTypeCreatedRecord(*t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListDimensionTypes lists the tenant's dimension types.
func (s *PgStore) ListDimensionTypes(ctx context.Context) ([]domain.DimensionType, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.DimensionType{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+dimTypeColumns+` FROM dimension_types WHERE tenant_id = $1 ORDER BY type_code`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanDimType(rows)
			if err != nil {
				return err
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	return out, err
}

// ── values ──────────────────────────────────────────────────────────────────

// scopeCompatible reports whether a parent can be shared with a value: tenant-wide for anyone, or the same entity's.
func scopeCompatible(parentEntity, valueEntity string) bool {
	return parentEntity == "" || parentEntity == valueEntity
}

// CreateDimensionValue opens a DRAFT value with its first hierarchy version.
func (s *PgStore) CreateDimensionValue(ctx context.Context, req domain.CreateDimensionValueRequest, actor string) (*registry.DimensionOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.DimensionOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		typ, err := dimType(ctx, tx, tid, req.TypeCode)
		if err != nil {
			return err
		}
		if req.LegalEntityID != "" {
			if err := entityExists(ctx, tx, tid, req.LegalEntityID); err != nil {
				return err
			}
		}
		// An entity's code never shadows a tenant-wide one, and a tenant-wide code never covers an entity's: resolving by code
		// would be ambiguous.
		var clash string
		err = tx.QueryRow(ctx, `SELECT code FROM dimension_values WHERE tenant_id = $1 AND type_id = $2 AND code = $3
			AND ((legal_entity_id IS NULL) <> ($4::text = ''))`, tid, typ.TypeID, req.Code, req.LegalEntityID).Scan(&clash)
		if err == nil {
			return fmt.Errorf("%w: code %s is already used in another scope of dimension type %s", registry.ErrConflict, req.Code, req.TypeCode)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		from := time.Now().UTC()
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		from = asOfDate(from)
		if req.ParentValueID != "" {
			if !typ.Hierarchical {
				return fmt.Errorf("%w: dimension type %s has no hierarchy", registry.ErrInvalidInput, typ.TypeCode)
			}
			if _, err := uuid.Parse(req.ParentValueID); err != nil {
				return fmt.Errorf("%w: parent_value_id names no value of this tenant", registry.ErrInvalidInput)
			}
			p, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1 AND v.tenant_id = $2`, req.ParentValueID, tid))
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: parent_value_id names no value of this tenant", registry.ErrInvalidInput)
			}
			if err != nil {
				return err
			}
			if p.TypeID != typ.TypeID {
				return fmt.Errorf("%w: a parent is of the same dimension type", registry.ErrInvalidInput)
			}
			if !scopeCompatible(p.LegalEntityID, req.LegalEntityID) {
				return fmt.Errorf("%w: a parent is tenant-wide or of the same entity", registry.ErrInvalidInput)
			}
		}
		id := uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO dimension_values (value_id, tenant_id, type_id, legal_entity_id, code, created_by, correlation_id)
			VALUES ($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,NULLIF($7,''))`, id, tid, typ.TypeID, req.LegalEntityID, req.Code, actor, req.CorrelationID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: code %s already exists in this scope of dimension type %s", registry.ErrConflict, req.Code, req.TypeCode)
			}
			return err
		}
		// The first version is the node's placement, approved with its creation.
		if err := insertDimVersion(ctx, tx, tid, id, 1, req.ParentValueID, req.Label, domain.HierarchyApproved, from, "", req.EvidenceRef, actor, true, req.CorrelationID); err != nil {
			return err
		}
		t, err := insertDimTransition(ctx, tx, tid, id, 1, nil, domain.DimDraft, from, "CreateDimensionValue", "", strings.TrimSpace(req.EvidenceRef), nil, actor, nil, 1, req.CorrelationID)
		if err != nil {
			return err
		}
		v, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1`, id))
		if err != nil {
			return err
		}
		h, err := dimLatestApproved(ctx, tx, id)
		if err != nil {
			return err
		}
		out = &registry.DimensionOutcome{Value: *v, Hierarchy: h, Transition: t}
		rec, err := events.DimensionValueCreatedRecord(*v, *h, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ProposeHierarchyChange is ReparentDimensionValue.
func (s *PgStore) ProposeHierarchyChange(ctx context.Context, valueID string, req domain.ReparentDimensionValueRequest, actor string, allowUnversioned bool) (*registry.DimensionOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.DimensionOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := lockDimValue(ctx, tx, tid, valueID)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, v.RecordVersion, allowUnversioned, "dimension value"); err != nil {
			return err
		}
		typ, err := dimType(ctx, tx, tid, v.TypeCode)
		if err != nil {
			return err
		}
		if !typ.Hierarchical {
			return fmt.Errorf("%w: dimension type %s has no hierarchy", registry.ErrInvalidInput, typ.TypeCode)
		}
		cur, err := dimLatestApproved(ctx, tx, valueID)
		if err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dimension_value_versions WHERE value_id = $1 AND status = 'PENDING'`, valueID).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%w: a hierarchy change for this value is already pending approval", registry.ErrConflict)
		}
		newParent := ""
		if req.ParentValueID != nil {
			newParent = strings.TrimSpace(*req.ParentValueID)
		}
		if newParent != "" {
			if _, err := uuid.Parse(newParent); err != nil {
				return fmt.Errorf("%w: parent_value_id names no value of this tenant", registry.ErrInvalidInput)
			}
			p, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1 AND v.tenant_id = $2`, newParent, tid))
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: parent_value_id names no value of this tenant", registry.ErrInvalidInput)
			}
			if err != nil {
				return err
			}
			switch {
			case p.ValueID == v.ValueID:
				return fmt.Errorf("%w: a value cannot be its own parent", registry.ErrInvalidInput)
			case p.TypeID != v.TypeID:
				return fmt.Errorf("%w: a parent is of the same dimension type", registry.ErrInvalidInput)
			case !scopeCompatible(p.LegalEntityID, v.LegalEntityID):
				return fmt.Errorf("%w: a parent is tenant-wide or of the same entity", registry.ErrInvalidInput)
			}
		}
		label := strings.TrimSpace(req.NewLabel)
		if label == "" {
			label = cur.Label
		}
		if newParent == cur.ParentValueID && label == cur.Label {
			return fmt.Errorf("%w: the hierarchy position and label are already these", registry.ErrInvalidInput)
		}
		today := asOfDate(time.Now().UTC())
		from := today
		if req.ValidFrom != "" {
			f, _ := domain.ParseDate(req.ValidFrom)
			from = f
		}
		curFrom, _ := domain.ParseDate(cur.ValidFrom)
		// A change is prospective: it never reaches back over periods already reported on the old hierarchy.
		if from.Before(today) {
			return fmt.Errorf("%w: a hierarchy change is prospective; valid_from cannot be before today (%s)", registry.ErrInvalidInput, fmtDate(today))
		}
		if !from.After(curFrom) {
			return fmt.Errorf("%w: valid_from must be after the current version began on %s", registry.ErrInvalidInput, cur.ValidFrom)
		}
		if err := checkNoLoop(ctx, tx, tid, v.TypeID, v.ValueID, newParent, from); err != nil {
			return err
		}
		var maxNo int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_no),0) FROM dimension_value_versions WHERE value_id = $1`, valueID).Scan(&maxNo); err != nil {
			return err
		}
		next := maxNo + 1
		status := domain.HierarchyApproved
		if typ.Controlled {
			status = domain.HierarchyPending
		}
		if status == domain.HierarchyApproved {
			if _, err := tx.Exec(ctx, `UPDATE dimension_value_versions SET valid_to = $3 WHERE value_id = $1 AND version_no = $2`, valueID, cur.VersionNo, from); err != nil {
				return err
			}
		}
		if err := insertDimVersion(ctx, tx, tid, valueID, next, newParent, label, status, from, strings.TrimSpace(req.Reason), req.EvidenceRef, actor, status == domain.HierarchyApproved, req.CorrelationID); err != nil {
			return err
		}
		nv, err := scanDimValue(tx.QueryRow(ctx, `WITH u AS (UPDATE dimension_values SET current_version = CASE WHEN $3 = 'APPROVED' THEN $4 ELSE current_version END,
			record_version = record_version + 1 WHERE value_id = $1 AND tenant_id = $2 RETURNING *)
			SELECT u.value_id, u.tenant_id, u.type_id, $5::text, COALESCE(u.legal_entity_id::text,''), u.code, u.status, u.current_version, u.record_version, u.created_by, u.created_at FROM u`,
			valueID, tid, status, next, v.TypeCode))
		if err != nil {
			return err
		}
		h, err := scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.version_no = $2`, valueID, next))
		if err != nil {
			return err
		}
		out = &registry.DimensionOutcome{Value: *nv, Hierarchy: h}
		eventType := events.EventDimensionHierarchyChanged
		if status == domain.HierarchyPending {
			eventType = events.EventDimensionHierarchyProposed
		}
		rec, err := events.DimensionHierarchyRecord(eventType, *nv, *h, cur, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ApproveHierarchyChange takes a pending version into effect.
func (s *PgStore) ApproveHierarchyChange(ctx context.Context, valueID string, versionNo int, req domain.ApproveDimensionHierarchyRequest, actor string, allowUnversioned bool) (*registry.DimensionOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.DimensionOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := lockDimValue(ctx, tx, tid, valueID)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, v.RecordVersion, allowUnversioned, "dimension value"); err != nil {
			return err
		}
		pend, err := scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.version_no = $2`, valueID, versionNo))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if pend.Status != domain.HierarchyPending {
			return fmt.Errorf("%w: hierarchy version %d is %s, not pending", registry.ErrStateConflict, versionNo, pend.Status)
		}
		// s4.18: a hierarchy change affecting reporting or control aggregation is independently reviewed.
		if pend.ProposedBy == actor {
			return fmt.Errorf("%w: a hierarchy change is approved by someone other than the principal who proposed it", registry.ErrSelfApproval)
		}
		cur, err := dimLatestApproved(ctx, tx, valueID)
		if err != nil {
			return err
		}
		today := asOfDate(time.Now().UTC())
		from, _ := domain.ParseDate(pend.ValidFrom)
		curFrom, _ := domain.ParseDate(cur.ValidFrom)
		if from.Before(today) {
			return fmt.Errorf("%w: the change was to take effect on %s, which has passed; propose it again", registry.ErrStateConflict, pend.ValidFrom)
		}
		if !from.After(curFrom) {
			return fmt.Errorf("%w: the hierarchy has moved on since this was proposed; propose it again", registry.ErrStateConflict)
		}
		// The hierarchy may have changed since it was proposed: check again.
		if err := checkNoLoop(ctx, tx, tid, v.TypeID, v.ValueID, pend.ParentValueID, from); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dimension_value_versions SET valid_to = $3 WHERE value_id = $1 AND version_no = $2`, valueID, cur.VersionNo, from); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dimension_value_versions SET status = 'APPROVED', approved_by = $3, approved_at = now() WHERE value_id = $1 AND version_no = $2`,
			valueID, versionNo, actor); err != nil {
			return err
		}
		nv, err := scanDimValue(tx.QueryRow(ctx, `WITH u AS (UPDATE dimension_values SET current_version = $3, record_version = record_version + 1 WHERE value_id = $1 AND tenant_id = $2 RETURNING *)
			SELECT u.value_id, u.tenant_id, u.type_id, $4::text, COALESCE(u.legal_entity_id::text,''), u.code, u.status, u.current_version, u.record_version, u.created_by, u.created_at FROM u`,
			valueID, tid, versionNo, v.TypeCode))
		if err != nil {
			return err
		}
		h, err := scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.version_no = $2`, valueID, versionNo))
		if err != nil {
			return err
		}
		out = &registry.DimensionOutcome{Value: *nv, Hierarchy: h}
		rec, err := events.DimensionHierarchyRecord(events.EventDimensionHierarchyChanged, *nv, *h, cur, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ChangeDimensionStatus activates or deactivates a value.
func (s *PgStore) ChangeDimensionStatus(ctx context.Context, valueID, command, to string, req domain.DimensionStatusRequest, actor string, allowUnversioned bool) (*registry.DimensionOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.DimensionOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := lockDimValue(ctx, tx, tid, valueID)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, v.RecordVersion, allowUnversioned, "dimension value"); err != nil {
			return err
		}
		if !domain.DimensionCanMoveTo(v.Status, to) {
			return fmt.Errorf("%w: a %s dimension value cannot move to %s", registry.ErrStateConflict, v.Status, to)
		}
		eff := asOfDate(time.Now().UTC())
		if req.EffectiveDate != "" {
			eff, _ = domain.ParseDate(req.EffectiveDate)
		}
		var lastSeq int
		var lastEff time.Time
		if err := tx.QueryRow(ctx, `SELECT seq, effective_date FROM dimension_value_transitions WHERE value_id = $1 ORDER BY seq DESC LIMIT 1`, valueID).Scan(&lastSeq, &lastEff); err != nil {
			return err
		}
		if eff.Before(lastEff) {
			return fmt.Errorf("%w: effective_date is before the last change, on %s", registry.ErrInvalidInput, fmtDate(lastEff))
		}
		var exc *time.Time
		if req.ExceptionUntil != "" {
			u, _ := domain.ParseDate(req.ExceptionUntil)
			if !u.After(eff) || u.Sub(eff) > domain.MaxPostingException {
				return fmt.Errorf("%w: exception_until must be after the deactivation and within 90 days of it", registry.ErrInvalidInput)
			}
			exc = &u
		}
		nv, err := scanDimValue(tx.QueryRow(ctx, `WITH u AS (UPDATE dimension_values SET status = $3::text, record_version = record_version + 1 WHERE value_id = $1 AND tenant_id = $2 RETURNING *)
			SELECT u.value_id, u.tenant_id, u.type_id, $4::text, COALESCE(u.legal_entity_id::text,''), u.code, u.status, u.current_version, u.record_version, u.created_by, u.created_at FROM u`,
			valueID, tid, to, v.TypeCode))
		if err != nil {
			return err
		}
		prev, before := v.Status, v.RecordVersion
		t, err := insertDimTransition(ctx, tx, tid, valueID, lastSeq+1, &prev, to, eff, command, strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), exc, actor, &before, nv.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.DimensionOutcome{Value: *nv, Transition: t}
		rec, err := events.DimensionStatusRecord(*nv, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── source mappings ─────────────────────────────────────────────────────────

const dimMappingColumns = `mapping_id, value_id, source_domain, source_object_id, version_no, valid_from, valid_to, evidence_ref, recorded_by, recorded_at`

func scanDimMapping(row pgx.Row) (*domain.SourceMapping, error) {
	var m domain.SourceMapping
	var from time.Time
	var to *time.Time
	if err := row.Scan(&m.MappingID, &m.ValueID, &m.SourceDomain, &m.SourceObjectID, &m.VersionNo, &from, &to, &m.EvidenceRef, &m.RecordedBy, &m.RecordedAt); err != nil {
		return nil, err
	}
	m.ValidFrom, m.ValidTo = fmtDate(from), fmtDatePtr(to)
	return &m, nil
}

// MapSourceObject maps an operational source object to a value.
func (s *PgStore) MapSourceObject(ctx context.Context, valueID string, req domain.MapSourceObjectRequest, actor string) (*registry.MappingOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.MappingOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := lockDimValue(ctx, tx, tid, valueID)
		if err != nil {
			return err
		}
		typ, err := dimType(ctx, tx, tid, v.TypeCode)
		if err != nil {
			return err
		}
		if v.Status != domain.DimActive {
			return fmt.Errorf("%w: a source object maps to an ACTIVE value, and this one is %s", registry.ErrStateConflict, v.Status)
		}
		dom, obj := strings.TrimSpace(req.SourceDomain), strings.TrimSpace(req.SourceObjectID)
		if typ.SourceDomain != "" && typ.SourceDomain != dom {
			return fmt.Errorf("%w: dimension type %s is fed by %s, not %s", registry.ErrInvalidInput, typ.TypeCode, typ.SourceDomain, dom)
		}
		from := asOfDate(time.Now().UTC())
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		open, err := scanDimMapping(tx.QueryRow(ctx, `SELECT `+dimMappingColumns+` FROM dimension_source_mappings WHERE tenant_id = $1 AND type_id = $2
			AND source_domain = $3 AND source_object_id = $4 AND valid_to IS NULL FOR UPDATE`, tid, v.TypeID, dom, obj))
		var prev *domain.SourceMapping
		no := 1
		switch {
		case err == nil:
			if open.ValueID == valueID {
				out = &registry.MappingOutcome{Mapping: *open, NoOp: true}
				return nil
			}
			openFrom, _ := domain.ParseDate(open.ValidFrom)
			if !from.After(openFrom) {
				return fmt.Errorf("%w: valid_from must be after the current mapping began on %s", registry.ErrInvalidInput, open.ValidFrom)
			}
			if _, err := tx.Exec(ctx, `UPDATE dimension_source_mappings SET valid_to = $2 WHERE mapping_id = $1`, open.MappingID, from); err != nil {
				return err
			}
			open.ValidTo = fmtDate(from)
			prev, no = open, open.VersionNo+1
		case errors.Is(err, pgx.ErrNoRows):
			var maxNo int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_no),0) FROM dimension_source_mappings WHERE tenant_id = $1 AND type_id = $2 AND source_domain = $3 AND source_object_id = $4`,
				tid, v.TypeID, dom, obj).Scan(&maxNo); err != nil {
				return err
			}
			no = maxNo + 1
		default:
			return err
		}
		m, err := scanDimMapping(tx.QueryRow(ctx, `INSERT INTO dimension_source_mappings (mapping_id, tenant_id, type_id, value_id, source_domain, source_object_id, version_no,
			valid_from, evidence_ref, recorded_by, correlation_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,'')) RETURNING `+dimMappingColumns,
			uuid.NewString(), tid, v.TypeID, valueID, dom, obj, no, from, strings.TrimSpace(req.EvidenceRef), actor, req.CorrelationID))
		if err != nil {
			return err
		}
		m.TypeCode = v.TypeCode
		out = &registry.MappingOutcome{Mapping: *m, Previous: prev}
		rec, err := events.DimensionSourceMappedRecord(*v, *m, prev, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── reads ───────────────────────────────────────────────────────────────────

// GetDimensionValue returns the value's identity.
func (s *PgStore) GetDimensionValue(ctx context.Context, valueID string) (*domain.DimensionValue, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(valueID); err != nil {
		return nil, nil
	}
	var out *domain.DimensionValue
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1 AND v.tenant_id = $2`, valueID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = v
		return err
	})
	return out, err
}

// statusAsOf is a value's status on day and the controlled exception that covers it, from the recorded transitions.
func dimStatusAsOf(ctx context.Context, tx pgx.Tx, valueID string, day time.Time) (string, *time.Time, error) {
	var status string
	var exc *time.Time
	err := tx.QueryRow(ctx, `SELECT to_status, exception_until FROM dimension_value_transitions WHERE value_id = $1 AND effective_date <= $2
		ORDER BY effective_date DESC, seq DESC LIMIT 1`, valueID, day).Scan(&status, &exc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return status, exc, err
}

func dimValueViewTx(ctx context.Context, tx pgx.Tx, v *domain.DimensionValue, asOf time.Time, history bool) (*domain.DimensionValueView, error) {
	day := asOfDate(asOf)
	view := &domain.DimensionValueView{Value: *v, AsOf: fmtDate(day), Path: []domain.PathNode{}}
	var err error
	if view.Hierarchy, err = dimVersionInForce(ctx, tx, v.ValueID, day); err != nil {
		return nil, err
	}
	status, exc, err := dimStatusAsOf(ctx, tx, v.ValueID, day)
	if err != nil {
		return nil, err
	}
	view.StatusAsOf = status
	view.Eligibility = domain.EvaluateDimensionPosting(v.ValueID, day, status, exc)
	// The roll-up path, as the hierarchy stood on that date, from the root down to the parent.
	var chain []domain.PathNode
	seen := map[string]bool{v.ValueID: true}
	for cur := view.Hierarchy; cur != nil && cur.ParentValueID != "" && len(chain) < 64; {
		if seen[cur.ParentValueID] {
			break
		}
		seen[cur.ParentValueID] = true
		pv, err := dimVersionInForce(ctx, tx, cur.ParentValueID, day)
		if err != nil {
			return nil, err
		}
		if pv == nil {
			break
		}
		chain = append(chain, domain.PathNode{ValueID: cur.ParentValueID, Code: cur.ParentCode, Label: pv.Label})
		cur = pv
	}
	for i := len(chain) - 1; i >= 0; i-- {
		view.Path = append(view.Path, chain[i])
	}
	if p, err := scanDimVersion(tx.QueryRow(ctx, dimVersionSelect+` WHERE ver.value_id = $1 AND ver.status = 'PENDING'`, v.ValueID)); err == nil {
		view.Pending = p
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if history {
		rows, err := tx.Query(ctx, dimVersionSelect+` WHERE ver.value_id = $1 ORDER BY ver.version_no`, v.ValueID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			h, err := scanDimVersion(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			view.History = append(view.History, *h)
		}
		rows.Close()
		trows, err := tx.Query(ctx, `SELECT `+dimTransitionColumns+` FROM dimension_value_transitions WHERE value_id = $1 ORDER BY seq`, v.ValueID)
		if err != nil {
			return nil, err
		}
		defer trows.Close()
		for trows.Next() {
			t, err := scanDimTransition(trows)
			if err != nil {
				return nil, err
			}
			view.Status = append(view.Status, *t)
		}
		if err := trows.Err(); err != nil {
			return nil, err
		}
	}
	return view, nil
}

// GetDimensionValueView returns the value as it was on asOf.
func (s *PgStore) GetDimensionValueView(ctx context.Context, valueID string, asOf time.Time, history bool) (*domain.DimensionValueView, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(valueID); err != nil {
		return nil, nil
	}
	var out *domain.DimensionValueView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		v, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1 AND v.tenant_id = $2`, valueID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = dimValueViewTx(ctx, tx, v, asOf, history)
		return err
	})
	return out, err
}

type dimNode struct {
	domain.HierarchyNode
	exception *time.Time
}

// dimNodesAsOf lists a type's values that existed on day, with the label, parent and status they had then.
func dimNodesAsOf(ctx context.Context, tx pgx.Tx, tid, typeCode, legalEntityID string, day time.Time) ([]dimNode, error) {
	typ, err := dimType(ctx, tx, tid, typeCode)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT v.value_id::text, v.code, COALESCE(v.legal_entity_id::text,''), ver.label, COALESCE(ver.parent_value_id::text,''),
		COALESCE((SELECT t.to_status FROM dimension_value_transitions t WHERE t.value_id = v.value_id AND t.effective_date <= $3 ORDER BY t.effective_date DESC, t.seq DESC LIMIT 1), ''),
		(SELECT t.exception_until FROM dimension_value_transitions t WHERE t.value_id = v.value_id AND t.effective_date <= $3 ORDER BY t.effective_date DESC, t.seq DESC LIMIT 1)
		FROM dimension_values v JOIN dimension_value_versions ver ON ver.tenant_id = v.tenant_id AND ver.value_id = v.value_id AND ver.status = 'APPROVED'
		  AND ver.valid_from <= $3 AND (ver.valid_to IS NULL OR ver.valid_to > $3)
		WHERE v.tenant_id = $1 AND v.type_id = $2 AND ($4::text = '' OR v.legal_entity_id IS NULL OR v.legal_entity_id::text = $4::text)
		ORDER BY v.code, v.value_id`, tid, typ.TypeID, day, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dimNode
	for rows.Next() {
		var n dimNode
		if err := rows.Scan(&n.ValueID, &n.Code, &n.LegalEntityID, &n.Label, &n.ParentValueID, &n.Status, &n.exception); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetHierarchyAsOf returns a type's hierarchy as it stood on asOf.
func (s *PgStore) GetHierarchyAsOf(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) (*domain.HierarchyAsOf, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.HierarchyAsOf
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if legalEntityID != "" {
			if err := entityExists(ctx, tx, tid, legalEntityID); err != nil {
				return err
			}
		}
		day := asOfDate(asOf)
		nodes, err := dimNodesAsOf(ctx, tx, tid, typeCode, legalEntityID, day)
		if err != nil {
			return err
		}
		parents := map[string]string{}
		for _, n := range nodes {
			if n.ParentValueID != "" {
				parents[n.ValueID] = n.ParentValueID
			}
		}
		res := &domain.HierarchyAsOf{TypeCode: typeCode, AsOf: fmtDate(day), Nodes: make([]domain.HierarchyNode, 0, len(nodes))}
		for _, n := range nodes {
			n.Depth = domain.HierarchyDepth(parents, n.ValueID)
			res.Nodes = append(res.Nodes, n.HierarchyNode)
		}
		sort.SliceStable(res.Nodes, func(i, j int) bool { return res.Nodes[i].Depth < res.Nodes[j].Depth })
		out = res
		return nil
	})
	return out, err
}

// ListPostingEligibleValues lists the values that could take a new posting on asOf.
func (s *PgStore) ListPostingEligibleValues(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) ([]registry.EligibleDimensionValue, error) {
	tid := domain.TenantFromContext(ctx)
	out := []registry.EligibleDimensionValue{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if legalEntityID != "" {
			if err := entityExists(ctx, tx, tid, legalEntityID); err != nil {
				return err
			}
		}
		day := asOfDate(asOf)
		nodes, err := dimNodesAsOf(ctx, tx, tid, typeCode, legalEntityID, day)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			el := domain.EvaluateDimensionPosting(n.ValueID, day, n.Status, n.exception)
			if el.Eligible {
				out = append(out, registry.EligibleDimensionValue{ValueID: n.ValueID, Code: n.Code, Label: n.Label, LegalEntityID: n.LegalEntityID, Exception: el.Exception})
			}
		}
		return nil
	})
	return out, err
}

// ResolveDimension finds a value by code or by operational source object.
func (s *PgStore) ResolveDimension(ctx context.Context, q domain.ResolveDimensionQuery) (*domain.DimensionResolution, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.DimensionResolution
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		typ, err := dimType(ctx, tx, tid, q.TypeCode)
		if err != nil {
			return err
		}
		day := asOfDate(q.AsOf)
		var v *domain.DimensionValue
		var mapping *domain.SourceMapping
		via := "CODE"
		if q.Code != "" {
			// The caller's entity's own value wins; otherwise the tenant-wide one. A code that exists only in another
			// entity is not found, never guessed.
			scopes := []string{""}
			if q.LegalEntityID != "" {
				scopes = []string{q.LegalEntityID, ""}
			}
			for _, entity := range scopes {
				cand, err := scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.tenant_id = $1 AND v.type_id = $2 AND v.code = $3
					AND ((v.legal_entity_id IS NULL AND $4::text = '') OR (v.legal_entity_id::text = $4::text AND $4::text <> ''))`, tid, typ.TypeID, q.Code, entity))
				if err == nil {
					v = cand
					break
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
		} else {
			via = "SOURCE_MAPPING"
			m, err := scanDimMapping(tx.QueryRow(ctx, `SELECT `+dimMappingColumns+` FROM dimension_source_mappings WHERE tenant_id = $1 AND type_id = $2
				AND source_domain = $3 AND source_object_id = $4 AND valid_from <= $5 AND (valid_to IS NULL OR valid_to > $5) ORDER BY version_no DESC LIMIT 1`,
				tid, typ.TypeID, strings.TrimSpace(q.SourceDomain), strings.TrimSpace(q.SourceObjectID), day))
			if err == nil {
				m.TypeCode = typ.TypeCode
				mapping = m
				v, err = scanDimValue(tx.QueryRow(ctx, dimValueSelect+` WHERE v.value_id = $1`, m.ValueID))
				if err != nil {
					return err
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if v == nil {
			return registry.ErrNotFound
		}
		view, err := dimValueViewTx(ctx, tx, v, day, false)
		if err != nil {
			return err
		}
		out = &domain.DimensionResolution{Via: via, View: view, Mapping: mapping}
		return nil
	})
	return out, err
}
