package store_test

// REF-08 financial dimensions against a real Postgres: stable codes, effective-dated hierarchy versions kept apart from
// identity, loops refused, independent approval of controlled hierarchies, posting eligibility reconstructed by date with a
// controlled exception, versioned source mappings, and what the database holds whatever the service does.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	dimSteward  = "p-dim-steward"
	dimReviewer = "p-dim-reviewer"
)

func dayFromNow(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }

func (f *orgFixture) dimType(t *testing.T, code string, hierarchical, controlled bool, source string) {
	t.Helper()
	_, err := f.s.CreateDimensionType(f.ctx, domain.CreateDimensionTypeRequest{TypeCode: code, Name: code, Hierarchical: hierarchical, Controlled: controlled, SourceDomain: source}, dimSteward)
	require.NoError(t, err)
}

func (f *orgFixture) dimValue(t *testing.T, typ, code, parent, entity string) *domain.DimensionValue {
	t.Helper()
	out, err := f.s.CreateDimensionValue(f.ctx, domain.CreateDimensionValueRequest{TypeCode: typ, LegalEntityID: entity, Code: code, Label: code + " label",
		ParentValueID: parent, ValidFrom: "2025-01-01", EvidenceRef: "setup"}, dimSteward)
	require.NoError(t, err)
	return &out.Value
}

func (f *orgFixture) activateDim(t *testing.T, id string, version int64, eff string) *domain.DimensionValue {
	t.Helper()
	out, err := f.s.ChangeDimensionStatus(f.ctx, id, "ActivateDimensionValue", domain.DimActive, domain.DimensionStatusRequest{ExpectedVersion: version, EvidenceRef: "e", EffectiveDate: eff}, dimSteward, false)
	require.NoError(t, err)
	return &out.Value
}

func (f *orgFixture) dimView(t *testing.T, id string, day time.Time) *domain.DimensionValueView {
	t.Helper()
	v, err := f.s.GetDimensionValueView(f.ctx, id, day, true)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

func reparent(parent string, label, from string, version int64) domain.ReparentDimensionValueRequest {
	r := domain.ReparentDimensionValueRequest{ExpectedVersion: version, NewLabel: label, ValidFrom: from, Reason: "reorganisation", EvidenceRef: "memo"}
	if parent == "" {
		r.Root = true
	} else {
		r.ParentValueID = &parent
	}
	return r
}

func TestDimension_TypesAreUniqueAndControlNeedsAHierarchy(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "COST_CENTER", true, true, "")
	_, err := f.s.CreateDimensionType(f.ctx, domain.CreateDimensionTypeRequest{TypeCode: "COST_CENTER", Name: "dup", Hierarchical: true}, dimSteward)
	require.ErrorIs(t, err, registry.ErrConflict)
	require.Error(t, f.execTenant(t, `INSERT INTO dimension_types (type_id, tenant_id, type_code, name, hierarchical, controlled, created_by) VALUES (gen_random_uuid(), $1, 'FLAT', 'n', false, true, 'x')`, f.tenantID),
		"a controlled hierarchy needs a hierarchical type")
	require.Error(t, f.execTenant(t, `UPDATE dimension_types SET name = 'renamed' WHERE type_code = 'COST_CENTER'`), "a type is immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM dimension_types WHERE type_code = 'COST_CENTER'`))
	list, err := f.s.ListDimensionTypes(f.ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.type.created' AND tenant_id = $1`, f.tenantID))
}

func TestDimension_CodesAreStableWithinTheirScope(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "REGION", true, false, "")
	f.dimType(t, "FLAT", false, false, "")
	second := newEntity(f, domain.EntityStatusActive)
	second.LegalEntityID = uuid.NewString()
	require.NoError(t, f.s.CreateEntity(f.ctx, second))

	world := f.dimValue(t, "REGION", "WORLD", "", "")
	assert.Equal(t, domain.DimDraft, world.Status)
	assert.Equal(t, 1, world.CurrentVersion)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.value.created' AND tenant_id = $1`, f.tenantID))

	mk := func(typ, code, parent, entity string) error {
		_, err := f.s.CreateDimensionValue(f.ctx, domain.CreateDimensionValueRequest{TypeCode: typ, LegalEntityID: entity, Code: code, Label: "l", ParentValueID: parent, EvidenceRef: "e"}, dimSteward)
		return err
	}
	require.ErrorIs(t, mk("REGION", "WORLD", "", ""), registry.ErrConflict, "unique per type, tenant-wide")
	require.ErrorIs(t, mk("REGION", "WORLD", "", f.entityID), registry.ErrConflict, "an entity's code never shadows a tenant-wide one")
	require.NoError(t, mk("REGION", "UK", "", f.entityID))
	require.ErrorIs(t, mk("REGION", "UK", "", f.entityID), registry.ErrConflict, "unique within the entity")
	require.NoError(t, mk("REGION", "UK", "", second.LegalEntityID), "another entity has its own namespace")
	require.ErrorIs(t, mk("REGION", "UK", "", ""), registry.ErrConflict, "a tenant-wide code never covers an entity's")
	require.NoError(t, mk("FLAT", "WORLD", "", ""), "another type has its own codes")

	require.ErrorIs(t, mk("NOPE", "X", "", ""), registry.ErrInvalidInput, "an unknown type")
	require.ErrorIs(t, mk("REGION", "X", "", uuid.NewString()), registry.ErrNotFound, "an unknown entity")
	require.ErrorIs(t, mk("FLAT", "CHILD", world.ValueID, ""), registry.ErrInvalidInput, "a type with no hierarchy takes no parent")
	require.ErrorIs(t, mk("REGION", "CHILD", uuid.NewString(), ""), registry.ErrInvalidInput, "a parent must exist")
	flat := f.dimValue(t, "FLAT", "OTHER", "", "")
	require.ErrorIs(t, mk("REGION", "CHILD", flat.ValueID, ""), registry.ErrInvalidInput, "a parent is of the same type")
	uk := f.valueID(t, "UK", f.entityID)
	require.ErrorIs(t, mk("REGION", "UKCHILD", uk, second.LegalEntityID), registry.ErrInvalidInput, "another entity's value is not a parent")
	require.NoError(t, mk("REGION", "UKCHILD", uk, f.entityID), "an entity's own value is")
	require.NoError(t, mk("REGION", "UNDERWORLD", world.ValueID, f.entityID), "a tenant-wide parent is shared")

	require.Error(t, f.execTenant(t, `UPDATE dimension_values SET code = 'CHANGED' WHERE value_id = $1`, world.ValueID), "a code never changes")
	require.Error(t, f.execTenant(t, `UPDATE dimension_values SET legal_entity_id = $2 WHERE value_id = $1`, world.ValueID, f.entityID))
	require.Error(t, f.execTenant(t, `DELETE FROM dimension_values WHERE value_id = $1`, world.ValueID))
	require.Error(t, f.execTenant(t, `INSERT INTO dimension_values (value_id, tenant_id, type_id, code, created_by) SELECT gen_random_uuid(), tenant_id, type_id, 'lower', 'x' FROM dimension_types WHERE type_code = 'REGION'`), "code shape")
}

func (f *orgFixture) valueID(t *testing.T, code, entity string) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT value_id::text FROM dimension_values WHERE tenant_id = $1 AND code = $2 AND COALESCE(legal_entity_id::text,'') = $3`, f.tenantID, code, entity).Scan(&id))
	return id
}

func TestDimension_InactiveRejectsNewPostingsAndHistoryIsReconstructable(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "PROJECT_CC", false, false, "")
	v := f.dimValue(t, "PROJECT_CC", "P100", "", "")
	id := v.ValueID
	change := func(to, command string, version int64, eff, until string) (*registry.DimensionOutcome, error) {
		return f.s.ChangeDimensionStatus(f.ctx, id, command, to, domain.DimensionStatusRequest{ExpectedVersion: version, Reason: "r", EvidenceRef: "e", EffectiveDate: eff, ExceptionUntil: until}, dimSteward, false)
	}
	_, err := change(domain.DimInactive, "DeactivateDimensionValue", 1, "2025-02-01", "")
	require.ErrorIs(t, err, registry.ErrStateConflict, "a draft is activated, not deactivated")
	_, err = change(domain.DimActive, "ActivateDimensionValue", 9, "2025-02-01", "")
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ChangeDimensionStatus(f.ctx, id, "ActivateDimensionValue", domain.DimActive, domain.DimensionStatusRequest{EvidenceRef: "e"}, dimSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)

	a, err := change(domain.DimActive, "ActivateDimensionValue", 1, "2025-02-01", "")
	require.NoError(t, err)
	_, err = change(domain.DimInactive, "DeactivateDimensionValue", a.Value.RecordVersion, "2025-01-15", "")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "not before the last change")
	d, err := change(domain.DimInactive, "DeactivateDimensionValue", a.Value.RecordVersion, "2025-06-01", "")
	require.NoError(t, err)
	r, err := change(domain.DimActive, "ActivateDimensionValue", d.Value.RecordVersion, "2025-09-01", "")
	require.NoError(t, err)
	_, err = change(domain.DimInactive, "DeactivateDimensionValue", r.Value.RecordVersion, "2025-12-01", "2026-05-01")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a controlled exception is bounded to 90 days")
	d2, err := change(domain.DimInactive, "DeactivateDimensionValue", r.Value.RecordVersion, "2025-12-01", "2025-12-15")
	require.NoError(t, err, "an explicit, dated, controlled exception")

	for name, tc := range map[string]struct {
		date      string
		status    string
		eligible  bool
		exception bool
		reason    string
	}{
		"before it existed":   {"2024-12-01", "", false, false, "NOT_YET_EFFECTIVE"},
		"a draft":             {"2025-01-15", domain.DimDraft, false, false, "NOT_ACTIVE"},
		"active":              {"2025-03-01", domain.DimActive, true, false, ""},
		"inactive":            {"2025-07-01", domain.DimInactive, false, false, "INACTIVE"},
		"active again":        {"2025-10-01", domain.DimActive, true, false, ""},
		"inside exception":    {"2025-12-10", domain.DimInactive, true, true, ""},
		"its last day":        {"2025-12-15", domain.DimInactive, true, true, ""},
		"after the exception": {"2025-12-16", domain.DimInactive, false, false, "INACTIVE"},
	} {
		day, _ := domain.ParseDate(tc.date)
		view := f.dimView(t, id, day)
		assert.Equal(t, tc.status, view.StatusAsOf, name)
		assert.Equal(t, tc.eligible, view.Eligibility.Eligible, name)
		assert.Equal(t, tc.exception, view.Eligibility.Exception, name)
		assert.Equal(t, tc.reason, view.Eligibility.Reason, name)
	}
	now := f.dimView(t, id, time.Now().UTC())
	assert.False(t, now.Eligibility.Eligible, "inactive today: new postings are rejected")
	require.NotNil(t, now.Hierarchy, "but it remains valid for history")
	assert.Equal(t, "P100 label", now.Hierarchy.Label)
	require.Len(t, now.Status, 5)
	assert.Equal(t, "2025-12-15", now.Status[4].ExceptionUntil)
	_ = d2

	// The eligible list is by date too.
	elig := func(date string) []registry.EligibleDimensionValue {
		day, _ := domain.ParseDate(date)
		out, err := f.s.ListPostingEligibleValues(f.ctx, "PROJECT_CC", "", day)
		require.NoError(t, err)
		return out
	}
	assert.Len(t, elig("2025-03-01"), 1)
	assert.Empty(t, elig("2025-07-01"))
	exc := elig("2025-12-10")
	require.Len(t, exc, 1)
	assert.True(t, exc[0].Exception)
	assert.Empty(t, elig("2026-01-01"))
	for name, n := range map[string]int{"dimension.value.activated": 2, "dimension.value.deactivated": 2} {
		assert.Equal(t, n, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = $2 AND tenant_id = $1`, f.tenantID, name), name)
	}
	require.Error(t, f.execTenant(t, `UPDATE dimension_value_transitions SET reason = 'x' WHERE value_id = $1`, id), "transitions are append-only")
	require.Error(t, f.execTenant(t, `INSERT INTO dimension_value_transitions (transition_id, tenant_id, value_id, seq, to_status, effective_date, command, evidence_ref, exception_until, actor, version_after)
		VALUES (gen_random_uuid(), $1, $2, 99, 'ACTIVE', '2025-01-01', 'X', 'e', '2026-01-01', 'x', 1)`, f.tenantID, id), "an exception belongs to a deactivation")
}

func TestDimension_TheHierarchyIsEffectiveDatedApartFromIdentityAndNeverLoops(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "REGION", true, false, "")
	a := f.dimValue(t, "REGION", "A", "", "")
	b := f.dimValue(t, "REGION", "B", a.ValueID, "")
	c := f.dimValue(t, "REGION", "C", b.ValueID, "")
	today := time.Now().UTC()
	later := today.AddDate(0, 0, 10)

	// Moving B to the root, and renaming it, from a future date.
	out, err := f.s.ProposeHierarchyChange(f.ctx, b.ValueID, reparent("", "B renamed", dayFromNow(10), 1), dimSteward, false)
	require.NoError(t, err)
	assert.Equal(t, domain.HierarchyApproved, out.Hierarchy.Status, "a type that is not controlled takes effect directly")
	assert.Equal(t, 2, out.Value.CurrentVersion)
	assert.Equal(t, b.ValueID, out.Value.ValueID, "identity is unchanged")
	assert.Equal(t, "B", out.Value.Code, "so is the code")

	before := f.dimView(t, c.ValueID, today)
	require.Len(t, before.Path, 2)
	assert.Equal(t, "A", before.Path[0].Code, "the roll-up as it stood: root first")
	assert.Equal(t, "B", before.Path[1].Code)
	assert.Equal(t, "B label", before.Path[1].Label, "and the label it had then")
	after := f.dimView(t, c.ValueID, later.AddDate(0, 0, 1))
	require.Len(t, after.Path, 1)
	assert.Equal(t, "B renamed", after.Path[0].Label, "the new label from its date")
	bView := f.dimView(t, b.ValueID, today)
	assert.Equal(t, "A", bView.Hierarchy.ParentCode)
	assert.Equal(t, dayFromNow(10), bView.Hierarchy.ValidTo)
	assert.Len(t, bView.History, 2)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.hierarchy.changed' AND tenant_id = $1`, f.tenantID))

	tree := func(day time.Time) map[string]domain.HierarchyNode {
		h, err := f.s.GetHierarchyAsOf(f.ctx, "REGION", "", day)
		require.NoError(t, err)
		m := map[string]domain.HierarchyNode{}
		for _, n := range h.Nodes {
			m[n.Code] = n
		}
		return m
	}
	now := tree(today)
	assert.Equal(t, 0, now["A"].Depth)
	assert.Equal(t, 1, now["B"].Depth)
	assert.Equal(t, 2, now["C"].Depth)
	assert.Equal(t, a.ValueID, now["B"].ParentValueID)
	moved := tree(later.AddDate(0, 0, 1))
	assert.Equal(t, 0, moved["B"].Depth, "historical roll-up is reconstructable on either side of the change")
	assert.Equal(t, 1, moved["C"].Depth)
	assert.Equal(t, "B renamed", moved["B"].Label)

	// Loops are refused, now and across later changes.
	_, err = f.s.ProposeHierarchyChange(f.ctx, a.ValueID, reparent(c.ValueID, "", dayFromNow(1), 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "A under its own grandchild")
	_, err = f.s.ProposeHierarchyChange(f.ctx, a.ValueID, reparent(a.ValueID, "", dayFromNow(1), 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a value is not its own parent")

	x := f.dimValue(t, "REGION", "X", "", "")
	y := f.dimValue(t, "REGION", "Y", "", "")
	_, err = f.s.ProposeHierarchyChange(f.ctx, x.ValueID, reparent(y.ValueID, "", dayFromNow(20), 1), dimSteward, false)
	require.NoError(t, err)
	_, err = f.s.ProposeHierarchyChange(f.ctx, y.ValueID, reparent(x.ValueID, "", dayFromNow(10), 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "fine on its own date, a loop once X moves under Y on the 20th")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM dimension_value_versions WHERE value_id = $1`, y.ValueID), "a refused change records nothing")

	// Prospective, and a change that changes nothing is refused.
	_, err = f.s.ProposeHierarchyChange(f.ctx, c.ValueID, reparent(a.ValueID, "", dayFromNow(-1), 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a hierarchy change never reaches back")
	_, err = f.s.ProposeHierarchyChange(f.ctx, c.ValueID, reparent(b.ValueID, "", "", 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "nothing changes")
	_, err = f.s.ProposeHierarchyChange(f.ctx, c.ValueID, reparent(a.ValueID, "", "", 9), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ProposeHierarchyChange(f.ctx, c.ValueID, reparent(uuid.NewString(), "", "", 1), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	// A rename alone is a version; the identity is the same.
	rn, err := f.s.ProposeHierarchyChange(f.ctx, c.ValueID, domain.ReparentDimensionValueRequest{ExpectedVersion: 1, ParentValueID: &b.ValueID, NewLabel: "C, renamed", ValidFrom: dayFromNow(2), Reason: "r", EvidenceRef: "e"}, dimSteward, false)
	require.NoError(t, err)
	assert.Equal(t, "C", rn.Value.Code)
	assert.Equal(t, "C, renamed", rn.Hierarchy.Label)

	require.Error(t, f.execTenant(t, `UPDATE dimension_value_versions SET label = 'x' WHERE value_id = $1 AND version_no = 1`, b.ValueID), "a version is immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM dimension_value_versions WHERE value_id = $1`, b.ValueID))
	require.Error(t, f.execTenant(t, `INSERT INTO dimension_value_versions (version_id, tenant_id, value_id, version_no, parent_value_id, label, status, valid_from, evidence_ref, proposed_by, approved_by, approved_at)
		VALUES (gen_random_uuid(), $1, $2, 50, $2, 'l', 'APPROVED', '2030-01-01', 'e', 'x', 'y', now())`, f.tenantID, b.ValueID), "its own parent")
}

func TestDimension_AControlledHierarchyChangeNeedsIndependentApproval(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "COST_CENTER", true, true, "")
	root := f.dimValue(t, "COST_CENTER", "ROOT", "", "")
	cc := f.dimValue(t, "COST_CENTER", "CC1", root.ValueID, "")
	other := f.dimValue(t, "COST_CENTER", "ROOT2", "", "")
	today := time.Now().UTC()

	p, err := f.s.ProposeHierarchyChange(f.ctx, cc.ValueID, reparent(other.ValueID, "", dayFromNow(5), 1), dimSteward, false)
	require.NoError(t, err)
	assert.Equal(t, domain.HierarchyPending, p.Hierarchy.Status)
	assert.Equal(t, 1, p.Value.CurrentVersion, "nothing is in force until it is approved")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.hierarchy.proposed' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.hierarchy.changed' AND tenant_id = $1`, f.tenantID))

	v := f.dimView(t, cc.ValueID, today.AddDate(0, 0, 6))
	assert.Equal(t, "ROOT", v.Hierarchy.ParentCode, "a pending change does not affect the roll-up")
	require.NotNil(t, v.Pending)
	assert.Equal(t, 2, v.Pending.VersionNo)

	_, err = f.s.ProposeHierarchyChange(f.ctx, cc.ValueID, reparent(root.ValueID, "x", dayFromNow(6), p.Value.RecordVersion), dimSteward, false)
	require.ErrorIs(t, err, registry.ErrConflict, "one pending change at a time")
	ver := p.Value.RecordVersion
	_, err = f.s.ApproveHierarchyChange(f.ctx, cc.ValueID, 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: ver, EvidenceRef: "e"}, dimSteward, false)
	require.ErrorIs(t, err, registry.ErrSelfApproval, "the proposer cannot approve")
	_, err = f.s.ApproveHierarchyChange(f.ctx, cc.ValueID, 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: 99, EvidenceRef: "e"}, dimReviewer, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ApproveHierarchyChange(f.ctx, cc.ValueID, 7, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: ver, EvidenceRef: "e"}, dimReviewer, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.ApproveHierarchyChange(f.ctx, cc.ValueID, 1, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: ver, EvidenceRef: "e"}, dimReviewer, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "version 1 is already approved")
	require.Error(t, f.execTenant(t, `UPDATE dimension_value_versions SET status = 'APPROVED', approved_by = proposed_by, approved_at = now() WHERE value_id = $1 AND version_no = 2`, cc.ValueID),
		"the table holds the independence rule even if the service is bypassed")

	ok, err := f.s.ApproveHierarchyChange(f.ctx, cc.ValueID, 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: ver, EvidenceRef: "reviewed"}, dimReviewer, false)
	require.NoError(t, err)
	assert.Equal(t, domain.HierarchyApproved, ok.Hierarchy.Status)
	assert.Equal(t, dimReviewer, ok.Hierarchy.ApprovedBy)
	assert.Equal(t, dimSteward, ok.Hierarchy.ProposedBy)
	assert.Equal(t, 2, ok.Value.CurrentVersion)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.hierarchy.changed' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, "ROOT", f.dimView(t, cc.ValueID, today.AddDate(0, 0, 2)).Hierarchy.ParentCode, "the old parent until the change date")
	assert.Equal(t, "ROOT2", f.dimView(t, cc.ValueID, today.AddDate(0, 0, 6)).Hierarchy.ParentCode)

	// The first version of a new node is its placement: no review is needed for it.
	require.NoError(t, func() error {
		_, err := f.s.CreateDimensionValue(f.ctx, domain.CreateDimensionValueRequest{TypeCode: "COST_CENTER", Code: "NEWCC", Label: "n", ParentValueID: root.ValueID, EvidenceRef: "e"}, dimSteward)
		return err
	}())

	// Approving re-checks for loops: the hierarchy may have changed since the proposal.
	a := f.dimValue(t, "COST_CENTER", "LA", "", "")
	b := f.dimValue(t, "COST_CENTER", "LB", "", "")
	pa, err := f.s.ProposeHierarchyChange(f.ctx, a.ValueID, reparent(b.ValueID, "", dayFromNow(15), 1), dimSteward, false)
	require.NoError(t, err)
	pb, err := f.s.ProposeHierarchyChange(f.ctx, b.ValueID, reparent(a.ValueID, "", dayFromNow(15), 1), dimSteward, false)
	require.NoError(t, err, "each is fine on its own")
	_, err = f.s.ApproveHierarchyChange(f.ctx, a.ValueID, 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: pa.Value.RecordVersion, EvidenceRef: "e"}, dimReviewer, false)
	require.NoError(t, err)
	_, err = f.s.ApproveHierarchyChange(f.ctx, b.ValueID, 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: pb.Value.RecordVersion, EvidenceRef: "e"}, dimReviewer, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the second would close a loop with the first, so approval refuses it")
}

func TestDimension_SourceMappingsAreVersioned(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "PROJECT", false, false, "PROJECT")
	f.dimType(t, "PLAIN", false, false, "")
	p1 := f.dimValue(t, "PROJECT", "P-ONE", "", "")
	p2 := f.dimValue(t, "PROJECT", "P-TWO", "", "")
	f.activateDim(t, p1.ValueID, 1, "2025-02-01")
	f.activateDim(t, p2.ValueID, 1, "2025-02-01")
	req := func(obj, from string) domain.MapSourceObjectRequest {
		return domain.MapSourceObjectRequest{SourceDomain: "PROJECT", SourceObjectID: obj, ValidFrom: from, EvidenceRef: "mapping-sheet"}
	}

	m1, err := f.s.MapSourceObject(f.ctx, p1.ValueID, req("PRJ-9", "2025-03-01"), dimSteward)
	require.NoError(t, err)
	assert.Equal(t, 1, m1.Mapping.VersionNo)
	assert.Nil(t, m1.Previous)
	again, err := f.s.MapSourceObject(f.ctx, p1.ValueID, req("PRJ-9", "2025-04-01"), dimSteward)
	require.NoError(t, err)
	assert.True(t, again.NoOp, "mapping it to the same value again changes nothing")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM dimension_source_mappings WHERE tenant_id = $1`, f.tenantID))

	m2, err := f.s.MapSourceObject(f.ctx, p2.ValueID, req("PRJ-9", "2025-08-01"), dimSteward)
	require.NoError(t, err)
	assert.Equal(t, 2, m2.Mapping.VersionNo)
	require.NotNil(t, m2.Previous)
	assert.Equal(t, "2025-08-01", m2.Previous.ValidTo, "the previous mapping ends where this one begins")
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM dimension_source_mappings WHERE tenant_id = $1`, f.tenantID), "both versions are kept")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'dimension.source.mapped' AND tenant_id = $1 AND payload->'payload'->>'previous_value_id' = $2`, f.tenantID, p1.ValueID))

	resolve := func(date string) (*domain.DimensionResolution, error) {
		d, _ := domain.ParseDate(date)
		return f.s.ResolveDimension(f.ctx, domain.ResolveDimensionQuery{TypeCode: "PROJECT", SourceDomain: "PROJECT", SourceObjectID: "PRJ-9", AsOf: d})
	}
	r1, err := resolve("2025-05-01")
	require.NoError(t, err)
	assert.Equal(t, "P-ONE", r1.View.Value.Code, "the mapping in force then")
	assert.Equal(t, "SOURCE_MAPPING", r1.Via)
	assert.Equal(t, 1, r1.Mapping.VersionNo)
	r2, err := resolve("2025-09-01")
	require.NoError(t, err)
	assert.Equal(t, "P-TWO", r2.View.Value.Code)
	assert.Equal(t, 2, r2.Mapping.VersionNo)
	_, err = resolve("2025-02-15")
	require.ErrorIs(t, err, registry.ErrNotFound, "before it was mapped there is no answer, not a guess")

	_, err = f.s.MapSourceObject(f.ctx, p1.ValueID, req("PRJ-9", "2025-07-01"), dimSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a mapping starts after the current one")
	bad := req("PRJ-10", "")
	bad.SourceDomain = "LOCATION"
	_, err = f.s.MapSourceObject(f.ctx, p1.ValueID, bad, dimSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the type is fed by the PROJECT domain")
	draft := f.dimValue(t, "PROJECT", "P-DRAFT", "", "")
	_, err = f.s.MapSourceObject(f.ctx, draft.ValueID, req("PRJ-11", ""), dimSteward)
	require.ErrorIs(t, err, registry.ErrStateConflict, "a source object maps to an ACTIVE value")
	plain := f.dimValue(t, "PLAIN", "ONE", "", "")
	f.activateDim(t, plain.ValueID, 1, "2025-02-01")
	_, err = f.s.MapSourceObject(f.ctx, plain.ValueID, domain.MapSourceObjectRequest{SourceDomain: "ANYTHING", SourceObjectID: "X", EvidenceRef: "e"}, dimSteward)
	require.NoError(t, err, "a type with no fixed source domain takes any")

	require.Error(t, f.execTenant(t, `DELETE FROM dimension_source_mappings WHERE tenant_id = $1`, f.tenantID))
	require.Error(t, f.execTenant(t, `UPDATE dimension_source_mappings SET value_id = $2 WHERE tenant_id = $1 AND version_no = 1 AND source_object_id = 'PRJ-9'`, f.tenantID, p2.ValueID))
	require.Error(t, f.execTenant(t, `INSERT INTO dimension_source_mappings (mapping_id, tenant_id, type_id, value_id, source_domain, source_object_id, version_no, valid_from, evidence_ref, recorded_by)
		SELECT gen_random_uuid(), $1, type_id, value_id, 'PROJECT', 'PRJ-9', 3, '2026-01-01', 'e', 'x' FROM dimension_values WHERE code = 'P-ONE'`, f.tenantID), "one open mapping per source object")
}

func TestDimension_ResolveByCodeAndTheScopeRules(t *testing.T) {
	f := newORGFixture(t)
	f.dimType(t, "REGION", true, false, "")
	second := newEntity(f, domain.EntityStatusActive)
	second.LegalEntityID = uuid.NewString()
	require.NoError(t, f.s.CreateEntity(f.ctx, second))
	wide := f.dimValue(t, "REGION", "WIDE", "", "")
	own := f.dimValue(t, "REGION", "OWN", "", f.entityID)
	f.activateDim(t, wide.ValueID, 1, "2025-02-01")
	today := time.Now().UTC()
	by := func(code, entity string) (*domain.DimensionResolution, error) {
		return f.s.ResolveDimension(f.ctx, domain.ResolveDimensionQuery{TypeCode: "REGION", Code: code, LegalEntityID: entity, AsOf: today})
	}
	r, err := by("WIDE", "")
	require.NoError(t, err)
	assert.Equal(t, "CODE", r.Via)
	assert.Equal(t, wide.ValueID, r.View.Value.ValueID)
	assert.True(t, r.View.Eligibility.Eligible, "resolution says whether it can take a posting")
	r, err = by("WIDE", f.entityID)
	require.NoError(t, err)
	assert.Equal(t, wide.ValueID, r.View.Value.ValueID, "a tenant-wide value serves every entity")
	r, err = by("OWN", f.entityID)
	require.NoError(t, err)
	assert.Equal(t, own.ValueID, r.View.Value.ValueID)
	assert.False(t, r.View.Eligibility.Eligible, "a draft cannot take postings")
	_, err = by("OWN", "")
	require.ErrorIs(t, err, registry.ErrNotFound, "an entity's own value is not found without the entity")
	_, err = by("OWN", second.LegalEntityID)
	require.ErrorIs(t, err, registry.ErrNotFound, "nor by another entity")
	_, err = by("MISSING", "")
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.ResolveDimension(f.ctx, domain.ResolveDimensionQuery{TypeCode: "NOPE", Code: "X", AsOf: today})
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	// A value created with a future start is not in the hierarchy yet.
	future, err := f.s.CreateDimensionValue(f.ctx, domain.CreateDimensionValueRequest{TypeCode: "REGION", Code: "SOON", Label: "s", ValidFrom: dayFromNow(30), EvidenceRef: "e"}, dimSteward)
	require.NoError(t, err)
	h, err := f.s.GetHierarchyAsOf(f.ctx, "REGION", "", today)
	require.NoError(t, err)
	for _, n := range h.Nodes {
		assert.NotEqual(t, future.Value.ValueID, n.ValueID)
	}
	forEntity, err := f.s.GetHierarchyAsOf(f.ctx, "REGION", second.LegalEntityID, today)
	require.NoError(t, err)
	var codes []string
	for _, n := range forEntity.Nodes {
		codes = append(codes, n.Code)
	}
	assert.Equal(t, []string{"WIDE"}, codes, "another entity sees the tenant-wide values and not this entity's own")
	_, err = f.s.GetHierarchyAsOf(f.ctx, "REGION", uuid.NewString(), today)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestDimension_TenantsNeverSeeEachOther(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	a.dimType(t, "REGION", true, false, "")
	v := a.dimValue(t, "REGION", "A", "", "")

	got, err := b.s.GetDimensionValue(b.ctx, v.ValueID)
	require.NoError(t, err)
	assert.Nil(t, got)
	view, err := b.s.GetDimensionValueView(b.ctx, v.ValueID, time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, view)
	_, err = b.s.ChangeDimensionStatus(b.ctx, v.ValueID, "ActivateDimensionValue", domain.DimActive, domain.DimensionStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"}, dimSteward, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.CreateDimensionValue(b.ctx, domain.CreateDimensionValueRequest{TypeCode: "REGION", Code: "X", Label: "l", EvidenceRef: "e"}, dimSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "another tenant's type is no type here")
	types, err := b.s.ListDimensionTypes(b.ctx)
	require.NoError(t, err)
	assert.Empty(t, types)
	b.dimType(t, "REGION", true, false, "")
	_, err = b.s.CreateDimensionValue(b.ctx, domain.CreateDimensionValueRequest{TypeCode: "REGION", Code: "B1", Label: "l", ParentValueID: v.ValueID, EvidenceRef: "e"}, dimSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "another tenant's value is no parent")
}

// Migration 000024 reverses cleanly while no type exists and applies again; with one it refuses.
func TestDimension_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'dimension_values'`

	f.dimType(t, "REGION", true, false, "")
	_, err := f.pool.Exec(ctx, read("000024_dimensions.down.sql"))
	require.Error(t, err, "down is refused while dimension types exist")
	assert.Contains(t, err.Error(), "000024 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000024_dimensions.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000024_dimensions.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
