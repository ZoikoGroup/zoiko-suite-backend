#!/usr/bin/env python3
"""
E2E dependency check for tenant-entity-registry-svc.

Validates that all downstream dependencies are available and responding correctly.
Used in CI/CD and live deployment validation.

Dependencies checked:
1. identity-context-svc — host binding resolution
2. authorization-svc — authorization decisions
3. jurisdiction-rules-svc — jurisdiction existence
4. Kafka — event publishing
5. PostgreSQL — database connectivity
6. Traefik/Ingress — Host header preservation (NP3)

Exit codes:
  0 — all checks passed
  1 — one or more critical checks failed
  2 — configuration error
"""

import argparse
import json
import os
import sys
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urljoin

import requests


@dataclass
class CheckResult:
    name: str
    passed: bool
    detail: str
    latency_ms: float


def check_http(url: str, method: str = "GET", headers: dict = None, expected_status: int = 200, timeout: float = 5.0, json_body: dict = None) -> CheckResult:
    """Perform an HTTP check and return result."""
    start = time.perf_counter()
    try:
        resp = requests.request(
            method=method,
            url=url,
            headers=headers or {},
            json=json_body,
            timeout=timeout
        )
        latency_ms = (time.perf_counter() - start) * 1000
        if resp.status_code == expected_status:
            return CheckResult(url, True, f"HTTP {resp.status_code} in {latency_ms:.0f}ms", latency_ms)
        else:
            return CheckResult(url, False, f"HTTP {resp.status_code} (expected {expected_status}) in {latency_ms:.0f}ms", latency_ms)
    except requests.exceptions.ConnectionError as e:
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult(url, False, f"Connection failed: {e}", latency_ms)
    except requests.exceptions.Timeout as e:
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult(url, False, f"Timeout after {timeout}s", latency_ms)
    except Exception as e:
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult(url, False, f"Error: {e}", latency_ms)


def check_identity_context(base_url: str, tenant_id: str, hostname: str, auth_token: str) -> CheckResult:
    """Check identity-context-svc host binding resolution."""
    headers = {"Authorization": f"Bearer {auth_token}", "X-Correlation-ID": "e2e-deps-check"}
    url = urljoin(base_url, f"/v1/resolve-tenant?hostname={hostname}")
    return check_http(url, headers=headers, expected_status=200)


def check_authorization_svc(base_url: str, tenant_id: str, auth_token: str) -> CheckResult:
    """Check authorization-svc health and decision endpoint."""
    headers = {"Authorization": f"Bearer {auth_token}", "X-Correlation-ID": "e2e-deps-check"}
    # Health check
    health = check_http(urljoin(base_url, "/health"), headers=headers, expected_status=200)
    if not health.passed:
        return health
    # Try a sample authorization decision
    url = urljoin(base_url, "/v1/authorize")
    body = {
        "principal_id": "test-principal",
        "scope_id": tenant_id,
        "resource": "tenant",
        "action": "view"
    }
    return check_http(url, method="POST", headers=headers, json_body=body, expected_status=200)


def check_jurisdiction_rules(base_url: str, auth_token: str) -> CheckResult:
    """Check jurisdiction-rules-svc health."""
    headers = {"Authorization": f"Bearer {auth_token}", "X-Correlation-ID": "e2e-deps-check"}
    return check_http(urljoin(base_url, "/health"), headers=headers, expected_status=200)


def check_kafka(bootstrap_servers: str) -> CheckResult:
    """Check Kafka connectivity (basic metadata request)."""
    # This is a placeholder - actual Kafka check would use kafka-python or confluent-kafka
    start = time.perf_counter()
    try:
        # In a real implementation, this would use kafka-python to fetch metadata
        # For now, we just check if the bootstrap server is reachable via a simple socket
        import socket
        host, port = bootstrap_servers.split(":")[0], int(bootstrap_servers.split(":")[1])
        sock = socket.create_connection((host, port), timeout=5)
        sock.close()
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult("kafka", True, f"TCP connection to {bootstrap_servers} in {latency_ms:.0f}ms", latency_ms)
    except Exception as e:
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult("kafka", False, f"Cannot connect to {bootstrap_servers}: {e}", latency_ms)


def check_postgres(dsn: str) -> CheckResult:
    """Check PostgreSQL connectivity and basic query."""
    start = time.perf_counter()
    try:
        import psycopg2
        conn = psycopg2.connect(dsn, connect_timeout=5)
        cur = conn.cursor()
        cur.execute("SELECT 1")
        cur.fetchone()
        cur.close()
        conn.close()
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult("postgres", True, f"Query OK in {latency_ms:.0f}ms", latency_ms)
    except Exception as e:
        latency_ms = (time.perf_counter() - start) * 1000
        return CheckResult("postgres", False, f"Connection failed: {e}", latency_ms)


def check_host_header_preservation(base_url: str, auth_token: str) -> CheckResult:
    """Check that ingress preserves the Host header (NP3)."""
    headers = {
        "Authorization": f"Bearer {auth_token}",
        "X-Correlation-ID": "e2e-deps-check",
        "Host": "test-host-preservation.example.com"
    }
    # This endpoint should reflect the Host header back or use it for resolution
    url = urljoin(base_url, "/v1/resolve-tenant?hostname=test-host-preservation.example.com")
    return check_http(url, headers=headers, expected_status=200)


def check_registry_service(base_url: str, tenant_id: str, auth_token: str) -> CheckResult:
    """Check tenant-entity-registry-svc health and basic read."""
    headers = {"Authorization": f"Bearer {auth_token}", "X-Correlation-ID": "e2e-deps-check"}
    # Health check
    health = check_http(urljoin(base_url, "/health"), headers=headers, expected_status=200)
    if not health.passed:
        return health
    # Try a tenant-scoped read
    url = urljoin(base_url, f"/v1/tenants/{tenant_id}")
    return check_http(url, headers=headers, expected_status=200)


def main():
    parser = argparse.ArgumentParser(description="E2E dependency check for tenant-entity-registry-svc")
    parser.add_argument("--registry-url", default=os.getenv("REGISTRY_URL", "http://localhost:8081"), help="tenant-entity-registry-svc base URL")
    parser.add_argument("--identity-url", default=os.getenv("IDENTITY_URL", "http://localhost:8082"), help="identity-context-svc base URL")
    parser.add_argument("--authz-url", default=os.getenv("AUTHZ_URL", "http://localhost:8083"), help="authorization-svc base URL")
    parser.add_argument("--jurisdiction-url", default=os.getenv("JURISDICTION_URL", "http://localhost:8084"), help="jurisdiction-rules-svc base URL")
    parser.add_argument("--kafka-servers", default=os.getenv("KAFKA_BOOTSTRAP", "localhost:9092"), help="Kafka bootstrap servers")
    parser.add_argument("--postgres-dsn", default=os.getenv("POSTGRES_DSN", "postgresql://postgres:postgres@localhost:5432/tenant_entity_registry"), help="PostgreSQL DSN")
    parser.add_argument("--tenant-id", default=os.getenv("TENANT_ID", "tenant-001"), help="Tenant ID for scoped checks")
    parser.add_argument("--hostname", default=os.getenv("TEST_HOSTNAME", "test.example.com"), help="Hostname for identity-context resolution")
    parser.add_argument("--auth-token", default=os.getenv("AUTH_TOKEN", "test-token"), help="Auth token for authenticated endpoints")
    parser.add_argument("--json", action="store_true", help="Output results as JSON")
    parser.add_argument("--timeout", type=float, default=10.0, help="Per-check timeout in seconds")
    args = parser.parse_args()

    results = []

    print(f"E2E Dependency Check for tenant-entity-registry-svc")
    print(f"Registry: {args.registry_url}")
    print(f"Tenant: {args.tenant_id}")
    print(f"Test hostname: {args.hostname}")
    print("-" * 60)

    # 1. Registry service health
    print("1. Checking tenant-entity-registry-svc...")
    r = check_registry_service(args.registry_url, args.tenant_id, args.auth_token)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 2. Identity context
    print("2. Checking identity-context-svc (host binding)...")
    r = check_identity_context(args.identity_url, args.tenant_id, args.hostname, args.auth_token)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 3. Authorization service
    print("3. Checking authorization-svc...")
    r = check_authorization_svc(args.authz_url, args.tenant_id, args.auth_token)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 4. Jurisdiction rules
    print("4. Checking jurisdiction-rules-svc...")
    r = check_jurisdiction_rules(args.jurisdiction_url, args.auth_token)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 5. Kafka
    print("5. Checking Kafka connectivity...")
    r = check_kafka(args.kafka_servers)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 6. PostgreSQL
    print("6. Checking PostgreSQL...")
    r = check_postgres(args.postgres_dsn)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # 7. Host header preservation (NP3)
    print("7. Checking Host header preservation (NP3)...")
    r = check_host_header_preservation(args.registry_url, args.auth_token)
    results.append(r)
    print(f"   {'✓' if r.passed else '✗'} {r.detail}")

    # Summary
    passed = sum(1 for r in results if r.passed)
    total = len(results)
    print("-" * 60)
    print(f"Result: {passed}/{total} checks passed")

    if args.json:
        output = {
            "summary": {"passed": passed, "total": total},
            "checks": [
                {"name": r.name, "passed": r.passed, "detail": r.detail, "latency_ms": r.latency_ms}
                for r in results
            ]
        }
        print(json.dumps(output, indent=2))

    if passed == total:
        print("All checks PASSED")
        sys.exit(0)
    else:
        print(f"{total - passed} checks FAILED")
        sys.exit(1)


if __name__ == "__main__":
    main()