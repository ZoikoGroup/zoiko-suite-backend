package model

// Groq provider: chat completions on Groq's OpenAI-compatible API
// (POST https://api.groq.com/openai/v1/chat/completions, Bearer auth).
// Parameters checked against Groq's documentation for openai/gpt-oss-120b:
//   - max_completion_tokens (max_tokens is deprecated);
//   - reasoning_effort low|medium|high and include_reasoning=false, so the
//     reply carries no reasoning (reasoning_format is not supported here);
//   - response_format json_schema with strict=true (supported for this
//     model; cannot be combined with tools or streaming, neither is used).
// No tools are ever sent. The API key comes from the server environment and
// is never logged, echoed in errors, or sent anywhere but Groq.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GroqDefaultModel is the model the backend uses unless GROQ_MODEL says otherwise.
const GroqDefaultModel = "openai/gpt-oss-120b"

// GroqBaseURL is Groq's OpenAI-compatible API root.
const GroqBaseURL = "https://api.groq.com/openai/v1"

// Groq calls a Groq-hosted model. Zero values are filled with defaults.
type Groq struct {
	APIKey  string
	Model   string
	BaseURL string       // tests point this at a local server; production uses GroqBaseURL
	Client  *http.Client // the Gateway's context deadline bounds each call
}

func (g *Groq) Name() string { return "groq:" + g.model() }

func (g *Groq) model() string {
	if g.Model == "" {
		return GroqDefaultModel
	}
	return g.Model
}

// draftSchema is the strict output contract: the answer text and the ids of
// the claims and evidence records it used. Every property is required and
// additionalProperties is false, as strict mode requires.
var draftSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"text", "claim_refs", "evidence_refs"},
	"properties": map[string]any{
		"text":          map[string]any{"type": "string"},
		"claim_refs":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"evidence_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqRequest struct {
	Model               string         `json:"model"`
	Messages            []groqMessage  `json:"messages"`
	Temperature         float64        `json:"temperature"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
	ReasoningEffort     string         `json:"reasoning_effort"`
	IncludeReasoning    bool           `json:"include_reasoning"`
	ResponseFormat      map[string]any `json:"response_format"`
	Stream              bool           `json:"stream"`
}

type groqResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ErrProvider is a non-retryable provider failure (bad request, auth,
// unexpected response). Its message never contains the key or the prompt.
var ErrProvider = errors.New("model provider error")

// maxGroqBody bounds how much of a response is read.
const maxGroqBody = 1 << 20

func (g *Groq) Generate(ctx context.Context, req Request) (Draft, error) {
	if g.APIKey == "" {
		return Draft{}, fmt.Errorf("%w: GROQ_API_KEY is not configured", ErrProvider)
	}
	system, user := BuildMessages(req)
	maxOut := req.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = 400
	}
	// gpt-oss reasons before answering; reasoning tokens count toward the
	// completion budget even though they are not returned.
	body, err := json.Marshal(groqRequest{
		Model:               g.model(),
		Messages:            []groqMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature:         0.2,
		MaxCompletionTokens: maxOut + 1024,
		ReasoningEffort:     "low",
		IncludeReasoning:    false,
		ResponseFormat: map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "suivo_answer", "strict": true, "schema": draftSchema},
		},
	})
	if err != nil {
		return Draft{}, fmt.Errorf("%w: encode request", ErrProvider)
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = GroqBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Draft{}, fmt.Errorf("%w: build request", ErrProvider)
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Draft{}, ctx.Err() // deadline: the pipeline reports MODEL_TIMEOUT
		}
		return Draft{}, fmt.Errorf("%w: network", ErrTransient)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxGroqBody))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Draft{}, fmt.Errorf("%w: status %d", ErrTransient, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		// Includes 3xx: redirects are never followed (egress allowlist,
		// Doc 06 §12), so a redirect is a provider error, not a new hop.
		// Never echo the response body: it may quote the request.
		return Draft{}, fmt.Errorf("%w: status %d", ErrProvider, resp.StatusCode)
	}
	var out groqResponse
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Draft{}, fmt.Errorf("%w: unexpected response", ErrProvider)
	}
	d := Draft{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	parsed, ok := decodeDraft(out.Choices[0].Message.Content)
	if !ok {
		// Not the contract: an empty draft fails output validation, which
		// triggers the single repair attempt and then a safe abstention.
		return d, nil
	}
	d.Text, d.ClaimRefs, d.EvidenceRefs = parsed.Text, parsed.ClaimRefs, parsed.EvidenceRefs
	return d, nil
}

type draftReply struct {
	Text         string   `json:"text"`
	ClaimRefs    []string `json:"claim_refs"`
	EvidenceRefs []string `json:"evidence_refs"`
}

// decodeDraft parses the model's reply strictly against draftSchema: an
// unknown field (for example a model-invented "actions" or "status") or
// trailing data rejects the whole reply, so a model can never smuggle a
// privileged field past the contract (QA-T042, Doc 06 §08).
func decodeDraft(content string) (draftReply, bool) {
	var parsed draftReply
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return draftReply{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return draftReply{}, false
	}
	return parsed, true
}

// httpClient never follows redirects: the provider host is the only
// approved egress (Doc 06 §12), so a 3xx is returned as is and treated as a
// provider error. A caller-supplied client is copied, never mutated.
func (g *Groq) httpClient() *http.Client {
	c := http.Client{Timeout: 60 * time.Second}
	if g.Client != nil {
		c = *g.Client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// SEC-T050: never dial a metadata or link-local address, even via DNS.
	c.Transport = guardedTransport(c.Transport)
	return &c
}
