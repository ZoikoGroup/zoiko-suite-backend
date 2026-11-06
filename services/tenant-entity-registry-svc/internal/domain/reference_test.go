package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func entry(code string, ct domain.ReferenceChange) domain.ReferenceEntryInput {
	e := domain.ReferenceEntryInput{Code: code, ChangeType: ct, Label: code + " label"}
	switch ct {
	case domain.ChangeRetire, domain.ChangeCorrect:
		e.Reason = "why"
	case domain.ChangeSupersede:
		e.Label, e.Reason, e.ReplacedByCode = "", "why", "NEW"
	}
	return e
}

func goodStage() domain.StageReferenceReleaseRequest {
	return domain.StageReferenceReleaseRequest{ReleaseVersion: "2026.1", SourcePublishedAt: "2026-01-01T00:00:00Z", EffectiveFrom: "2026-02-01",
		Compatibility: domain.CompatAutoAdopt, EvidenceRef: "e", Entries: []domain.ReferenceEntryInput{entry("GB", domain.ChangeAdd)}}
}

func TestStageReferenceReleaseRequest_Validate(t *testing.T) {
	require.NoError(t, goodStage().Validate())
	for name, mut := range map[string]func(*domain.StageReferenceReleaseRequest){
		"bad version":  func(r *domain.StageReferenceReleaseRequest) { r.ReleaseVersion = "2026:1" },
		"no evidence":  func(r *domain.StageReferenceReleaseRequest) { r.EvidenceRef = " " },
		"compat":       func(r *domain.StageReferenceReleaseRequest) { r.Compatibility = "MAYBE" },
		"checksum":     func(r *domain.StageReferenceReleaseRequest) { r.Checksum = "ABC" },
		"no entries":   func(r *domain.StageReferenceReleaseRequest) { r.Entries = nil },
		"published at": func(r *domain.StageReferenceReleaseRequest) { r.SourcePublishedAt = "2026-01-01" },
		"effective":    func(r *domain.StageReferenceReleaseRequest) { r.EffectiveFrom = "soon" },
		"duplicate code": func(r *domain.StageReferenceReleaseRequest) {
			r.Entries = append(r.Entries, entry("GB", domain.ChangeAdd))
		},
		"bad code":       func(r *domain.StageReferenceReleaseRequest) { r.Entries[0].Code = "G B" },
		"unknown change": func(r *domain.StageReferenceReleaseRequest) { r.Entries[0].ChangeType = "REMOVE" },
		"no label":       func(r *domain.StageReferenceReleaseRequest) { r.Entries[0].Label = "" },
		"bad origin":     func(r *domain.StageReferenceReleaseRequest) { r.Entries[0].Origin = "GUESSED" },
		"replacement":    func(r *domain.StageReferenceReleaseRequest) { r.Entries[0].ReplacedByCode = "X" },
		"supersede alone": func(r *domain.StageReferenceReleaseRequest) {
			r.Entries[0] = entry("OLD", domain.ChangeSupersede)
			r.Entries[0].ReplacedByCode = ""
		},
		"self replace": func(r *domain.StageReferenceReleaseRequest) { r.Entries[0] = entry("NEW", domain.ChangeSupersede) },
		"retire why": func(r *domain.StageReferenceReleaseRequest) {
			r.Entries[0] = entry("OLD", domain.ChangeRetire)
			r.Entries[0].Reason = ""
		},
		"metadata size": func(r *domain.StageReferenceReleaseRequest) {
			r.Entries[0].Metadata = map[string]any{"x": strings.Repeat("a", domain.MaxEntryMetadata)}
		},
		"too many": func(r *domain.StageReferenceReleaseRequest) {
			r.Entries = nil
			for i := 0; i <= domain.MaxReleaseEntries; i++ {
				r.Entries = append(r.Entries, domain.ReferenceEntryInput{Code: "C" + strings.Repeat("x", 1) + time.Duration(i).String(), ChangeType: domain.ChangeAdd, Label: "l"})
			}
		},
	} {
		r := goodStage()
		r.Entries = append([]domain.ReferenceEntryInput(nil), r.Entries...)
		mut(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestReferenceRequests_Validate(t *testing.T) {
	good := domain.CreateReferenceSetRequest{SetCode: "ISO_4217_CURRENCY", Name: "n", SourceAuthority: "ISO"}
	require.NoError(t, good.Validate())
	for name, mut := range map[string]func(*domain.CreateReferenceSetRequest){
		"lower": func(r *domain.CreateReferenceSetRequest) { r.SetCode = "iso" },
		"name":  func(r *domain.CreateReferenceSetRequest) { r.Name = "" },
		"auth":  func(r *domain.CreateReferenceSetRequest) { r.SourceAuthority = " " },
		"desc":  func(r *domain.CreateReferenceSetRequest) { r.Description = strings.Repeat("x", 1001) },
	} {
		r := good
		mut(&r)
		assert.Error(t, r.Validate(), name)
	}
	assert.Error(t, domain.PublishReferenceReleaseRequest{}.Validate())
	assert.NoError(t, domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}.Validate())
	assert.NoError(t, domain.ReferenceQuery{SetCode: "ISO_4217_CURRENCY", Code: "GBP"}.Validate())
	assert.Error(t, domain.ReferenceQuery{SetCode: "iso", Code: "GBP"}.Validate())
	assert.Error(t, domain.ReferenceQuery{SetCode: "ISO_4217_CURRENCY", Code: "G P"}.Validate())
	assert.Error(t, domain.ReferenceQuery{SetCode: "ISO_4217_CURRENCY", Code: "GBP", Release: "a:b"}.Validate())
}

func TestCheckEntries(t *testing.T) {
	prior := map[string]domain.CodeState{
		"GB":  {Exists: true, Active: true, Origin: domain.OriginOfficial},
		"OLD": {Exists: true, Active: false, Origin: domain.OriginOfficial},
		"XX":  {Exists: true, Active: true, Origin: domain.OriginCustom},
	}
	add := func(c string) domain.ReferenceEntryInput { return entry(c, domain.ChangeAdd) }
	sup := func(c, by string) domain.ReferenceEntryInput {
		e := entry(c, domain.ChangeSupersede)
		e.ReplacedByCode = by
		return e
	}
	custom := add("MY")
	custom.Origin = domain.OriginCustom
	wrongOrigin := entry("XX", domain.ChangeModifyMetadata)

	for name, tc := range map[string]struct {
		entries []domain.ReferenceEntryInput
		custom  bool
		want    string // empty: allowed
	}{
		"add new":                 {[]domain.ReferenceEntryInput{add("FR")}, false, ""},
		"add existing":            {[]domain.ReferenceEntryInput{add("GB")}, false, "already exists"},
		"retired code not reused": {[]domain.ReferenceEntryInput{add("OLD")}, false, "not reused"},
		"modify active":           {[]domain.ReferenceEntryInput{entry("GB", domain.ChangeModifyMetadata)}, false, ""},
		"correct active":          {[]domain.ReferenceEntryInput{entry("GB", domain.ChangeCorrect)}, false, ""},
		"modify retired":          {[]domain.ReferenceEntryInput{entry("OLD", domain.ChangeModifyMetadata)}, false, "not an active"},
		"modify unknown":          {[]domain.ReferenceEntryInput{entry("ZZ", domain.ChangeCorrect)}, false, "not an active"},
		"retire active":           {[]domain.ReferenceEntryInput{entry("GB", domain.ChangeRetire)}, false, ""},
		"retire retired":          {[]domain.ReferenceEntryInput{entry("OLD", domain.ChangeRetire)}, false, "not an active"},
		"supersede by existing":   {[]domain.ReferenceEntryInput{sup("GB", "XX")}, true, ""},
		"supersede by one added":  {[]domain.ReferenceEntryInput{sup("GB", "NEW"), add("NEW")}, false, ""},
		"supersede by unknown":    {[]domain.ReferenceEntryInput{sup("GB", "NEW")}, false, "not an active"},
		"supersede by retired":    {[]domain.ReferenceEntryInput{sup("GB", "OLD")}, false, "not an active"},
		"replacement retired too": {[]domain.ReferenceEntryInput{sup("GB", "XX"), entry("XX", domain.ChangeRetire)}, true, "not an active"},
		"custom refused":          {[]domain.ReferenceEntryInput{custom}, false, "no user-assigned"},
		"custom allowed":          {[]domain.ReferenceEntryInput{custom}, true, ""},
		"origin never changes":    {[]domain.ReferenceEntryInput{wrongOrigin}, true, "origin does not change"},
	} {
		err := domain.CheckEntries(prior, tc.entries, tc.custom)
		if tc.want == "" {
			assert.NoError(t, err, name)
		} else if assert.Error(t, err, name) {
			assert.Contains(t, err.Error(), tc.want, name)
		}
	}
}

func rel(v string, seq int, eff string) domain.ReleaseRef {
	return domain.ReleaseRef{ReleaseVersion: v, EffectiveFrom: eff, PublishSeq: seq}
}

func TestResolveReference(t *testing.T) {
	assert.Nil(t, domain.ResolveReference(nil), "a code never added")
	assert.Nil(t, domain.ResolveReference([]domain.ReferenceHistoryItem{{Release: rel("r1", 1, "2026-01-01"), ChangeType: domain.ChangeModifyMetadata, Label: "x"}}), "a history must begin with its ADD")

	added := domain.ReferenceHistoryItem{Release: rel("r1", 1, "2026-01-01"), ChangeType: domain.ChangeAdd, Label: "Old name", Origin: domain.OriginOfficial, Metadata: map[string]any{"a": 1.0}}
	renamed := domain.ReferenceHistoryItem{Release: rel("r2", 2, "2026-03-01"), ChangeType: domain.ChangeCorrect, Label: "New name", Origin: domain.OriginOfficial, Metadata: map[string]any{"a": 2.0}}
	superseded := domain.ReferenceHistoryItem{Release: rel("r3", 3, "2026-06-01"), ChangeType: domain.ChangeSupersede, Origin: domain.OriginOfficial, ReplacedByCode: "NEW", Reason: "renamed"}
	retired := domain.ReferenceHistoryItem{Release: rel("r3", 3, "2026-06-01"), ChangeType: domain.ChangeRetire, Origin: domain.OriginOfficial, Reason: "gone"}

	r := domain.ResolveReference([]domain.ReferenceHistoryItem{added})
	require.NotNil(t, r)
	assert.Equal(t, domain.CodeActive, r.Status)
	assert.Equal(t, "Old name", r.Label)
	assert.Equal(t, "2026-01-01", r.ValidFrom)
	assert.Empty(t, r.ValidTo)
	assert.Equal(t, "r1", r.Release.ReleaseVersion)

	r = domain.ResolveReference([]domain.ReferenceHistoryItem{added, renamed})
	assert.Equal(t, "New name", r.Label, "the governing release's label")
	assert.Equal(t, 2.0, r.Metadata["a"])
	assert.Equal(t, "r2", r.Release.ReleaseVersion, "the release that last changed it governs")
	assert.Equal(t, "2026-01-01", r.ValidFrom, "validity starts with the ADD")
	assert.Equal(t, "CORRECT", r.ChangeType)

	r = domain.ResolveReference([]domain.ReferenceHistoryItem{added, renamed, superseded})
	assert.Equal(t, domain.CodeSuperseded, r.Status)
	assert.Equal(t, "NEW", r.SupersededBy)
	assert.Equal(t, "2026-06-01", r.ValidTo)
	assert.Equal(t, "New name", r.Label, "a retired or superseded code keeps its last label")
	assert.Equal(t, 2.0, r.Metadata["a"], "and its last metadata")
	assert.Len(t, r.History, 3)

	r = domain.ResolveReference([]domain.ReferenceHistoryItem{added, retired})
	assert.Equal(t, domain.CodeRetired, r.Status)
	assert.Empty(t, r.SupersededBy)
	assert.Equal(t, "2026-06-01", r.ValidTo)
}
