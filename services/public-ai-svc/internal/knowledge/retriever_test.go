package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// conceptEmbedder is a deterministic stand-in for an approved embedding
// model: each dimension is a concept, so texts with no shared words can
// still be close (the recall a vector model adds over BM25).
type conceptEmbedder struct{ failQueries bool }

var concepts = [][]string{
	{"contract", "contracts", "agreement", "agreements", "signature", "signatures", "legal", "clause", "clauses"},
	{"invoice", "invoices", "payment", "payments", "payable", "billing"},
	{"payroll", "salary", "salaries", "employee", "employees", "staff"},
	{"authority", "delegation", "delegate", "register"},
}

func (conceptEmbedder) ModelVersion() string { return "concept-test-v1" }

func (e conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.failQueries && len(texts) == 1 {
		return nil, errors.New("embedding service unavailable")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(concepts)+1)
		v[len(concepts)] = 0.05 // never a zero vector
		for _, w := range tokenRe.FindAllString(strings.ToLower(t), -1) {
			for d, words := range concepts {
				for _, cw := range words {
					if w == cw {
						v[d]++
					}
				}
			}
		}
		out[i] = v
	}
	return out, nil
}

type revokedSet map[string]bool

func (r revokedSet) SourceRevoked(id, ver string) bool { return r[id] || r[ver] }

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func testIndex() *Index {
	chunk := func(id, title, text, class string) Chunk {
		return Chunk{ChunkID: id + "#001", SourceID: id, SourceVersionID: id + "-v1", DisplayTitle: title, PlainText: text,
			Classification: class, Audience: Audience, MarketCodes: []string{MarketAll}, Locale: "en-US",
			ValidFrom: t0.Add(-time.Hour), ValidUntil: t0.Add(time.Hour), ReleaseID: "kb-r1"}
	}
	return NewIndex(Manifest{ReleaseID: "kb-r1", Chunks: []Chunk{
		chunk("legal", "Legal workspace", "The legal workspace governs contracts and clauses.", Classification),
		chunk("finance", "Accounts payable", "Accounts payable handles invoices and payments.", Classification),
		chunk("private", "Payroll runbook", "Payroll salary and employee records for staff.", "INTERNAL"),
		chunk("authority", "Delegation register", "Delegation of authority register for approvals.", Classification),
	}})
}

func q(text string) Query { return Query{Text: text, Locale: "en-US", Now: t0, MaxChunks: 4} }

func hybrid(t *testing.T, emb Embedder) *Hybrid {
	t.Helper()
	h, err := NewHybrid(context.Background(), testIndex(), emb)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The lexical index is itself a Retriever, so R0 behaviour is unchanged.
func TestIndexIsTheDefaultRetriever(t *testing.T) {
	var r Retriever = testIndex()
	res := r.Retrieve(context.Background(), q("contracts clauses"), nil)
	if res.Mode != "lexical" || res.Answerability != Answerable || res.Hits[0].Chunk.SourceID != "legal" {
		t.Fatalf("got %+v", res)
	}
}

// Vector recall finds evidence BM25 misses (no shared words).
func TestHybridAddsVectorRecall(t *testing.T) {
	question := "Where do I manage agreements and signatures?"
	if lex := testIndex().Search(q(question), nil); lex.Answerability != NotFound {
		t.Fatalf("precondition: lexical should miss, got %s", lex.Answerability)
	}
	res := hybrid(t, conceptEmbedder{}).Retrieve(context.Background(), q(question), nil)
	if res.Mode != "hybrid" || res.Answerability != Answerable || res.Hits[0].Chunk.SourceID != "legal" {
		t.Fatalf("got %+v", res)
	}
}

// Mandatory filters run before ranking: a private chunk that is the closest
// vector match is never returned (Doc 03 §14, CT-030).
func TestHybridFiltersBeforeRanking(t *testing.T) {
	res := hybrid(t, conceptEmbedder{}).Retrieve(context.Background(), q("salaries of employees"), nil)
	for _, h := range res.Hits {
		if h.Chunk.SourceID == "private" {
			t.Fatal("private chunk retrieved")
		}
	}
	if res.Answerability != NotFound || res.CandidatesBeforeACL == res.CandidatesAfterACL {
		t.Fatalf("want the private match filtered and nothing answerable, got %+v", res)
	}
}

// A revoked source that ranks first is never answered around with a
// weaker vector match.
func TestHybridKeepsRevokedOutcome(t *testing.T) {
	res := hybrid(t, conceptEmbedder{}).Retrieve(context.Background(), q("delegation authority register"), revokedSet{"authority": true})
	if res.Answerability != Revoked || len(res.Hits) != 0 {
		t.Fatalf("got %+v", res)
	}
}

// If the embedder fails at query time the lexical result stands.
func TestHybridFallsBackToLexical(t *testing.T) {
	h := hybrid(t, conceptEmbedder{})
	h.emb = conceptEmbedder{failQueries: true}
	res := h.Retrieve(context.Background(), q("contracts clauses"), nil)
	if res.Mode != "lexical-fallback" || res.Answerability != Answerable || res.Hits[0].Chunk.SourceID != "legal" {
		t.Fatalf("got %+v", res)
	}
}

type badEmbedder struct{}

func (badEmbedder) ModelVersion() string { return "bad" }
func (badEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil // empty vectors
}

func TestNewHybridRejectsBadVectors(t *testing.T) {
	if _, err := NewHybrid(context.Background(), testIndex(), badEmbedder{}); err == nil {
		t.Fatal("empty vectors must be rejected")
	}
	if _, err := NewHybrid(context.Background(), testIndex(), nil); err == nil {
		t.Fatal("nil embedder must be rejected")
	}
}
