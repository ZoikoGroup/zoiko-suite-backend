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
