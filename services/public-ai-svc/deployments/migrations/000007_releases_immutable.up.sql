-- Doc 08 §04/§07, OPR-033: a signed release artifact is immutable once
-- stored. Promotion and rollback only move release_pointer; the manifest and
-- signature rows are never rewritten or removed, so the history of what was
-- served stays verifiable. A trigger rejects UPDATE and DELETE, like the
-- revocations ledger and the control audit.
CREATE FUNCTION public_ai.reject_release_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'releases are immutable';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER releases_immutable
    BEFORE UPDATE OR DELETE ON public_ai.releases
    FOR EACH ROW EXECUTE FUNCTION public_ai.reject_release_change();
