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
	// Doc 03 §13: split on semantic boundaries; 300-700 token band, 40-80
	// token overlap where a section must be windowed. Tokens are estimated
	// at 0.75 words per token (no tokenizer is approved yet); the estimate
	// is recorded in the manifest's chunk policy.
	chunkMaxTokens     = 700
	chunkOverlapTokens = 60
	chunkMinTokens     = 40 // fragments smaller than this join the next section
	wordsPerToken      = 0.75
	chunkMaxWords      = int(chunkMaxTokens * wordsPerToken)     // 525
	chunkOverlapWords  = int(chunkOverlapTokens * wordsPerToken) // 45
)

// SourceFile is one fixture/import file: registry metadata plus body text.
type SourceFile struct {
	Source Source `json:"source"`
	Body   string `json:"body"`
}

// IngestConfig is the release-level input (allowed hosts, taxonomy, routes).
type IngestConfig struct {
	AllowedHosts  []string            `json:"allowed_hosts"`
	CapabilityIDs []string            `json:"capability_ids"`
	Routes        []Route             `json:"routes"`
	ApprovedBy    []string            `json:"approved_by"`
	EvalRunID     string              `json:"eval_run_id"`
	Synonyms      map[string][]string `json:"synonyms"`
	// Crosswalk maps modules to capabilities, domains and public routes
	// (Doc 03 §05). Unmapped or inconsistent entries block publication.
	Crosswalk []CrosswalkEntry `json:"capability_crosswalk,omitempty"`
	Synthetic bool             `json:"synthetic,omitempty"`
}

// CrosswalkEntry is one module in the capability crosswalk.
type CrosswalkEntry struct {
	ModuleID       string `json:"module_id"`
	CapabilityID   string `json:"capability_id"`
	Domain         string `json:"domain"`
	PublicRouteKey string `json:"public_route_key"`
	Synthetic      bool   `json:"synthetic,omitempty"`
}

// checkCrosswalk returns why the crosswalk cannot be published, if at all.
func checkCrosswalk(cfg IngestConfig) error {
	caps, routes, seen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range cfg.CapabilityIDs {
		caps[c] = true
	}
	for _, r := range cfg.Routes {
		routes[r.RouteKey] = true
	}
	for _, e := range cfg.Crosswalk {
		switch {
		case e.ModuleID == "" || e.Domain == "":
			return fmt.Errorf("crosswalk entry %q: module_id and domain are required", e.ModuleID)
		case seen[e.ModuleID]:
			return fmt.Errorf("crosswalk module %s is mapped twice", e.ModuleID)
		case !caps[e.CapabilityID]:
			return fmt.Errorf("crosswalk module %s maps to unknown capability %q", e.ModuleID, e.CapabilityID)
		case !routes[e.PublicRouteKey]:
			return fmt.Errorf("crosswalk module %s maps to unpublished route %q", e.ModuleID, e.PublicRouteKey)
		}
		seen[e.ModuleID] = true
	}
	return nil
}

// Report is the per-source pipeline outcome.
type Report struct {
	File     string
	SourceID string
	State    string
	Reason   string
}

// privateMarkers catch a private document mislabelled PUBLIC (KB-002).
var privateMarkers = regexp.MustCompile(`(?i)\b(internal only|internal use only|confidential|staff only|not for distribution|do not share externally)\b`)

// liveStatusMarkers catch live platform or admin-dashboard data (counters,
// readiness, cluster or container state): never public knowledge.
var liveStatusMarkers = regexp.MustCompile(`(?i)\b\d+\s*/\s*\d+\s+(?:services?|targets?)\b|\baction required\b|\bservices? (?:are )?ready\b|\bactive microservices\b|\bcluster readiness\b|\bprobes? responding\b|\btargets? active\b|\b(?:docker|kubernetes|k8s) (?:status|containers?)\b|\bp\d{2} latency\b|\buptime\b`)

// ApprovalDraft is the review state of content written for owners to approve.
const ApprovalDraft = "DRAFT_PENDING_REVIEW"

// IngestOptions adjust the pipeline. AllowDraft admits DRAFT_PENDING_REVIEW
// sources (marked Draft) and is for local/dev only.
type IngestOptions struct {
	AllowDraft bool
}

// publicLicences may be published and indexed (KB-017).
var publicLicences = map[string]bool{"OWNED_PUBLIC": true, "PUBLIC_REUSE": true, "LICENSED_PUBLIC": true}

// highImpactSubjects need an in-date review (KB-028).
var highImpactSubjects = map[string]bool{"PRICING": true, "TRUST": true}

var hiddenContent = regexp.MustCompile(`(?i)<!--|<script|<style|<iframe|javascript:|[\x{200B}-\x{200F}\x{2060}\x{FEFF}]`)

// IngestDir runs every *.source.json in dir/sources through the pipeline and
// returns a release manifest of the STAGED sources plus a report per file.
// dir/release.json supplies IngestConfig.
func IngestDir(dir, releaseID string, now time.Time) (Manifest, []Report, error) {
	return IngestDirWith(dir, releaseID, now, IngestOptions{})
}

// IngestDirWith is IngestDir with options. Besides *.source.json it reads
// *.md articles with front matter (see ParseMarkdownSource).
func IngestDirWith(dir, releaseID string, now time.Time, opts IngestOptions) (Manifest, []Report, error) {
	var cfg IngestConfig
	if err := readJSON(filepath.Join(dir, "release.json"), &cfg); err != nil {
		return Manifest{}, nil, fmt.Errorf("release.json: %w", err)
	}
	if err := checkCrosswalk(cfg); err != nil {
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
	mds, err := filepath.Glob(filepath.Join(dir, "sources", "*.md"))
	if err != nil {
		return Manifest{}, nil, err
	}
	sort.Strings(mds)
	for _, f := range mds {
		raw, err := os.ReadFile(f)
		if err != nil {
			reports = append(reports, Report{File: filepath.Base(f), State: StateQuarantineFetch, Reason: err.Error()})
			continue
		}
		sf, err := ParseMarkdownSource(string(raw))
		if err != nil {
			reports = append(reports, Report{File: filepath.Base(f), State: StateQuarantineParse, Reason: err.Error()})
			continue
		}
		inputs = append(inputs, namedSource{filepath.Base(f), sf})
	}
	m, rs := IngestWith(cfg, inputs, releaseID, now, opts)
	return m, append(reports, rs...), nil
}

type namedSource struct {
	name string
	sf   SourceFile
}

// Ingest is the pure pipeline over already-read inputs.
func Ingest(cfg IngestConfig, inputs []namedSource, releaseID string, now time.Time) (Manifest, []Report) {
	return IngestWith(cfg, inputs, releaseID, now, IngestOptions{})
}

// IngestWith is Ingest with options.
func IngestWith(cfg IngestConfig, inputs []namedSource, releaseID string, now time.Time, opts IngestOptions) (Manifest, []Report) {
	m := Manifest{
		ReleaseID: releaseID, Kind: KindKB, IndexSchemaVersion: SchemaVersion, CreatedAt: now.UTC(),
		ApprovedBy: cfg.ApprovedBy, EvalRunID: cfg.EvalRunID, ChunkPolicy: fmt.Sprintf("section-boundaries; tokens<=%d overlap=%d min=%d; tokenizer=estimate(%.2f words/token)", chunkMaxTokens, chunkOverlapTokens, chunkMinTokens, wordsPerToken),
		RedactionPolicy: "public-only-v1", AllowedHosts: cfg.AllowedHosts, Routes: validRoutes(cfg),
		Sources: []Source{}, Chunks: []Chunk{}, Synonyms: cfg.Synonyms,
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
		state, reason := check(cfg, caps, src, body, now, opts)
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
				src.Draft = src.ApprovalState == ApprovalDraft
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
func check(cfg IngestConfig, caps map[string]bool, s Source, body string, now time.Time, opts IngestOptions) (string, string) {
	draft := s.ApprovalState == ApprovalDraft
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
	if s.EffectiveFrom == nil || s.ExpiresAt == nil || (s.ApprovalTS == nil && !draft) || len(s.MarketCodes) == 0 {
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
	if privateMarkers.MatchString(body) {
		return StateQuarantinePrivate, "body is marked internal/confidential"
	}
	if liveStatusMarkers.MatchString(s.Title + "\n" + body) {
		return StateQuarantinePrivate, "live platform status or admin dashboard data"
	}
	if !publicLicences[s.License] {
		return StateQuarantinePrivate, "license does not permit public use"
	}
	for _, c := range s.CapabilityIDs {
		if !caps[c] {
			return StateReviewUnmapped, "capability_id not in taxonomy: " + c
		}
	}
	switch {
	case draft && !opts.AllowDraft:
		return StateHoldApproval, "draft pending review (KB_ALLOW_DRAFT admits drafts in local/dev only)"
	case draft && (s.AuthorityTier == "P1" || highImpactSubjects[s.SubjectType]):
		return StateHoldApproval, "P1 and high-impact content is never served as a draft"
	case draft:
		// Admitted for local/dev with a visible "Draft content" label.
	case s.ApprovalState != "APPROVED" || len(s.ApproverIDs) == 0:
		return StateHoldApproval, "not approved"
	}
	for _, a := range s.ApproverIDs {
		if a == s.OwnerID {
			return StateHoldApproval, "owner may not approve own source"
		}
	}
	if !draft && s.AuthorityTier == "P1" && len(s.ApproverIDs) < 2 {
		return StateHoldApproval, "P1 sources need two approvers"
	}
	if !draft && (s.AuthorityTier == "P1" || highImpactSubjects[s.SubjectType]) && s.ReviewDueAt != nil && s.ReviewDueAt.Before(now) {
		return StateHoldApproval, "high-impact review overdue"
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

// estTokens estimates a token count from words (see wordsPerToken).
func estTokens(s string) int {
	return int(float64(len(strings.Fields(s)))/wordsPerToken + 0.5)
}

// chunkSource splits on semantic boundaries (one chunk per heading section,
// Doc 03 §13), keeps the heading on the chunk, joins fragments under
// chunkMinTokens to the following section so no chunk is a bare label, and
// windows any section over chunkMaxTokens with chunkOverlapTokens overlap.
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

	// A short preamble above the first heading (e.g. a label line) is not a
	// passage of its own: it joins the first section. Headed sections always
	// stay separate (semantic boundaries).
	var packed []section
	carry := ""
	for i, sec := range sections {
		if sec.anchor == "top" && estTokens(sec.text) < chunkMinTokens && i < len(sections)-1 {
			carry += sec.text
			continue
		}
		sec.text = carry + sec.text
		carry = ""
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
				Draft: src.Draft,
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
