-- Migration: 001100_ar09_collections.down.sql
DROP TABLE IF EXISTS ar09_history;
DROP TABLE IF EXISTS ar09_holds;
DROP TABLE IF EXISTS ar09_promises;
DROP TABLE IF EXISTS ar09_dunning_actions;
DROP TABLE IF EXISTS ar09_case_items;
DROP TABLE IF EXISTS ar09_collection_cases;
DROP TABLE IF EXISTS ar09_dunning_policies;
DROP FUNCTION IF EXISTS ar09_guard_promise();
DROP FUNCTION IF EXISTS ar09_guard_hold();
DROP FUNCTION IF EXISTS ar09_guard_case();
