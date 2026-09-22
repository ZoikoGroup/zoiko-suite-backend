"""Contract queries for audit.sh.

Split out of the audit script rather than inlined as heredocs, for two reasons,
and the second one has cost an audit run elsewhere in this estate:

  - The queries are assertions about the contract (which routes the spec
    declares, which event names it documents, which series the alerts read),
    and they are easier to read and change here than escaped inside shell.

  - Every `print()` from a heredoc on Windows emits CRLF. CR is not IFS
    whitespace, so each token the shell reads back carries a trailing '\\r' and
    every `grep` for it fails against output that plainly contains it. That
    produces false FAILs on checks whose subject is demonstrably present — the
    worst kind of audit result, because it points at working code and costs
    somebody a morning. This module writes LF only, whatever the platform.

Usage:  python spec_query.py <command> [path]
"""

import io
import sys

import yaml

GROUP = "access-control"


def _emit(lines):
    # LF only. sys.stdout on Windows would translate '\n' to '\r\n'.
    data = "".join(str(x) + "\n" for x in lines).encode("utf-8")
    sys.stdout.buffer.write(data)


def _load(path):
    with io.open(path, encoding="utf-8") as fh:
        return yaml.safe_load(fh)


def openapi_paths(path):
    _emit(sorted(_load(path).get("paths", {})))


def openapi_methods(path):
    """Every declared operation, as 'METHOD path'.

    The route surface is not just which paths exist: /v1/role-definitions/{id}
    serving GET but not PATCH would be a different service with the same path
    list.
    """
    out = []
    for p, item in _load(path).get("paths", {}).items():
        for method in ("get", "post", "patch", "delete", "put"):
            if method in item:
                out.append("%s %s" % (method.upper(), p))
    _emit(sorted(out))


def openapi_error_codes(path):
    """Every error_code the ServiceErrorCode enum declares.

    The enum, not the examples. Examples are illustrative and necessarily
    partial; the point of this query is to diff the contract against the
    handler exhaustively, and a partial list would silently pass a code the
    contract never mentions.

    The envelope middleware's separate error/detail dialect is documented as
    EnvelopeError and is deliberately out of scope here — it is shared across
    every service and is not this one's to enumerate.
    """
    _emit(_load(path)["components"]["schemas"]["ServiceErrorCode"]["enum"])


def asyncapi_events(path):
    """Every event name the document declares, from the message payload consts."""
    names = set()
    for name, msg in _load(path)["components"]["messages"].items():
        if "name" in msg:
            names.add(msg["name"])
    _emit(sorted(names))


def _our_group(path):
    for g in _load(path)["groups"]:
        if g["name"] == GROUP:
            return g
    return {"rules": []}


def alert_names(path):
    _emit(r["alert"] for r in _our_group(path)["rules"])


def alert_series(path):
    """Every metric series this service's alerts read.

    Emitted so the audit can assert each one actually appears on /metrics. An
    alert whose expression names a series that does not exist never fires, and
    looks identical to an alert that is simply not firing.
    """
    import re

    series = set()
    for rule in _our_group(path)["rules"]:
        for match in re.findall(r"\b(access_control_[a-z_]+|readiness_up)\b", rule["expr"]):
            series.add(match)
    _emit(sorted(series))


def alert_runbook_sections(path):
    """The RUNBOOK section each alert points at.

    An alert whose description cites a section that does not exist sends
    whoever is paged to a page that is not there.
    """
    import re

    out = []
    for rule in _our_group(path)["rules"]:
        for match in re.findall(r"RUNBOOK section ([0-9.]+)", rule["annotations"].get("description", "")):
            out.append(match.rstrip("."))
    _emit(sorted(set(out)))


def alert_count(path):
    _emit([len(_our_group(path)["rules"])])


if __name__ == "__main__":
    command = sys.argv[1].replace("-", "_")
    target = sys.argv[2] if len(sys.argv) > 2 else None
    globals()[command](target)
