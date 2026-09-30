"""Contract queries for audit.sh.

Split out of the audit script rather than inlined as heredocs. Two reasons, and
the second one cost an audit run:

  - The queries are assertions about the contract (which routes the spec
    declares, which error codes it enumerates, which series the alerts read),
    and they are easier to read and change here than escaped inside shell.

  - Every `print()` from a heredoc on Windows emits CRLF. CR is not IFS
    whitespace, so each token the shell read back carried a trailing '\\r' and
    every `grep` for it failed against output that plainly contained it. That
    produced five false FAILs on checks whose subject was demonstrably present
    — the worst kind of audit result, because it points at working code. This
    module writes LF only, whatever the platform.

Usage:  python spec_query.py <command> [path]
"""

import io
import re
import sys

import yaml


def _emit(lines):
    # LF only. sys.stdout on Windows would translate '\n' to '\r\n'.
    data = "".join(str(x) + "\n" for x in lines).encode("utf-8")
    sys.stdout.buffer.write(data)


def _load(path):
    with io.open(path, encoding="utf-8") as fh:
        return yaml.safe_load(fh)


def openapi_paths(path):
    _emit(sorted(_load(path).get("paths", {})))


def openapi_error_codes(path):
    d = _load(path)
    _emit(d["components"]["schemas"]["Error"]["properties"]["error_code"]["enum"])


def _our_group(path):
    for g in _load(path)["groups"]:
        if g["name"] == "delegated-authority":
            return g
    return {"rules": []}


def alert_names(path):
    _emit(r["alert"] for r in _our_group(path)["rules"])


def alert_series(path):
    """Every metric series this service's alerts read.

    An alert on a misspelled series is silent forever and looks exactly like
    one that never fires because nothing is wrong.
    """
    out = set()
    for r in _our_group(path)["rules"]:
        out.update(
            re.findall(r"\b(delegated_authority_[a-z_]+|readiness_up)\b", str(r["expr"]))
        )
    _emit(sorted(out))


def runbook_refs(path):
    """Every "RUNBOOK section N.N" pointer in THIS service's alerts.

    Scoped to our group deliberately: the rules file holds every service's
    alerts, and gateway-auth's "RUNBOOK section 4.2" names gateway-auth's
    runbook, not ours. Checking the whole file reported five of its pointers as
    dangling against a runbook they were never about.
    """
    out = set()
    for r in _our_group(path)["rules"]:
        out.update(
            re.findall(r"RUNBOOK section ([0-9]+\.[0-9]+)", str(r.get("annotations", {})))
        )
    _emit(sorted(out))


def parses(path):
    _load(path)
    _emit(["ok"])


COMMANDS = {
    "openapi-paths": openapi_paths,
    "openapi-error-codes": openapi_error_codes,
    "alert-names": alert_names,
    "alert-series": alert_series,
    "runbook-refs": runbook_refs,
    "parses": parses,
}

if __name__ == "__main__":
    if len(sys.argv) < 3 or sys.argv[1] not in COMMANDS:
        sys.stderr.write("usage: spec_query.py <%s> <path>\n" % "|".join(COMMANDS))
        raise SystemExit(2)
    COMMANDS[sys.argv[1]](sys.argv[2])
