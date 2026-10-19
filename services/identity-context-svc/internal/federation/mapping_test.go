package federation

import (
	"errors"
	"strings"
	"testing"
)

const (
	roleA   = "11111111-1111-4111-8111-111111111111"
	roleB   = "22222222-2222-4222-8222-222222222222"
	roleC   = "33333333-3333-4333-8333-333333333333"
	entity1 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	entity2 = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func entry(attr, val, role, ent string) MappingEntry {
	return MappingEntry{Attribute: attr, Value: val, RoleDefinitionID: role, LegalEntityID: ent}
}

func TestMappingValidation(t *testing.T) {
	ok := MappingConfig{Entries: []MappingEntry{entry("groups", "AP-Clerks", roleA, entity1)}}.Normalize()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*MappingConfig){
		"unknown attribute":                      func(c *MappingConfig) { c.Entries[0].Attribute = "memberOf" },
		"an attribute that names roles directly": func(c *MappingConfig) { c.Entries[0].Attribute = "role_definition_id" },
		"empty value":                            func(c *MappingConfig) { c.Entries[0].Value = "" },
		"long value":                             func(c *MappingConfig) { c.Entries[0].Value = strings.Repeat("x", 256) },
		"role not a uuid":                        func(c *MappingConfig) { c.Entries[0].RoleDefinitionID = "admin" },
		"entity not a uuid":                      func(c *MappingConfig) { c.Entries[0].LegalEntityID = "le-1" },
		"no entity":                              func(c *MappingConfig) { c.Entries[0].LegalEntityID = "" },
		"slo too small":                          func(c *MappingConfig) { c.DeprovisionSLOSeconds = 5 },
		"slo too large":                          func(c *MappingConfig) { c.DeprovisionSLOSeconds = 999999 },
		"too many roles per user":                func(c *MappingConfig) { c.MaxRolesPerUser = 500 },
		"duplicate entry":                        func(c *MappingConfig) { c.Entries = append(c.Entries, c.Entries[0]) },
		"too many entries": func(c *MappingConfig) {
			for i := 0; i < 201; i++ {
				c.Entries = append(c.Entries, entry("groups", "g"+strings.Repeat("x", i%50)+string(rune('a'+i%26))+string(rune('a'+i/26)), roleA, entity1))
			}
		},
	}
	for name, mut := range cases {
		c := MappingConfig{Entries: []MappingEntry{entry("groups", "AP-Clerks", roleA, entity1)}}.Normalize()
		mut(&c)
		if err := c.Normalize().Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// an empty mapping is valid: identities are provisioned with no access
	if err := (MappingConfig{}).Normalize().Validate(); err != nil {
		t.Fatalf("empty mapping: %v", err)
	}
}

func TestMappingFingerprintIsOrderIndependent(t *testing.T) {
	a := MappingConfig{Entries: []MappingEntry{entry("groups", "A", roleA, entity1), entry("department", "Fin", roleB, entity1)}}
	b := MappingConfig{Entries: []MappingEntry{entry("department", "Fin", roleB, entity1), entry("groups", "A", roleA, entity1)}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("a reordered mapping is the same mapping")
	}
	for name, mut := range map[string]func(*MappingConfig){
		"role":   func(c *MappingConfig) { c.Entries[0].RoleDefinitionID = roleC },
		"value":  func(c *MappingConfig) { c.Entries[0].Value = "B" },
		"entity": func(c *MappingConfig) { c.Entries[0].LegalEntityID = entity2 },
		"slo":    func(c *MappingConfig) { c.DeprovisionSLOSeconds = 60 },
		"extra":  func(c *MappingConfig) { c.Entries = append(c.Entries, entry("title", "Boss", roleC, entity1)) },
	} {
		c := MappingConfig{Entries: []MappingEntry{entry("groups", "A", roleA, entity1), entry("department", "Fin", roleB, entity1)}}
		mut(&c)
		if c.Fingerprint() == a.Fingerprint() {
			t.Errorf("a change of %s must change the fingerprint", name)
		}
	}
	// case of identifiers is normalised, case of directory values is not
	u := MappingConfig{Entries: []MappingEntry{entry("groups", "A", strings.ToUpper(roleA), entity1)}}
	l := MappingConfig{Entries: []MappingEntry{entry("groups", "A", roleA, entity1)}}
	if u.Fingerprint() != l.Fingerprint() {
		t.Fatal("ids compare case-insensitively")
	}
	v := MappingConfig{Entries: []MappingEntry{entry("groups", "a", roleA, entity1)}}
	if v.Fingerprint() == l.Fingerprint() {
		t.Fatal("directory values are exact")
	}
}

func TestEvaluate(t *testing.T) {
	cfg := MappingConfig{Entries: []MappingEntry{
		entry("groups", "AP-Clerks", roleA, entity1),
		entry("groups", "AP-Readers", roleB, entity1),
		entry("department", "Finance", roleA, entity1), // a second way to the same role
		entry("title", "Controller", roleC, entity2),
	}}.Normalize()

	d, ignored, ok := Evaluate(cfg, Attributes{"groups": {"AP-Clerks", "Everyone", "Admins"}, "department": {"Finance"}})
	if !ok || len(d) != 1 || d[0].RoleDefinitionID != roleA {
		t.Fatalf("two attributes entitling one role are one assignment: %+v ok=%v", d, ok)
	}
	if ignored != 2 {
		t.Fatalf("values that match nothing are ignored, not granted: %d", ignored)
	}

	// the directory cannot name a role: only the approved mapping can
	d, _, ok = Evaluate(cfg, Attributes{"groups": {"SystemAdministrator", "PLATFORM_ADMIN", roleC}, "roles": {"Admin"}})
	if !ok || len(d) != 0 {
		t.Fatalf("an attribute naming a privileged role, or a role id, yields nothing: %+v", d)
	}

	// the same role reached through a different attribute is the same assignment key
	d1, _, _ := Evaluate(cfg, Attributes{"groups": {"AP-Clerks"}})
	d2, _, _ := Evaluate(cfg, Attributes{"department": {"Finance"}})
	if len(d1) != 1 || len(d2) != 1 || d1[0].EntryKey != d2[0].EntryKey {
		t.Fatalf("keyed by role and entity, so changing the entitling attribute does not revoke and regrant: %+v %+v", d1, d2)
	}
	// but the same role in another entity is another assignment
	if AssignmentKey(roleA, entity1) == AssignmentKey(roleA, entity2) {
		t.Fatal("entity is part of the key")
	}

	// nothing in, nothing out
	if d, _, ok := Evaluate(cfg, nil); !ok || len(d) != 0 {
		t.Fatalf("%+v", d)
	}
	if d, _, ok := Evaluate(MappingConfig{}.Normalize(), Attributes{"groups": {"x"}}); !ok || len(d) != 0 {
		t.Fatalf("no mapping, no access: %+v", d)
	}
}

func TestEvaluateQuarantinesInsteadOfChoosing(t *testing.T) {
	cfg := MappingConfig{MaxRolesPerUser: 2, Entries: []MappingEntry{
		entry("groups", "g1", roleA, entity1), entry("groups", "g2", roleB, entity1), entry("groups", "g3", roleC, entity1),
	}}.Normalize()
	if _, _, ok := Evaluate(cfg, Attributes{"groups": {"g1", "g2"}}); !ok {
		t.Fatal("at the limit is fine")
	}
	d, _, ok := Evaluate(cfg, Attributes{"groups": {"g1", "g2", "g3"}})
	if ok || len(d) != 0 {
		t.Fatalf("over the limit is an ambiguity the caller quarantines, never 'the first N': %+v ok=%v", d, ok)
	}
}
