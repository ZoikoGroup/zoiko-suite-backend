package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry/mapping"
)

// Change is one classified difference between a contract version and the version it supersedes.
type Change struct {
	Class string `json:"class"`
	Code  string `json:"code"`
	Path  string `json:"path"`
}

// Requirements are the obligations a classification places on publication (§6.4).
type Requirements struct {
	NewMajorVersion            bool `json:"new_major_version,omitempty"`
	MigrationPlan              bool `json:"migration_plan,omitempty"`
	Reauthorization            bool `json:"connection_reauthorization,omitempty"`
	Recertification            bool `json:"recertification,omitempty"`
	Reapproval                 bool `json:"reapproval,omitempty"`
	ReplayAnalysis             bool `json:"affected_domain_replay_analysis,omitempty"`
	SupportedUntil             bool `json:"supported_until,omitempty"`
	PinnedConnectionsInventory bool `json:"pinned_connections_inventory,omitempty"`
}

// CompatReport is the result of comparing a candidate version with its baseline.
type CompatReport struct {
	BaselineVersionID string       `json:"baseline_version_id,omitempty"`
	BaselineSemver    string       `json:"baseline_semver,omitempty"`
	Class             string       `json:"class"`
	Changes           []Change     `json:"changes"`
	Requires          Requirements `json:"requires"`
}

// Classify compares candidate to baseline (nil for a connector's first version) and returns the §6.4
// classification. Classification is by the strictest change found.
func Classify(baseline *ContractVersion, candidate *ContractVersion) *CompatReport {
	r := &CompatReport{Class: ClassNone, Changes: []Change{}}
	if baseline == nil {
		return r
	}
	r.BaselineVersionID, r.BaselineSemver = baseline.ContractVersionID, baseline.Semver
	add := func(class, code, path string) {
		r.Changes = append(r.Changes, Change{class, code, path})
		if classRank[class] > classRank[r.Class] {
			r.Class = class
		}
	}
	old, nu := &baseline.Content, &candidate.Content

	oldOps, newOps := opsByID(old), opsByID(nu)
	for _, id := range sortedKeys(oldOps) {
		o, ok := oldOps[id]
		if !ok {
			continue
		}
		n, still := newOps[id]
		path := "operations." + id
		if !still {
			add(ClassBreaking, "operation_removed", path)
			continue
		}
		if o.Direction != n.Direction || o.Channel != n.Channel || o.Method != n.Method {
			add(ClassBreaking, "operation_shape_changed", path)
		}
		if o.EndpointTemplate != n.EndpointTemplate {
			add(ClassBreaking, "operation_path_changed", path+".endpoint_template")
		}
		if o.ProviderIdempotency.Supported && !n.ProviderIdempotency.Supported {
			add(ClassBreaking, "provider_idempotency_dropped", path+".provider_idempotency")
		}
		if o.Irreversible != n.Irreversible {
			add(ClassMaterialBusiness, "irreversibility_changed", path+".irreversible")
		}
		if !o.Deprecated && n.Deprecated {
			add(ClassLifecycle, "operation_deprecated", path)
		}
		// the schemas an operation names
		for _, pair := range [][3]string{{"request_schema", o.RequestSchema, n.RequestSchema}, {"response_schema", o.ResponseSchema, n.ResponseSchema}} {
			if pair[1] != pair[2] {
				diffSchemaRefs(old, nu, pair[1], pair[2], path+"."+pair[0], add)
			}
		}
		// the authentication an operation uses
		if oa, na := profileOf(old, ProfileAuth, o.AuthProfile), profileOf(nu, ProfileAuth, n.AuthProfile); oa != nil && na != nil {
			var a, b authSpec
			if json.Unmarshal(oa.Spec, &a) == nil && json.Unmarshal(na.Spec, &b) == nil {
				if a.Mechanism != b.Mechanism || a.SignatureAlgorithm != b.SignatureAlgorithm || a.Audience != b.Audience {
					add(ClassMaterialSecurity, "auth_mechanism_changed", path+".auth_profile")
				}
				if !sameSet(a.ScopesRequired, b.ScopesRequired) {
					add(ClassMaterialSecurity, "required_scopes_changed", path+".auth_profile")
				}
			}
		}
		// the callback contract
		if oc, nc := profileOf(old, ProfileCallback, o.CallbackProfile), profileOf(nu, ProfileCallback, n.CallbackProfile); oc != nil || nc != nil {
			switch {
			case oc == nil || nc == nil:
				add(ClassMaterialSecurity, "callback_profile_changed", path+".callback_profile")
			default:
				var a, b callbackSpec
				if json.Unmarshal(oc.Spec, &a) == nil && json.Unmarshal(nc.Spec, &b) == nil &&
					(a.Mechanism != b.Mechanism || a.SignatureAlgorithm != b.SignatureAlgorithm) {
					// a new signing algorithm is a new version, never a silent fallback (NP-49)
					add(ClassMaterialSecurity, "callback_signing_changed", path+".callback_profile")
				}
			}
		}
		// where the data goes
		if oe, ne := profileOf(old, ProfileEndpoint, o.EndpointProfile), profileOf(nu, ProfileEndpoint, n.EndpointProfile); oe != nil && ne != nil {
			var a, b endpointSpec
			if json.Unmarshal(oe.Spec, &a) == nil && json.Unmarshal(ne.Spec, &b) == nil &&
				(!sameSet(a.Hosts, b.Hosts) || !sameSet(a.Regions, b.Regions) || !sameSet(a.Schemes, b.Schemes) || !sameInts(a.Ports, b.Ports) ||
					!sameSet(a.ApprovedRedirectHosts, b.ApprovedRedirectHosts) || a.PrivateEndpoint != b.PrivateEndpoint || a.Redirects != b.Redirects) {
				add(ClassMaterialEgress, "endpoint_changed", path+".endpoint_profile")
			}
		}
		if !sameSet(o.AllowedRegions, n.AllowedRegions) {
			add(ClassMaterialEgress, "data_regions_changed", path+".allowed_regions")
		}
	}
	for _, id := range sortedKeys(newOps) {
		if _, existed := oldOps[id]; !existed {
			add(ClassPotentiallyCompatible, "operation_added", "operations."+id)
		}
	}

	// mappings: the same mapping key now meaning something else is a business change
	oldMaps, newMaps := mappingHashes(old), mappingHashes(nu)
	for _, k := range sortedKeys(oldMaps) {
		h, ok := newMaps[k]
		if !ok {
			continue
		}
		if h != oldMaps[k] {
			add(ClassMaterialBusiness, "mapping_semantics_changed", "mappings."+k)
		}
	}

	finishRequirements(r, baseline, candidate)
	sort.SliceStable(r.Changes, func(i, j int) bool {
		if classRank[r.Changes[i].Class] != classRank[r.Changes[j].Class] {
			return classRank[r.Changes[i].Class] > classRank[r.Changes[j].Class]
		}
		return r.Changes[i].Path < r.Changes[j].Path
	})
	return r
}

func finishRequirements(r *CompatReport, baseline, candidate *ContractVersion) {
	var req Requirements
	for _, c := range r.Changes {
		switch c.Class {
		case ClassBreaking:
			req.NewMajorVersion, req.MigrationPlan, req.PinnedConnectionsInventory = true, true, true
		case ClassMaterialSecurity:
			req.Reauthorization, req.Recertification, req.Reapproval = true, true, true
		case ClassMaterialEgress:
			req.Reapproval, req.Recertification = true, true
		case ClassMaterialBusiness:
			req.ReplayAnalysis, req.Recertification = true, true
		case ClassLifecycle:
			req.SupportedUntil, req.PinnedConnectionsInventory, req.MigrationPlan = true, true, true
		}
	}
	r.Requires = req
}

// Enforce checks candidate against the requirements its classification places on it. It returns issues, not a
// verdict, so they join the rest of validation.
func (r *CompatReport) Enforce(baseline, candidate *ContractVersion) []Issue {
	var is []Issue
	add := func(p, rule string) { is = append(is, Issue{p, rule}) }
	if baseline == nil {
		return is
	}
	bm, bn, _ := parseSemver(baseline.Semver)
	cm, cn, _ := parseSemver(candidate.Semver)
	switch {
	case semverCompare(candidate.Semver, baseline.Semver) <= 0:
		add("semver", "must be greater than "+baseline.Semver)
	case r.Requires.NewMajorVersion && cm <= bm:
		add("semver", "a breaking change needs a new major version")
	case (r.Requires.Reauthorization || r.Requires.Reapproval || r.Requires.ReplayAnalysis) && cm == bm && cn <= bn:
		add("semver", "a material change needs at least a new minor version")
	}
	if r.Requires.MigrationPlan && candidate.MigrationPlanRef == "" {
		add("migration_plan_ref", "required: this change affects connections pinned to "+baseline.Semver)
	}
	if r.Requires.ReplayAnalysis && candidate.ReplayAnalysisRef == "" {
		add("replay_analysis_ref", "required: a mapping meaning changed, so affected domains must analyse replay")
	}
	if r.Requires.SupportedUntil {
		for _, c := range r.Changes {
			if c.Code != "operation_deprecated" {
				continue
			}
			id := strings.TrimPrefix(c.Path, "operations.")
			for _, o := range candidate.Content.Operations {
				if o.ID == id && (o.SupportedUntil == nil || !o.SupportedUntil.After(candidate.EffectiveFrom)) {
					add(c.Path+".supported_until", "must be after this version's effective_from")
				}
			}
		}
	}
	return is
}

// ── schema comparison ────────────────────────────────────────────────────────

func diffSchemaRefs(old, nu *Content, oldRef, newRef, path string, add func(class, code, path string)) {
	if oldRef == "" || newRef == "" {
		add(ClassBreaking, "operation_schema_added_or_removed", path)
		return
	}
	os, ns := schemaByRef(old, oldRef), schemaByRef(nu, newRef)
	if os == nil || ns == nil {
		add(ClassBreaking, "schema_unresolved", path)
		return
	}
	if idOf(oldRef) != idOf(newRef) {
		add(ClassBreaking, "schema_replaced", path)
		return
	}
	if os.MediaType != ns.MediaType {
		add(ClassBreaking, "media_type_changed", path)
	}
	if ns.MaxSizeBytes < os.MaxSizeBytes {
		add(ClassBreaking, "size_limit_lowered", path)
	}
	var a, b jsonSchema
	if json.Unmarshal(os.ValidationProfile, &a) != nil || json.Unmarshal(ns.ValidationProfile, &b) != nil {
		if string(os.ValidationProfile) != string(ns.ValidationProfile) {
			add(ClassBreaking, "format_profile_changed", path)
		}
		return
	}
	diffNode("$", &a, &b, path, add)
}

type jsonSchema struct {
	Type                 string                 `json:"type"`
	Required             []string               `json:"required"`
	Properties           map[string]*jsonSchema `json:"properties"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
	Items                *jsonSchema            `json:"items"`
	Enum                 []json.RawMessage      `json:"enum"`
	Pattern              string                 `json:"pattern"`
	MinLength            *int                   `json:"minLength"`
	MaxLength            *int                   `json:"maxLength"`
	Minimum              *json.Number           `json:"minimum"`
	Maximum              *json.Number           `json:"maximum"`
}

func diffNode(at string, a, b *jsonSchema, path string, add func(class, code, path string)) {
	p := path + "#" + at
	if a.Type != b.Type {
		add(ClassBreaking, "schema_type_changed", p)
		return
	}
	reqA, reqB := setOf(a.Required), setOf(b.Required)
	for name := range a.Properties {
		if _, ok := b.Properties[name]; !ok {
			// removing a field, or renaming it (which is a removal and an addition), breaks consumers
			add(ClassBreaking, "property_removed", p+"."+name)
		}
	}
	for name, pb := range b.Properties {
		pa, existed := a.Properties[name]
		if !existed {
			if reqB[name] {
				add(ClassBreaking, "required_property_added", p+"."+name)
			} else {
				add(ClassPotentiallyCompatible, "optional_property_added", p+"."+name)
			}
			continue
		}
		diffNode(at+"."+name, pa, pb, path, add)
	}
	for name := range reqB {
		if !reqA[name] {
			if _, existed := a.Properties[name]; existed {
				add(ClassBreaking, "property_became_required", p+"."+name)
			}
		}
	}
	for name := range reqA {
		if !reqB[name] {
			if _, still := b.Properties[name]; still {
				add(ClassPotentiallyCompatible, "property_became_optional", p+"."+name)
			}
		}
	}
	if (a.AdditionalProperties == nil || *a.AdditionalProperties) && b.AdditionalProperties != nil && !*b.AdditionalProperties {
		add(ClassBreaking, "additional_properties_closed", p)
	}
	if (a.AdditionalProperties != nil && !*a.AdditionalProperties) && (b.AdditionalProperties == nil || *b.AdditionalProperties) {
		add(ClassPotentiallyCompatible, "additional_properties_opened", p)
	}
	if a.Items != nil && b.Items != nil {
		diffNode(at+"[]", a.Items, b.Items, path, add)
	} else if (a.Items == nil) != (b.Items == nil) {
		add(ClassBreaking, "items_changed", p)
	}
	// enumerations
	if len(a.Enum) > 0 || len(b.Enum) > 0 {
		ea, eb := rawSet(a.Enum), rawSet(b.Enum)
		switch {
		case len(a.Enum) == 0 && len(b.Enum) > 0:
			add(ClassBreaking, "enum_introduced", p)
		case len(a.Enum) > 0 && len(b.Enum) == 0:
			add(ClassPotentiallyCompatible, "enum_removed_constraint", p)
		default:
			for v := range ea {
				if !eb[v] {
					add(ClassBreaking, "enum_value_removed", p)
					break
				}
			}
			for v := range eb {
				if !ea[v] {
					add(ClassPotentiallyCompatible, "enum_value_added", p)
					break
				}
			}
		}
	}
	// constraints: tightening breaks, loosening does not; a pattern cannot be compared, so any change breaks
	if a.Pattern != b.Pattern {
		add(ClassBreaking, "pattern_changed", p)
	}
	if tighterMin(a.MinLength, b.MinLength) || tighterMax(a.MaxLength, b.MaxLength) {
		add(ClassBreaking, "length_constraint_tightened", p)
	}
	if numTighterMin(a.Minimum, b.Minimum) || numTighterMax(a.Maximum, b.Maximum) {
		add(ClassBreaking, "range_constraint_tightened", p)
	}
}

func tighterMin(a, b *int) bool { return b != nil && (a == nil || *b > *a) }
func tighterMax(a, b *int) bool { return b != nil && (a == nil || *b < *a) }

func numTighterMin(a, b *json.Number) bool {
	if b == nil {
		return false
	}
	if a == nil {
		return true
	}
	return cmpNum(*b, *a) > 0
}
func numTighterMax(a, b *json.Number) bool {
	if b == nil {
		return false
	}
	if a == nil {
		return true
	}
	return cmpNum(*b, *a) < 0
}

func cmpNum(x, y json.Number) int {
	fx, errx := strconv.ParseFloat(string(x), 64)
	fy, erry := strconv.ParseFloat(string(y), 64)
	if errx != nil || erry != nil {
		return 1 // unparseable: treat as changed in the strict direction
	}
	switch {
	case fx < fy:
		return -1
	case fx > fy:
		return 1
	}
	return 0
}

// ── helpers ──────────────────────────────────────────────────────────────────

func opsByID(c *Content) map[string]*Operation {
	m := map[string]*Operation{}
	for i := range c.Operations {
		m[c.Operations[i].ID] = &c.Operations[i]
	}
	return m
}

func schemaByRef(c *Content, ref string) *Schema {
	for i := range c.Schemas {
		if c.Schemas[i].Ref == ref {
			return &c.Schemas[i]
		}
	}
	return nil
}

func profileOf(c *Content, kind, name string) *Profile {
	if name == "" {
		return nil
	}
	for i := range c.Profiles {
		if c.Profiles[i].Kind == kind && c.Profiles[i].Name == name {
			return &c.Profiles[i]
		}
	}
	return nil
}

func mappingHashes(c *Content) map[string]string {
	out := map[string]string{}
	for _, m := range c.Mappings {
		if comp, err := mapping.CompileJSON(m.Spec); err == nil {
			out[m.Key+"@"+strconv.Itoa(m.Version)] = comp.Hash()
		}
	}
	return out
}

func idOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func setOf(l []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range l {
		m[s] = true
	}
	return m
}

func rawSet(l []json.RawMessage) map[string]bool {
	m := map[string]bool{}
	for _, s := range l {
		m[string(s)] = true
	}
	return m
}

func sameSet(a, b []string) bool {
	x, y := setOf(a), setOf(b)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

func sameInts(a, b []int) bool {
	x, y := map[int]bool{}, map[int]bool{}
	for _, v := range a {
		x[v] = true
	}
	for _, v := range b {
		y[v] = true
	}
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

func parseSemver(s string) (major, minor int, ok bool) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// semverCompare orders two MAJOR.MINOR.PATCH strings numerically (so 1.10.0 > 1.9.0).
func semverCompare(a, b string) int {
	pa, pb := semverRE.FindStringSubmatch(a), semverRE.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// DescribeChanges renders a report's changes for an operator, without values.
func (r *CompatReport) DescribeChanges() string {
	parts := make([]string, 0, len(r.Changes))
	for _, c := range r.Changes {
		parts = append(parts, fmt.Sprintf("%s %s at %s", c.Class, c.Code, c.Path))
	}
	return strings.Join(parts, "; ")
}

var _ = time.Second
