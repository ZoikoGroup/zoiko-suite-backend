"""Unit tests for migrate.py's baseline detection (no database needed).

    python -m unittest deployments/migrate_test.py
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import migrate  # noqa: E402


class Created(unittest.TestCase):
    def test_tables_indexes_columns_constraints(self):
        sql = """
        CREATE TABLE IF NOT EXISTS public.delegation_grants (id uuid);
        CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON t (x);
        ALTER TABLE delegation_grants
            ADD COLUMN IF NOT EXISTS version BIGINT,
            ADD COLUMN reason TEXT;
        ALTER TABLE t ADD CONSTRAINT t_check CHECK (x > 0) NOT VALID;
        CREATE OR REPLACE FUNCTION f_x() RETURNS trigger AS $$ BEGIN
            CREATE TABLE not_really_created (x int);  -- inside a body: ignored
        END $$ LANGUAGE plpgsql;
        CREATE TRIGGER trg_x BEFORE UPDATE ON t FOR EACH ROW EXECUTE FUNCTION f_x();
        CREATE POLICY p_x ON t USING (true);
        CREATE TYPE mood AS ENUM ('a');
        """
        got = set((k, n, t) for k, n, t in migrate.created(sql))
        want = {("rel", "delegation_grants", None), ("rel", "idx_a", None),
                ("col", "version", "delegation_grants"), ("col", "reason", "delegation_grants"),
                ("con", "t_check", None), ("fn", "f_x", None), ("trg", "trg_x", None),
                ("pol", "p_x", None), ("typ", "mood", None)}
        self.assertEqual(want, got)

    def test_comments_are_not_objects(self):
        self.assertEqual([], migrate.created("-- CREATE TABLE ghost (x int);"))


class Dropped(unittest.TestCase):
    def test_later_drops_and_renames(self):
        sql = """DROP INDEX IF EXISTS idx_old; ALTER TABLE t DROP COLUMN IF EXISTS c1;
                 ALTER TABLE t RENAME COLUMN c2 TO c3; DROP TABLE IF EXISTS public.gone;"""
        self.assertEqual({"idx_old", "c1", "c2", "gone"}, migrate.dropped(sql))


class Checksum(unittest.TestCase):
    def test_line_endings_do_not_change_the_checksum(self):
        # init-db.sh hashes with `tr -d '\r'`; a Windows checkout must agree.
        import tempfile
        d = tempfile.mkdtemp()
        a, b = os.path.join(d, "a.sql"), os.path.join(d, "b.sql")
        open(a, "wb").write(b"SELECT 1;\r\nSELECT 2;\r\n")
        open(b, "wb").write(b"SELECT 1;\nSELECT 2;\n")
        self.assertEqual(migrate.checksum(a), migrate.checksum(b))


class Services(unittest.TestCase):
    def test_every_listed_database_has_a_mounted_directory(self):
        svcs = migrate.services()
        self.assertGreater(len(svcs), 0)
        for db, d in svcs:
            self.assertTrue(os.path.isdir(d), f"{db}: {d} does not exist")
            self.assertTrue(migrate.files(d), f"{db}: no *.up.sql in {d}")


if __name__ == "__main__":
    unittest.main()
