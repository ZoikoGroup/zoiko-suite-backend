-- Migration 000024: WFC-04 Cases (Full Implementation) - DOWN

DROP TRIGGER IF EXISTS work_items_updated_at ON work_items;
DROP TABLE IF EXISTS work_item_assignments CASCADE;
DROP TABLE IF EXISTS work_items CASCADE;
DROP TABLE IF EXISTS case_aliases CASCADE;
DROP TABLE IF EXISTS case_transitions CASCADE;
DROP TABLE IF EXISTS case_links CASCADE;
DROP TABLE IF EXISTS case_notes CASCADE;
DROP TABLE IF EXISTS case_members CASCADE;
DROP TABLE IF EXISTS cases CASCADE;

DROP TYPE IF EXISTS case_state;
DROP TYPE IF EXISTS case_severity;
DROP TYPE IF EXISTS case_confidentiality;
DROP TYPE IF EXISTS case_family;