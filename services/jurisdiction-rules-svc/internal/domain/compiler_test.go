package domain

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

func baseInput() CompileInput {
	m := PackManifest{PackID: "jur.gb.tax.core", PackVersion: "2026.08.1", JurisdictionIDs: []string{"GB"}, Regimes: []string{"VAT"},
		EffectiveFrom: "2026-08-01", SourceRegisterVersion: ptr(14)}
	return CompileInput{
		PackRef: "jur.gb.tax.core", Version: "2026.08.1", Manifest: m, ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Jurisdictions: []Named{{ID: "j-gb", Code: "GB"}}, Regimes: []Named{{ID: "r-vat", Code: "VAT"}},
		Rules: []CompileRule{{
			RuleID: "rule-1", JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: "STD", RuleName: "Standard",
			EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"applies":["B2C"],"rate":"0.2000"}`),
			RuleStatus: "ACTIVE", RegimeID: ptr("r-vat"), InterpretationID: ptr("i-1"), SourceIDs: []string{"s-1"}, Precedence: ptr(1)}},
		Sources: []CompileSource{{SourceID: "s-1", JurisdictionID: "j-gb", Authority: "HMRC", SourceType: "STATUTE", AuthorityLevel: "BINDING_LAW",
			Title: "VAT Act", Location: "https://x", SnapshotHash: "sha256:" + strings.Repeat("b", 64), Language: "en", Reviewed: true}},
		Interpretations: []CompileInterpretation{{InterpretationID: "i-1", JurisdictionID: "j-gb", Subject: "s", Decision: "d", Rationale: "r",
			Approved: true, SourceIDs: []string{"s-1"}}},
	}
}

func codes(r CompileReport, sev string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity == sev {
			out = append(out, f.Code)
		}
	}
	return out
}

func TestCompile_CleanInputProducesReproducibleArtifact(t *testing.T) {
	a := Compile(baseInput())
	require.False(t, a.Report.HasErrors(), "%+v", a.Report.Findings)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, a.ArtifactDigest)
	assert.False(t, a.Report.TestsExecuted, "the compiler never claims to have run the certification tests")

	// Same inputs, different slice order: identical bytes and digest.
	in := baseInput()
	in.Rules = append(in.Rules, CompileRule{RuleID: "rule-0", JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: "ZERO", RuleName: "Zero",
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{}`), RuleStatus: "ACTIVE",
		RegimeID: ptr("r-vat"), InterpretationID: ptr("i-1"), SourceIDs: []string{"s-1"}})
	x := Compile(in)
	in.Rules[0], in.Rules[1] = in.Rules[1], in.Rules[0]
	y := Compile(in)
	require.False(t, x.Report.HasErrors())
	assert.Equal(t, x.ArtifactDigest, y.ArtifactDigest)
	assert.Equal(t, string(x.ArtifactJSON), string(y.ArtifactJSON))

	// Any content change changes the digest; the status of a rule does not.
	in2 := baseInput()
	in2.Rules[0].Payload = json.RawMessage(`{"applies":["B2B"],"rate":"0.2000"}`)
	assert.NotEqual(t, a.ArtifactDigest, Compile(in2).ArtifactDigest)
	in3 := baseInput()
	in3.Rules[0].RuleStatus = "SUPERSEDED"
	assert.Equal(t, a.ArtifactDigest, Compile(in3).ArtifactDigest, "later lifecycle changes do not alter a compiled artifact")

	var art map[string]any
	require.NoError(t, json.Unmarshal(a.ArtifactJSON, &art))
	assert.Equal(t, ArtifactFormat, art["format"])
	assert.Contains(t, art, "sbom")
	assert.NotContains(t, art, "compiled_at", "no clock inside the digested content")
}

func TestCompile_RejectsEachViolation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CompileInput)
		code   string
	}{
		{"identity mismatch", func(in *CompileInput) { in.Version = "2026.08.2" }, "JUR-C001"},
		{"missing rule module", func(in *CompileInput) { in.MissingRuleIDs = []string{"ghost"} }, "JUR-C020"},
		{"rule outside scope", func(in *CompileInput) { in.Rules[0].JurisdictionID = "j-other" }, "JUR-C022"},
		{"no regime", func(in *CompileInput) { in.Rules[0].RegimeID = nil }, "JUR-C023"},
		{"regime outside scope", func(in *CompileInput) { in.Rules[0].RegimeID = ptr("r-other") }, "JUR-C023"},
		{"no sources", func(in *CompileInput) { in.Rules[0].SourceIDs = nil }, "JUR-C024"},
		{"no interpretation", func(in *CompileInput) { in.Rules[0].InterpretationID = nil }, "JUR-C025"},
		{"unapproved interpretation (JUR-NEG-18)", func(in *CompileInput) { in.Interpretations[0].Approved = false }, "JUR-C026"},
		{"unreviewed source", func(in *CompileInput) { in.Sources[0].Reviewed = false }, "JUR-C027"},
		{"superseded source", func(in *CompileInput) { in.Sources[0].Superseded = true }, "JUR-C027"},
		{"unknown source", func(in *CompileInput) { in.Rules[0].SourceIDs = []string{"nope"} }, "JUR-C027"},
		{"retired rule", func(in *CompileInput) { in.Rules[0].RuleStatus = "RETIRED" }, "JUR-C030"},
		{"float in payload (JUR-NEG-25)", func(in *CompileInput) { in.Rules[0].Payload = json.RawMessage(`{"rate":0.2}`) }, "JUR-C050"},
		{"exponent in payload", func(in *CompileInput) { in.Rules[0].Payload = json.RawMessage(`{"n":[1,{"x":1e3}]}`) }, "JUR-C050"},
		{"self dependency", func(in *CompileInput) {
			in.Dependencies = []CompileDependency{{Ref: "jur.gb.tax.core", Version: "1", Group: "dependencies"}}
		}, "JUR-C060"},
		{"missing dependency pack", func(in *CompileInput) {
			in.Dependencies = []CompileDependency{{Ref: "global.tax.core", Version: "3.4", Group: "dependencies", IsPack: true}}
		}, "JUR-C061"},
		{"withdrawn dependency", func(in *CompileInput) {
			in.Dependencies = []CompileDependency{{Ref: "global.tax.core", Version: "3.4", Group: "dependencies", IsPack: true, Found: true, Status: "WITHDRAWN", ArtifactDigest: ptr("sha256:x")}}
		}, "JUR-C062"},
		{"uncompiled dependency", func(in *CompileInput) {
			in.Dependencies = []CompileDependency{{Ref: "global.tax.core", Version: "3.4", Group: "dependencies", IsPack: true, Found: true, Status: "REVIEW"}}
		}, "JUR-C063"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.mutate(&in)
			out := Compile(in)
			assert.True(t, out.Report.HasErrors())
			assert.Contains(t, codes(out.Report, SeverityError), c.code)
			assert.Empty(t, out.ArtifactJSON, "no artifact is produced from a failing compile")
			assert.Empty(t, out.ArtifactDigest)
		})
	}
}

func TestCompile_OverlapNeedsDeclaredResolution(t *testing.T) {
	mk := func(id string, from, to *time.Time, prec *int, supersedes *string) CompileRule {
		r := baseInput().Rules[0]
		r.RuleID, r.Precedence, r.SupersedesRuleID = id, prec, supersedes
		r.EffectiveFrom = *from
		r.EffectiveTo = to
		return r
	}
	d := func(m int) *time.Time { t := time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC); return &t }

	run := func(a, b CompileRule) CompileReport {
		in := baseInput()
		in.Rules = []CompileRule{a, b}
		return Compile(in).Report
	}
	// Overlap, equal precedence: JUR-NEG-02.
	r := run(mk("a", d(1), nil, ptr(1), nil), mk("b", d(3), nil, ptr(1), nil))
	assert.Contains(t, codes(r, SeverityError), "JUR-C040")
	// Overlap, no precedence declared at all.
	r = run(mk("a", d(1), nil, nil, nil), mk("b", d(3), nil, nil, nil))
	assert.Contains(t, codes(r, SeverityError), "JUR-C040")
	// Overlap with distinct declared precedence is a legitimate layering.
	r = run(mk("a", d(1), nil, ptr(1), nil), mk("b", d(3), nil, ptr(2), nil))
	assert.NotContains(t, codes(r, SeverityError), "JUR-C040")
	// Explicit supersedes link also resolves it.
	r = run(mk("a", d(1), nil, nil, nil), mk("b", d(3), nil, nil, ptr("a")))
	assert.NotContains(t, codes(r, SeverityError), "JUR-C040")
	// Back-to-back (half-open) intervals do not overlap: the old rule ends the day the new one starts.
	r = run(mk("a", d(1), d(3), nil, nil), mk("b", d(3), nil, nil, nil))
	assert.NotContains(t, codes(r, SeverityError), "JUR-C040")
	// Different rule codes never conflict.
	a, b := mk("a", d(1), nil, nil, nil), mk("b", d(1), nil, nil, nil)
	b.RuleCode = "OTHER"
	assert.NotContains(t, codes(run(a, b), SeverityError), "JUR-C040")
}

func TestCompile_Warnings(t *testing.T) {
	in := baseInput()
	in.Rules[0].RuleStatus = "DRAFT"
	in.Sources[0].AuthorityLevel = "GUIDANCE"
	in.Dependencies = []CompileDependency{{Ref: "ref.iso4217", Version: "2026.06", Group: "dependencies"}}
	out := Compile(in)
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	w := codes(out.Report, SeverityWarning)
	assert.Contains(t, w, "JUR-W031", "DRAFT rule")
	assert.Contains(t, w, "JUR-W030", "non-binding sources only (JUR-NEG-23)")
	assert.Contains(t, w, "JUR-W060", "external dependency cannot be verified")

	empty := baseInput()
	empty.Rules, empty.Sources, empty.Interpretations = nil, nil, nil
	assert.Contains(t, codes(Compile(empty).Report, SeverityWarning), "JUR-W010")
}

func TestFloatLint(t *testing.T) {
	assert.Equal(t, "", floatInJSON([]byte(`{"a":1,"b":[2,3],"c":"0.20","d":12345678901234567890}`)))
	assert.NotEqual(t, "", floatInJSON([]byte(`{"a":{"b":[1,2.5]}}`)))
	assert.NotEqual(t, "", floatInJSON([]byte(`{"a":1E5}`)))
	assert.Equal(t, "", floatInJSON(nil))
}

// ── signing ─────────────────────────────────────────────────────────────────

func signedFixture(t *testing.T) (VerifyInput, ed25519.PrivateKey) {
	t.Helper()
	out := Compile(baseInput())
	require.False(t, out.Report.HasErrors())
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	s, err := NewEd25519Signer("pack-key-1", priv)
	require.NoError(t, err)
	sig, err := s.Sign(SigningMessage(out.ArtifactDigest))
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(sig)
	return VerifyInput{
		ArtifactJSON: string(out.ArtifactJSON), StoredDigest: out.ArtifactDigest, VersionDigest: out.ArtifactDigest,
		Signature: &b64, SignatureKeyID: ptr("pack-key-1"),
		Key: &TrustedKey{KeyRef: "pack-key-1", Algorithm: AlgorithmEd25519, PublicKey: pub, Status: KeyActive},
	}, priv
}

func TestVerify_AcceptsAGoodSignature_AndFailsClosedOtherwise(t *testing.T) {
	good, _ := signedFixture(t)
	assert.True(t, VerifyArtifact(good).Verified)

	retired := good
	k := *good.Key
	k.Status = KeyRetired
	retired.Key = &k
	assert.True(t, VerifyArtifact(retired).Verified, "a rotated-out key still verifies what it signed (historical reconstruction)")

	revoked := good
	k2 := *good.Key
	k2.Status = KeyRevoked
	revoked.Key = &k2
	assert.Contains(t, VerifyArtifact(revoked).Reasons, ReasonKeyRevoked)

	unsigned := good
	unsigned.Signature, unsigned.SignatureKeyID = nil, nil
	r := VerifyArtifact(unsigned)
	assert.False(t, r.Verified)
	assert.Contains(t, r.Reasons, ReasonUnsigned, "JUR-NEG-04")

	unknown := good
	unknown.Key = nil
	assert.Contains(t, VerifyArtifact(unknown).Reasons, ReasonKeyUnknown)

	tampered := good
	tampered.ArtifactJSON = strings.Replace(good.ArtifactJSON, "Standard", "Standarr", 1)
	r = VerifyArtifact(tampered)
	assert.False(t, r.Verified)
	assert.Contains(t, r.Reasons, ReasonDigestMismatch, "JUR-NEG-03")

	wrongKey := good
	otherPub, _, _ := ed25519.GenerateKey(nil)
	k3 := *good.Key
	k3.PublicKey = otherPub
	wrongKey.Key = &k3
	assert.Contains(t, VerifyArtifact(wrongKey).Reasons, ReasonSignatureInvalid)

	badRecorded := good
	badRecorded.VersionDigest = "sha256:" + strings.Repeat("0", 64)
	assert.Contains(t, VerifyArtifact(badRecorded).Reasons, ReasonRecordedDigestBad)

	garbage := good
	garbage.Signature = ptr("%%%not-base64")
	assert.Contains(t, VerifyArtifact(garbage).Reasons, ReasonSignatureNotB64)
}

func TestSignature_IsDomainSeparated(t *testing.T) {
	in, priv := signedFixture(t)
	raw := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(in.StoredDigest))) // no domain prefix
	in.Signature = &raw
	assert.Contains(t, VerifyArtifact(in).Reasons, ReasonSignatureInvalid, "a signature over the bare digest is not a pack signature")
}

func TestLoadSignerFromFile(t *testing.T) {
	dir := t.TempDir()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	p := filepath.Join(dir, "k")
	require.NoError(t, os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600))
	s, err := LoadEd25519SignerFromFile("k1", p)
	require.NoError(t, err)
	assert.Equal(t, "k1", s.KeyRef())
	assert.Len(t, s.PublicKey(), ed25519.PublicKeySize)

	require.NoError(t, os.WriteFile(p, []byte("short"), 0o600))
	_, err = LoadEd25519SignerFromFile("k1", p)
	assert.Error(t, err)
	_, err = LoadEd25519SignerFromFile("k1", filepath.Join(dir, "missing"))
	assert.Error(t, err)
	_, err = NewEd25519Signer("", make([]byte, ed25519.PrivateKeySize))
	assert.Error(t, err)
}
