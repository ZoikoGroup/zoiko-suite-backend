package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// manifestDomain versions the canonicalisation so it can change without
// silently colliding with old hashes.
const manifestDomain = "zoiko.currency-manifest.v1\n"

// ManifestHash recomputes the manifest hash of rows: SHA-256 (lowercase hex) of
// the domain tag followed by one line per row,
//
//	<alpha>|<numeric>|<quoted name>|<minor_unit>|<flag>\n
//
// with the lines sorted so row order in the file does not matter. The name is
// strconv.Quote'd so a '|' or newline inside it cannot forge a different row
// boundary. minor_unit is taken verbatim from the submitted number text.
func ManifestHash(rows []ImportRow) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, strings.Join([]string{
			r.AlphaCode, r.NumericCode, strconv.Quote(r.Name), r.MinorUnit.String(), strconv.FormatBool(r.FundOrMetalFlag),
		}, "|"))
	}
	sort.Strings(lines)
	h := sha256.New()
	h.Write([]byte(manifestDomain))
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeHash lowercases and strips an optional "sha256:" prefix.
func NormalizeHash(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "sha256:")
}
