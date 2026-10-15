-- 000032 down: nothing to undo. 000032 only re-asserts grants that 000011 and
-- 000031 already intended; revoking them here would leave the service role
-- unable to run partition maintenance or the purge, which no earlier state of
-- this schema meant. 000011's and 000031's own down migrations remove the
-- functions (and with them the grants).
SELECT 1;
