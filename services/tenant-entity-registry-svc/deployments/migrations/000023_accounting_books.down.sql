-- 000023_accounting_books.down.sql
-- Accounting books are the identity ledgers and reports reference. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM accounting_books) THEN
        RAISE EXCEPTION '000023 down refused: accounting books exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS book_transitions;
DROP TABLE IF EXISTS book_basis_versions;
DROP TABLE IF EXISTS accounting_books;
DROP FUNCTION IF EXISTS book_transition_guard();
DROP FUNCTION IF EXISTS book_basis_guard();
DROP FUNCTION IF EXISTS accounting_book_guard();
DROP FUNCTION IF EXISTS book_entity_tenant_guard();
