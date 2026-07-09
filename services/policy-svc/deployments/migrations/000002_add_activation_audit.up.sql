-- 000002_add_activation_audit.up.sql
-- Policy Service — add activation audit columns to policy_versions
--
-- Closes a gap found 2026-07-08: activated_by_principal_id was already
-- required as API input on POST .../activate (see internal/handler), but
-- nothing persisted who activated a version or when. 04-data-model.md
-- names both activated_by and activated_at on PolicyVersion, and the
-- "preserve effective-dated activation" evidence obligation
-- (03-microservices.md §8.1) is not fully met without recording the actor.
--
-- Nullable: NULL means "never activated" (still DRAFT). Set once on a
-- version's first (and only) real activation transition and never
-- overwritten afterwards — including when the version is later
-- superseded, its own activation history stands unchanged.

ALTER TABLE policy_versions
    ADD COLUMN activated_by_principal_id TEXT,
    ADD COLUMN activated_at              TIMESTAMPTZ;
