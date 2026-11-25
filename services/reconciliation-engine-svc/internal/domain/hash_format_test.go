package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ZS-SVC-Z-001 INV-24 (docs/architecture/DQC_GAP_MATRIX.md, step D).

func goldenItems() []PopulationItemInput {
	d := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return []PopulationItemInput{
		{SourceRecordID: "rec-2", RefID: "REF-2", AmountMinorUnits: -250, Currency: "EUR", OccurredAt: d.AddDate(0, 0, 3),
			Dimensions: map[string]string{"entity": "E2", "account": "1200"}},
		{SourceRecordID: "rec-1", RefID: "REF-1", AmountMinorUnits: 100, Currency: "USD", OccurredAt: d},
	}
}

// goldenRpopV1 is HashPopulation's output for goldenItems under serialization
// "rpop-v1". Every population snapshot ever frozen was hashed this way, and each
// is re-derived against this exact format. If this test fails, the serialization
// changed: do NOT update the constant — give the new format a new identifier
// ("rpop-v2"), teach RecomputePopulationHash both, and keep this vector for v1,
// or every existing hash silently stops verifying.
const goldenRpopV1 = "d0dfdc9b70f1974873cb99fadc51b9137306eb26d68d1106c19567cc1dfeb191"

func TestHashPopulation_SerializationV1IsPinned(t *testing.T) {
	if got := HashPopulation(goldenItems()); got != goldenRpopV1 {
		t.Fatalf("HashPopulation = %s\nwant the pinned %s\nthe rpop-v1 serialization changed; see the comment on goldenRpopV1", got, goldenRpopV1)
	}
}

func TestRecomputePopulationHash_UnderTheRecordedFormat(t *testing.T) {
	got, err := RecomputePopulationHash(PopulationHashAlgorithm, PopulationHashSerialization, goldenItems())
	if err != nil || got != goldenRpopV1 {
		t.Fatalf("recompute = %s, %v; want %s", got, err, goldenRpopV1)
	}
	// Order of items must not matter: the hash sorts them.
	rev := goldenItems()
	rev[0], rev[1] = rev[1], rev[0]
	if again, _ := RecomputePopulationHash(PopulationHashAlgorithm, PopulationHashSerialization, rev); again != goldenRpopV1 {
		t.Fatal("hash depends on item order")
	}
}

// An unknown format is refused, not guessed at: re-deriving under the wrong
// format would report a false mismatch (or worse, a false match).
func TestRecomputePopulationHash_UnknownFormatIsRefused(t *testing.T) {
	for _, tc := range []struct{ alg, ser string }{
		{"md5", PopulationHashSerialization},
		{"sha256", "rpop-v9"},
		{"", ""},
		{"SHA256", PopulationHashSerialization},
	} {
		got, err := RecomputePopulationHash(tc.alg, tc.ser, goldenItems())
		if !errors.Is(err, ErrUnknownHashFormat) || got != "" {
			t.Errorf("(%q, %q) = %q, %v; want a refusal with no hash", tc.alg, tc.ser, got, err)
		}
	}
}

func TestHashFormatConstantsAreWellFormed(t *testing.T) {
	if !strings.HasPrefix(PopulationHashSerialization, "rpop-v") || !strings.HasPrefix(CertificationManifestSerialization, "rcm-v") {
		t.Fatalf("serialization identifiers %q / %q do not match the migration's CHECKs", PopulationHashSerialization, CertificationManifestSerialization)
	}
}
