# ADR-007: Groq as the model provider (openai/gpt-oss-120b)

- Status: Proposed. Pending Global CTO, CISO and DPO (DEC-05 vendor, region and DPA).
- Date: 2026-10-28
- Context: The product owner selected Groq with `openai/gpt-oss-120b`. Until now only the deterministic
  mock provider existed (ADR-003).

## Decision

- **Provider:** Groq is added behind the existing `model.Provider` interface (`internal/model/groq.go`).
  There is no Go Groq SDK, so it calls Groq's documented OpenAI-compatible REST API
  (`POST https://api.groq.com/openai/v1/chat/completions`) with the standard library. No new
  dependency was added.
- **Request** (checked against Groq's documentation for this model):
  - `max_completion_tokens` (`max_tokens` is deprecated);
  - `reasoning_effort: low` and `include_reasoning: false`, so no reasoning is returned
    (`reasoning_format` is not supported for gpt-oss);
  - `response_format: json_schema` with `strict: true`, the draft contract `{text, claim_refs,
    evidence_refs}`;
  - temperature 0.2;
  - no tools and no streaming.
- **Configuration** is server-only:
  - `MODEL_PROVIDER=groq`;
  - `GROQ_API_KEY` is required, otherwise the service refuses to start;
  - `GROQ_MODEL` defaults to `openai/gpt-oss-120b` and must be in `PUBLIC_AI_ALLOWED_MODEL_IDS`.
  - The key is never logged or echoed in errors, and the frontend never sees it.
- **System prompt:** the Suivo main system prompt is stored verbatim in
  `internal/model/prompts/suivo_system_prompt.txt`. It is embedded, pinned by hash in
  `model.PromptVersion`, and replaces the earlier L1-PROMPT-001 text.
- **Controls stay in code:**
  - Retrieval eligibility (audience, approval, validity, market, locale, revocation), typed claims,
    refusals, clarification, output validation and the commit-time revocation check run whatever the
    provider.
  - The model sees only eligible records and verified claims, each with its metadata.
  - A draft that cites anything it was not given, invents a figure or breaks the contract is
    rejected; one repair attempt follows, then a safe abstention.
- **Errors:**
  - 429 and 5xx are retried once by the gateway;
  - other statuses are not retried;
  - a deadline is reported as MODEL_TIMEOUT;
  - repeated failures open the circuit breaker; the visitor sees UNAVAILABLE with official routes.

## Consequences

- Visitor questions (redacted) and approved public excerpts are sent to Groq. A DPA, region and
  retention decision is needed before production (DEC-05, DEC-06).
- The prompt names the assistant "Suivo", while the public display name is "ZoikoSuite Assistant"
  until Legal and Brand clear the name. The output validator still rejects replies that say
  "Suivo", so such a draft is repaired or abstained, not shown.
- The prompt's module taxonomy (4 domains, 10 modules) was supplied with the prompt. The engineering
  documents name the ten modules and four domains (Doc 03 §5) but not their stable IDs (see
  `docs/open-decisions.md`, OD-01; corrected 2026-10-28).
