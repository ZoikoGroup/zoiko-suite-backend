package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goodManifest = `{"pack_id":"jur.gb.tax.core","pack_version":"2026.08.1","jurisdiction_ids":["GB"],"regimes":["VAT"],
"effective_from":"2026-08-01","dependencies":[{"ref":"ref.iso4217","version":"2026.06"}],"source_register_version":14}`

func TestParseManifest_Accepts(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifest))
	require.NoError(t, err)
	assert.Equal(t, "jur.gb.tax.core", m.PackID)
	assert.Equal(t, "2026.08.1", m.PackVersion)
}

func TestParseManifest_Rejects(t *testing.T) {
	cases := map[string]string{
		"server-owned status":        `{"status":"released",` + goodManifest[1:],
		"server-owned signature":     `{"signature":"x",` + goodManifest[1:],
		"server-owned artifact":      `{"artifact_digest":"sha256:x",` + goodManifest[1:],
		"bad pack id":                strings.Replace(goodManifest, "jur.gb.tax.core", "GB", 1),
		"bad version":                strings.Replace(goodManifest, "2026.08.1", "v1", 1),
		"no jurisdictions":           strings.Replace(goodManifest, `["GB"]`, `[]`, 1),
		"no regimes":                 strings.Replace(goodManifest, `["VAT"]`, `[]`, 1),
		"duplicate jurisdiction":     strings.Replace(goodManifest, `["GB"]`, `["GB","GB"]`, 1),
		"bad date":                   strings.Replace(goodManifest, "2026-08-01", "01/08/2026", 1),
		"end before start":           strings.Replace(goodManifest, `"effective_from":"2026-08-01",`, `"effective_from":"2026-08-01","effective_to":"2026-07-01",`, 1),
		"floating dependency":        strings.Replace(goodManifest, `"2026.06"`, `"latest"`, 1),
		"range dependency":           strings.Replace(goodManifest, `"2026.06"`, `"^2026"`, 1),
		"dependency without version": strings.Replace(goodManifest, `"version":"2026.06"`, `"version":""`, 1),
		"bad rule module":            strings.Replace(goodManifest, `"source_register_version":14`, `"rule_modules":["not-a-uuid"]`, 1),
		"trailing data":              goodManifest + `{}`,
		"not json":                   `pack`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest([]byte(raw))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrManifestInvalid))
		})
	}
}

func TestCompareVersions_IsNumeric(t *testing.T) {
	assert.Equal(t, 1, CompareVersions("2026.08.10", "2026.08.9"))
	assert.Equal(t, -1, CompareVersions("2026.08.9", "2026.08.10"))
	assert.Equal(t, 0, CompareVersions("2026.08.1", "2026.08.1"))
	assert.Equal(t, 1, CompareVersions("2026.08.1.1", "2026.08.1"))
}

func TestDigest_IgnoresKeyOrderAndWhitespace_NotContent(t *testing.T) {
	a, err := DigestOf([]byte(`{"b":1,"a":{"y":1,"x":2}}`))
	require.NoError(t, err)
	b, _ := DigestOf([]byte("{ \"a\": {\"x\":2, \"y\":1}, \"b\": 1 }"))
	c, _ := DigestOf([]byte(`{"b":1,"a":{"y":1,"x":3}}`))
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.True(t, ValidDigest(a))
	large, _ := DigestOf([]byte(`{"n":12345678901234567890}`))
	large2, _ := DigestOf([]byte(`{"n":12345678901234567891}`))
	assert.NotEqual(t, large, large2, "large numbers keep full precision (no float rounding)")
}

func TestValidateSource_RequiresIdentifiableSnapshot(t *testing.T) {
	ok := CreateSourceParams{JurisdictionID: "j", Authority: "HMRC", SourceType: "STATUTE", AuthorityLevel: "BINDING_LAW",
		Title: "t", Location: "https://x", SnapshotHash: "sha256:" + strings.Repeat("a", 64)}
	require.NoError(t, ValidateSource(ok))
	bad := ok
	bad.SnapshotHash = "md5:abc"
	assert.Error(t, ValidateSource(bad))
	bad = ok
	bad.Location = " "
	assert.Error(t, ValidateSource(bad))
	bad = ok
	d := "31/12/2026"
	bad.PublishedOn = &d
	assert.Error(t, ValidateSource(bad))
}

func TestValidPackRef(t *testing.T) {
	for _, ok := range []string{"jur.gb.tax.core", "global.reference", "us.state.ca-sales.core"} {
		assert.True(t, ValidPackRef(ok), ok)
	}
	for _, bad := range []string{"GB", "jur", "Jur.gb", ".a.b", "jur..x", "jur.gb tax"} {
		assert.False(t, ValidPackRef(bad), bad)
	}
}
