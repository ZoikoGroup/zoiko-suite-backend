-- Migration 000013: admit VERIFIED_CLIENT as a source-channel basis.
--
-- domain.BasisVerifiedClient ("the channel came from the verified IdP client,
-- azp") was added with the S1-B1 work, but 000009's CHECK still listed only
-- the four bases that existed then. Nothing produced the value until
-- /v1/authenticate could verify a client (Group 1 register R-1), so the gap was
-- invisible; with a verified client, EVERY human resolve failed 503 on this
-- constraint ("persist session evidence: ... source_channel_basis_known").
-- Found by the 14 Oct 2026 live auth-plane E2E.

ALTER TABLE session_contexts DROP CONSTRAINT IF EXISTS session_contexts_source_channel_basis_known;
ALTER TABLE session_contexts ADD CONSTRAINT session_contexts_source_channel_basis_known CHECK (
    source_channel_basis IS NULL OR source_channel_basis IN (
        'SERVER_DERIVED', 'VERIFIED_CLIENT', 'CLIENT_ASSERTED_CONSISTENT', 'REJECTED_INCONSISTENT', 'NOT_PRESENTED'));
