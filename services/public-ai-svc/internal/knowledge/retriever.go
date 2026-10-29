package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Retriever finds eligible evidence for a query inside one pinned release.
// Every implementation applies the mandatory filters (classification,
// audience, revocation, validity, market, locale) before ranking (Doc 03 §14).
// The lexical *Index is the default; Hybrid adds vector recall (ADR-002).
type Retriever interface {
	Retrieve(ctx context.Context, q Query, rev Revocations) Result
	ByIntent(q Query, rev Revocations) Result
	ReleaseID() string
}

// Retrieve implements Retriever with lexical search only.
func (x *Index) Retrieve(_ context.Context, q Query, rev Revocations) Result { return x.Search(q, rev) }

// Embedder turns text into vectors. It receives only approved public chunk
// text and the visitor's redacted question. No embedding model is approved
// for R0 (ADR-002), so none is configured by default and retrieval stays
// lexical. ModelVersion is pinned: a different version means a full re-embed.
type Embedder interface {
	ModelVersion() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	// DefaultMinCosine is the vector recall floor. It must be re-tuned on
	// the golden and KB sets for the approved embedding model.
	DefaultMinCosine = 0.5
	// rrfK is the reciprocal-rank-fusion constant.
	rrfK = 60
	// fuseDepth is how many candidates each method contributes to fusion.
	fuseDepth  = 20
	embedBatch = 64
)

// Hybrid fuses lexical (BM25) and vector recall with reciprocal rank fusion
// inside the already-authorized candidate set (Doc 03 §13-14). Chunk vectors
// are computed once per release. If the embedder fails at query time the
// lexical result is returned unchanged, so vector search can only add
// recall, never remove a safety outcome.
type Hybrid struct {
	lex       *Index
	emb       Embedder
	vecs      [][]float32 // unit length, aligned with lex.chunks
	MinCosine float64
	Timeout   time.Duration // per-query embedding deadline
}

// NewHybrid embeds every chunk of the release.
func NewHybrid(ctx context.Context, idx *Index, emb Embedder) (*Hybrid, error) {
	if emb == nil {
		return nil, errors.New("hybrid retrieval needs an embedder")
	}
	h := &Hybrid{lex: idx, emb: emb, MinCosine: DefaultMinCosine, Timeout: 800 * time.Millisecond}
	dim := 0
	for start := 0; start < len(idx.chunks); start += embedBatch {
		end := min(start+embedBatch, len(idx.chunks))
		texts := make([]string, 0, end-start)
		for _, c := range idx.chunks[start:end] {
			texts = append(texts, c.DisplayTitle+" "+c.PlainText)
		}
		vs, err := emb.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed release %s with %s: %w", idx.releaseID, emb.ModelVersion(), err)
		}
		if len(vs) != len(texts) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d chunks", len(vs), len(texts))
		}
		for _, v := range vs {
			if dim == 0 {
				dim = len(v)
			}
			u, ok := unit(v)
			if !ok || len(v) != dim {
				return nil, fmt.Errorf("embedder returned an empty or inconsistent vector")
			}
			h.vecs = append(h.vecs, u)
		}
	}
	return h, nil
}

// ReleaseID is the pinned release.
func (h *Hybrid) ReleaseID() string { return h.lex.ReleaseID() }

// ModelVersion is the embedding model that built the vectors.
func (h *Hybrid) ModelVersion() string { return h.emb.ModelVersion() }

// ByIntent is the lexical intent fallback.
func (h *Hybrid) ByIntent(q Query, rev Revocations) Result { return h.lex.ByIntent(q, rev) }

// Retrieve runs lexical and vector recall and fuses the two rankings.
func (h *Hybrid) Retrieve(ctx context.Context, q Query, rev Revocations) Result {
	max := q.MaxChunks
	if max <= 0 {
		max = 4
	}
	wide := q
	wide.MaxChunks = fuseDepth
	lex := h.lex.Search(wide, rev)
	if lex.Answerability == Stale || lex.Answerability == Revoked {
		// A stale or revoked source outranked everything eligible: never
		// answer around it with a weaker vector match.
		lex.Mode = "hybrid"
		return lex
	}
	qctx, cancel := context.WithTimeout(ctx, h.Timeout)
	qv, err := h.emb.Embed(qctx, []string{h.lex.expand(q.Text)})
	cancel()
	var u []float32
	ok := err == nil && len(qv) == 1 && len(h.vecs) > 0 && len(qv[0]) == len(h.vecs[0])
	if ok {
		u, ok = unit(qv[0])
	}
	if !ok {
		lex.Mode = "lexical-fallback"
		return truncate(lex, max)
	}

	type cand struct {
		i   int
		cos float64
	}
	var vec []cand
	for i, c := range h.lex.chunks {
		cos := dot(u, h.vecs[i])
		if cos < h.MinCosine {
			continue
		}
		lex.CandidatesBeforeACL++
		if h.lex.eligibilityOf(c, q, rev) != eligible { // filters before ranking
			continue
		}
		lex.CandidatesAfterACL++
		vec = append(vec, cand{i, cos})
	}
	sort.SliceStable(vec, func(a, b int) bool { return vec[a].cos > vec[b].cos })
	if len(vec) > fuseDepth {
		vec = vec[:fuseDepth]
	}

	score := map[string]float64{}
	byID := map[string]Chunk{}
	for rank, hit := range lex.Hits {
		score[hit.Chunk.ChunkID] += 1 / float64(rrfK+rank+1)
		byID[hit.Chunk.ChunkID] = hit.Chunk
	}
	for rank, c := range vec {
		ch := h.lex.chunks[c.i]
		score[ch.ChunkID] += 1 / float64(rrfK+rank+1)
		byID[ch.ChunkID] = ch
	}
	res := Result{Answerability: NotFound, ReleaseID: lex.ReleaseID, Mode: "hybrid",
		CandidatesBeforeACL: lex.CandidatesBeforeACL, CandidatesAfterACL: lex.CandidatesAfterACL}
	for id, s := range score {
		res.Hits = append(res.Hits, Hit{Chunk: byID[id], Score: s})
	}
	sort.SliceStable(res.Hits, func(a, b int) bool {
		if res.Hits[a].Score != res.Hits[b].Score {
			return res.Hits[a].Score > res.Hits[b].Score
		}
		return res.Hits[a].Chunk.ChunkID < res.Hits[b].Chunk.ChunkID
	})
	res = truncate(res, max)
	if len(res.Hits) > 0 {
		res.Answerability, res.TopScore = Answerable, res.Hits[0].Score
	}
	return res
}

func truncate(r Result, max int) Result {
	if len(r.Hits) > max {
		r.Hits = r.Hits[:max]
	}
	return r
}

func unit(v []float32) ([]float32, bool) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, false
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out, true
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
