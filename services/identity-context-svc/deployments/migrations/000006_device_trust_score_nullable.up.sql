-- Migration: 000006_device_trust_score_nullable.up.sql
--
-- Lets device_trust_score be absent, because it always is.
--
-- 000005 declared it NOT NULL DEFAULT 0, matching the resolver, which wrote a
-- literal 0 with a TODO beside it. On a 0-100 trust scale 0 is not "unknown",
-- it is "maximally untrusted" — so every session ever issued carried an
-- evidence record asserting its device had been assessed and failed. Nothing
-- assesses devices: no fingerprint claim reaches this service from anywhere.
--
-- NULL now means not measured. The 0-100 CHECK is unaffected — a NULL makes it
-- evaluate to NULL, which passes — so a real score, when one exists, is still
-- range-enforced.
--
-- Note the deliberate difference from risk_signal_source, which uses an
-- 'UNAVAILABLE' sentinel rather than NULL. A risk score has several possible
-- producers and naming which one answered is worth a column. Device trust has
-- one conceptual source, so present-or-absent carries the whole distinction and
-- a second column would say nothing.
--
-- Existing rows are left at 0 rather than backfilled to NULL: they were written
-- by a resolver that genuinely meant "0", and rewriting evidence to say
-- something the original record did not is not a migration this table should
-- ever perform.

ALTER TABLE session_contexts
    ALTER COLUMN device_trust_score DROP NOT NULL,
    ALTER COLUMN device_trust_score DROP DEFAULT;
