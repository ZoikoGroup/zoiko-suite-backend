-- Migration: 000013_add_eventing_outbox.down.sql
--
-- Manual rollback only. Events still undelivered in eventing_outbox are lost
-- with the table: the carried-over legacy rows still exist in outbox_events,
-- but anything produced after 000013 exists nowhere else. Drain the outbox
-- (outbox_backlog_events = 0) before running this.

DROP TABLE IF EXISTS eventing_outbox;
