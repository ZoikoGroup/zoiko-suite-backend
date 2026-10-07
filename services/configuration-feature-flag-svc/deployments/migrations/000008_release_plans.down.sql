-- Migration: 000008_release_plans.down.sql
--
-- Restores the original two-event outbox CHECK that 000003 created (a
-- rollback would also have to remove the event builders from
-- internal/events, otherwise the narrowed constraint starts failing enqueues
-- again — noted here because the Go side of this batch lives in a different
-- change than the migrations).

ALTER TABLE event_outbox DROP CONSTRAINT event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN ('config.updated', 'feature_flag.updated'));

DROP FUNCTION IF EXISTS config_environment_manifest(TEXT);

DROP POLICY IF EXISTS flag_retirements_read_all_write_admin ON flag_retirements;
DROP TABLE IF EXISTS flag_retirements;

DROP POLICY IF EXISTS release_plans_read_all_write_scoped ON release_plans;
DROP TABLE IF EXISTS release_plans;