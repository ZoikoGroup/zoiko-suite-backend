-- Migration: 000009_add_elimination_pair_scope.up.sql
--
-- SPEC_DEVIATIONS A5: "The elimination-bound check on proposals is
-- tenant-wide" — CreateEliminationProposal checked an ELIMINATION
-- adjustment's amount against every eliminable intercompany balance in the
-- whole tenant, not the specific pair it eliminates. These columns let an
-- ELIMINATION proposal name the pair, so the bound check (and the stored
-- evidence of what it was checked against) is scoped to that pair and
-- period, not the tenant. NULL for MANUAL adjustments, which have no
-- intercompany pair to bound against.

ALTER TABLE consolidation_adjustments
    ADD COLUMN source_legal_entity_id UUID,
    ADD COLUMN target_legal_entity_id UUID;
