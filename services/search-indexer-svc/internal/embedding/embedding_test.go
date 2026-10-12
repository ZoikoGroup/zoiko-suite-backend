package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-indexer-svc/internal/domain"
)

func spec() domain.EmbeddingSpec {
	return domain.EmbeddingSpec{Model: "m", ModelVersion: "2026-01", Dimensions: 3,
		SourceFields: []string{"title", "body"}, Preprocessing: ProfileNFKCWhitespaceV1, Similarity: "cosinesimil"}
}

func TestPreprocess_NormalisesAndCollapses(t *testing.T) {
	out, err := Preprocess(ProfileNFKCWhitespaceV1, "  ﬁle\t\tlate\n\nGST\u0007 ")
	require.NoError(t, err)
	assert.Equal(t, "file late GST", out)

	_, err = Preprocess("none", "x")
	assert.Error(t, err, "an unknown profile is refused, never treated as a no-op")
}

// Only the pinned fields, in pinned order, from projected values.
func TestDocumentText_UsesPinnedFieldsInOrder(t *testing.T) {
	text := DocumentText(spec(), map[string]any{"body": "overdue", "title": "GST", "secret": "x"})
	assert.Equal(t, "title: GST\nbody: overdue", text)
	assert.Empty(t, DocumentText(spec(), map[string]any{"other": "x"}))
}

func provider(t *testing.T, respond func(embedRequest) embedResponse) (*HTTPProvider, *embedRequest) {
	t.Helper()
	var seen embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&seen))
		_ = json.NewEncoder(w).Encode(respond(seen))
	}))
	t.Cleanup(srv.Close)
	return NewHTTPProvider(srv.URL, "tok", 0), &seen
}

func echo(dims int) func(embedRequest) embedResponse {
	return func(req embedRequest) embedResponse {
		out := embedResponse{Model: req.Model, ModelVersion: req.ModelVersion}
		for i := range req.Input {
			out.Data = append(out.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: make([]float32, dims)})
		}
		return out
	}
}

func TestHTTPProvider_SendsPreprocessedTextUnderThePin(t *testing.T) {
	p, seen := provider(t, echo(3))
	vecs, err := p.Embed(context.Background(), spec(), []string{"  late\nfiling "})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Len(t, vecs[0], 3)
	assert.Equal(t, "m", seen.Model)
	assert.Equal(t, "2026-01", seen.ModelVersion)
	assert.Equal(t, []string{"late filing"}, seen.Input)
}

// NP-35: a silent provider-side model change is refused.
func TestHTTPProvider_OtherModelIsAMismatch(t *testing.T) {
	p, _ := provider(t, func(req embedRequest) embedResponse {
		out := echo(3)(req)
		out.ModelVersion = "2026-02"
		return out
	})
	_, err := p.Embed(context.Background(), spec(), []string{"x"})
	assert.True(t, errors.Is(err, ErrModelMismatch), "got %v", err)
}

func TestHTTPProvider_OtherWidthIsAMismatch(t *testing.T) {
	p, _ := provider(t, echo(4))
	_, err := p.Embed(context.Background(), spec(), []string{"x"})
	assert.True(t, errors.Is(err, ErrModelMismatch), "got %v", err)
}

// A provider that does not echo its model is not agreeing with the pin.
func TestHTTPProvider_SilenceAboutTheModelIsAMismatch(t *testing.T) {
	p, _ := provider(t, func(req embedRequest) embedResponse {
		out := echo(3)(req)
		out.Model, out.ModelVersion = "", ""
		return out
	})
	_, err := p.Embed(context.Background(), spec(), []string{"x"})
	assert.True(t, errors.Is(err, ErrModelMismatch))
}

func TestUnconfigured_RefusesEverything(t *testing.T) {
	_, err := Unconfigured{}.Embed(context.Background(), spec(), []string{"x"})
	assert.ErrorIs(t, err, ErrNoProvider)
	assert.False(t, Unconfigured{}.Configured())
}
