#!/usr/bin/env python3
"""
Register evidence-requirements-svc event schemas with schema-registry-svc.

This script registers the two event schemas (evidence.requirement.satisfied and
evidence.requirement.missing) with the schema-registry-svc. It uses a fixed
Idempotency-Key per event + version and requires the principal to hold the
schema-publish grant.

Usage:
    python3 register-evidence-requirements-schemas.py [--url URL] [--token TOKEN]

Environment variables:
    SCHEMA_REGISTRY_URL - Base URL of schema-registry-svc (default: http://localhost:8093)
    SCHEMA_PUBLISH_TOKEN - Bearer token for schema-publish principal
"""

import argparse
import json
import os
import sys
import uuid
from pathlib import Path

import requests


def load_schema(file_path: Path) -> dict:
    """Load a JSON schema from file."""
    with open(file_path, 'r') as f:
        return json.load(f)


def register_schema(base_url: str, token: str, event_name: str, schema: dict, version: str, idempotency_key: str) -> bool:
    """Register a single schema version with schema-registry-svc."""
    url = f"{base_url}/v1/schemas/{event_name}/versions"
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "Idempotency-Key": idempotency_key,
    }
    payload = {
        "schema": json.dumps(schema),
        "version": version,
    }

    try:
        response = requests.post(url, headers=headers, json=payload, timeout=30)
        if response.status_code == 201:
            print(f"  ✓ Registered {event_name} v{version}")
            return True
        elif response.status_code == 200:
            print(f"  ✓ Already registered {event_name} v{version}")
            return True
        else:
            print(f"  ✗ Failed to register {event_name} v{version}: {response.status_code} {response.text}")
            return False
    except Exception as e:
        print(f"  ✗ Error registering {event_name} v{version}: {e}")
        return False


def main():
    parser = argparse.ArgumentParser(description="Register evidence-requirements-svc event schemas")
    parser.add_argument("--url", default=os.getenv("SCHEMA_REGISTRY_URL", "http://localhost:8093"),
                        help="Schema registry base URL")
    parser.add_argument("--token", default=os.getenv("SCHEMA_PUBLISH_TOKEN", ""),
                        help="Bearer token for schema-publish principal")
    args = parser.parse_args()

    if not args.token:
        print("Error: SCHEMA_PUBLISH_TOKEN not set and --token not provided", file=sys.stderr)
        return 1

    schemas_dir = Path(__file__).parent / "schemas"
    if not schemas_dir.exists():
        print(f"Error: schemas directory not found at {schemas_dir}", file=sys.stderr)
        return 1

    # Fixed Idempotency-Keys per event + version (as per ER-W5)
    idempotency_keys = {
        "evidence.requirement.satisfied": "evidence-requirements-svc:schema:satisfied:v1",
        "evidence.requirement.missing": "evidence-requirements-svc:schema:missing:v1",
    }

    schemas = [
        ("evidence.requirement.satisfied", "evidence.requirement.satisfied.v1.json", "1.0"),
        ("evidence.requirement.missing", "evidence.requirement.missing.v1.json", "1.0"),
    ]

    all_ok = True
    for event_name, filename, version in schemas:
        schema_file = schemas_dir / filename
        if not schema_file.exists():
            print(f"Error: Schema file not found: {schema_file}", file=sys.stderr)
            all_ok = False
            continue

        schema = load_schema(schema_file)
        idempotency_key = idempotency_keys[event_name]

        print(f"Registering {event_name} v{version}...")
        ok = register_schema(args.url, args.token, event_name, schema, version, idempotency_key)
        if not ok:
            all_ok = False

    if all_ok:
        print("\nAll schemas registered successfully.")
        return 0
    else:
        print("\nSome schemas failed to register.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())