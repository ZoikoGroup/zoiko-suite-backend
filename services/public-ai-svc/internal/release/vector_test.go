package release

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/public-ai-svc/internal/knowledge"
)

type lenEmbedder struct{ err error }

func (lenEmbedder) ModelVersion() string { return "len-test-v1" }
func (e lenEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)%7) + 1, 1}
	}
	return out, nil
}

// R0 default: no embedder, so retrieval is the lexical index itself.
func TestRetrieverDefaultsToLexical(t *testing.T) {
	snap, err := setup(t).rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Retriever.(*knowledge.Index); !ok {
		t.Fatalf("retriever %T, want *knowledge.Index", snap.Retriever)
	}
}

// With an embedder the release is served hybrid; if embedding the release
// fails it is served lexically and VectorError says why (never not-ready).
func TestRuntimeEmbedderSlot(t *testing.T) {
	e := setup(t)
	rt := New(e.st, e.pub)
	rt.Embedder = lenEmbedder{}
	if err := rt.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	snap, _ := rt.Snapshot()
	if h, ok := snap.Retriever.(*knowledge.Hybrid); !ok || h.ModelVersion() != "len-test-v1" || rt.VectorError() != nil {
		t.Fatalf("retriever %T, vector error %v", snap.Retriever, rt.VectorError())
	}

	failing := New(e.st, e.pub)
	failing.Embedder = lenEmbedder{err: errors.New("model endpoint down")}
	if err := failing.Refresh(ctx); err != nil {
		t.Fatalf("a vector failure must not make the runtime not-ready: %v", err)
	}
	snap, _ = failing.Snapshot()
	if _, ok := snap.Retriever.(*knowledge.Index); !ok || failing.VectorError() == nil {
		t.Fatalf("want lexical fallback with an error, got %T %v", snap.Retriever, failing.VectorError())
	}
}
