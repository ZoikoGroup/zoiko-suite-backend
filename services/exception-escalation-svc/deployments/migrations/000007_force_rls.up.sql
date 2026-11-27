-- Defence in depth: FORCE row-level security so the policies also bind the
-- table owner, not only the non-owner app role (2026-10-28 Blocks 6-10 audit:
-- RLS was ENABLEd but not FORCEd on these tables).
ALTER TABLE escalation_records FORCE ROW LEVEL SECURITY;
ALTER TABLE exception_cases FORCE ROW LEVEL SECURITY;
