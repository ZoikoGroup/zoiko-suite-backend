#!/usr/bin/env python3
"""Regenerate openapi.yaml from the routes the handlers actually register.

Routes are read from internal/handler/*.go, so the document cannot list a
route the service does not serve or miss one it does. Validate with:

    python scripts/gen_openapi.py && python -c "import yaml,openapi_spec_validator as v; v.validate(yaml.safe_load(open('openapi.yaml')))"
"""
import os
import re
import sys

import yaml

svc = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "..")
src = "".join(open(os.path.join(svc, "internal/handler", f), encoding="utf8").read() for f in ("ncd_handler.go", "handler.go"))

routes = set()
for m, p in re.findall(r'r\.(Get|Post|Delete)\("(/v1/[^"]*)"', src):
    routes.add((m.lower(), p))
for prefix, body in re.findall(r'r\.Route\("(/v1/[a-z-]+)", func\(r chi\.Router\) \{(.*?)\n\t\}\)', src, re.S):
    nested = dict(re.findall(r'r\.Route\("(/[a-z]+)", func\(r chi\.Router\) \{(.*?)\n\t\t\}\)', body, re.S))
    flat = body
    for inner, ib in nested.items():
        flat = flat.replace(ib, "")
        for m, p in re.findall(r'r\.(Get|Post|Delete)\("(/[^"]*)"', ib):
            routes.add((m.lower(), prefix + inner + ("" if p == "/" else p)))
    for m, p in re.findall(r'r\.(Get|Post|Delete)\("(/[^"]*)"', flat):
        routes.add((m.lower(), prefix + ("" if p == "/" else p)))
routes = sorted(routes)

TAGS = [
    ("/v1/communication-intents", "NCD-01 registry"), ("/v1/templates", "NCD-01 registry"),
    ("/v1/render-previews", "NCD-01 registry"), ("/v1/intents", "NCD-01 registry"),
    ("/v1/recipient-resolution", "NCD-02 recipient & channel"), ("/v1/channel-decision", "NCD-02 recipient & channel"),
    ("/v1/suppressions", "NCD-02 recipient & channel"), ("/v1/preferences", "NCD-02 recipient & channel"),
    ("/v1/revalidate", "NCD-02 recipient & channel"), ("/v1/communications", "Communications (10.1)"),
    ("/v1/evidence", "NCD-04 evidence"), ("/v1/provider-events", "NCD-04 evidence"), ("/v1/streams", "NCD-04 evidence"),
    ("/v1/reputation", "NCD-04 evidence"), ("/v1/provider-bindings", "NCD-03 routing"), ("/v1/exceptions", "NCD-03 routing"),
    ("/v1/bulk-sends", "NCD-03 routing"), ("/v1/approvals", "Maker-checker"), ("/v1/acknowledgments", "NCD-05 notices"),
    ("/v1/regulated-notices", "NCD-05 notices"), ("/v1/notifications", "Legacy register"),
    ("/v1/document-templates", "BIZ-03 document templates"),
]


def tag(p):
    for pre, t in TAGS:
        if p.startswith(pre):
            return t
    return "Other"


UNSIGNED = ("/v1/provider-events/", "/v1/notifications/webhooks/", "/v1/notifications/unsubscribe")
JSON = {"application/json": {"schema": {"type": "object"}}}
paths = {}
for m, p in routes:
    params = [{"name": n, "in": "path", "required": True, "schema": {"type": "string"}}
              for n in re.findall(r"\{([a-zA-Z_]+)\}", p)]
    op = {
        "tags": [tag(p)],
        "operationId": m + "_" + re.sub(r"[^A-Za-z0-9]+", "_", p).strip("_"),
        "parameters": params,
        "responses": {
            "200": {"description": "OK", "content": JSON},
            "400": {"$ref": "#/components/responses/Invalid"},
            "401": {"$ref": "#/components/responses/Unauthenticated"},
            "403": {"$ref": "#/components/responses/Forbidden"},
            "404": {"$ref": "#/components/responses/NotFound"},
            "409": {"$ref": "#/components/responses/Conflict"},
            "422": {"$ref": "#/components/responses/Refused"},
            "503": {"$ref": "#/components/responses/Unavailable"},
        },
    }
    if p.startswith(UNSIGNED):
        op["description"] = (
            "Exempt from the 4-envelope: the caller carries no ZoikoSuite identity. Authenticated by HMAC-SHA256 over "
            "'<timestamp>.<body>' inside a 5-minute replay window: the NCD plane's provider events with "
            "X-NCD-Signature and X-NCD-Timestamp, the legacy webhook with X-Zoiko-Signature: t=<unix>,v1=<hex> "
            "(several v1 values allowed during a secret rotation). With no secret configured every request is "
            "refused (INV-27).")
    else:
        op["parameters"] += [{"$ref": "#/components/parameters/" + h} for h in ("TenantId", "PrincipalId", "CorrelationId")]
        if m != "get":
            op["parameters"] += [{"$ref": "#/components/parameters/" + h}
                                 for h in ("RequestId", "SourceChannel", "LegalEntityId", "IdempotencyKey")]
    if m == "post":
        op["requestBody"] = {"required": False, "content": JSON}
        op["responses"]["201"] = {"description": "Created", "content": JSON}
        op["responses"]["202"] = {"description": "Accepted: queued, or filed for maker-checker approval", "content": JSON}
    paths.setdefault(p, {})[m] = op


def hdr(name, desc, req=True):
    return {"name": name, "in": "header", "required": req, "schema": {"type": "string"}, "description": desc}


def resp(desc, ref="Error"):
    return {"description": desc, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/" + ref}}}}


doc = {
    "openapi": "3.0.3",
    "info": {
        "title": "notification-svc - Notification, Communication & Delivery Control (ZS-SVC-Y-001)",
        "version": "2.0.0",
        "description": (
            "NCD-01 to NCD-05 control plane plus the legacy delivery register. No response exposes a generic 'sent' "
            "status (3.3). Delivery states: CREATED, SUBMITTED_TO_PROVIDER, PROVIDER_ACCEPTED, PROVIDER_PENDING, "
            "DELIVERY_UNKNOWN, DELIVERED_TO_MAILBOX, DELIVERED_TO_INBOX, DELIVERED_TO_NETWORK, FAILED, BOUNCED "
            "(legacy register: PENDING, RETRY_SCHEDULED, PROVIDER_ACCEPTED, DELIVERED_TO_INBOX, DELIVERY_UNKNOWN, FAILED; "
            "stored_status keeps the column). Legal sufficiency is always NOT_DETERMINED_BY_NCD. Every write needs the six "
            "envelope headers; Idempotency-Key replays the first response with X-Idempotent-Replay: true."),
    },
    "servers": [{"url": "http://localhost:8133"}],
    "tags": [{"name": t} for t in sorted({tag(p) for _, p in routes})],
    "paths": dict(sorted(paths.items())),
    "components": {
        "parameters": {
            "TenantId": hdr("X-Tenant-Id", "Gateway-verified tenant."),
            "PrincipalId": hdr("X-Principal-Id", "Gateway-verified actor. Maker-checker compares this, never a body field."),
            "CorrelationId": hdr("X-Correlation-ID", "Correlation id carried onto every event.", False),
            "RequestId": hdr("X-Request-Id", "Mandatory on writes (request tracing)."),
            "SourceChannel": hdr("X-Source-Channel", "api, import, integration, mobile, scheduled_job, system or web."),
            "LegalEntityId": hdr("X-Legal-Entity-Id", "Mandatory on writes (INV-02); tenant-wide operations authorize on it."),
            "IdempotencyKey": hdr("Idempotency-Key", "Mandatory on writes (INV-08); a repeat is answered from the first response."),
        },
        "schemas": {
            "Error": {"type": "object", "required": ["error_code"],
                      "properties": {"error_code": {"type": "string"}, "error_message": {"type": "string"}}},
            "EnvelopeError": {"type": "object", "properties": {
                "error": {"type": "string"}, "detail": {"type": "string"},
                "violations": {"type": "array", "items": {"type": "object"}}}},
            "Refusal": {"type": "object", "required": ["error_code", "reason_code"], "properties": {
                "error_code": {"type": "string"}, "error_message": {"type": "string"},
                "reason_code": {"type": "string", "enum": ["NCD-%03d" % i for i in range(1, 21)]},
                "reason": {"type": "string"}}},
        },
        "responses": {
            "Invalid": {"description": "Invalid request, or the 4-envelope is incomplete", "content": {"application/json": {
                "schema": {"oneOf": [{"$ref": "#/components/schemas/Error"}, {"$ref": "#/components/schemas/EnvelopeError"}]}}}},
            "Unauthenticated": resp("No principal or tenant, or an unauthenticated callback"),
            "Forbidden": resp("Not granted, or segregation of duties"),
            "NotFound": resp("Not found in this tenant"),
            "Conflict": resp("The object's state does not permit the command"),
            "Refused": resp("A 10.3 coded refusal (NCD-001 to NCD-020)", "Refusal"),
            "Unavailable": resp("A dependency or the store is unavailable; nothing was done"),
        },
    },
}
with open(os.path.join(svc, "openapi.yaml"), "w", encoding="utf8", newline="\n") as f:
    yaml.safe_dump(doc, f, sort_keys=False, allow_unicode=True, width=120)
print(len(routes), "operations")
