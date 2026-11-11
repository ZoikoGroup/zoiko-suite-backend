#!/usr/bin/env python3
"""
Register governance-decision-log-svc's event schemas in schema-registry-svc.

Rule R9 (group decision C3): the decision log publishes every event under its
Doc 03 wire name AND its Event Catalogue canonical name during the deprecation
window, and both names are registered. Four registrations, two schemas:

  governance.decision.recorded                  schemas/governance.decision.recorded.v1.json
  com.zoikosuite.governance.decision.recorded   (same schema)
  governance.decision.replayed                  schemas/governance.decision.replayed.v1.json
  com.zoikosuite.governance.decision.replayed   (same schema)

The schema files live in the service (services/governance-decision-log-svc/
schemas) and the service's own test (TestR9_EmittedEnvelopesMatchTheRegisteredSchemas)
holds them to what the producer emits.

Each registration uses a fixed Idempotency-Key, so a re-run returns the
version first stored instead of claiming a new one. Afterwards the latest
version of every name is read back and compared with the file: a registry
holding a different contract under one of these names fails the run.

The caller needs SCHEMA_PUBLISH on the platform scope (schema-registry-svc
checks it in authorization-svc). Usage:

  python register-governance-decision-log-schemas.py [--url http://127.0.0.1:8093]
         [--principal <id>] [--tenant <uuid>] [--dry-run]
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
import uuid
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")

HERE = Path(__file__).resolve().parent
SCHEMAS = HERE.parent.parent / "services" / "governance-decision-log-svc" / "schemas"
OWNER = "governance-decision-log-svc"
PLATFORM = "00000000-0000-0000-0000-00000000f001"

EVENTS = [
    ("governance.decision.recorded", "governance.decision.recorded.v1.json"),
    ("com.zoikosuite.governance.decision.recorded", "governance.decision.recorded.v1.json"),
    ("governance.decision.replayed", "governance.decision.replayed.v1.json"),
    ("com.zoikosuite.governance.decision.replayed", "governance.decision.replayed.v1.json"),
]


def call(method, url, headers, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {}
        except ValueError:
            return e.code, {"error": raw[:200].decode("utf8", "replace")}
    except (urllib.error.URLError, OSError) as e:
        return 0, {"error": f"unreachable: {e}"}


def envelope(principal, tenant, write):
    rid = str(uuid.uuid4())
    h = {
        "X-Tenant-Id": tenant,
        "X-Principal-Id": principal,
        "X-Legal-Entity-Id": PLATFORM,
        "X-Request-Id": rid,
        "X-Correlation-ID": "schema-registration-" + rid[:8],
        "X-Source-Channel": "system",
    }
    if not write:
        h["X-Purpose-Context"] = "event_contract_verification"
    return h


def main():
    ap = argparse.ArgumentParser(description="Register governance-decision-log-svc event schemas (R9: wire + canonical names)")
    ap.add_argument("--url", default=os.getenv("SCHEMA_REGISTRY_URL", "http://127.0.0.1:8093"))
    ap.add_argument("--principal", default=os.getenv("SCHEMA_PUBLISHER_PRINCIPAL_ID", "33333333-3333-3333-3333-333333333333"),
                    help="a principal holding SCHEMA_PUBLISH (default: the local demo admin)")
    ap.add_argument("--tenant", default=os.getenv("SCHEMA_PUBLISHER_TENANT_ID", "11111111-1111-1111-1111-111111111111"))
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    failures = 0
    print(f"Registering {OWNER} event schemas at {a.url}")
    for name, file in EVENTS:
        schema = json.loads((SCHEMAS / file).read_text(encoding="utf-8"))
        key = f"{OWNER}:v1:{name}"
        if a.dry_run:
            print(f"  would register {name} from {file} (Idempotency-Key {key})")
            continue
        h = envelope(a.principal, a.tenant, write=True)
        h["Idempotency-Key"] = key
        s, b = call("POST", f"{a.url}/v1/schemas/{name}/versions", h,
                    {"json_schema": schema, "compatibility_mode": "BACKWARD", "owning_service": OWNER})
        if s in (200, 201):
            print(f"  registered {name}: version {b.get('version')}")
        else:
            failures += 1
            print(f"  FAILED {name}: HTTP {s} {json.dumps(b)[:300]}")

    if a.dry_run:
        return 0

    print("Verifying the registry holds these contracts")
    for name, file in EVENTS:
        want = json.loads((SCHEMAS / file).read_text(encoding="utf-8"))
        s, b = call("GET", f"{a.url}/v1/schemas/{name}/versions/latest", envelope(a.principal, a.tenant, write=False))
        got = b.get("json_schema")
        if isinstance(got, str):
            got = json.loads(got)
        if s == 200 and got == want and b.get("owning_service") == OWNER:
            print(f"  ok {name}: version {b.get('version')}, owner {OWNER}")
        else:
            failures += 1
            print(f"  FAILED {name}: HTTP {s}, owner {b.get('owning_service')}, schema {'matches' if got == want else 'DIFFERS'}")

    print("done" if failures == 0 else f"{failures} failure(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
