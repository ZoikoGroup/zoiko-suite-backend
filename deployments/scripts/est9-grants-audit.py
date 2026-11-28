#!/usr/bin/env python3
"""EST-9 · Grants audit test.

Greps every action constant from the codebase (policy-svc handlers,
access-control migrations, and registered_actions_test.go) and verifies
each one is present in access-control-svc's permission_definitions table.
Exit code = number of missing actions.
"""
import argparse, json, os, re, subprocess, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]  # zoiko-suite-backend-platform
ACCESS_CONTROL = ROOT / "services" / "access-control-svc"

# Patterns that match Go action constants and SQL INSERT action names
GO_CONST_PATTERN = re.compile(r'"([A-Z0-9_]+_(?:CREATE|READ|APPROVE|ACTIVATE|SUSPEND|RETIRE|REDELIVER|MANAGE|RUN|WAIVER|SIMULATE|EVALUATE|DRAFT))"')
SQL_INSERT_PATTERN = re.compile(r"\(\s*'([A-Z0-9_]+_(?:CREATE|READ|APPROVE|ACTIVATE|SUSPEND|RETIRE|REDELIVER|MANAGE|RUN|WAIVER|SIMULATE|EVALUATE|DRAFT))'")

# Known action prefixes we care about (from the governance plane + policy-svc)
ACTION_PREFIXES = (
    "POLICY_",
    "ACCOUNTING_POLICY_",
    "SPEND_POLICY_",
    "COMMERCIAL_",
    "FINCTRL_POLICY_",
    "INVENTORY_POLICY_",
    "MATCH_POLICY_",
    "RESIDENCY_POLICY_",
    "RETENTION_POLICY_",
    "SECRET_POLICY_",
    "SECURITY_POLICY_",
)

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def grep_codebase():
    """Find all action-like constants in .go files (excluding tests)."""
    actions = set()
    for go_file in ROOT.rglob("services/**/*.go"):
        if "_test.go" in str(go_file):
            continue
        try:
            text = go_file.read_text(encoding="utf-8", errors="ignore")
        except Exception:
            continue
        for m in GO_CONST_PATTERN.finditer(text):
            action = m.group(1)
            if action.startswith(ACTION_PREFIXES):
                actions.add(action)
    return actions


def grep_migrations():
    """Find all action names in access-control migration .up.sql files."""
    actions = set()
    for sql_file in ACCESS_CONTROL.glob("deployments/migrations/*_*.up.sql"):
        try:
            text = sql_file.read_text(encoding="utf-8", errors="ignore")
        except Exception:
            continue
        for m in SQL_INSERT_PATTERN.finditer(text):
            action = m.group(1)
            if action.startswith(ACTION_PREFIXES):
                actions.add(action)
    return actions


def grep_registered_actions_test():
    """Extract expected actions from registered_actions_test.go."""
    actions = set()
    test_file = ACCESS_CONTROL / "internal" / "store" / "registered_actions_test.go"
    try:
        text = test_file.read_text(encoding="utf-8", errors="ignore")
    except Exception:
        return actions
    # Find map literals like "ACTION_NAME": "TIER"
    for m in re.finditer(r'"([A-Z0-9_]+)"\s*:\s*"(STANDARD|HIGH|CRITICAL)"', text):
        actions.add(m.group(1))
    return actions


def query_db(action):
    """Check if action exists in permission_definitions."""
    try:
        out = subprocess.run(
            ["docker", "exec", "-i", "zoiko-postgres", "psql", "-U", "postgres", "-d", "access_control", "-t", "-c",
             f"SELECT action_name FROM permission_definitions WHERE action_name = '{action}';"],
            capture_output=True, text=True, timeout=10
        )
        return action in out.stdout
    except Exception as e:
        return False


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--fix", action="store_true", help="Print missing actions for manual registration")
    args = ap.parse_args()

    print("Scanning codebase for action constants...")
    code_actions = grep_codebase()
    print(f"  Found {len(code_actions)} action constants in .go files")

    print("Scanning access-control migrations...")
    migration_actions = grep_migrations()
    print(f"  Found {len(migration_actions)} action names in migrations")

    print("Reading registered_actions_test.go...")
    test_actions = grep_registered_actions_test()
    print(f"  Found {len(test_actions)} actions in test expectations")

    # Union of all expected actions
    all_expected = code_actions | migration_actions | test_actions
    print(f"Total unique expected actions: {len(all_expected)}")

    # Check each against the database
    missing = []
    for action in sorted(all_expected):
        if action.startswith(ACTION_PREFIXES):
            if query_db(action):
                check(f"action registered: {action}", True)
            else:
                check(f"action registered: {action}", False, "MISSING from permission_definitions")
                missing.append(action)

    if args.fix and missing:
        print("\n--- Missing actions (for migration) ---")
        for a in missing:
            print(f"    ('{a}', 'LEGACY', 'STANDARD', 'Enforced by <service>: <description>'),")

    failed = len(missing)
    passed = len(all_expected) - failed
    print(f"\n{passed}/{len(all_expected)} actions registered in DB")
    sys.exit(failed)


if __name__ == "__main__":
    main()