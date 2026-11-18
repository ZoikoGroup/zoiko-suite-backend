-- 000036 down. An operation is the record of a background compile or test run
-- and of who asked for it; refused while one is still queued or running.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM async_operations WHERE state IN ('queued', 'running', 'waiting_external', 'cancel_requested')) THEN
        RAISE EXCEPTION '000036 down refused: async operations are still in progress';
    END IF;
END
$$;

DROP TABLE IF EXISTS async_operations;
