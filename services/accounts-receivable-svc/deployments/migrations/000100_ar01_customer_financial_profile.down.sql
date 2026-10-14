-- Migration: 000100_ar01_customer_financial_profile.down.sql
DROP TABLE IF EXISTS ar01_profile_history;
DROP TABLE IF EXISTS ar01_credit_limit_requests;
DROP TABLE IF EXISTS ar01_payment_term_versions;
DROP TABLE IF EXISTS ar01_profile_versions;
DROP TABLE IF EXISTS ar01_financial_profiles;
DROP FUNCTION IF EXISTS ar01_check_term_overlap();
DROP FUNCTION IF EXISTS ar01_check_version_order();
