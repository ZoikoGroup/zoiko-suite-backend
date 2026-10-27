package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/policy"
)

// Ingestion states (Doc 03 §09). Only STAGED sources enter a release.
const (
	StateStaged             = "STAGED"
	StateRejectUnregistered = "REJECT_UNREGISTERED"
	StateQuarantineFetch    = "QUARANTINE_FETCH"
	StateQuarantineParse    = "QUARANTINE_PARSE"
	StateQuarantineSchema   = "QUARANTINE_SCHEMA"
	StateQuarantinePrivate  = "QUARANTINE_PRIVATE"
	StateReviewConflict     = "REVIEW_CONFLICT"
	StateReviewUnmapped     = "REVIEW_UNMAPPED"
	StateHoldApproval       = "HOLD_APPROVAL"
	StateQuarantineIndex    = "QUARANTINE_INDEX"
	StateBlockPromotion     = "BLOCK_PROMOTION"
)

// Chunk policy dev default: 300–700 tokens with 40–80 overlap, approximated
// as ≤450 words with a 45-word overlap (ADR-002).
const (
	chunkMaxWords     = 450
	chunkOverlapWords = 45
)

// SourceFile is one fixture/import file: registry metadata plus body text.
type SourceFile struct {
	Source Source `json:"source"`
	Body   string `json:"body"`
}

// IngestConfig is the release-level input (allowed hosts, taxonomy, routes).
type IngestConfig struct {
	AllowedHosts  []string `json:"allowed_hosts"`
	CapabilityIDs []string `json:"capability_ids"`
	Routes        []Route  `json:"routes"`
	ApprovedBy    []string `json:"approved_by"`
	EvalRunID     string   `json:"eval_run_id"`
}

// Report is the per-source pipeline outcome.
type Report struct {
	File     string
	SourceID string
	State    string
	Reason   string
}

var hiddenContent = regexp.MustCompile(`(?i)<!--|<script|<style|<iframe|javascript:|[\x{200B}-\x{200F}\x{2060}\x{FEFF}]`)

// IngestDir runs every *.source.json in dir/sources through the pipeline and
// returns a release manifest of the STAGED sources plus a report per file.
// dir/release.json supplies IngestConfig.
func IngestDir(dir, releaseID string, now time.Time) (Manifest, []Report, error) {
	var cfg IngestConfig
	if err := readJSON(filepath.Join(dir, "release.json"), &cfg); err != nil {
		return Manifest{}, nil, fmt.Errorf("release.json: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "sources", "*.source.json"))
	if err != nil {
		return Manifest{}, nil, err
	}
	sort.Strings(files)
	var inputs []namedSource
	var reports []Report
	for _, f := range files {
		var sf SourceFile
		if err := readJSON(f, &sf); err != nil {
			reports = append(reports, Report{File: filepath.Base(f), State: StateQuarantineParse, Reason: "not valid JSON"})
			continue
		}
		inputs = append(inputs, namedSource{filepath.Base(f), sf})
	}
	m, rs := Ingest(cfg, inputs, releaseID, now)
	return m, append(reports, rs...), nil
}

type namedSource struct {
	name string
	sf   SourceFile
}

// Ingest is the pure pipeline over already-read inputs.
func Ingest(cfg IngestConfig, inputs []namedSource, releaseID string, now time.Time) (Manifest, []Report) {
	m := Manifest{
		ReleaseID: releaseID, Kind: KindKB, IndexSchemaVersion: SchemaVersion, CreatedAt: now.UTC(),
		ApprovedBy: cfg.ApprovedBy, EvalRunID: cfg.EvalRunID, ChunkPolicy: fmt.Sprintf("words<=%d overlap=%d", chunkMaxWords, chunkOverlapWords),
		RedactionPolicy: "public-only-v1", AllowedHosts: cfg.AllowedHosts, Routes: validRoutes(cfg),
		Sources: []Source{}, Chunks: []Chunk{},
	}
	caps := map[string]bool{}
	for _, c := range cfg.CapabilityIDs {
		caps[c] = true
	}
	seenHash := map[string]string{}
	var reports []Report
	for _, in := range inputs {
		src, body := in.sf.Source, in.sf.Body
		rep := Report{File: in.name, SourceID: src.SourceID}
		state, reason := check(cfg, caps, src, body)
		if state == StateStaged {
			text := normalizeBody(body)
			h := sha256Hex(text)
			if src.ContentSHA256 != "" && src.ContentSHA256 != h {
				state, reason = StateQuarantineSchema, "content_sha256 does not match body"
			} else if other, dup := seenHash[h]; dup {
				state, reason = StateReviewConflict, "duplicate content of "+other
			} else {
				seenHash[h] = src.SourceID
				src.ContentSHA256 = h
				chunks := chunkSource(src, text, releaseID)
				if len(chunks) == 0 {
					state, reason = StateBlockPromotion, "no indexable text"
				} else {
					m.Sources = append(m.Sources, src)
					m.Chunks = append(m.Chunks, chunks...)
				}
			}
		}
		rep.State, rep.Reason = state, reason
		reports = append(reports, rep)
	}
	return m, reports
}

// check runs REGISTER → ACQUIRE → SENSITIVE SCAN → TAXONOMY MAP → REVIEW →
// injection scan, returning the first failure.
func check(cfg IngestConfig, caps map[string]bool, s Source, body string) (string, string) {
	missing := []string{}
	for name, v := range map[string]string{
		"source_id": s.SourceID, "source_version_id": s.SourceVersionID, "source_type": s.SourceType,
		"origin_uri": s.OriginURI, "url_key": s.URLKey, "title": s.Title, "product_id": s.ProductID,
		"audience": s.Audience, "classification": s.Classification, "pii_state": s.PIIState,
		"authority_tier": s.AuthorityTier, "subject_type": s.SubjectType, "owner_id": s.OwnerID,
		"locale": s.Locale, "approval_state": s.ApprovalState, "license": s.License,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if s.EffectiveFrom == nil || s.ExpiresAt == nil || s.ApprovalTS == nil || len(s.MarketCodes) == 0 {
		missing = append(missing, "effective_from/expires_at/approval_ts/market_codes")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return StateRejectUnregistered, "missing " + strings.Join(missing, ", ")
	}
	u, err := url.Parse(s.OriginURI)
	if err != nil || u.Scheme != "https" || !hostAllowed(cfg.AllowedHosts, u.Hostname()) {
		return StateQuarantineFetch, "origin_uri is not an allowlisted https host"
	}
	if s.ProductID != ProductID || s.Audience != Audience || s.Classification != Classification || s.PIIState != PIIClear {
		return StateQuarantinePrivate, "not PUBLIC_APPROVED / PUBLIC_ANONYMOUS / CLEAR for zoikosuite"
	}
	if policy.Redact(body).Found() {
		return StateQuarantinePrivate, "sensitive data detected in body"
	}
	for _, c := range s.CapabilityIDs {
		if !caps[c] {
			return StateReviewUnmapped, "capability_id not in taxonomy: " + c
		}
	}
	if s.ApprovalState != "APPROVED" || len(s.ApproverIDs) == 0 {
		return StateHoldApproval, "not approved"
	}
	for _, a := range s.ApproverIDs {
		if a == s.OwnerID {
			return StateHoldApproval, "owner may not approve own source"
		}
	}
	if s.AuthorityTier == "P1" && len(s.ApproverIDs) < 2 {
		return StateHoldApproval, "P1 sources need two approvers"
	}
	if hiddenContent.MatchString(body) || policy.LooksLikeInjection(body) {
		return StateQuarantineIndex, "hidden content or instruction injection"
	}
	return StateStaged, ""
}

func hostAllowed(hosts []string, h string) bool {
	for _, a := range hosts {
		if strings.EqualFold(a, h) {
			return true
		}
	}
	return false
}

func validRoutes(cfg IngestConfig) []Route {
	var out []Route
	for _, r := range cfg.Routes {
		u, err := url.Parse(r.URL)
		if err != nil || u.Scheme != "https" || !hostAllowed(cfg.AllowedHosts, u.Hostname()) || r.RouteKey == "" || r.ApprovalRef == "" {
			continue // rejected: non-HTTPS, javascript:, off-allowlist or unapproved
		}
		out = append(out, r)
	}
	return out
}

var spaceRe = regexp.MustCompile(`[ \t]+`)

func normalizeBody(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRe.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// chunkSource splits on headings, then windows each section by words.
func chunkSource(src Source, text, releaseID string) []Chunk {
	type section struct{ anchor, title, text string }
	var sections []section
	cur := section{anchor: "top", title: src.Title}
	flush := func() {
		if strings.TrimSpace(cur.text) != "" {
			sections = append(sections, cur)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			flush()
			h := strings.TrimSpace(strings.TrimLeft(line, "#"))
			cur = section{anchor: strings.Trim(slugRe.ReplaceAllString(strings.ToLower(h), "-"), "-"), title: h}
			continue
		}
		if strings.TrimSpace(cur.text) == "" && cur.title != "" && strings.TrimSpace(line) != "" {
			line = HeadingPrefix(cur.title) + line
		}
		cur.text += line + "\n"
	}
	flush()

	// Pack consecutive small sections together (headings kept as sentences)
	// so a chunk is a coherent passage, then window anything still too long.
	var packed []section
	for _, sec := range sections {
		n := len(strings.Fields(sec.text))
		if k := len(packed); k > 0 && len(strings.Fields(packed[k-1].text))+n <= chunkMaxWords {
			packed[k-1].text += sec.text
			continue
		}
		packed = append(packed, sec)
	}

	var out []Chunk
	seg := 0
	for _, sec := range packed {
		words := strings.Fields(sec.text)
		for start := 0; start < len(words); {
			end := start + chunkMaxWords
			if end > len(words) {
				end = len(words)
			}
			seg++
			body := strings.Join(words[start:end], " ")
			out = append(out, Chunk{
				ChunkID: fmt.Sprintf("%s#%03d", src.SourceVersionID, seg), SourceID: src.SourceID, SourceVersionID: src.SourceVersionID,
				SegmentNumber: seg, DocumentAnchor: sec.anchor, DisplayTitle: sec.title, URLKey: src.URLKey,
				PlainText: body, TextSHA256: sha256Hex(body), Classification: src.Classification, Audience: src.Audience,
				AuthorityTier: src.AuthorityTier, MarketCodes: src.MarketCodes, Locale: src.Locale, PlanCodes: src.PlanCodes,
				IntentIDs: src.IntentIDs, ValidFrom: *src.EffectiveFrom, ValidUntil: validUntil(src), ReleaseID: releaseID,
			})
			if end == len(words) {
				break
			}
			start = end - chunkOverlapWords
		}
	}
	return out
}

func validUntil(s Source) time.Time {
	until := *s.ExpiresAt
	if s.EffectiveUntil != nil && s.EffectiveUntil.Before(until) {
		until = *s.EffectiveUntil
	}
	return until
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
