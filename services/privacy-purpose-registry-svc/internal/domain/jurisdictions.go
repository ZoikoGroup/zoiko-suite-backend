package domain

import "strings"

// PlatformScopeID is the explicit platform-scope sentinel. A request that
// sends X-Tenant-Id: <PlatformScopeID> is asking to act in the platform
// plane (Zoiko as independent controller, §23.1/§24) — the only way to read
// or write a platform (tenant_id IS NULL) record. It mirrors
// authorization-svc's platform entity UUID, so the same value is also the
// scope passed to the authorization check. A MISSING X-Tenant-Id is never
// interpreted as platform scope: the handler refuses it (TENANT_SCOPE_REQUIRED).
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// isoAlpha2 is the ISO 3166-1 alpha-2 country registry (249 officially
// assigned codes). §8.1: "invalid jurisdiction ... blocks activation" — the
// previous check accepted ANY two uppercase letters, so "ZZ" or "QQ" passed;
// membership in the actual registry is required instead.
var isoAlpha2 = func() map[string]bool {
	const codes = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ " +
		"BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
		"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ " +
		"DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
		"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY " +
		"HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
		"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY " +
		"MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ " +
		"NA NC NE NF NG NI NL NO NP NR NU NZ OM " +
		"PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
		"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ " +
		"TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ " +
		"UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"
	m := map[string]bool{}
	for _, c := range strings.Fields(codes) {
		m[c] = true
	}
	return m
}()

// regimeJurisdictions are the non-country privacy regimes the platform
// recognises. Sub-national jurisdictions (e.g. US state laws) are NOT
// accepted until the PDC jurisdiction catalogue exists to define them —
// accepting a free-form "US-XX" would reintroduce the arbitrary-string hole.
var regimeJurisdictions = map[string]bool{"EU": true, "EEA": true, "GLOBAL": true}

// NormalizeCode canonicalises a registry code: trimmed and upper-cased. The
// canonical form is what is stored, so the jurisdiction filter on
// GET /privacy/ropa (a case-sensitive JSONB match) cannot miss a row that was
// written as "gb".
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// NormalizeCodes canonicalises every element, preserving blanks as "" so a
// blank entry is reported by validation rather than silently dropped.
func NormalizeCodes(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = NormalizeCode(s)
	}
	return out
}

// NormalizeRefs trims opaque reference strings without changing case —
// references are case-sensitive identifiers, unlike registry codes.
func NormalizeRefs(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}
