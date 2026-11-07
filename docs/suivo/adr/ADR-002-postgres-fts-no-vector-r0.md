# ADR-002: Lexical retrieval over signed release bundles (no vector in R0)

- Status: Proposed — pending Global CTO + Knowledge Lead
- Date: 2026-10-27 (revised during STEP 2)
- Context: R0 retrieval may only see the PUBLIC_APPROVED namespace (Doc 06 §09). Doc 03 §11 requires
  immutable releases, promotion by atomic pointer swap and no in-place edits of production chunks.
  Doc 04 §07 leaves the search engine open; no document requires vector search for R0.
- Decision:
  - A knowledge release (`kind = kb`) is a signed, immutable manifest containing the approved sources,
    their chunks and the official route registry. A claim release (`kind = claims`) is a separately
    signed P1 registry. Both are stored in `public_ai.releases`; promotion changes only
    `public_ai.release_pointer` (atomic swap).
  - At runtime the active release is verified and loaded into an in-process lexical index
    (BM25-style term scoring + exact-ID lookup). Mandatory filters run before ranking in the Doc 05 §18
    order: release → classification → audience → effective/revoked → search → rank → answerability.
  - Revocations live in the append-only `public_ai.revocations` ledger (DB trigger rejects
    UPDATE/DELETE) and are applied on top of whichever release is active, so rollback never restores a
    revoked source or claim.
  - A `Retriever` interface keeps the door open for Postgres FTS or vector search later.
- Why not Postgres FTS tables (the original STEP 0 proposal): editable source/chunk rows would allow
  in-place mutation of production content, which Doc 03 §11 forbids, and would duplicate the editorial
  plane that belongs to the R1 staff console (Doc 09). Corpus size for R0 is small.
- Consequences: deterministic, testable retrieval; reload on pointer change; memory grows with corpus
  size (re-evaluate before R1 or when the corpus exceeds a few thousand chunks).
- Dev defaults: chunk band 300–700 tokens with 40–80 overlap (approximated by words), pending KB
  governance confirmation.

## Update 2026-10-28: Retriever interface and vector slot

- `knowledge.Retriever` (`Retrieve`, `ByIntent`, `ReleaseID`) is the only way the pipeline searches. The lexical `*knowledge.Index` implements it and stays the R0 default, so behaviour is unchanged.
- `knowledge.Hybrid` is the vector slot from Doc 03 §13–14:
  - it embeds every chunk of a release once, through a pinned `knowledge.Embedder` (`ModelVersion`, `Embed`);
  - it adds vector recall to BM25 and fuses the two rankings with reciprocal rank fusion;
  - the mandatory filters run before vector candidates are ranked;
  - if the keyword ranking says the best match is stale or revoked, that outcome stands;
  - if the embedder fails on a query, the keyword result is used unchanged.
- It is switched on by setting `release.Runtime.Embedder`. If embedding a release fails, that release is served with keyword search only and `Runtime.VectorError()` reports why; the service never goes not-ready because of it.
- Not done, pending approval:
  - no embedding model is chosen or implemented, so nothing sets `Embedder` in `cmd/server`;
  - `DefaultMinCosine` (0.5) is a placeholder and must be tuned on the golden and KB sets for the approved model.
- Embedding inputs are approved public chunk text and the visitor's redacted question only. A hosted embedding model is a new data processor, so it needs the same DPA and region decision as the chat model (DEC-05).
