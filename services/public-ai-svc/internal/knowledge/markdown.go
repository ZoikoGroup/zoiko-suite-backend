package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ParseMarkdownSource reads a public-KB article: YAML-style front matter
// between "---" lines, then the body in markdown ("##" headings). Required
// keys: source_id, title, domain, module_id, audience, status, owner,
// effective_from, expires_at, locale. Optional: intent_ids, capability_ids,
// market_codes (comma lists), url_key, canonical_url, approvers,
// approval_ts, subject_type, version.
//
// The article goes through the same ingest checks as any source; front
// matter cannot widen audience or skip approval. Without canonical_url the
// origin is a placeholder host that is never displayed or linked.
func ParseMarkdownSource(raw string) (SourceFile, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(raw, "---\n") {
		return SourceFile{}, errors.New("missing front matter")
	}
	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return SourceFile{}, errors.New("unterminated front matter")
	}
	fm := map[string]string{}
	for _, line := range strings.Split(raw[4:4+end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return SourceFile{}, fmt.Errorf("front matter line without key: %q", line)
		}
		fm[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	body := strings.TrimSpace(strings.TrimPrefix(raw[4+end+4:], "\n"))
	for _, k := range []string{"source_id", "title", "domain", "module_id", "audience", "status", "owner", "effective_from", "expires_at", "locale"} {
		if fm[k] == "" {
			return SourceFile{}, fmt.Errorf("front matter %s is required", k)
		}
	}
	from, err := parseDate(fm["effective_from"])
	if err != nil {
		return SourceFile{}, fmt.Errorf("effective_from: %w", err)
	}
	until, err := parseDate(fm["expires_at"])
	if err != nil {
		return SourceFile{}, fmt.Errorf("expires_at: %w", err)
	}
	var approvedAt *time.Time
	if v := fm["approval_ts"]; v != "" {
		t, err := parseDate(v)
		if err != nil {
			return SourceFile{}, fmt.Errorf("approval_ts: %w", err)
		}
		approvedAt = &t
	}
	urlKey := orDefault(fm["url_key"], fm["source_id"])
	origin := orDefault(fm["canonical_url"], "https://"+PlaceholderHost+"/"+urlKey)
	version := orDefault(fm["version"], "v1")
	src := Source{
		SourceID: fm["source_id"], SourceVersionID: fm["source_id"] + "-" + version, SourceType: "MARKDOWN",
		OriginURI: origin, URLKey: urlKey, Title: fm["title"], ProductID: ProductID, Audience: fm["audience"],
		Classification: Classification, PIIState: PIIClear, AuthorityTier: "P3",
		SubjectType: orDefault(fm["subject_type"], "CAPABILITY"), OwnerID: fm["owner"], ApproverIDs: list(fm["approvers"]),
		MarketCodes: orList(list(fm["market_codes"]), []string{MarketAll}), Locale: fm["locale"], PlanCodes: []string{},
		CapabilityIDs: list(fm["capability_ids"]), IntentIDs: list(fm["intent_ids"]), ApprovalState: fm["status"],
		ApprovalTS: approvedAt, EffectiveFrom: &from, ExpiresAt: &until, SigningRef: "kbctl-dev", License: "OWNED_PUBLIC",
		Domain: fm["domain"], ModuleID: fm["module_id"],
	}
	return SourceFile{Source: src, Body: body}, nil
}

// PlaceholderHost is the origin for articles without a real public URL.
// Anything on a .invalid host is never shown or linked (see IsPlaceholderURL).
const PlaceholderHost = "pending-review.invalid"

// IsPlaceholderURL reports whether a URL is a placeholder that must never be
// displayed: no real public page exists for it yet.
func IsPlaceholderURL(u string) bool {
	host := strings.ToLower(u)
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return host == "" || strings.HasSuffix(host, ".invalid") || strings.HasSuffix(host, ".example") ||
		strings.HasSuffix(host, ".test") || host == "example.com" || strings.HasSuffix(host, ".example.com") || host == "localhost"
}

func parseDate(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	return time.Parse("2006-01-02", v)
}

func list(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orList(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
