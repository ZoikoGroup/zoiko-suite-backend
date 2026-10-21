-- Migration: 000007_control_certifications.down.sql
--
-- Manual rollback only. Every recorded certification's authority snapshot,
-- accepted exceptions and assessed exposure are lost; the runs' states and
-- transition history are untouched.

DROP TABLE IF EXISTS control_certifications;
