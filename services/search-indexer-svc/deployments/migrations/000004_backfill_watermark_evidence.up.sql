-- 000004 — generation backfill state, the checkpoint-watermark unit reset, and
-- the embedding model on search evidence. Idempotent, like every migration here.

-- ── Generation backfill (INV-21, NP-18) ──────────────────────────────────
--
-- A new generation used to receive only events that arrived AFTER it was
-- activated: the consumer wrote into the ACTIVE generation alone, and
-- validation passed an empty index ("engine=0 ledger=4"), so activating a
-- rebuild replaced a populated index with an empty one (found live, 30 Sep
-- 2026). A generation is now backfilled by replaying its source topic, and
-- READY requires the backfill to have COMPLETED. NULL is a generation created
-- before this migration, which was never backfilled.
ALTER TABLE index_generations ADD COLUMN IF NOT EXISTS backfill_state TEXT;
ALTER TABLE index_generations ADD COLUMN IF NOT EXISTS backfill_note  TEXT NOT NULL DEFAULT '';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'index_generations_backfill_state') THEN
        ALTER TABLE index_generations ADD CONSTRAINT index_generations_backfill_state
            CHECK (backfill_state IS NULL OR backfill_state IN ('PENDING', 'RUNNING', 'COMPLETE', 'FAILED'));
    END IF;
END $$;

-- ── Watermark unit change ────────────────────────────────────────────────
--
-- Before 30 Sep 2026 the watermark was wall-clock milliseconds (~1.8e12); it is
-- now the consumer group's committed offset sum. The upsert keeps
-- GREATEST(old, new), so an old wall-clock value would pin the watermark above
-- any real offset forever and esr.index_checkpoint.advanced would never fire
-- again. No real offset sum is anywhere near 1e11; anything above it is a
-- wall-clock value and is reset to "not yet measured".
UPDATE index_checkpoints SET watermark = -1 WHERE watermark > 100000000000;

-- ── TC-02 for semantic answers ───────────────────────────────────────────
--
-- Which pinned "model@version" answered a semantic search. Empty for lexical.
ALTER TABLE search_evidence ADD COLUMN IF NOT EXISTS embedding_model TEXT NOT NULL DEFAULT '';
