#!/usr/bin/env python
"""Contract queries for scripts/audit.sh.

Kept in a file rather than in inline heredocs because python's print() emits
CRLF on Windows, CR is not IFS whitespace, and every token read back into a
shell variable would carry a trailing CR that makes each grep for it fail
against output plainly containing it.

Every query reads openapi.yaml / asyncapi.yaml with a line scanner rather than
a YAML parser, so the audit has no dependency beyond a stock python. The files
are ours and hand-written; a scanner is enough and cannot be broken by a
missing package on a CI image.

Usage:  python scripts/spec_query.py <query> [arg...]
"""

import os
import re
import sys

SVC = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def read(name):
    with open(os.path.join(SVC, name), encoding="utf-8") as fh:
        return fh.read()


def out(lines):
    # "\n".join + a single write, not print() per line: print's line terminator
    # is os.linesep-translated on Windows and every caller here greps the result.
    sys.stdout.write("\n".join(str(x) for x in lines))
    sys.stdout.write("\n")


def openapi_paths():
    """Every path key under the top-level `paths:` block."""
    paths, in_paths = [], False
    for line in read("openapi.yaml").splitlines():
        if line.startswith("paths:"):
            in_paths = True
            continue
        if in_paths:
            # A new top-level key ends the block.
            if line and not line[0].isspace():
                break
            m = re.match(r"^  (/[^\s:]*):\s*$", line)
            if m:
                paths.append(m.group(1))
    return paths


def openapi_operations():
    """`METHOD path` for every operation, in declaration order."""
    ops, current, in_paths = [], None, False
    for line in read("openapi.yaml").splitlines():
        if line.startswith("paths:"):
            in_paths = True
            continue
        if not in_paths:
            continue
        if line and not line[0].isspace():
            break
        m = re.match(r"^  (/[^\s:]*):\s*$", line)
        if m:
            current = m.group(1)
            continue
        m = re.match(r"^    (get|post|put|patch|delete):\s*$", line)
        if m and current:
            ops.append("%s %s" % (m.group(1).upper(), current))
    return ops


def openapi_error_codes():
    """Every backtick-quoted lower_snake token in the spec's prose.

    The audit greps these against the handler, so a documented code that no
    longer exists is caught. Deliberately permissive: it is better to over-
    collect and let the shell filter than to miss one.
    """
    text = read("openapi.yaml")
    codes = set(re.findall(r"`([a-z][a-z0-9_]{4,})`", text))
    # Metric names are backticked prose too, and a template name appears in an
    # example. Neither is an error code, and grepping the handler for them would
    # fail for a reason that has nothing to do with drift.
    codes = {c for c in codes if not c.startswith("notification_") or c == "notification_not_found"}
    # Words that appear in backticks but are field names or values, not codes.
    noise = {
        "legal_entity_id", "recipient_principal_id", "next_attempt_at", "read_at",
        "correlation_id", "unread_only", "delivery_attempts", "provider_response",
        "recipient_address", "recipient_address_source", "created_at", "sent_at",
        "failure_reason", "source_event_type", "source_reference", "status",
        "notification_id", "tenant_id", "delivered_at", "receipt", "template",
        "variables", "required_variables", "subject", "channel", "unread_count",
        "event_id", "event_type", "published_at", "created_by_principal_id",
        "last_attempt_at", "event_outbox", "aggregate_key", "notifications",
        "registration_received", "error",
    }
    return sorted(c for c in codes if c not in noise)


def asyncapi_event_types():
    """The enum under the envelope's event_type."""
    text = read("asyncapi.yaml")
    m = re.search(r"event_type:\s*\n\s*type: string\s*\n\s*enum: \[([^\]]*)\]", text)
    if not m:
        return []
    return sorted(x.strip() for x in m.group(1).split(",") if x.strip())


def migration_event_types():
    """The event types migration 000005's CHECK constraint allows."""
    text = read("deployments/migrations/000005_event_outbox.up.sql")
    m = re.search(r"CHECK \(event_type IN \(([^)]*)\)\)", text)
    if not m:
        return []
    return sorted(x.strip().strip("'") for x in m.group(1).split(",") if x.strip())


def go_event_types():
    """The event types internal/events declares."""
    text = read("internal/events/publisher.go")
    return sorted(set(re.findall(r'Type\w+\s*=\s*"(notification\.[a-z]+)"', text)))


def openapi_channels():
    """The channels a SEND request accepts.

    Scoped to the SendNotificationRequest schema rather than matched on the
    first `channel:` in the file. The unread-count response has one too, whose
    enum is [IN_APP] — matching that would have this query report that the
    service accepts exactly one channel, silently and wrongly, and the audit
    check built on it would then pass while proving nothing.
    """
    text = read("openapi.yaml")
    start = text.find("SendNotificationRequest:")
    if start < 0:
        return []
    end = text.find("\n    Notification:", start)
    block = text[start:end if end > 0 else len(text)]
    m = re.search(r"channel:\s*\n\s*type: string\s*\n\s*enum: \[([^\]]*)\]", block)
    if not m:
        return []
    return sorted(x.strip() for x in m.group(1).split(",") if x.strip())


def postman_requests():
    """`METHOD url` for every request in the collection."""
    import json
    with open(os.path.join(SVC, "postman_collection.json"), encoding="utf-8") as fh:
        doc = json.load(fh)

    found = []

    def walk(items):
        for item in items:
            if "item" in item:
                walk(item["item"])
            req = item.get("request")
            if not req:
                continue
            url = req["url"] if isinstance(req["url"], str) else req["url"].get("raw", "")
            found.append("%s %s" % (req.get("method", "GET"), url))

    walk(doc["item"])
    return found


QUERIES = {
    "openapi-paths": openapi_paths,
    "openapi-operations": openapi_operations,
    "openapi-error-codes": openapi_error_codes,
    "openapi-channels": openapi_channels,
    "asyncapi-event-types": asyncapi_event_types,
    "migration-event-types": migration_event_types,
    "go-event-types": go_event_types,
    "postman-requests": postman_requests,
}


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in QUERIES:
        sys.stderr.write("usage: spec_query.py <%s>\n" % "|".join(sorted(QUERIES)))
        return 2
    out(QUERIES[sys.argv[1]]())
    return 0


if __name__ == "__main__":
    sys.exit(main())
