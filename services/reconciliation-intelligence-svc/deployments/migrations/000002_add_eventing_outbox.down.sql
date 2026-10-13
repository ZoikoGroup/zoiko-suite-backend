-- Migration: 000002_add_eventing_outbox.down.sql
--
-- Manual rollback only. Any event still in eventing_outbox is lost with the
-- table. Drain the outbox (outbox_backlog_events = 0) before running this.

DROP TABLE IF EXISTS eventing_outbox;
