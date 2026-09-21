package config

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("ENV", "local")
	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 8096, cfg.Port)
	assert.Equal(t, "search_indexer", cfg.DB.Name)
	assert.Equal(t, []string{"http://localhost:9200"}, cfg.OpenSearch.Addresses)
	assert.Equal(t, "search-indexer-svc", cfg.Kafka.GroupID)
}

// NP-57. Outside local development the cursor key has NO default, because a
// fixed fallback would be public the moment this repository is read — and a
// cursor signed with a public key is not signed.
func TestLoad_RefusesToStartWithoutACursorKeyOutsideLocal(t *testing.T) {
	t.Setenv("ENV", "staging")
	t.Setenv("CURSOR_SIGNING_KEY_HEX", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CURSOR_SIGNING_KEY_HEX")
	assert.Contains(t, err.Error(), "NP-57")
}

func TestLoad_LocalGetsADevelopmentCursorKey(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("CURSOR_SIGNING_KEY_HEX", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.CursorSigningKey)
	assert.Contains(t, string(cfg.CursorSigningKey), "not-for-production",
		"the development key must be obviously a development key in any log that prints it")
}

func TestLoad_RefusesAShortCursorKey(t *testing.T) {
	t.Setenv("ENV", "production")
	t.Setenv("CURSOR_SIGNING_KEY_HEX", hex.EncodeToString([]byte("too-short")))

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32 bytes")
}

func TestLoad_RefusesNonHexCursorKey(t *testing.T) {
	t.Setenv("ENV", "production")
	t.Setenv("CURSOR_SIGNING_KEY_HEX", "not hex at all")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hex-encoded")
}

func TestLoad_AcceptsAValidCursorKey(t *testing.T) {
	t.Setenv("ENV", "production")
	t.Setenv("CURSOR_SIGNING_KEY_HEX", strings.Repeat("ab", 32))

	cfg, err := Load()
	require.NoError(t, err)
	assert.Len(t, cfg.CursorSigningKey, 32)
}

// NP-08 / §7.3. A minimum cell of 1 is no suppression at all, and the whole
// point is that a facet count of one can reveal a single privileged record.
func TestLoad_RefusesFacetMinCountBelowTwo(t *testing.T) {
	t.Setenv("ENV", "local")
	for _, v := range []string{"0", "1", "-5"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("FACET_MIN_COUNT", v)
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FACET_MIN_COUNT")
		})
	}
}

func TestLoad_RejectsUnparseableDurations(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("CHECKPOINT_INTERVAL", "sixty seconds")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHECKPOINT_INTERVAL")
}

// A zero or negative interval would spin a ticker as fast as the scheduler
// allows, which is a self-inflicted denial of service on Postgres and the
// engine both.
func TestLoad_RejectsNonPositiveDurations(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("RESTRICTION_VERIFY_INTERVAL", "0s")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be positive")
}

func TestLoad_RequiresAtLeastOneOpenSearchAddress(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("OPENSEARCH_ADDRESSES", " , ")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPENSEARCH_ADDRESSES")
}

func TestSplitCSV_TrimsAndDropsEmpties(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, splitCSV(" a , b , "))
	assert.Nil(t, splitCSV("   "))
}
