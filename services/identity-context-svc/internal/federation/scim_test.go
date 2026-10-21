package federation

import (
	"errors"
	"strings"
	"testing"
)

const goodUser = `{
  "schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],
  "userName":"alice@corp.example","externalId":"dir-123","displayName":"Alice A",
  "emails":[{"value":"alice@corp.example","primary":true}],"active":true,
  "groups":[{"value":"g-1","display":"AP-Clerks"},{"value":"g-2","display":"Everyone"}],
  "title":"Clerk",
  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Finance"}
}`

func scimErr(t *testing.T, err error, typ string) {
	t.Helper()
	var se *SCIMError
	if !errors.As(err, &se) || se.Type != typ {
		t.Fatalf("want scim error %q, got %v", typ, err)
	}
}

func TestParseSCIMUser(t *testing.T) {
	u, err := ParseSCIMUser([]byte(goodUser))
	if err != nil {
		t.Fatal(err)
	}
	if u.UserName != "alice@corp.example" || u.ExternalID != "dir-123" || u.Email != "alice@corp.example" || !u.Active || u.DisplayName != "Alice A" {
		t.Fatalf("%+v", u)
	}
	if got := strings.Join(u.Attrs["groups"], ","); got != "AP-Clerks,Everyone,g-1,g-2" {
		t.Fatalf("a group is known by both its id and its name: %s", got)
	}
	if u.Attrs["department"][0] != "Finance" || u.Attrs["title"][0] != "Clerk" {
		t.Fatalf("%+v", u.Attrs)
	}
}

func TestParseSCIMUserRefuses(t *testing.T) {
	cases := map[string]string{
		"no externalId (userName is not a stable identity)": `{"userName":"a@b.example","emails":[{"value":"a@b.example"}]}`,
		"externalId with a space":                           `{"userName":"a@b.example","externalId":"a b","emails":[{"value":"a@b.example"}]}`,
		"no userName":                                       `{"externalId":"x","emails":[{"value":"a@b.example"}]}`,
		"no email at all":                                   `{"userName":"alice","externalId":"x"}`,
		"junk email":                                        `{"userName":"alice","externalId":"x","emails":[{"value":"not an email"}]}`,
		"not json":                                          `nope`,
		"too many groups":                                   `{"userName":"a@b.example","externalId":"x","groups":[` + strings.Repeat(`{"value":"g"},`, 201) + `{"value":"g"}]}`,
	}
	for name, body := range cases {
		if _, err := ParseSCIMUser([]byte(body)); err == nil {
			t.Errorf("%s: must be refused", name)
		} else {
			var se *SCIMError
			if !errors.As(err, &se) || se.Status != 400 {
				t.Errorf("%s: want a 400 SCIM error, got %v", name, err)
			}
		}
	}
	// email taken from an email-shaped userName when none is listed
	u, err := ParseSCIMUser([]byte(`{"userName":"bob@corp.example","externalId":"x"}`))
	if err != nil || u.Email != "bob@corp.example" {
		t.Fatalf("%+v %v", u, err)
	}
	// inactive is representable
	if u, err := ParseSCIMUser([]byte(`{"userName":"bob@corp.example","externalId":"x","active":false}`)); err != nil || u.Active {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestPatch(t *testing.T) {
	base, _ := ParseSCIMUser([]byte(goodUser))
	apply := func(ops string) (*SCIMUser, error) {
		p, err := ParsePatch([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":` + ops + `}`))
		if err != nil {
			return nil, err
		}
		return base.ApplyPatch(p)
	}

	// deactivation, in the forms directories send it
	for name, ops := range map[string]string{
		"path active":           `[{"op":"replace","path":"active","value":false}]`,
		"capitalised op":        `[{"op":"Replace","path":"active","value":false}]`,
		"pathless object":       `[{"op":"replace","value":{"active":false}}]`,
		"string False":          `[{"op":"replace","path":"active","value":"False"}]`,
		"qualified with schema": `[{"op":"replace","path":"urn:ietf:params:scim:schemas:core:2.0:User:active","value":false}]`,
	} {
		u, err := apply(ops)
		if err != nil || u.Active {
			t.Errorf("%s: %+v %v", name, u, err)
		}
	}

	// groups: add, remove by value, remove by selector, replace
	u, err := apply(`[{"op":"add","path":"groups","value":[{"value":"g-9","display":"Treasury"}]}]`)
	if err != nil || !contains(u.Attrs["groups"], "Treasury") || !contains(u.Attrs["groups"], "AP-Clerks") {
		t.Fatalf("add: %+v %v", u, err)
	}
	u, err = apply(`[{"op":"remove","path":"groups","value":[{"value":"g-1","display":"AP-Clerks"}]}]`)
	if err != nil || contains(u.Attrs["groups"], "AP-Clerks") || !contains(u.Attrs["groups"], "Everyone") {
		t.Fatalf("remove by value: %+v %v", u, err)
	}
	u, err = apply(`[{"op":"remove","path":"groups[value eq \"g-2\"]"}]`)
	if err != nil || contains(u.Attrs["groups"], "g-2") || !contains(u.Attrs["groups"], "g-1") {
		t.Fatalf("remove by selector: %+v %v", u, err)
	}
	u, err = apply(`[{"op":"replace","path":"groups","value":[{"value":"only","display":"Only"}]}]`)
	if err != nil || strings.Join(u.Attrs["groups"], ",") != "Only,only" {
		t.Fatalf("replace: %+v %v", u, err)
	}
	if u, err = apply(`[{"op":"remove","path":"groups"}]`); err != nil || len(u.Attrs["groups"]) != 0 {
		t.Fatalf("remove all: %+v %v", u, err)
	}

	// attributes
	u, err = apply(`[{"op":"replace","path":"displayName","value":"Alice B"},{"op":"replace","path":"title","value":"Lead"},
		{"op":"replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Treasury"},
		{"op":"replace","path":"emails[type eq \"work\"].value","value":"alice.b@corp.example"}]`)
	if err != nil || u.DisplayName != "Alice B" || u.Attrs["title"][0] != "Lead" || u.Attrs["department"][0] != "Treasury" || u.Email != "alice.b@corp.example" {
		t.Fatalf("%+v %v", u, err)
	}

	// what is refused, with the right scimType
	for name, tc := range map[string]struct{ ops, typ string }{
		"externalId is the identity":     {`[{"op":"replace","path":"externalId","value":"other"}]`, "mutability"},
		"a role cannot be named by path": {`[{"op":"add","path":"role_definition_id","value":"x"}]`, "invalidPath"},
		"an unknown path":                {`[{"op":"replace","path":"password","value":"x"}]`, "invalidPath"},
		"bad verb":                       {`[{"op":"move","path":"active","value":false}]`, "invalidSyntax"},
		"active must be boolean":         {`[{"op":"replace","path":"active","value":"maybe"}]`, "invalidValue"},
		"cannot remove active":           {`[{"op":"remove","path":"active"}]`, "mutability"},
		"selector only with remove":      {`[{"op":"replace","path":"groups[value eq \"g-1\"]","value":[]}]`, "invalidPath"},
		"bad email":                      {`[{"op":"replace","path":"emails","value":[{"value":"nope","primary":true}]}]`, "invalidValue"},
		"pathless needs an object":       {`[{"op":"replace","value":"x"}]`, "invalidValue"},
	} {
		if _, err := apply(tc.ops); err == nil {
			t.Errorf("%s: must be refused", name)
		} else {
			scimErr(t, err, tc.typ)
		}
	}

	// atomic: a patch that fails halfway changes nothing
	if _, err := apply(`[{"op":"replace","path":"displayName","value":"Changed"},{"op":"replace","path":"externalId","value":"x"}]`); err == nil {
		t.Fatal("the second operation is refused")
	}
	if base.DisplayName != "Alice A" {
		t.Fatalf("the original was modified: %q", base.DisplayName)
	}
	// and the original is never aliased by a patched copy
	u, _ = apply(`[{"op":"add","path":"groups","value":[{"value":"zzz"}]}]`)
	if contains(base.Attrs["groups"], "zzz") || u == base {
		t.Fatal("a patch must not alias the record it started from")
	}

	if _, err := ParsePatch([]byte(`{"Operations":[]}`)); err == nil {
		t.Fatal("no operations")
	}
	if _, err := ParsePatch([]byte(`x`)); err == nil {
		t.Fatal("not json")
	}
}

func TestFilter(t *testing.T) {
	for in, want := range map[string][2]string{
		`userName eq "alice@corp.example"`: {"username", "alice@corp.example"},
		`externalId eq "dir-123"`:          {"externalid", "dir-123"},
		` USERNAME EQ "x" `:                {"username", "x"},
	} {
		a, v, err := ParseFilter(in)
		if err != nil || a != want[0] || v != want[1] {
			t.Errorf("%q: %q %q %v", in, a, v, err)
		}
	}
	for _, bad := range []string{``, `userName co "a"`, `active eq true`, `userName eq "a" or userName eq "b"`, `userName eq "a" and active eq true`, `emails.value eq "x"`} {
		if _, _, err := ParseFilter(bad); err == nil {
			t.Errorf("%q must be refused, not guessed at", bad)
		} else {
			scimErr(t, err, "invalidFilter")
		}
	}
}

func TestSCIMResourceHasNoRoleOrSecret(t *testing.T) {
	l := Link{LinkID: "l-1", Subject: "dir-123", Username: "alice@corp.example", Status: LinkActive}
	res := SCIMResource(l, "Alice", "alice@corp.example", Attributes{"groups": {"g"}, "department": {"Fin"}}, "/scim/v2/t/c/Users/l-1")
	if res["id"] != "l-1" || res["externalId"] != "dir-123" || res["active"] != true {
		t.Fatalf("%+v", res)
	}
	l.Status = LinkDeprovisioned
	if SCIMResource(l, "A", "a@b.example", nil, "x")["active"] != false {
		t.Fatal("a deprovisioned person is inactive")
	}
	for k := range res {
		if strings.Contains(strings.ToLower(k), "role") || strings.Contains(strings.ToLower(k), "password") || strings.Contains(strings.ToLower(k), "token") {
			t.Fatalf("unexpected attribute %q", k)
		}
	}
}
