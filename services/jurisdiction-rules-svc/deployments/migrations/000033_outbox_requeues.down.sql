-- 000033 down. The requeue log is the evidence of who put a refused event back on
-- the wire and why; it is refused while any requeue is recorded.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM outbox_requeues) THEN
        RAISE EXCEPTION '000033 down refused: outbox requeues are recorded and would be lost';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_event_outbox_parked;
DROP TABLE IF EXISTS outbox_requeues;
DROP FUNCTION IF EXISTS jur_outbox_requeue_immutable();
-- UPDATE on event_outbox stays granted: the relay has always needed it to mark
-- events published, whatever granted it first.
