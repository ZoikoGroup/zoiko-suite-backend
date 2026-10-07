#!/usr/bin/env bash
#
# OpenAPI compatibility gate (ORG §9.2 gate 1). Diffs openapi.yaml against a
# base revision and lists every breaking change. A breaking change is not
# forbidden — it must be deliberate: list it in SPEC_DEVIATIONS.md or the
# release notes before the gate is overridden.
#
# Usage: scripts/contract_gate.sh [base-git-ref]   (default: HEAD)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_REF="${1:-HEAD}"
base="$(mktemp)"
trap 'rm -f "$base"' EXIT
git -C "$HERE" show "$BASE_REF:services/tenant-entity-registry-svc/openapi.yaml" > "$base" || exit 2
cd "$HERE" && go run github.com/oasdiff/oasdiff@v1.32.1 breaking "$base" openapi.yaml --fail-on ERR
