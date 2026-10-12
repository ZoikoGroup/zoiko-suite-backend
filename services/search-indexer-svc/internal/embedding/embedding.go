// Package embedding produces the vectors a semantic scope is searched by.
//
// §10.1 pins the model, its version and the preprocessing in the IndexContract;
// OD-10 leaves WHICH provider and model to an AIG/Privacy/Security/residency
// decision that has not been made. So this package takes no position on the
// provider. It speaks one small HTTP protocol to whatever EMBEDDING_PROVIDER_URL
// names, and it treats every answer as untrusted:
//
//   - the provider must echo the model and version it used, and they must
//     match the contract's pin exactly — NP-35 "embedding model changes
//     silently at provider → pinned model/version mismatch blocks indexing/query
//     generation";
//   - every vector must have the pinned width;
//   - the text sent is preprocessed HERE, by the pinned profile, so a provider
//     that changed its own normalisation cannot move the embedding space.
//
// With no provider configured, Unconfigured refuses every call. A semantic scope
// then cannot build a generation or answer a query — loud, like an R2 scope with
// no hydrator — rather than silently degrading to lexical results that claim to
// be semantic.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
)

var (
	// ErrNoProvider means no embedding provider is configured. A deployment
	// state, not a transient failure.
	ErrNoProvider = errors.New("no embedding provider is configured (EMBEDDING_PROVIDER_URL)")
	// ErrModelMismatch means the provider answered with a model, version or
	// width other than the pinned one. ESR-019 SEMANTIC_MODEL_MISMATCH.
	ErrModelMismatch = errors.New("embedding provider answered with a model other than the pinned one")
	// ErrUnavailable means the provider could not be reached or answered with
	// something unusable.
	ErrUnavailable = errors.New("embedding provider unavailable")
)

// Embedder turns preprocessed text into vectors under a pinned spec.
type Embedder interface {
	// Embed returns one vector per input, in order, or an error. Callers pass
	// RAW text; preprocessing is applied inside, by the spec's pinned profile.
	Embed(ctx context.Context, spec domain.EmbeddingSpec, inputs []string) ([][]float32, error)
	// Configured reports whether a provider exists at all, so a control-plane
	// act can refuse up front rather than build a generation nothing can fill.
	Configured() bool
}

// ── Pinned preprocessing ─────────────────────────────────────────────────────

// ProfileNFKCWhitespaceV1 is the one supported preprocessing profile: Unicode
// NFKC, every run of whitespace (including line breaks) collapsed to a single
// space, control characters removed, trimmed. Versioned in its name because it
// is part of the embedding pin: changing what it does means a new name, a new
// contract version and a new generation.
const ProfileNFKCWhitespaceV1 = "nfkc-ws-v1"

// SupportedProfile reports whether a contract may pin this profile.
func SupportedProfile(name string) bool { return name == ProfileNFKCWhitespaceV1 }

// SupportedSimilarity reports whether the engine can index this space type.
func SupportedSimilarity(name string) bool {
	switch name {
	case "cosinesimil", "l2", "innerproduct":
		return true
	}
	return false
}

// Preprocess applies a pinned profile. Unknown profiles return an error rather
// than the input unchanged: an unrecognised profile silently treated as "none"
// is exactly the unpinned drift §10.1 forbids.
func Preprocess(profile, text string) (string, error) {
	if profile != ProfileNFKCWhitespaceV1 {
		return "", fmt.Errorf("unsupported preprocessing profile %q", profile)
	}
	normalised := norm.NFKC.String(text)
	var b strings.Builder
	space := false
	for _, r := range normalised {
		switch {
		case unicode.IsSpace(r):
			space = true
		case unicode.IsControl(r):
			// dropped
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// DocumentText builds the text a document is embedded from: the pinned source
// fields, in the pinned order, from the PROJECTED field values.
//
// Projected, not raw payload. A vector is a representation of content, and
// content the contract did not register — or registered as prohibited — must
// no more reach an embedding than it may reach the lexical index (INV-26). The
// field name is included so "status: overdue" and "title: overdue" embed
// differently, which they are.
func DocumentText(spec domain.EmbeddingSpec, fields map[string]any) string {
	parts := make([]string, 0, len(spec.SourceFields))
	for _, name := range spec.SourceFields {
		v, ok := fields[name]
		if !ok || v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			continue
		}
		parts = append(parts, name+": "+s)
	}
	return strings.Join(parts, "\n")
}

// ── Unconfigured ─────────────────────────────────────────────────────────────

// Unconfigured is the Embedder when no provider exists. It refuses everything.
type Unconfigured struct{}

func (Unconfigured) Embed(context.Context, domain.EmbeddingSpec, []string) ([][]float32, error) {
	return nil, ErrNoProvider
}
func (Unconfigured) Configured() bool { return false }

// ── HTTP provider ────────────────────────────────────────────────────────────

// HTTPProvider speaks the provider protocol:
//
//	POST {base}/v1/embeddings
//	{"model": "...", "model_version": "...", "input": ["...", ...]}
//	→ 200 {"model": "...", "model_version": "...",
//	       "data": [{"index": 0, "embedding": [0.1, ...]}, ...]}
//
// Deliberately minimal, so any provider behind an adapter can satisfy it and
// OD-10 stays open. The provider must echo model and model_version; a response
// that omits them is treated as a mismatch, not as agreement.
type HTTPProvider struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewHTTPProvider(baseURL, token string, timeout time.Duration) *HTTPProvider {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

func (p *HTTPProvider) Configured() bool { return true }

type embedRequest struct {
	Model        string   `json:"model"`
	ModelVersion string   `json:"model_version"`
	Input        []string `json:"input"`
}

type embedResponse struct {
	Model        string `json:"model"`
	ModelVersion string `json:"model_version"`
	Data         []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (p *HTTPProvider) Embed(ctx context.Context, spec domain.EmbeddingSpec, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	prepared := make([]string, len(inputs))
	for i, in := range inputs {
		out, err := Preprocess(spec.Preprocessing, in)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrModelMismatch, err)
		}
		prepared[i] = out
	}

	body, err := json.Marshal(embedRequest{Model: spec.Model, ModelVersion: spec.ModelVersion, Input: prepared})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	// Trace headers only, and only when there is a request behind this call.
	// NOT the caller's identity: the provider is a processor of text, not a
	// party to the authorization decision, and handing it principal ids would
	// widen what it learns about who searches for what (§9.2).
	if env, ok := envelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			req.Header.Set(envelope.HeaderRequestID, env.RequestID)
		}
		if env.CorrelationID != "" {
			req.Header.Set(envelope.HeaderCorrelationID, env.CorrelationID)
		}
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: provider answered %d: %s", ErrUnavailable, resp.StatusCode, raw)
	}

	var out embedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: undecodable response: %v", ErrUnavailable, err)
	}
	return checkPinned(spec, out, len(inputs))
}

// checkPinned is NP-35 applied to one response.
func checkPinned(spec domain.EmbeddingSpec, out embedResponse, want int) ([][]float32, error) {
	if out.Model != spec.Model || out.ModelVersion != spec.ModelVersion {
		return nil, fmt.Errorf("%w: pinned %s@%s, provider used %q@%q",
			ErrModelMismatch, spec.Model, spec.ModelVersion, out.Model, out.ModelVersion)
	}
	if len(out.Data) != want {
		return nil, fmt.Errorf("%w: asked for %d vectors, got %d", ErrUnavailable, want, len(out.Data))
	}
	sort.SliceStable(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vectors := make([][]float32, want)
	for i, d := range out.Data {
		if d.Index != i {
			return nil, fmt.Errorf("%w: response indexes are not 0..%d", ErrUnavailable, want-1)
		}
		if len(d.Embedding) != spec.Dimensions {
			return nil, fmt.Errorf("%w: pinned width %d, provider returned %d",
				ErrModelMismatch, spec.Dimensions, len(d.Embedding))
		}
		vectors[i] = d.Embedding
	}
	return vectors, nil
}
