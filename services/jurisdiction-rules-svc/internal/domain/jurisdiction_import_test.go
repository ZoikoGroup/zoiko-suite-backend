package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var importFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func goodAdd() ImportItemInput {
	return ImportItemInput{
		Action: ImportActionAdd, Target: JurisdictionRef{Code: "GB-NEW", Type: "STATE_PROVINCE"},
		JurisdictionName: "New", AuthorityType: "STATE", EffectiveFrom: &importFrom,
		Parent: &JurisdictionRef{Code: "GB", Type: "COUNTRY"},
	}
}

func TestImportItem_WellFormedRowsPass(t *testing.T) {
	for name, it := range map[string]ImportItemInput{
		"add":                goodAdd(),
		"add with no parent": func() ImportItemInput { i := goodAdd(); i.Parent = nil; return i }(),
		"rename":             {Action: ImportActionUpdate, Target: JurisdictionRef{Code: "GB", Type: "COUNTRY"}, JurisdictionName: "United Kingdom"},
		"move by id":         {Action: ImportActionUpdate, Target: JurisdictionRef{JurisdictionID: "3f2b8a52-6c3e-4f55-9c3c-6a1b9d0a8e11"}, Parent: &JurisdictionRef{JurisdictionID: "7a1c0d8e-2b1f-4f7a-a0a4-1d6a3f4b2c90"}},
		"retire":             {Action: ImportActionRetire, Target: JurisdictionRef{Code: "YU", Type: "COUNTRY"}, Successor: &JurisdictionRef{Code: "RS", Type: "COUNTRY"}, Note: "dissolved"},
		"lower-case action":  {Action: "retire", Target: JurisdictionRef{Code: "YU", Type: "COUNTRY"}},
	} {
		if errs := it.Validate(); len(errs) != 0 {
			t.Errorf("%s refused: %v", name, errs)
		}
	}
}

func TestImportItem_MalformedRowsAreRefusedWithWhy(t *testing.T) {
	id := "3f2b8a52-6c3e-4f55-9c3c-6a1b9d0a8e11"
	cases := map[string]struct {
		mutate func(*ImportItemInput)
		want   string
	}{
		"unknown action":        {func(i *ImportItemInput) { i.Action = "MERGE" }, "action must be"},
		"no action":             {func(i *ImportItemInput) { i.Action = "" }, "action must be"},
		"no target":             {func(i *ImportItemInput) { i.Target = JurisdictionRef{} }, "target"},
		"a code with no type":   {func(i *ImportItemInput) { i.Target = JurisdictionRef{Code: "GB"} }, "both code and type"},
		"an id and a code":      {func(i *ImportItemInput) { i.Target = JurisdictionRef{JurisdictionID: id, Code: "GB", Type: "COUNTRY"} }, "not both"},
		"an id that is no id":   {func(i *ImportItemInput) { i.Target = JurisdictionRef{JurisdictionID: "GB"} }, "not a UUID"},
		"add names an id":       {func(i *ImportItemInput) { i.Target = JurisdictionRef{JurisdictionID: id} }, "assigned by the registry"},
		"add with no name":      {func(i *ImportItemInput) { i.JurisdictionName = " " }, "needs jurisdiction_name"},
		"add with no authority": {func(i *ImportItemInput) { i.AuthorityType = "" }, "needs authority_type"},
		"add with no date":      {func(i *ImportItemInput) { i.EffectiveFrom = nil }, "needs effective_from"},
		"add with a successor":  {func(i *ImportItemInput) { i.Successor = &JurisdictionRef{Code: "X", Type: "Y"} }, "only to a RETIRE"},
		"over-long code":        {func(i *ImportItemInput) { i.Target.Code = strings.Repeat("A", MaxImportCodeLen+1) }, "longer than"},
		"over-long type":        {func(i *ImportItemInput) { i.Target.Type = strings.Repeat("A", MaxImportTypeLen+1) }, "longer than"},
		"over-long name":        {func(i *ImportItemInput) { i.JurisdictionName = strings.Repeat("x", MaxImportNameLen+1) }, "longer than"},
		"over-long note":        {func(i *ImportItemInput) { i.Note = strings.Repeat("x", MaxImportNoteLen+1) }, "longer than"},
		"bad parent":            {func(i *ImportItemInput) { i.Parent = &JurisdictionRef{Code: "GB"} }, "parent"},
	}
	for name, c := range cases {
		it := goodAdd()
		c.mutate(&it)
		errs := it.Validate()
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: %v, want an error mentioning %q", name, errs, c.want)
		}
	}

	for name, c := range map[string]struct {
		it   ImportItemInput
		want string
	}{
		"an update that changes nothing": {ImportItemInput{Action: ImportActionUpdate, Target: JurisdictionRef{Code: "GB", Type: "COUNTRY"}}, "must change"},
		"an update with a successor":     {ImportItemInput{Action: ImportActionUpdate, Target: JurisdictionRef{Code: "GB", Type: "COUNTRY"}, JurisdictionName: "x", Successor: &JurisdictionRef{Code: "X", Type: "Y"}}, "only to a RETIRE"},
		"a retire that also renames":     {ImportItemInput{Action: ImportActionRetire, Target: JurisdictionRef{Code: "GB", Type: "COUNTRY"}, JurisdictionName: "x"}, "only a target"},
		"a retire with a date":           {ImportItemInput{Action: ImportActionRetire, Target: JurisdictionRef{Code: "GB", Type: "COUNTRY"}, EffectiveFrom: &importFrom}, "only a target"},
	} {
		if errs := c.it.Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: %v, want %q", name, errs, c.want)
		}
	}
}

// Everything wrong with a row is reported together, so a whole file's problems
// are seen in one pass.
func TestImportItem_ReportsEveryProblemAtOnce(t *testing.T) {
	errs := ImportItemInput{Action: ImportActionAdd, Target: JurisdictionRef{Code: "GB"}}.Validate()
	if len(errs) < 4 {
		t.Errorf("errors = %v, want the target, name, authority type and date all named", errs)
	}
}

func TestImportItem_NormalisationIgnoresPaddingNotMeaning(t *testing.T) {
	padded := goodAdd()
	padded.Target.Code, padded.JurisdictionName, padded.Action = "  GB-NEW ", " New  ", " add "
	if got := padded.Normalised(); got.Target.Code != "GB-NEW" || got.JurisdictionName != "New" || got.Action != "ADD" {
		t.Errorf("normalised = %+v", got)
	}
	// An all-blank parent is no parent, not an empty one.
	blank := goodAdd()
	blank.Parent = &JurisdictionRef{Code: " ", Type: " "}
	if blank.Normalised().Parent != nil {
		t.Error("a blank parent stayed a parent")
	}
	// Case in a code is meaning: it is not folded.
	lower := goodAdd()
	lower.Target.Code = "gb-new"
	if lower.Normalised().Target.Code != "gb-new" {
		t.Error("a code's case was changed")
	}
	// A time zone is not meaning either.
	zone := goodAdd()
	local := importFrom.In(time.FixedZone("x", 5*3600))
	zone.EffectiveFrom = &local
	if !zone.Normalised().EffectiveFrom.Equal(importFrom) || zone.Normalised().EffectiveFrom.Location() != time.UTC {
		t.Error("effective_from was not normalised to UTC")
	}
}

func TestImportHash_IsTheContentNotTheFormatting(t *testing.T) {
	items := []ImportItemInput{goodAdd()}
	base := ImportHash("ISO 3166-2", "2026-1", items)
	if len(base) != 64 {
		t.Fatalf("hash = %q", base)
	}

	padded := goodAdd()
	padded.Target.Code, padded.JurisdictionName = " GB-NEW  ", "  New "
	local := importFrom.In(time.FixedZone("x", -8*3600))
	padded.EffectiveFrom = &local
	if got := ImportHash(" ISO 3166-2 ", " 2026-1", []ImportItemInput{padded}); got != base {
		t.Error("the same content hashed differently because of padding or a time zone")
	}

	renamed := goodAdd()
	renamed.JurisdictionName = "Newer"
	second := goodAdd()
	second.Target.Code = "GB-TWO"
	for name, h := range map[string]string{
		"another version":  ImportHash("ISO 3166-2", "2026-2", items),
		"another standard": ImportHash("ISO 3166-1", "2026-1", items),
		"another value":    ImportHash("ISO 3166-2", "2026-1", []ImportItemInput{renamed}),
		"another row":      ImportHash("ISO 3166-2", "2026-1", []ImportItemInput{goodAdd(), second}),
		"rows reordered":   ImportHash("ISO 3166-2", "2026-1", []ImportItemInput{second, goodAdd()}),
		"no rows":          ImportHash("ISO 3166-2", "2026-1", nil),
	} {
		if h == base {
			t.Errorf("%s hashed the same as the original", name)
		}
	}
	if ImportHash("a", "b", nil) != ImportHash("a", "b", nil) {
		t.Error("the hash is not deterministic")
	}
}

func TestProposeImportParams_Validate(t *testing.T) {
	good := ProposeImportParams{
		SourceStandard: "ISO 3166-2", SourceVersion: "2026-1", EvidenceRef: "https://example.test/f.csv",
		Summary: "2026 changes", Items: []ImportItemInput{goodAdd()}, ActorID: "maker-1",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good proposal refused: %v", err)
	}
	for name, mutate := range map[string]func(*ProposeImportParams){
		"no actor":           func(p *ProposeImportParams) { p.ActorID = " " },
		"no standard":        func(p *ProposeImportParams) { p.SourceStandard = "" },
		"over-long standard": func(p *ProposeImportParams) { p.SourceStandard = strings.Repeat("x", MaxImportSourceLen+1) },
		"no version":         func(p *ProposeImportParams) { p.SourceVersion = " " },
		"over-long version":  func(p *ProposeImportParams) { p.SourceVersion = strings.Repeat("x", MaxImportVersion+1) },
		"no evidence":        func(p *ProposeImportParams) { p.EvidenceRef = "" },
		"no summary":         func(p *ProposeImportParams) { p.Summary = "" },
		"no rows":            func(p *ProposeImportParams) { p.Items = nil },
		"too many rows":      func(p *ProposeImportParams) { p.Items = make([]ImportItemInput, MaxImportItems+1) },
	} {
		p := good
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidImport) {
			t.Errorf("%s: %v, want ErrInvalidImport", name, err)
		}
	}
	// A bad row does not fail the batch: it is kept with its errors.
	bad := good
	bad.Items = []ImportItemInput{{Action: "MERGE"}}
	if err := bad.Validate(); err != nil {
		t.Errorf("a batch with a bad row was refused as a whole: %v", err)
	}
}

func TestImportItemError_MatchesItsSentinelAndNamesTheRow(t *testing.T) {
	var err error = &ImportItemError{ItemNo: 7, Message: "parent: no such jurisdiction"}
	if !errors.Is(err, ErrImportItemFailed) || errors.Is(err, ErrImportDecided) || errors.Is(err, ErrInvalidImport) {
		t.Error("the row error matches the wrong sentinels")
	}
	if !strings.Contains(err.Error(), "row 7") || !strings.Contains(err.Error(), "parent") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestJurisdictionRef_String(t *testing.T) {
	if got := (JurisdictionRef{Code: "GB", Type: "COUNTRY"}).String(); !strings.Contains(got, "COUNTRY") || !strings.Contains(got, "GB") {
		t.Errorf("by code = %q", got)
	}
	if got := (JurisdictionRef{JurisdictionID: "abc"}).String(); !strings.Contains(got, "abc") {
		t.Errorf("by id = %q", got)
	}
}
