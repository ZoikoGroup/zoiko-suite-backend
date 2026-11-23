-- Reverts 000041 by doing nothing, on purpose. The definition before it
-- depended on the order 000037 and 000038 ran (see the up migration), so there
-- is no single earlier state to restore, and this one is correct for the
-- schema 000040 leaves: every column it reads exists from 000038 on. The
-- 000037 and 000038 down migrations each restore 000026's definition.
SELECT 1;
