package domain

// MDM-04 reference releases (ZS-SVC-U-001 s15-16).
//
// A reference set is a governed code list. It changes only by releases: each a set of per-code changes with a source
// authority, a publication time kept apart from its business effective date, a compatibility statement and evidence. The
// state of a code on a date, or under one exact release, is derived from the published releases only, by the two pure
// rules here: CheckEntries (may these changes be released) and ResolveReference (what is the code's state).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	refSetCodeShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)
	refCodeShape    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	refVersionShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	sha256Shape     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReferenceChange is what a release does to one code.
type ReferenceChange string

const (
	ChangeAdd            ReferenceChange = "ADD"
	ChangeModifyMetadata ReferenceChange = "MODIFY_METADATA"
	ChangeSupersede      ReferenceChange = "SUPERSEDE"
	ChangeRetire         ReferenceChange = "RETIRE"
	ChangeCorrect        ReferenceChange = "CORRECT"
)

// Valid reports whether c is a known change type.
func (c ReferenceChange) Valid() bool {
	switch c {
	case ChangeAdd, ChangeModifyMetadata, ChangeSupersede, ChangeRetire, ChangeCorrect:
		return true
	}
	return false
}

// Code origins: an official code comes from the set's authority, a custom one is user-assigned.
const (
	OriginOfficial = "OFFICIAL"
	OriginCustom   = "CUSTOM"
)

// Release compatibility: how a consumer may adopt the release.
const (
	CompatAutoAdopt         = "AUTO_ADOPT"
	CompatRequiresMigration = "REQUIRES_MIGRATION"
	CompatPreserveOld       = "PRESERVE_OLD"
)

// Release and code statuses.
const (
	ReleaseStaged    = "STAGED"
	ReleasePublished = "PUBLISHED"
	CodeActive       = "ACTIVE"
	CodeRetired      = "RETIRED"
	CodeSuperseded   = "SUPERSEDED"
)

// Bounds that keep a release reviewable and storable.
const (
	MaxReleaseEntries = 5000
	MaxEntryMetadata  = 8 * 1024
)

// ReferenceSet is a governed code list.
type ReferenceSet struct {
	SetID           string    `json:"set_id"`
	TenantID        string    `json:"tenant_id"`
	SetCode         string    `json:"set_code"`
	Name            string    `json:"name"`
	Description     string    `json:"description,omitempty"`
	SourceAuthority string    `json:"source_authority"`
	AllowCustom     bool      `json:"allow_custom"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	// CurrentRelease is the release in force today, when one is.
	CurrentRelease string `json:"current_release,omitempty"`
}

// CreateReferenceSetRequest defines a reference set.
type CreateReferenceSetRequest struct {
	SetCode         string `json:"set_code"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	SourceAuthority string `json:"source_authority"`
	AllowCustom     bool   `json:"allow_custom,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateReferenceSetRequest) Validate() error {
	switch {
	case !refSetCodeShape.MatchString(r.SetCode):
		return fmt.Errorf("set_code must be upper case letters, digits and underscore, starting with a letter (2 to 40 characters)")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case len(r.Description) > 1000:
		return fmt.Errorf("description is too long")
	case strings.TrimSpace(r.SourceAuthority) == "" || len(r.SourceAuthority) > 300:
		return fmt.Errorf("source_authority is required: name the publisher or maintenance authority")
	}
	return nil
}

// ReferenceEntryInput is one change to one code within a release.
type ReferenceEntryInput struct {
	Code           string          `json:"code"`
	ChangeType     ReferenceChange `json:"change_type"`
	Label          string          `json:"label,omitempty"`
	Origin         string          `json:"origin,omitempty"` // OFFICIAL when absent
	Metadata       map[string]any  `json:"metadata,omitempty"`
	ReplacedByCode string          `json:"replaced_by_code,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// OriginOrDefault is the entry's origin, OFFICIAL when not stated.
func (e ReferenceEntryInput) OriginOrDefault() string {
	if e.Origin == "" {
		return OriginOfficial
	}
	return e.Origin
}

func (e ReferenceEntryInput) validate() error {
	if !refCodeShape.MatchString(e.Code) {
		return fmt.Errorf("code %q must be letters, digits and . _ : - (1 to 64 characters)", e.Code)
	}
	if !e.ChangeType.Valid() {
		return fmt.Errorf("%s: change_type must be ADD, MODIFY_METADATA, SUPERSEDE, RETIRE or CORRECT", e.Code)
	}
	if o := e.OriginOrDefault(); o != OriginOfficial && o != OriginCustom {
		return fmt.Errorf("%s: origin must be OFFICIAL or CUSTOM", e.Code)
	}
	live := e.ChangeType != ChangeRetire && e.ChangeType != ChangeSupersede
	if live && strings.TrimSpace(e.Label) == "" {
		return fmt.Errorf("%s: a label is required", e.Code)
	}
	if len(e.Label) > 300 {
		return fmt.Errorf("%s: label is too long", e.Code)
	}
	if mb, err := json.Marshal(e.Metadata); err != nil || len(mb) > MaxEntryMetadata {
		return fmt.Errorf("%s: metadata must be a JSON object of at most %d bytes", e.Code, MaxEntryMetadata)
	}
	if (e.ChangeType == ChangeSupersede) != (e.ReplacedByCode != "") {
		return fmt.Errorf("%s: replaced_by_code is given for SUPERSEDE and only then", e.Code)
	}
	if e.ReplacedByCode != "" {
		if !refCodeShape.MatchString(e.ReplacedByCode) {
			return fmt.Errorf("%s: replaced_by_code is not a valid code", e.Code)
		}
		if e.ReplacedByCode == e.Code {
			return fmt.Errorf("%s: a code cannot replace itself", e.Code)
		}
	}
	if (e.ChangeType == ChangeRetire || e.ChangeType == ChangeSupersede || e.ChangeType == ChangeCorrect) && strings.TrimSpace(e.Reason) == "" {
		return fmt.Errorf("%s: %s states why", e.Code, e.ChangeType)
	}
	return nil
}

// StageReferenceReleaseRequest stages a release; it governs nothing until another principal publishes it.
type StageReferenceReleaseRequest struct {
	ReleaseVersion    string                `json:"release_version"`
	SourcePublishedAt string                `json:"source_published_at"` // RFC 3339
	EffectiveFrom     string                `json:"effective_from"`      // YYYY-MM-DD
	SourceAuthority   string                `json:"source_authority,omitempty"`
	Compatibility     string                `json:"compatibility"`
	Checksum          string                `json:"checksum,omitempty"`
	EvidenceRef       string                `json:"evidence_ref"`
	Entries           []ReferenceEntryInput `json:"entries"`
	CorrelationID     string                `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r StageReferenceReleaseRequest) Validate() error {
	switch {
	case !refVersionShape.MatchString(r.ReleaseVersion):
		return fmt.Errorf("release_version must be letters, digits and . _ - (1 to 40 characters)")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required: the retrieval or provenance reference of the release")
	case r.Compatibility != CompatAutoAdopt && r.Compatibility != CompatRequiresMigration && r.Compatibility != CompatPreserveOld:
		return fmt.Errorf("compatibility must be AUTO_ADOPT, REQUIRES_MIGRATION or PRESERVE_OLD")
	case r.Checksum != "" && !sha256Shape.MatchString(r.Checksum):
		return fmt.Errorf("checksum must be a lower case SHA-256 hex digest")
	case len(r.SourceAuthority) > 300:
		return fmt.Errorf("source_authority is too long")
	case len(r.Entries) == 0:
		return fmt.Errorf("a release changes at least one code")
	case len(r.Entries) > MaxReleaseEntries:
		return fmt.Errorf("a release holds at most %d entries", MaxReleaseEntries)
	}
	if _, err := time.Parse(time.RFC3339, r.SourcePublishedAt); err != nil {
		return fmt.Errorf("source_published_at must be an RFC 3339 timestamp")
	}
	if _, err := ParseDate(r.EffectiveFrom); err != nil {
		return fmt.Errorf("effective_from %v", err)
	}
	seen := map[string]bool{}
	for _, e := range r.Entries {
		if err := e.validate(); err != nil {
			return err
		}
		if seen[e.Code] {
			return fmt.Errorf("%s appears twice: a release makes one change to a code", e.Code)
		}
		seen[e.Code] = true
	}
	return nil
}

// PublishReferenceReleaseRequest publishes a staged release.
type PublishReferenceReleaseRequest struct {
	EvidenceRef   string `json:"evidence_ref"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r PublishReferenceReleaseRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// ReferenceRelease is a release of a set.
type ReferenceRelease struct {
	ReleaseID         string     `json:"release_id"`
	SetCode           string     `json:"set_code"`
	ReleaseVersion    string     `json:"release_version"`
	Status            string     `json:"status"`
	SourcePublishedAt time.Time  `json:"source_published_at"`
	EffectiveFrom     string     `json:"effective_from"`
	SourceAuthority   string     `json:"source_authority"`
	Compatibility     string     `json:"compatibility"`
	Checksum          string     `json:"checksum,omitempty"`
	EvidenceRef       string     `json:"evidence_ref"`
	StagedBy          string     `json:"staged_by"`
	StagedAt          time.Time  `json:"staged_at"`
	ReleasedBy        string     `json:"released_by,omitempty"`
	ReleasedAt        *time.Time `json:"released_at,omitempty"`
	ReleaseEvidence   string     `json:"release_evidence,omitempty"`
	PublishSeq        int        `json:"publish_seq,omitempty"`
	EntryCount        int        `json:"entry_count"`
	// Changes counts the entries by change type.
	Changes map[string]int `json:"changes,omitempty"`
}

// CodeState is what a set holds for one code before a release.
type CodeState struct {
	Exists bool
	Active bool
	Origin string
}

// CheckEntries reports why a release's changes cannot be made to a set whose codes stand as prior says, or nil. Order inside
// a release does not matter: each code is changed once, and a replacement need only be live once the release is applied.
func CheckEntries(prior map[string]CodeState, entries []ReferenceEntryInput, allowCustom bool) error {
	post := map[string]CodeState{}
	for k, v := range prior {
		post[k] = v
	}
	for _, e := range entries {
		st := prior[e.Code]
		switch e.ChangeType {
		case ChangeAdd:
			if st.Exists {
				if st.Active {
					return fmt.Errorf("%s already exists; change it with MODIFY_METADATA or CORRECT", e.Code)
				}
				return fmt.Errorf("%s was retired and is not reused", e.Code)
			}
			post[e.Code] = CodeState{Exists: true, Active: true, Origin: e.OriginOrDefault()}
		case ChangeModifyMetadata, ChangeCorrect:
			if !st.Active {
				return fmt.Errorf("%s is not an active code of the set", e.Code)
			}
			if e.OriginOrDefault() != st.Origin {
				return fmt.Errorf("%s is %s and stays so: a code's origin does not change", e.Code, st.Origin)
			}
		case ChangeRetire, ChangeSupersede:
			if !st.Active {
				return fmt.Errorf("%s is not an active code of the set", e.Code)
			}
			post[e.Code] = CodeState{Exists: true, Active: false, Origin: st.Origin}
		}
		if e.OriginOrDefault() == OriginCustom && !allowCustom {
			return fmt.Errorf("%s: this set takes no user-assigned codes", e.Code)
		}
	}
	for _, e := range entries {
		if e.ChangeType == ChangeSupersede && !post[e.ReplacedByCode].Active {
			return fmt.Errorf("%s is replaced by %s, which is not an active code once the release is applied", e.Code, e.ReplacedByCode)
		}
	}
	return nil
}

// ReleaseRef names the release a fact comes from.
type ReleaseRef struct {
	ReleaseVersion    string    `json:"release_version"`
	EffectiveFrom     string    `json:"effective_from"`
	SourcePublishedAt time.Time `json:"source_published_at"`
	SourceAuthority   string    `json:"source_authority"`
	Compatibility     string    `json:"compatibility"`
	Checksum          string    `json:"checksum,omitempty"`
	EvidenceRef       string    `json:"evidence_ref"`
	PublishSeq        int       `json:"publish_seq"`
}

// ReferenceHistoryItem is one published change to a code, with the release that made it.
type ReferenceHistoryItem struct {
	Release        ReleaseRef      `json:"release"`
	ChangeType     ReferenceChange `json:"change_type"`
	Label          string          `json:"label,omitempty"`
	Origin         string          `json:"origin"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
	ReplacedByCode string          `json:"replaced_by_code,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// ReferenceResolution is a code as it stood, with the release that governed it and its supersession.
type ReferenceResolution struct {
	SetCode string `json:"set_code"`
	Code    string `json:"code"`
	AsOf    string `json:"as_of"`
	Status  string `json:"status"` // ACTIVE, RETIRED or SUPERSEDED
	Label   string `json:"label"`
	Origin  string `json:"origin"`
	// Metadata is the code's metadata as the last release that stated it left it.
	Metadata map[string]any `json:"metadata"`
	// Release governs the code on the date: the latest published release that changed it.
	Release ReleaseRef `json:"release"`
	// ValidFrom is when the code was added; ValidTo when it was retired or superseded (the effective date of that release).
	ValidFrom    string `json:"valid_from"`
	ValidTo      string `json:"valid_to,omitempty"`
	ChangeType   string `json:"change_type"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// Supersedes lists the codes this one took over from.
	Supersedes []string `json:"supersedes,omitempty"`
	// PinnedRelease is set when the answer was asked for under one exact release.
	PinnedRelease string                 `json:"pinned_release,omitempty"`
	History       []ReferenceHistoryItem `json:"history,omitempty"`
}

// ResolveReference derives a code's state from its published changes in release order (oldest first), already limited to the
// date or release asked for. It returns nil when the code had not been added.
func ResolveReference(history []ReferenceHistoryItem) *ReferenceResolution {
	if len(history) == 0 || history[0].ChangeType != ChangeAdd {
		return nil
	}
	out := &ReferenceResolution{Metadata: map[string]any{}}
	last := history[len(history)-1]
	for _, h := range history {
		if h.ChangeType == ChangeAdd {
			out.ValidFrom = h.Release.EffectiveFrom
		}
		if strings.TrimSpace(h.Label) != "" {
			out.Label = h.Label
		}
		if h.ChangeType != ChangeRetire && h.ChangeType != ChangeSupersede {
			out.Metadata = h.Metadata
			if out.Metadata == nil {
				out.Metadata = map[string]any{}
			}
		}
		out.Origin = h.Origin
	}
	out.Status = CodeActive
	switch last.ChangeType {
	case ChangeRetire:
		out.Status, out.ValidTo = CodeRetired, last.Release.EffectiveFrom
	case ChangeSupersede:
		out.Status, out.ValidTo, out.SupersededBy = CodeSuperseded, last.Release.EffectiveFrom, last.ReplacedByCode
	}
	out.Release, out.ChangeType = last.Release, string(last.ChangeType)
	out.History = history
	return out
}

// ReferenceQuery is the query behind GET /v1/reference/{set}/{code}.
type ReferenceQuery struct {
	SetCode string
	Code    string
	AsOf    time.Time
	// Release pins the answer to one exact release version and those before it, whatever the date; empty for the date alone.
	Release string
	History bool
}

// Validate checks the query.
func (q ReferenceQuery) Validate() error {
	if !refSetCodeShape.MatchString(q.SetCode) {
		return fmt.Errorf("the set is a reference set code")
	}
	if !refCodeShape.MatchString(q.Code) {
		return fmt.Errorf("the code is not a valid reference code")
	}
	if q.Release != "" && !refVersionShape.MatchString(q.Release) {
		return fmt.Errorf("release is not a valid release version")
	}
	return nil
}
