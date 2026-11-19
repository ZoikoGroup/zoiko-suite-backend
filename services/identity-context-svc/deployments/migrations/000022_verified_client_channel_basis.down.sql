-- Reverts 000013. Refused while any session records a VERIFIED_CLIENT basis:
-- the narrower CHECK could not be re-added over those rows, and rewriting them
-- would falsify session evidence.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM session_contexts WHERE source_channel_basis = 'VERIFIED_CLIENT') THEN
        RAISE EXCEPTION '000013 down refused: sessions record a VERIFIED_CLIENT source channel basis';
    END IF;
END $$;

ALTER TABLE session_contexts DROP CONSTRAINT IF EXISTS session_contexts_source_channel_basis_known;
ALTER TABLE session_contexts ADD CONSTRAINT session_contexts_source_channel_basis_known CHECK (
    source_channel_basis IS NULL OR source_channel_basis IN (
        'SERVER_DERIVED', 'CLIENT_ASSERTED_CONSISTENT', 'REJECTED_INCONSISTENT', 'NOT_PRESENTED'));
