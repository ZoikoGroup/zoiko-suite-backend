#!/usr/bin/env python3
"""Tracked migrations for EXISTING databases.

init-db.sh runs only when the Postgres volume is created, so a migration added
later never reached an existing stack — and nothing recorded what had run. This
session found delegated_authority stuck at 000004 and notification on a
pre-merge lineage, each discovered only when the code failed against it.

Each database gets a schema_migrations table (init-db.sh now writes the same
table on a fresh volume). Commands:

  status   [--db NAME]                       what each database has and lacks
  baseline [--db NAME] [--assume-through F]   one-time, for an UNTRACKED database:
           record what is already applied. Detected from the objects each
           migration creates; a gap (a later migration present, an earlier one
           missing) is refused — name the last applied file with --assume-through
  up       [--db NAME] [--dry-run]            apply pending files in order

A file runs exactly as written (40 migrations manage their own transaction and
one uses CREATE INDEX CONCURRENTLY, so the runner cannot wrap them) with
ON_ERROR_STOP, and is recorded only after it succeeds. A recorded file whose
contents changed since is reported, never re-run.

Services and their migration directories are read from docker-compose.yml's
postgres mounts (<host dir>:/migrations/<name>) and init-db.sh's database list.
"""
import argparse, hashlib, os, re, subprocess, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.stdout.reconfigure(encoding="utf-8")


def services():
    """[(database, host migrations dir)] from init-db.sh + docker-compose.yml."""
    init = open(os.path.join(HERE, "init-db.sh"), encoding="utf8").read()
    block = re.search(r'SERVICES="\n(.*?)\n"', init, re.S).group(1)
    pairs = [l.strip().split(":") for l in block.splitlines() if ":" in l]
    compose = open(os.path.join(HERE, "docker-compose.yml"), encoding="utf8").read()
    mounts = dict((m[1], m[0]) for m in re.findall(r"-\s+(\.\./services/[^:]+):/migrations/([^:\s]+)", compose))
    out = []
    for db, d in pairs:
        if d not in mounts:
            sys.exit(f"init-db.sh lists {db}:{d} but docker-compose.yml mounts no /migrations/{d}")
        out.append((db, os.path.normpath(os.path.join(HERE, mounts[d]))))
    return out


def psql(container, db, sql=None, file=None):
    cmd = ["docker", "exec", "-i", container, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", db, "-tA"]
    data = open(file, "rb").read() if file else sql.encode()
    r = subprocess.run(cmd, input=data, capture_output=True)
    return r.returncode, r.stdout.decode("utf8", "replace").strip(), r.stderr.decode("utf8", "replace").strip()


def q(container, db, sql):
    rc, out, err = psql(container, db, sql)
    if rc != 0:
        raise RuntimeError(err)
    return out


TRACK = """CREATE TABLE IF NOT EXISTS schema_migrations (
    filename   TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    method     TEXT NOT NULL
);"""


def files(d):
    return sorted(f for f in os.listdir(d) if f.endswith(".up.sql"))


def checksum(path):
    return hashlib.sha256(open(path, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


def db_exists(container, db):
    return q(container, "postgres", f"SELECT 1 FROM pg_database WHERE datname = {lit(db)}") == "1"


def tracked(container, db):
    if q(container, db, "SELECT to_regclass('public.schema_migrations') IS NOT NULL") != "t":
        return None
    rows = q(container, db, "SELECT filename || '|' || checksum FROM schema_migrations")
    return dict(r.split("|", 1) for r in rows.splitlines() if r)


def lit(s):
    return "'" + s.replace("'", "''") + "'"


# ── detection for baseline ───────────────────────────────────────────────────

IDENT = r'"?([A-Za-z_][A-Za-z0-9_]*)"?'
QUAL = r'(?:"?[A-Za-z_][A-Za-z0-9_]*"?\.)?' + IDENT


def strip_bodies(sql):
    sql = re.sub(r"--[^\n]*", "", sql)
    return re.sub(r"\$([A-Za-z_]*)\$.*?\$\1\$", "''", sql, flags=re.S)


def created(sql):
    """Objects a migration creates, as (kind, name, table)."""
    s = strip_bodies(sql)
    objs = []
    for m in re.finditer(r"CREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?" + QUAL, s, re.I):
        objs.append(("rel", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+(?:MATERIALIZED\s+)?VIEW\s+(?:IF\s+NOT\s+EXISTS\s+)?" + QUAL, s, re.I):
        objs.append(("rel", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?" + IDENT, s, re.I):
        objs.append(("rel", m.group(1).lower(), None))
    for stmt in s.split(";"):
        t = re.search(r"ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?" + QUAL, stmt, re.I)
        if not t:
            continue
        for c in re.finditer(r"ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?" + IDENT, stmt, re.I):
            objs.append(("col", c.group(1).lower(), t.group(1).lower()))
    for m in re.finditer(r"ADD\s+CONSTRAINT\s+" + IDENT, s, re.I):
        objs.append(("con", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+" + QUAL, s, re.I):
        objs.append(("fn", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+(?:CONSTRAINT\s+)?TRIGGER\s+" + IDENT, s, re.I):
        objs.append(("trg", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+POLICY\s+" + IDENT, s, re.I):
        objs.append(("pol", m.group(1).lower(), None))
    for m in re.finditer(r"CREATE\s+TYPE\s+" + QUAL, s, re.I):
        objs.append(("typ", m.group(1).lower(), None))
    return objs


def dropped(sql):
    s = strip_bodies(sql)
    names = set()
    for m in re.finditer(r"DROP\s+(?:TABLE|INDEX|VIEW|MATERIALIZED\s+VIEW|FUNCTION|TRIGGER|POLICY|TYPE|CONSTRAINT)\s+"
                         r"(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?" + QUAL, s, re.I):
        names.add(m.group(1).lower())
    for m in re.finditer(r"DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?" + IDENT, s, re.I):
        names.add(m.group(1).lower())
    for m in re.finditer(r"RENAME\s+(?:COLUMN\s+)?" + IDENT + r"\s+TO", s, re.I):
        names.add(m.group(1).lower())
    return names


EXISTS = {
    "rel": "SELECT to_regclass({n}) IS NOT NULL",
    "col": "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = {t} AND column_name = {n})",
    "con": "SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = {n})",
    "fn": "SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = {n})",
    "trg": "SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = {n})",
    "pol": "SELECT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = {n})",
    "typ": "SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = {n})",
}


def detect(container, db, d):
    """[(file, state, missing)] with state applied | missing | unknown."""
    fs = files(d)
    texts = [open(os.path.join(d, f), encoding="utf8", errors="replace").read() for f in fs]
    later_drops = [set().union(*[dropped(t) for t in texts[i + 1:]]) if i + 1 < len(texts) else set() for i in range(len(texts))]
    checks, idx = [], []
    for i, t in enumerate(texts):
        for kind, name, table in created(t):
            if name in later_drops[i]:
                continue
            checks.append(EXISTS[kind].format(n=lit(name), t=lit(table or "")))
            idx.append((i, kind + ":" + (table + "." if table else "") + name))
    present = q(container, db, ";\n".join(checks) + ";").splitlines() if checks else []
    result = []
    for i, f in enumerate(fs):
        mine = [(o, p) for (j, o), p in zip(idx, present) if j == i]
        if not mine:
            result.append((f, "unknown", []))
        else:
            missing = [o for o, p in mine if p != "t"]
            result.append((f, "missing" if missing else "applied", missing))
    return result


# ── commands ─────────────────────────────────────────────────────────────────

def cmd_status(a):
    for db, d in targets(a):
        fs = files(d)
        if not db_exists(a.container, db):
            print(f"{db:32} MISSING DATABASE ({len(fs)} migrations on disk) — `up --db {db} --create-missing`")
            continue
        t = tracked(a.container, db)
        if t is None:
            print(f"{db:32} UNTRACKED  ({len(fs)} migrations on disk) — run baseline")
            continue
        pending = [f for f in fs if f not in t]
        changed = [f for f in fs if f in t and t[f] != checksum(os.path.join(d, f))]
        line = f"{db:32} {len(fs) - len(pending)}/{len(fs)} applied"
        if pending:
            line += f", pending: {', '.join(pending)}"
        if changed:
            line += f"  !! edited after applying: {', '.join(changed)}"
        print(line)


def cmd_baseline(a):
    rc = 0
    for db, d in targets(a):
        if not db_exists(a.container, db):
            print(f"{db}: database does not exist — nothing to baseline; `up --db {db} --create-missing` creates and migrates it")
            continue
        if tracked(a.container, db):
            print(f"{db}: already tracked — baseline is one-time")
            continue
        fs = files(d)
        if a.assume_through:
            if a.assume_through not in fs:
                print(f"{db}: --assume-through {a.assume_through} is not a migration here")
                rc = 1
                continue
            upto, method = fs.index(a.assume_through) + 1, "baseline-assumed"
        else:
            states = detect(a.container, db, d)
            first_missing = next((i for i, s in enumerate(states) if s[1] == "missing"), len(states))
            later_applied = [s[0] for s in states[first_missing:] if s[1] == "applied"]
            if later_applied:
                f, _, miss = states[first_missing]
                print(f"{db}: GAP — {f} looks unapplied (missing {', '.join(miss[:4])}) but {later_applied[0]} looks applied."
                      f" Inspect and re-run with --db {db} --assume-through <last applied file>.")
                rc = 1
                continue
            upto, method = first_missing, "baseline-detected"
        # --except: files inside the applied range that did NOT run — the case
        # two migrations sharing one number produces (one of the pair ran).
        unknown_except = [f for f in (a.except_ or []) if f not in fs[:upto]]
        if unknown_except:
            print(f"{db}: --except {', '.join(unknown_except)} is not inside the applied range")
            rc = 1
            continue
        applied = [f for f in fs[:upto] if f not in (a.except_ or [])]
        rows = ",".join(f"({lit(f)}, {lit(checksum(os.path.join(d, f)))}, {lit(method)})" for f in applied)
        sql = TRACK + (f"\nINSERT INTO schema_migrations (filename, checksum, method) VALUES {rows} ON CONFLICT DO NOTHING;" if rows else "")
        pending = [f for f in fs if f not in applied]
        if a.dry_run:
            print(f"{db}: would record {len(applied)}/{len(fs)} as applied ({method}); pending after: {', '.join(pending) or 'none'}")
            continue
        q(a.container, db, sql)
        print(f"{db}: recorded {len(applied)}/{len(fs)} ({method}); pending: {', '.join(pending) or 'none'}")
    return rc


def cmd_up(a):
    rc = 0
    for db, d in targets(a):
        if not db_exists(a.container, db):
            if not a.create_missing:
                print(f"{db}: database does not exist — re-run with --create-missing to create, migrate and grant it")
                rc = 1
                continue
            if a.dry_run:
                print(f"{db}: would create the database and apply all {len(files(d))} migrations")
                continue
            q(a.container, "postgres", f'CREATE DATABASE "{db}"')
            q(a.container, db, TRACK)
            print(f"{db}: created")
        t = tracked(a.container, db)
        if t is None:
            print(f"{db}: UNTRACKED — run `migrate.py baseline --db {db}` first; refusing to guess what has run")
            rc = 1
            continue
        for f in files(d):
            path = os.path.join(d, f)
            if f in t:
                if t[f] != checksum(path):
                    print(f"{db}: !! {f} was edited after it was applied — not re-run; reconcile by hand")
                continue
            if a.dry_run:
                print(f"{db}: would apply {f}")
                continue
            code, _, err = psql(a.container, db, file=path)
            if code != 0:
                print(f"{db}: FAILED {f}\n{err}\nStopping this database; nothing after {f} was attempted.")
                rc = 1
                break
            q(a.container, db, f"INSERT INTO schema_migrations (filename, checksum, method) VALUES ({lit(f)}, {lit(checksum(path))}, 'migrate.py')")
            print(f"{db}: applied {f}")
    if a.create_missing and not a.dry_run:
        grant_runtime_roles(a)
    return rc


def grant_runtime_roles(a):
    """Runtime access for databases created here, the way init-db.sh grants it
    on a fresh volume: per-service app_ roles (create-app-roles.sh, idempotent),
    else the shared zoiko_app with default privileges for later tables."""
    r = subprocess.run(["docker", "exec", a.container, "bash", "-c",
                        "[ -f /scripts/create-app-roles.sh ] && bash /scripts/create-app-roles.sh"], capture_output=True, text=True)
    if r.returncode != 0:
        print("note: create-app-roles.sh did not run:", (r.stderr or r.stdout).strip()[:200])
    for db, _ in targets(a):
        if q(a.container, "postgres", f"SELECT 1 FROM pg_roles WHERE rolname = {lit('app_' + db)}") == "1":
            continue
        if q(a.container, "postgres", "SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app'") != "1":
            continue
        q(a.container, db, f"""GRANT CONNECT ON DATABASE "{db}" TO zoiko_app;
            GRANT USAGE ON SCHEMA public TO zoiko_app;
            GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
            GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;
            ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zoiko_app;
            ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO zoiko_app;""")


def targets(a):
    all_ = services()
    if a.db:
        sel = [s for s in all_ if s[0] == a.db]
        if not sel:
            sys.exit(f"unknown database {a.db}")
        return sel
    return all_


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["status", "baseline", "up"])
    ap.add_argument("--db")
    ap.add_argument("--container", default=os.environ.get("PG_CONTAINER", "zoiko-postgres"))
    ap.add_argument("--assume-through")
    ap.add_argument("--except", dest="except_", action="append",
                    help="baseline: a file inside the applied range that did not run (repeatable)")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--create-missing", action="store_true",
                    help="up: create a database init-db.sh lists but this volume lacks, migrate and grant it")
    a = ap.parse_args()
    if (a.assume_through or a.except_) and not a.db:
        sys.exit("--assume-through / --except need --db: they describe one database")
    sys.exit({"status": cmd_status, "baseline": cmd_baseline, "up": cmd_up}[a.command](a) or 0)
