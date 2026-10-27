package knowledge

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Answerability (Doc 05 §18). The model can never override BLOCKED.
const (
	Answerable = "ANSWERABLE"
	NotFound   = "NOT_FOUND"
	Blocked    = "BLOCKED"
	Stale      = "STALE"   // only expired evidence matched → ABSTAIN_STALE
	Revoked    = "REVOKED" // only revoked evidence matched → ABSTAIN_REVOKED
)

// Query is a retrieval request. Market "" means the visitor gave none: only
// market-neutral ("ALL") evidence is eligible.
type Query struct {
	Text      string
	Market    string
	Locale    string
	Now       time.Time
	MaxChunks int
}

// Hit is one eligible chunk with its score.
type Hit struct {
	Chunk Chunk
	Score float64
}

// Result is the retrieval outcome.
type Result struct {
	Answerability string
	Hits          []Hit
	ReleaseID     string
}

// Index is an immutable, release-pinned lexical index (BM25).
type Index struct {
	releaseID string
	chunks    []Chunk
	sources   map[string]Source
	tf        []map[string]int
	df        map[string]int
	lengths   []int
	avgLen    float64
}

var tokenRe = regexp.MustCompile(`[a-z0-9]+`)

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can do does for from has have how i if in into is it its
		me my of on or our so than that the their them then there these they this to us was we what when where which who
		why will with you your about any also just more most not only other some such tell please zoikosuite`) {
		stopwords[w] = true
	}
}

// Tokens lower-cases, removes stopwords and applies light stemming.
func Tokens(s string) []string {
	var out []string
	for _, t := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if stopwords[t] || len(t) < 2 {
			continue
		}
		out = append(out, stem(t))
	}
	return out
}

func stem(t string) string {
	for _, suf := range []string{"ations", "ation", "ings", "ing", "ies", "es", "s"} {
		if len(t) > len(suf)+3 && strings.HasSuffix(t, suf) {
			if suf == "ies" {
				return t[:len(t)-3] + "y"
			}
			return t[:len(t)-len(suf)]
		}
	}
	return t
}

// NewIndex builds an index over a verified manifest.
func NewIndex(m Manifest) *Index {
	idx := &Index{releaseID: m.ReleaseID, df: map[string]int{}, sources: map[string]Source{}}
	for _, s := range m.Sources {
		idx.sources[s.SourceVersionID] = s
	}
	total := 0
	for _, c := range m.Chunks {
		toks := Tokens(c.DisplayTitle + " " + c.PlainText)
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t := range tf {
			idx.df[t]++
		}
		idx.chunks = append(idx.chunks, c)
		idx.tf = append(idx.tf, tf)
		idx.lengths = append(idx.lengths, len(toks))
		total += len(toks)
	}
	if len(idx.chunks) > 0 {
		idx.avgLen = float64(total) / float64(len(idx.chunks))
	}
	return idx
}

// ReleaseID is the pinned release.
func (x *Index) ReleaseID() string { return x.releaseID }

// Source returns the registry record for a source version.
func (x *Index) Source(sourceVersionID string) (Source, bool) {
	s, ok := x.sources[sourceVersionID]
	return s, ok
}

// CitationLinks returns one approved page per url_key, from the release's
// source registry (origin_uri was checked against allowed_hosts at ingest).
func (x *Index) CitationLinks() []contract.CitationLink {
	seen := map[string]bool{}
	out := []contract.CitationLink{}
	for _, c := range x.chunks {
		s, ok := x.sources[c.SourceVersionID]
		if !ok || seen[s.URLKey] {
			continue
		}
		seen[s.URLKey] = true
		out = append(out, contract.CitationLink{URLKey: s.URLKey, URL: s.OriginURI, Title: s.Title})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URLKey < out[j].URLKey })
	return out
}

// Revocations is the set of revoked source ids / source version ids.
type Revocations interface {
	SourceRevoked(sourceID, sourceVersionID string) bool
}

type eligibility int

const (
	eligible eligibility = iota
	denied
	expired
	revoked
)

// eligibilityOf applies the mandatory filters in order: classification →
// audience → effective/revoked → market → locale (Doc 05 §18).
func (x *Index) eligibilityOf(c Chunk, q Query, rev Revocations) eligibility {
	if c.Classification != Classification || c.Audience != Audience || c.ReleaseID != x.releaseID {
		return denied
	}
	if rev != nil && rev.SourceRevoked(c.SourceID, c.SourceVersionID) {
		return revoked
	}
	if q.Now.Before(c.ValidFrom) || !q.Now.Before(c.ValidUntil) {
		return expired
	}
	if !marketOK(c.MarketCodes, q.Market) {
		return denied
	}
	if q.Locale != "" && c.Locale != "" && !strings.EqualFold(langOf(c.Locale), langOf(q.Locale)) {
		return denied
	}
	return eligible
}

func marketOK(codes []string, market string) bool {
	for _, m := range codes {
		if m == MarketAll || (market != "" && m == market) {
			return true
		}
	}
	return false
}

func langOf(locale string) string {
	if i := strings.IndexByte(locale, '-'); i > 0 {
		return locale[:i]
	}
	return locale
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// minScore is the answerability floor; below it the corpus has no
	// adequate evidence and the assistant abstains.
	minScore = 1.5
)

func (x *Index) score(i int, qtoks []string) (float64, int) {
	n := float64(len(x.chunks))
	s, matched := 0.0, 0
	for _, t := range qtoks {
		f := float64(x.tf[i][t])
		if f == 0 {
			continue
		}
		matched++
		idf := math.Log(1 + (n-float64(x.df[t])+0.5)/(float64(x.df[t])+0.5))
		s += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*float64(x.lengths[i])/math.Max(x.avgLen, 1)))
	}
	return s, matched
}

// Search runs filtered retrieval and decides answerability.
func (x *Index) Search(q Query, rev Revocations) Result {
	res := Result{Answerability: NotFound, ReleaseID: x.releaseID}
	qtoks := uniq(Tokens(q.Text))
	if len(qtoks) == 0 || len(x.chunks) == 0 {
		return res
	}
	need := 1
	if len(qtoks) >= 3 {
		need = 2
	}
	bestIneligible := eligible
	bestIneligibleScore := 0.0
	for i, c := range x.chunks {
		sc, matched := x.score(i, qtoks)
		if sc < minScore || matched < need {
			continue
		}
		switch el := x.eligibilityOf(c, q, rev); el {
		case eligible:
			res.Hits = append(res.Hits, Hit{Chunk: c, Score: sc})
		case expired, revoked:
			if sc > bestIneligibleScore {
				bestIneligible, bestIneligibleScore = el, sc
			}
		}
	}
	sort.SliceStable(res.Hits, func(i, j int) bool {
		if res.Hits[i].Score != res.Hits[j].Score {
			return res.Hits[i].Score > res.Hits[j].Score
		}
		return res.Hits[i].Chunk.ChunkID < res.Hits[j].Chunk.ChunkID
	})
	max := q.MaxChunks
	if max <= 0 {
		max = 4
	}
	if len(res.Hits) > max {
		res.Hits = res.Hits[:max]
	}
	switch {
	case len(res.Hits) > 0:
		res.Answerability = Answerable
	case bestIneligible == revoked:
		res.Answerability = Revoked
	case bestIneligible == expired:
		res.Answerability = Stale
	}
	return res
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
