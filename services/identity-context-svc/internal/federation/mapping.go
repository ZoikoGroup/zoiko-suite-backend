package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Mapping statuses mirror connection versions.
const (
	MapPending    = VerPending
	MapApproved   = VerApproved
	MapRejected   = VerRejected
	MapSuperseded = VerSuperseded
)

// mappableAttributes are the directory attributes an approved mapping may read. Anything else a directory
// sends is ignored: an attribute is not a way to name a role.
var mappableAttributes = map[string]bool{"groups": true, "roles": true, "department": true, "title": true, "userType": true}

var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// MappingEntry says: a user whose attribute holds Value is entitled to the baseline role RoleDefinitionID in
// the legal entity LegalEntityID. The role is authoritative in access-control-svc, which also decides whether
// it is baseline: an entry naming a privileged role is refused when the mapping is configured, again when it
// is approved, and a final time by access control when the assignment is requested.
type MappingEntry struct {
	Attribute        string `json:"attribute"`
	Value            string `json:"value"`
	RoleDefinitionID string `json:"role_definition_id"`
	LegalEntityID    string `json:"legal_entity_id"`
}

// Key identifies an entry independent of its position, so a reordered mapping is the same mapping.
func (e MappingEntry) Key() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{e.Attribute, e.Value, e.RoleDefinitionID, e.LegalEntityID}, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// MappingConfig is one version of a connection's provisioning mapping.
type MappingConfig struct {
	Entries []MappingEntry `json:"entries"`
	// DeprovisionSLOSeconds is how long a deprovisioned person may keep any access before it is an incident.
	DeprovisionSLOSeconds int `json:"deprovision_slo_seconds"`
	// MaxRolesPerUser bounds what one directory record can obtain, however many attributes match.
	MaxRolesPerUser int `json:"max_roles_per_user"`
}

const (
	defaultDeprovisionSLO = 300
	defaultMaxRoles       = 10
	maxMappingEntries     = 200
)

// Normalize applies defaults and canonical ordering.
func (c MappingConfig) Normalize() MappingConfig {
	if c.DeprovisionSLOSeconds == 0 {
		c.DeprovisionSLOSeconds = defaultDeprovisionSLO
	}
	if c.MaxRolesPerUser == 0 {
		c.MaxRolesPerUser = defaultMaxRoles
	}
	entries := make([]MappingEntry, 0, len(c.Entries))
	for _, e := range c.Entries {
		e.Attribute, e.Value = strings.TrimSpace(e.Attribute), strings.TrimSpace(e.Value)
		e.RoleDefinitionID, e.LegalEntityID = strings.ToLower(strings.TrimSpace(e.RoleDefinitionID)), strings.ToLower(strings.TrimSpace(e.LegalEntityID))
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key() < entries[j].Key() })
	c.Entries = entries
	return c
}

// Validate checks structure only. Whether each role is baseline is access control's answer, asked by the
// service (it needs the network).
func (c MappingConfig) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }
	if len(c.Entries) > maxMappingEntries {
		return bad("a mapping may hold at most %d entries", maxMappingEntries)
	}
	if c.DeprovisionSLOSeconds < 30 || c.DeprovisionSLOSeconds > 86400 {
		return bad("deprovision_slo_seconds must be between 30 and 86400")
	}
	if c.MaxRolesPerUser < 1 || c.MaxRolesPerUser > 50 {
		return bad("max_roles_per_user must be between 1 and 50")
	}
	seen := map[string]bool{}
	for i, e := range c.Entries {
		if !mappableAttributes[e.Attribute] {
			return bad("entry %d: attribute must be one of groups, roles, department, title, userType", i)
		}
		if e.Value == "" || len(e.Value) > 255 {
			return bad("entry %d: value is required (at most 255 characters)", i)
		}
		if !uuidLike.MatchString(e.RoleDefinitionID) || !uuidLike.MatchString(e.LegalEntityID) {
			return bad("entry %d: role_definition_id and legal_entity_id must be UUIDs", i)
		}
		if seen[e.Key()] {
			return bad("entry %d duplicates another entry", i)
		}
		seen[e.Key()] = true
	}
	return nil
}

// Fingerprint is the SHA-256 of the canonical form: what an approver approves.
func (c MappingConfig) Fingerprint() string {
	b, _ := json.Marshal(c.Normalize())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RoleIDs returns the distinct roles an approved mapping can request.
func (c MappingConfig) RoleIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.Entries {
		if !seen[e.RoleDefinitionID] {
			seen[e.RoleDefinitionID] = true
			out = append(out, e.RoleDefinitionID)
		}
	}
	sort.Strings(out)
	return out
}

// MappingVersion is a stored version.
type MappingVersion struct {
	ConnectionID   string        `json:"connection_id"`
	TenantID       string        `json:"tenant_id"`
	Version        int           `json:"version"`
	Status         string        `json:"status"`
	Config         MappingConfig `json:"config"`
	Fingerprint    string        `json:"fingerprint"`
	CreatedBy      string        `json:"created_by"`
	CreatedAt      time.Time     `json:"created_at"`
	DecidedBy      string        `json:"decided_by,omitempty"`
	DecidedAt      *time.Time    `json:"decided_at,omitempty"`
	DecisionReason string        `json:"decision_reason,omitempty"`
}

// Desired is one assignment a directory record is entitled to under a mapping.
type Desired struct {
	EntryKey         string
	RoleDefinitionID string
	LegalEntityID    string
}

// Attributes are the directory attributes mappings read, as stored on a link.
type Attributes map[string][]string

// Evaluate returns the assignments a record is entitled to. It is pure: the same attributes and mapping give
// the same answer. Values that match no entry are simply not entitlements, which is also how an attempt to
// name a privileged group in the directory fails: a mapping lists what may be granted, the directory does not.
//
// More distinct roles than MaxRolesPerUser is not "grant the first N": it is an ambiguity, and the caller
// quarantines it (ok=false) instead of choosing for the directory.
func Evaluate(cfg MappingConfig, attrs Attributes) (desired []Desired, ignored int, ok bool) {
	have := map[string]map[string]bool{}
	total := 0
	for name, vals := range attrs {
		have[name] = map[string]bool{}
		for _, v := range vals {
			have[name][v] = true
			total++
		}
	}
	seen := map[string]bool{}
	matched := 0
	for _, e := range cfg.Entries {
		if have[e.Attribute][e.Value] {
			matched++
			// one assignment per role and entity, however many attributes name it
			k := e.RoleDefinitionID + "|" + e.LegalEntityID
			if !seen[k] {
				seen[k] = true
				desired = append(desired, Desired{EntryKey: AssignmentKey(e.RoleDefinitionID, e.LegalEntityID), RoleDefinitionID: e.RoleDefinitionID, LegalEntityID: e.LegalEntityID})
			}
		}
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].EntryKey < desired[j].EntryKey })
	ignored = total - matched
	if ignored < 0 {
		ignored = 0
	}
	if len(desired) > cfg.MaxRolesPerUser {
		return nil, ignored, false
	}
	return desired, ignored, true
}

// AssignmentKey identifies the assignment of one role in one entity. It is what a person's federation
// assignments are keyed by, so changing WHICH attribute entitles them to a role does not revoke and re-grant it.
func AssignmentKey(roleDefinitionID, legalEntityID string) string {
	sum := sha256.Sum256([]byte(roleDefinitionID + "\x1f" + legalEntityID))
	return hex.EncodeToString(sum[:8])
}
