package domain

import (
	"encoding/json"
	"testing"
)

// The freshness window is optional. A profile without one must encode exactly as
// it did before the field existed, or every rule version already approved would
// fail the digest check the store makes on every load.
func TestCertificationProfile_WithoutAWindow_EncodesAsItAlwaysDid(t *testing.T) {
	b, err := json.Marshal(CertificationProfile{PermitAcceptedRisk: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"permit_accepted_risk":true}`; got != want {
		t.Fatalf("encoding = %s, want %s — the optional field must be omitted when absent", got, want)
	}
}

func TestCertificationProfile_WindowMustBePositive(t *testing.T) {
	pos, zero, neg := int64(60), int64(0), int64(-1)
	if err := (CertificationProfile{MaxEvidenceAgeSeconds: &pos}).Validate(); err != nil {
		t.Errorf("positive window refused: %v", err)
	}
	if err := (CertificationProfile{}).Validate(); err != nil {
		t.Errorf("absent window refused: %v", err)
	}
	for _, n := range []*int64{&zero, &neg} {
		if err := (CertificationProfile{MaxEvidenceAgeSeconds: n}).Validate(); err == nil {
			t.Errorf("window %d accepted", *n)
		}
	}
}

func TestRuleSetDigest_IsUnchangedByAnAbsentWindow(t *testing.T) {
	a := RuleSet{AmountScale: 2, CertificationProfile: CertificationProfile{PermitAcceptedRisk: true}}
	d1, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	win := int64(3600)
	a.CertificationProfile.MaxEvidenceAgeSeconds = &win
	d2, _ := a.Digest()
	if d1 == d2 {
		t.Fatal("declaring a window must change the rule set's digest — it is part of what is pinned")
	}
}

func TestCertifyRequest_Validate(t *testing.T) {
	id, sha := "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	bad := "nope"
	if err := (CertifyRequest{}).Validate(); err != nil {
		t.Errorf("empty request refused: %v", err)
	}
	if err := (CertifyRequest{LineageManifestID: &id, LineageManifestSHA256: &sha}).Validate(); err != nil {
		t.Errorf("valid reference refused: %v", err)
	}
	for name, r := range map[string]CertifyRequest{
		"id only":  {LineageManifestID: &id},
		"sha only": {LineageManifestSHA256: &sha},
		"bad id":   {LineageManifestID: &bad, LineageManifestSHA256: &sha},
		"bad sha":  {LineageManifestID: &id, LineageManifestSHA256: &bad},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
