package federation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// A deliberately small SCIM 2.0 (RFC 7643/7644) User resource: what a directory needs to create, update and
// deactivate people, and nothing that could name a role. Attributes outside the set below are ignored.

const (
	scimUserSchema       = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimEnterpriseSchema = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	scimErrorSchema      = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimListSchema       = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimPatchSchema      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

	maxSCIMValues = 200
	maxSCIMString = 255
)

// SCIMError is an RFC 7644 error: its status and scimType are what a directory acts on.
type SCIMError struct {
	Status int
	Type   string
	Detail string
}

func (e *SCIMError) Error() string { return fmt.Sprintf("scim %d %s: %s", e.Status, e.Type, e.Detail) }

func scimBad(t, f string, a ...any) *SCIMError {
	return &SCIMError{Status: http.StatusBadRequest, Type: t, Detail: fmt.Sprintf(f, a...)}
}

// Body renders the RFC 7644 error document.
func (e *SCIMError) Body() map[string]any {
	b := map[string]any{"schemas": []string{scimErrorSchema}, "status": fmt.Sprint(e.Status), "detail": e.Detail}
	if e.Type != "" {
		b["scimType"] = e.Type
	}
	return b
}

// SCIMUser is the part of a directory record provisioning acts on.
type SCIMUser struct {
	UserName    string
	ExternalID  string
	DisplayName string
	Email       string
	Active      bool
	Attrs       Attributes
}

type scimValue struct {
	Value   string `json:"value"`
	Display string `json:"display"`
	Primary bool   `json:"primary"`
	Type    string `json:"type"`
}

type scimUserDoc struct {
	Schemas     []string `json:"schemas"`
	UserName    string   `json:"userName"`
	ExternalID  string   `json:"externalId"`
	DisplayName string   `json:"displayName"`
	Name        *struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails   []scimValue     `json:"emails"`
	Active   *bool           `json:"active"`
	Groups   []scimValue     `json:"groups"`
	Roles    []scimValue     `json:"roles"`
	Title    string          `json:"title"`
	UserType string          `json:"userType"`
	Ent      json.RawMessage `json:"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"`
}

var emailShape = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]{1,255}$`)

// ParseSCIMUser reads a User resource for create or replace.
func ParseSCIMUser(raw []byte) (*SCIMUser, error) {
	var d scimUserDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, scimBad("invalidSyntax", "the request body is not a SCIM User")
	}
	u := &SCIMUser{Active: true, Attrs: Attributes{}}
	u.UserName = strings.TrimSpace(d.UserName)
	if u.UserName == "" || len(u.UserName) > 320 {
		return nil, scimBad("invalidValue", "userName is required")
	}
	u.ExternalID = strings.TrimSpace(d.ExternalID)
	if !validSubject(u.ExternalID) {
		// userName is a login handle that can be renamed or reassigned; only externalId is the directory's
		// stable identifier, so without one there is nothing safe to bind an identity to
		return nil, scimBad("invalidValue", "externalId (the directory's stable identifier) is required")
	}
	if d.Active != nil {
		u.Active = *d.Active
	}
	u.DisplayName = strings.TrimSpace(d.DisplayName)
	if u.DisplayName == "" && d.Name != nil {
		u.DisplayName = strings.TrimSpace(d.Name.Formatted)
		if u.DisplayName == "" {
			u.DisplayName = strings.TrimSpace(d.Name.GivenName + " " + d.Name.FamilyName)
		}
	}
	if u.DisplayName == "" {
		u.DisplayName = u.UserName
	}
	if len(u.DisplayName) > maxSCIMString {
		return nil, scimBad("invalidValue", "displayName is too long")
	}
	for _, e := range d.Emails {
		if e.Primary || u.Email == "" {
			if v := strings.TrimSpace(e.Value); v != "" {
				u.Email = v
			}
		}
	}
	if u.Email == "" && strings.Contains(u.UserName, "@") {
		u.Email = u.UserName
	}
	if !emailShape.MatchString(u.Email) {
		return nil, scimBad("invalidValue", "an email address is required (emails or an email-shaped userName)")
	}
	if err := u.setValues("groups", d.Groups); err != nil {
		return nil, err
	}
	if err := u.setValues("roles", d.Roles); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"title": d.Title, "userType": d.UserType} {
		if err := u.setString(name, v); err != nil {
			return nil, err
		}
	}
	if len(d.Ent) > 0 {
		var ent struct {
			Department string `json:"department"`
		}
		if json.Unmarshal(d.Ent, &ent) == nil {
			if err := u.setString("department", ent.Department); err != nil {
				return nil, err
			}
		}
	}
	return u, nil
}

func (u *SCIMUser) setString(name, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		delete(u.Attrs, name)
		return nil
	}
	if len(v) > maxSCIMString {
		return scimBad("invalidValue", "%s is too long", name)
	}
	u.Attrs[name] = []string{v}
	return nil
}

// setValues records both the value and the display of each member: a directory may identify a group by either,
// and the mapping names whichever the tenant chose.
func (u *SCIMUser) setValues(name string, vs []scimValue) error {
	if len(vs) > maxSCIMValues {
		return scimBad("invalidValue", "too many %s values", name)
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range vs {
		for _, s := range []string{strings.TrimSpace(v.Value), strings.TrimSpace(v.Display)} {
			if s == "" || seen[s] {
				continue
			}
			if len(s) > maxSCIMString {
				return scimBad("invalidValue", "a %s value is too long", name)
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		delete(u.Attrs, name)
	} else {
		sort.Strings(out)
		u.Attrs[name] = out
	}
	return nil
}

// Clone copies a user so a patch can be validated before it is applied.
func (u *SCIMUser) Clone() *SCIMUser {
	c := *u
	c.Attrs = Attributes{}
	for k, v := range u.Attrs {
		c.Attrs[k] = append([]string(nil), v...)
	}
	return &c
}

// PatchOp is one RFC 7644 PATCH operation.
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// ParsePatch reads a PatchOp request.
func ParsePatch(raw []byte) ([]PatchOp, error) {
	var req struct {
		Schemas    []string  `json:"schemas"`
		Operations []PatchOp `json:"Operations"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Operations) == 0 {
		return nil, scimBad("invalidSyntax", "the request body is not a SCIM PatchOp with at least one operation")
	}
	if len(req.Operations) > 50 {
		return nil, scimBad("invalidSyntax", "too many operations")
	}
	return req.Operations, nil
}

var valueSelector = regexp.MustCompile(`(?i)^(groups|roles)\[\s*value\s+eq\s+"([^"]{1,255})"\s*\]$`)

// ApplyPatch applies the operations to a copy and returns it; the caller keeps the original on any error, so a
// patch that fails halfway changes nothing.
func (u *SCIMUser) ApplyPatch(ops []PatchOp) (*SCIMUser, error) {
	c := u.Clone()
	for i, op := range ops {
		if err := c.applyOne(op); err != nil {
			if se, ok := err.(*SCIMError); ok {
				se.Detail = fmt.Sprintf("operation %d: %s", i, se.Detail)
			}
			return nil, err
		}
	}
	return c, nil
}

func (u *SCIMUser) applyOne(op PatchOp) error {
	verb := strings.ToLower(strings.TrimSpace(op.Op))
	if verb != "add" && verb != "replace" && verb != "remove" {
		return scimBad("invalidSyntax", "op must be add, replace or remove")
	}
	path := strings.TrimSpace(op.Path)
	if path == "" {
		// no path: the value is an object of attributes (the form some directories send for "active")
		if verb == "remove" || len(op.Value) == 0 {
			return scimBad("noTarget", "a path is required")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(op.Value, &obj); err != nil {
			return scimBad("invalidValue", "a pathless operation needs an object value")
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := u.applyOne(PatchOp{Op: verb, Path: k, Value: obj[k]}); err != nil {
				return err
			}
		}
		return nil
	}
	path = strings.TrimPrefix(path, scimUserSchema+":")
	lower := strings.ToLower(path)

	if m := valueSelector.FindStringSubmatch(path); m != nil {
		if verb != "remove" {
			return scimBad("invalidPath", "a value selector is supported with remove only")
		}
		name := strings.ToLower(m[1])
		var keep []string
		for _, v := range u.Attrs[name] {
			if v != m[2] {
				keep = append(keep, v)
			}
		}
		u.setAttr(name, keep)
		return nil
	}

	switch {
	case lower == "active":
		if verb == "remove" {
			return scimBad("mutability", "active cannot be removed")
		}
		var b bool
		if err := json.Unmarshal(op.Value, &b); err != nil {
			// some directories send the string "False"
			var s string
			if json.Unmarshal(op.Value, &s) != nil || (strings.ToLower(s) != "true" && strings.ToLower(s) != "false") {
				return scimBad("invalidValue", "active must be a boolean")
			}
			b = strings.ToLower(s) == "true"
		}
		u.Active = b
	case lower == "username":
		return u.setScalar(verb, op.Value, func(s string) { u.UserName = s }, "userName")
	case lower == "displayname":
		return u.setScalar(verb, op.Value, func(s string) { u.DisplayName = s }, "displayName")
	case lower == "externalid":
		return scimBad("mutability", "externalId is the directory's stable identifier and cannot change")
	case lower == "emails" || strings.HasPrefix(lower, "emails["):
		if verb == "remove" {
			return scimBad("mutability", "an email address cannot be removed")
		}
		return u.patchEmail(op.Value)
	case lower == "name" || strings.HasPrefix(lower, "name."):
		return nil // composed into displayName by the directory; not an attribute of ours
	case lower == "groups" || lower == "roles":
		return u.patchValues(lower, verb, op.Value)
	case lower == "title" || lower == "usertype":
		name := "title"
		if lower == "usertype" {
			name = "userType"
		}
		return u.patchString(name, verb, op.Value)
	case lower == "department" || lower == strings.ToLower(scimEnterpriseSchema)+":department":
		return u.patchString("department", verb, op.Value)
	default:
		return scimBad("invalidPath", "the path %q is not supported", path)
	}
	return nil
}

func (u *SCIMUser) setAttr(name string, vals []string) {
	if len(vals) == 0 {
		delete(u.Attrs, name)
		return
	}
	sort.Strings(vals)
	u.Attrs[name] = vals
}

func (u *SCIMUser) setScalar(verb string, raw json.RawMessage, set func(string), name string) error {
	if verb == "remove" {
		return scimBad("mutability", "%s cannot be removed", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" || len(s) > 320 {
		return scimBad("invalidValue", "%s must be a non-empty string", name)
	}
	set(strings.TrimSpace(s))
	return nil
}

func (u *SCIMUser) patchEmail(raw json.RawMessage) error {
	var list []scimValue
	if json.Unmarshal(raw, &list) != nil {
		var one scimValue
		if json.Unmarshal(raw, &one) != nil {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return scimBad("invalidValue", "emails value is not understood")
			}
			one.Value = s
		}
		list = []scimValue{one}
	}
	for _, e := range list {
		if v := strings.TrimSpace(e.Value); v != "" && (e.Primary || len(list) == 1 || u.Email == "") {
			if !emailShape.MatchString(v) {
				return scimBad("invalidValue", "not an email address")
			}
			u.Email = v
		}
	}
	return nil
}

func (u *SCIMUser) patchString(name, verb string, raw json.RawMessage) error {
	if verb == "remove" {
		delete(u.Attrs, name)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return scimBad("invalidValue", "%s must be a string", name)
	}
	return u.setString(name, s)
}

func (u *SCIMUser) patchValues(name, verb string, raw json.RawMessage) error {
	if verb == "remove" && len(raw) == 0 {
		delete(u.Attrs, name)
		return nil
	}
	var vs []scimValue
	if err := json.Unmarshal(raw, &vs); err != nil {
		var one scimValue
		if json.Unmarshal(raw, &one) != nil {
			return scimBad("invalidValue", "%s must be a list of values", name)
		}
		vs = []scimValue{one}
	}
	tmp := &SCIMUser{Attrs: Attributes{}}
	if err := tmp.setValues(name, vs); err != nil {
		return err
	}
	given := tmp.Attrs[name]
	cur := u.Attrs[name]
	switch verb {
	case "replace":
		u.setAttr(name, given)
	case "add":
		have := map[string]bool{}
		for _, v := range cur {
			have[v] = true
		}
		merged := append([]string(nil), cur...)
		for _, v := range given {
			if !have[v] {
				merged = append(merged, v)
			}
		}
		if len(merged) > maxSCIMValues {
			return scimBad("invalidValue", "too many %s values", name)
		}
		u.setAttr(name, merged)
	case "remove":
		drop := map[string]bool{}
		for _, v := range given {
			drop[v] = true
		}
		var keep []string
		for _, v := range cur {
			if !drop[v] {
				keep = append(keep, v)
			}
		}
		u.setAttr(name, keep)
	}
	return nil
}

// ── filters and resources ────────────────────────────────────────────────────

var filterEq = regexp.MustCompile(`(?i)^\s*(userName|externalId)\s+eq\s+"([^"]{1,320})"\s*$`)

// ParseFilter understands `userName eq "x"` and `externalId eq "x"`, which is what directories use to look a
// person up before creating them. Anything else is refused rather than guessed at.
func ParseFilter(f string) (attr, value string, err error) {
	m := filterEq.FindStringSubmatch(f)
	if m == nil {
		return "", "", scimBad("invalidFilter", "only `userName eq \"…\"` and `externalId eq \"…\"` filters are supported")
	}
	return strings.ToLower(m[1]), m[2], nil
}

// SCIMResource renders a person as a SCIM User.
func SCIMResource(l Link, displayName, email string, attrs Attributes, location string) map[string]any {
	res := map[string]any{
		"schemas":     []string{scimUserSchema, scimEnterpriseSchema},
		"id":          l.LinkID,
		"externalId":  l.Subject,
		"userName":    l.Username,
		"displayName": displayName,
		"emails":      []map[string]any{{"value": email, "primary": true}},
		"active":      l.Status == LinkActive,
		"meta":        map[string]any{"resourceType": "User", "created": l.CreatedAt, "location": location},
	}
	for _, name := range []string{"groups", "roles"} {
		if vs := attrs[name]; len(vs) > 0 {
			var list []map[string]any
			for _, v := range vs {
				list = append(list, map[string]any{"value": v})
			}
			res[name] = list
		}
	}
	for _, name := range []string{"title", "userType"} {
		if vs := attrs[name]; len(vs) > 0 {
			res[name] = vs[0]
		}
	}
	if vs := attrs["department"]; len(vs) > 0 {
		res[scimEnterpriseSchema] = map[string]any{"department": vs[0]}
	}
	return res
}
