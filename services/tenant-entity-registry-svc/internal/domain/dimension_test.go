package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDimensionLifecycle(t *testing.T) {
	allowed := map[[2]string]bool{
		{DimDraft, DimActive}: true, {DimActive, DimInactive}: true, {DimInactive, DimActive}: true,
	}
	all := []string{DimDraft, DimActive, DimInactive}
	for _, from := range all {
		for _, to := range all {
			assert.Equal(t, allowed[[2]string{from, to}], DimensionCanMoveTo(from, to), "%s -> %s", from, to)
		}
	}
	assert.False(t, DimensionCanMoveTo("WEIRD", DimActive))
}

func TestEvaluateDimensionPosting(t *testing.T) {
	day := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	until := func(s string) *time.Time { d, _ := ParseDate(s); return &d }
	for name, tc := range map[string]struct {
		status    string
		exception *time.Time
		eligible  bool
		exc       bool
		reason    string
	}{
		"active":                       {DimActive, nil, true, false, ""},
		"draft":                        {DimDraft, nil, false, false, "NOT_ACTIVE"},
		"inactive":                     {DimInactive, nil, false, false, "INACTIVE"},
		"inactive inside an exception": {DimInactive, until("2026-06-20"), true, true, ""},
		"inactive on the last day":     {DimInactive, until("2026-06-12"), true, true, ""},
		"inactive after the exception": {DimInactive, until("2026-06-11"), false, false, "INACTIVE"},
		"not yet effective":            {"", nil, false, false, "NOT_YET_EFFECTIVE"},
		"an exception on an active value changes nothing": {DimActive, until("2026-06-20"), true, false, ""},
	} {
		got := EvaluateDimensionPosting("v1", day, tc.status, tc.exception)
		assert.Equal(t, tc.eligible, got.Eligible, name)
		assert.Equal(t, tc.exc, got.Exception, name)
		assert.Equal(t, tc.reason, got.Reason, name)
		assert.Equal(t, "2026-06-12", got.AsOf)
	}
}

func TestHierarchyLoopAndDepth(t *testing.T) {
	parents := map[string]string{"B": "A", "C": "B", "D": "C"}
	assert.True(t, HierarchyLoop(parents, "A", "D"), "A under its own descendant")
	assert.True(t, HierarchyLoop(parents, "B", "D"))
	assert.True(t, HierarchyLoop(parents, "A", "A"), "a value is not its own parent")
	assert.False(t, HierarchyLoop(parents, "D", "A"), "moving a leaf up is fine")
	assert.False(t, HierarchyLoop(parents, "C", ""), "becoming a root is fine")
	assert.False(t, HierarchyLoop(parents, "X", "D"), "a separate value under the chain is fine")
	assert.True(t, HierarchyLoop(map[string]string{"P": "Q", "Q": "P"}, "Z", "P"), "an existing loop is never walked forever")
	assert.Equal(t, 0, HierarchyDepth(parents, "A"))
	assert.Equal(t, 3, HierarchyDepth(parents, "D"))
	assert.LessOrEqual(t, HierarchyDepth(map[string]string{"P": "Q", "Q": "P"}, "P"), 3, "bounded on a bad structure")
}

func TestCreateDimensionTypeValidate(t *testing.T) {
	require.NoError(t, CreateDimensionTypeRequest{TypeCode: "COST_CENTER", Name: "Cost centre", Hierarchical: true, Controlled: true, SourceDomain: "PROJECT"}.Validate())
	for name, r := range map[string]CreateDimensionTypeRequest{
		"lower":           {TypeCode: "cost", Name: "n"},
		"one char":        {TypeCode: "A", Name: "n"},
		"digit first":     {TypeCode: "1AB", Name: "n"},
		"no name":         {TypeCode: "AB", Name: " "},
		"controlled flat": {TypeCode: "AB", Name: "n", Controlled: true},
		"long domain":     {TypeCode: "AB", Name: "n", SourceDomain: strings.Repeat("x", 41)},
		"long code":       {TypeCode: "A" + strings.Repeat("B", 40), Name: "n"},
	} {
		assert.Error(t, r.Validate(), name)
	}
}

func TestCreateDimensionValueValidate(t *testing.T) {
	ok := CreateDimensionValueRequest{TypeCode: "COST_CENTER", Code: "CC-100.A", Label: "Sales", EvidenceRef: "e"}
	require.NoError(t, ok.Validate())
	mut := func(f func(*CreateDimensionValueRequest)) CreateDimensionValueRequest { r := ok; f(&r); return r }
	for name, r := range map[string]CreateDimensionValueRequest{
		"no type":     mut(func(r *CreateDimensionValueRequest) { r.TypeCode = "" }),
		"lower code":  mut(func(r *CreateDimensionValueRequest) { r.Code = "cc" }),
		"space code":  mut(func(r *CreateDimensionValueRequest) { r.Code = "A B" }),
		"no label":    mut(func(r *CreateDimensionValueRequest) { r.Label = " " }),
		"no evidence": mut(func(r *CreateDimensionValueRequest) { r.EvidenceRef = "" }),
		"bad date":    mut(func(r *CreateDimensionValueRequest) { r.ValidFrom = "x" }),
	} {
		assert.Error(t, r.Validate(), name)
	}
}

func TestReparentValidate(t *testing.T) {
	p := "0190a000-0000-7000-8000-00000000aaaa"
	empty := ""
	good := ReparentDimensionValueRequest{ParentValueID: &p, Reason: "r", EvidenceRef: "e"}
	require.NoError(t, good.Validate())
	require.NoError(t, ReparentDimensionValueRequest{Root: true, Reason: "r", EvidenceRef: "e", NewLabel: "n"}.Validate())
	for name, r := range map[string]ReparentDimensionValueRequest{
		"neither":     {Reason: "r", EvidenceRef: "e"},
		"both":        {ParentValueID: &p, Root: true, Reason: "r", EvidenceRef: "e"},
		"empty id":    {ParentValueID: &empty, Reason: "r", EvidenceRef: "e"},
		"no reason":   {ParentValueID: &p, EvidenceRef: "e"},
		"no evidence": {ParentValueID: &p, Reason: "r"},
		"bad date":    {ParentValueID: &p, Reason: "r", EvidenceRef: "e", ValidFrom: "x"},
	} {
		assert.Error(t, r.Validate(), name)
	}
	assert.Error(t, ApproveDimensionHierarchyRequest{}.Validate())
	assert.NoError(t, ApproveDimensionHierarchyRequest{EvidenceRef: "e"}.Validate())
}

func TestDimensionStatusRequestValidate(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	soon := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	far := time.Now().UTC().AddDate(0, 0, 120).Format("2006-01-02")
	assert.NoError(t, DimensionStatusRequest{EvidenceRef: "e"}.ValidateFor(DimActive))
	assert.Error(t, DimensionStatusRequest{}.ValidateFor(DimActive))
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e"}.ValidateFor(DimInactive), "deactivating states why")
	assert.NoError(t, DimensionStatusRequest{EvidenceRef: "e", Reason: "r", ExceptionUntil: soon}.ValidateFor(DimInactive))
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e", Reason: "r", ExceptionUntil: far}.ValidateFor(DimInactive), "bounded to 90 days")
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e", Reason: "r", ExceptionUntil: today}.ValidateFor(DimInactive), "after the deactivation")
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e", ExceptionUntil: soon}.ValidateFor(DimActive), "an exception applies only to a deactivation")
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e", Reason: "r", ExceptionUntil: "x"}.ValidateFor(DimInactive))
	assert.Error(t, DimensionStatusRequest{EvidenceRef: "e", EffectiveDate: "x"}.ValidateFor(DimActive))
}

func TestMapSourceAndResolveValidate(t *testing.T) {
	assert.NoError(t, MapSourceObjectRequest{SourceDomain: "PROJECT", SourceObjectID: "P-1", EvidenceRef: "e"}.Validate())
	assert.Error(t, MapSourceObjectRequest{SourceObjectID: "P-1", EvidenceRef: "e"}.Validate())
	assert.Error(t, MapSourceObjectRequest{SourceDomain: "PROJECT", EvidenceRef: "e"}.Validate())
	assert.Error(t, MapSourceObjectRequest{SourceDomain: "PROJECT", SourceObjectID: "P-1"}.Validate())
	assert.Error(t, MapSourceObjectRequest{SourceDomain: "PROJECT", SourceObjectID: "P-1", EvidenceRef: "e", ValidFrom: "x"}.Validate())

	assert.NoError(t, ResolveDimensionQuery{TypeCode: "T", Code: "C"}.Validate())
	assert.NoError(t, ResolveDimensionQuery{TypeCode: "T", SourceDomain: "PROJECT", SourceObjectID: "P"}.Validate())
	assert.Error(t, ResolveDimensionQuery{TypeCode: "T"}.Validate(), "name a way to find it")
	assert.Error(t, ResolveDimensionQuery{TypeCode: "T", Code: "C", SourceDomain: "P", SourceObjectID: "1"}.Validate(), "not both")
	assert.Error(t, ResolveDimensionQuery{Code: "C"}.Validate(), "the type is required")
	assert.Error(t, ResolveDimensionQuery{TypeCode: "T", SourceDomain: "P"}.Validate(), "domain and object go together")
}
