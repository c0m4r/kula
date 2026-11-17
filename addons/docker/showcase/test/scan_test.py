"""Checks kula-scan's reports on the showcase stack.

Run by ./showcase.sh test, in the tester service, after showcase.sh has run
kula-scan against Kula directly and through Caddy and kept each report and
exit status in /reports. Prints PASS or FAIL per scan and exits 1 when any
fails.
"""

import json
import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

REPORTS = Path("/reports")
AI = os.environ.get("SHOWCASE_AI") == "1"


@dataclass
class Scan:
    """One scan and what it must report: its exit status, and every FAIL and
    WARN, each with the reason it is expected. No finding may be an ERROR."""

    name: str
    title: str
    exit: int
    fail: dict[str, str] = field(default_factory=dict)
    warn: dict[str, str] = field(default_factory=dict)


# The probes of the AI assistant skip without it: ./showcase.sh up --ai.
AI_CHECKS = {"RATE-OLLAMA", "INPUT-CHAT", "RATE-CHAT"}

SCANS = [
    Scan(
        "direct",
        "kula-scan -aggressive -fuzz, directly under the base path",
        exit=0,
        fail={
            "BYPASS-XFF": "web.trust_proxy trusts X-Forwarded-For on the direct port, "
            "which only local clients reach; the scan ignores it (-ignore)",
        },
        warn={
            "DISC-002": "the direct port is plain HTTP; Caddy serves HTTPS",
        },
    ),
    Scan(
        "proxy",
        "kula-scan -aggressive -fuzz, through Caddy over HTTPS",
        exit=0,
        warn={
            "TLS-003": "Caddy's local CA issues certificates for 12 hours and "
            "renews them; TLS-003 wants 14 days left",
            "TLS-004": "Caddy's certificate comes from its own local CA, which no "
            "system trusts: with -insecure the scanner verifies it itself",
        },
    ),
]


def describe(f: dict[str, Any]) -> str:
    text = f"{f['status']} {f['id']} [{f['severity']}] {f.get('detail', '')}"
    if f.get("evidence"):
        text += f" (evidence: {f['evidence']})"
    return text


def judge(scan: Scan) -> list[str]:
    """Lists how the scan's report differs from what it must report."""
    exit_status = int((REPORTS / f"{scan.name}.exit").read_text())
    text = (REPORTS / f"{scan.name}.json").read_text()
    try:
        report = json.loads(text)
    except ValueError:
        return [f"exit {exit_status} and no JSON report: {text[:200]!r}"]

    problems = []
    if exit_status != scan.exit or report["exit_code"] != exit_status:
        problems.append(
            f"exit {exit_status} (report says {report['exit_code']}), want {scan.exit}"
        )
    found: dict[str, dict[str, Any]] = {f["id"]: f for f in report["findings"]}
    for status, want in (("FAIL", scan.fail), ("WARN", scan.warn)):
        for fid, f in found.items():
            if f["status"] == status and fid not in want:
                problems.append(f"unexpected {describe(f)}")
        for fid, why in want.items():
            got = found.get(fid)
            if got is None or got["status"] != status:
                state = got["status"] if got else "not reported"
                problems.append(f"{fid} is {state}, want {status}: {why}")
    for f in found.values():
        if f["status"] == "ERROR":
            problems.append(f"the probe could not complete: {describe(f)}")
        skipped_ai = f["status"] == "SKIP" and f["id"] in AI_CHECKS
        if AI and skipped_ai:
            problems.append(f"skipped with the AI assistant on: {describe(f)}")
    return problems


def main() -> int:
    failures = 0
    for scan in SCANS:
        try:
            problems = judge(scan)
        except (OSError, ValueError, KeyError) as err:
            problems = [f"no report: {err}"]
        if problems:
            failures += 1
            print(f"FAIL {scan.title}", flush=True)
            for p in problems:
                print(f"     {p}", flush=True)
            continue
        expected = sorted(scan.fail) + sorted(scan.warn)
        detail = f"expected: {', '.join(expected)}" if expected else ""
        print(f"PASS {scan.title}" + (f" — {detail}" if detail else ""), flush=True)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
