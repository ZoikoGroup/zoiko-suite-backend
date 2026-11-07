#!/usr/bin/env python3
"""
Register evidence-requirements-svc event schemas in schema-registry-svc.

ER-W5: Register the two asyncapi.yaml schemas (evidence.requirement.satisfied, .missing)
with fixed Idempotency-Key per event + version.
"""

import json
import sys
import os
import argparse
import urllib.request
import urllib.error
from pathlib import Path

# Schema registry endpoint
SCHEMA_REGISTRY_URL = os.getenv("SCHEMA_REGISTRY_URL", "http://localhost:8093")

# Shared Determination schema (from asyncapi.yaml components/schemas/Determination)
# This is the payload for both evidence.requirement.satisfied and evidence.requirement.missing
DETERMINATION_SCHEMA = {
    "type": "object",
    "required": [
        "evaluation_id", "tenant_id", "legal_entity_id", "domain_code",
        "action_type", "outcome", "requirement_ids", "idempotency_key",
        "evaluated_for_principal_id", "correlation_id", "causation_id",
        "jurisdiction_context"
    ],
    "properties": {
        "evaluation_id": {"type": "string", "format": "uuid"},
        "tenant_id": {"type": "string", "format": "uuid"},
        "legal_entity_id": {"type": "string", "format": "uuid"},
        "domain_code": {"type": "string"},
        "action_type": {"type": "string"},
        "outcome": {"type": "string", "enum": ["SATISFIED", "MISSING"]},
        "requirement_ids": {"type": "array", "items": {"type": "string", "format": "uuid"}},
        "unmet": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "evidence_requirement_id": {"type": "string", "format": "uuid"},
                    "evidence_type": {"type": "string"},
                    "reason": {"type": "string"}
                }
            }
        },
        "idempotency_key": {"type": "string"},
        "evaluated_for_principal_id": {"type": "string"},
        "correlation_id": {"type": "string"},
        "causation_id": {"type": "string"},
        "jurisdiction_context": {"type": "string"}
    }
}

# Events to register (from asyncapi.yaml)
EVENTS = {
    "evidence.requirement.satisfied": {
        "event_name": "evidence.requirement.satisfied",
        "owning_service": "evidence-requirements-svc",
        "compatibility_mode": "BACKWARD",
        "json_schema": DETERMINATION_SCHEMA,
    },
    "evidence.requirement.missing": {
        "event_name": "evidence.requirement.missing",
        "owning_service": "evidence-requirements-svc",
        "compatibility_mode": "BACKWARD",
        "json_schema": DETERMINATION_SCHEMA,
    }
}

# Fixed Idempotency-Keys per event + version (as required by ER-W5)
IDEMPOTENCY_KEYS = {
    "evidence.requirement.satisfied": "evidence-requirements-svc:v1:satisfied",
    "evidence.requirement.missing": "evidence-requirements-svc:v1:missing",
}


def register_schema(event_name: str, schema: dict, idempotency_key: str, legal_entity_id: str = None) -> dict:
    """Register a schema version via schema-registry-svc."""
    url = f"{SCHEMA_REGISTRY_URL}/v1/schemas/{event_name}/versions"

    body = {
        "json_schema": schema,
        "compatibility_mode": "BACKWARD",
        "owning_service": "evidence-requirements-svc",
    }

    data = json.dumps(body).encode("utf-8")

    headers = {
        "Content-Type": "application/json",
        "Idempotency-Key": idempotency_key,
        "X-Principal-Id": "schema-registration-script",
        "X-Tenant-Id": "00000000-0000-0000-0000-000000000000",
    }
    if legal_entity_id:
        headers["X-Legal-Entity-Id"] = legal_entity_id

    req = urllib.request.Request(url, data=data, headers=headers, method="POST")

    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return {"status": resp.status, "body": json.loads(resp.read().decode())}
    except urllib.error.HTTPError as e:
        error_body = e.read().decode() if e.fp else "{}"
        try:
            return {"status": e.code, "body": json.loads(error_body)}
        except json.JSONDecodeError:
            return {"status": e.code, "body": {"error": error_body}}


def main():
    global SCHEMA_REGISTRY_URL
    parser = argparse.ArgumentParser(description="Register evidence-requirements-svc event schemas")
    parser.add_argument("--url", default=SCHEMA_REGISTRY_URL, help="Schema registry URL")
    parser.add_argument("--legal-entity-id", help="Legal entity ID for SCHEMA_PUBLISH check")
    parser.add_argument("--dry-run", action="store_true", help="Print what would be registered without calling the API")
    args = parser.parse_args()

    SCHEMA_REGISTRY_URL = args.url

    print(f"Registering evidence-requirements-svc schemas to {SCHEMA_REGISTRY_URL}")

    for event_name, event_config in EVENTS.items():
        idempotency_key = IDEMPOTENCY_KEYS[event_name]
        schema = event_config["json_schema"]

        if args.dry_run:
            print(f"  Would register: {event_name}")
            print(f"    Idempotency-Key: {idempotency_key}")
            print(f"    Owning service: {event_config['owning_service']}")
            print(f"    Compatibility: {event_config['compatibility_mode']}")
            continue

        result = register_schema(event_name, schema, idempotency_key, args.legal_entity_id)
        if result["status"] in (201, 200):
            print(f"  ✅ {event_name}: registered as version {result['body'].get('version', 'unknown')}")
        elif result["status"] == 409 and "originally stored" in str(result["body"]):
            # Idempotent replay - already registered
            print(f"  ⏭️  {event_name}: already registered (idempotent replay)")
        else:
            print(f"  ❌ {event_name}: {result['status']} {result['body']}")
            sys.exit(1)

    print("All schemas registered successfully")


if __name__ == "__main__":
    main()