# ADR-002: Postgres FTS retrieval (no vector in R0)

- Status: Proposed — pending Global CTO + Knowledge Lead
- Date: 2026-10-27
- Context: R0 retrieval needs PUBLIC_APPROVED namespace only; vector search not required by docs.
- Decision: Use Postgres full-text search (lexical) + exact-ID lookup with mandatory filters (audience/classification/effective/revoked). Define Retriever interface to allow adding vector later. Chunking band 300–700 tokens, 40–80 overlap.
- Consequences: Simple, deterministic, fits existing Postgres usage; future extension possible via interface.
- Dev defaults: Chunk band as above (pending KB governance confirmation).
