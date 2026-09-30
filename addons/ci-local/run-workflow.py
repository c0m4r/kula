#!/usr/bin/env python3
"""
Minimal GitHub Actions runner for the addons/ci-local.sh container.

It runs a workflow *job* from .github/workflows/ the way a GitHub-hosted
ubuntu-latest runner would: the environment comes from the ci-local image
(addons/ci-local/Dockerfile), and the only actions it knows are the
first-party ones the Kula workflows use --

    actions/checkout       already done by entrypoint.sh (the snapshot is
                           unpacked into $GITHUB_WORKSPACE and committed)
    actions/setup-go       pick the toolcache Go, Node.js and Python
    actions/setup-node     versions the workflows ask for and put them on
    actions/setup-python   PATH, like the hosted runner does

Everything else fails loudly rather than being skipped silently: a CI
equivalent that quietly drops a security step would be worse than no CI
equivalent at all. Supported step keys are `name`, `uses`, `with`, `run`,
`id`, `env`, `working-directory`, `shell`, `timeout-minutes`,
`continue-on-error` and `if`; `if` understands the common expression
subset documented by GitHub (see Expression).

GitHub's command files are honoured: a step that appends to $GITHUB_PATH,
$GITHUB_ENV or $GITHUB_OUTPUT affects the steps after it. As on GitHub, a
failed step does not end the job: later steps still evaluate their `if`, whose
implicit `success()` skips them unless they ask for `always()` or `failure()`.
A job-level `if` is evaluated too.

Usage:
    run-workflow.py list [WORKFLOW...]
    run-workflow.py run WORKFLOW [JOB]
    run-workflow.py run-all
"""

import argparse
import dataclasses
import json
import math
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from collections import deque
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence, Tuple

WORKFLOW_DIR = ".github/workflows"

# GitHub's shell mapping for Linux runners. The default bash invocation
# enables -e and pipefail, so a failing command anywhere in a step fails the
# step; `sh` only gets -e.
SHELLS: Dict[str, List[str]] = {
    "bash": ["bash", "--noprofile", "--norc", "-eo", "pipefail"],
    "sh": ["sh", "-e"],
    "python": ["python3"],
    "python3": ["python3"],
}

COLOR = sys.stdout.isatty() and os.environ.get("NO_COLOR") is None


def paint(code: str, text: str) -> str:
    return f"\033[{code}m{text}\033[0m" if COLOR else text


def bold(text: str) -> str:
    return paint("1", text)


def green(text: str) -> str:
    return paint("32", text)


def red(text: str) -> str:
    return paint("31", text)


def cyan(text: str) -> str:
    return paint("36", text)


def yellow(text: str) -> str:
    return paint("33", text)


def warn(message: str) -> None:
    print(f"{yellow('ci-local:')} {message}", file=sys.stderr, flush=True)


class Unsupported(Exception):
    """A workflow construct this runner deliberately does not emulate."""


# ---------------------------------------------------------------------------
# Expression evaluation
#
# A small recursive-descent parser over the expression subset GitHub
# documents: context lookups, literals, !, &&, ||, ==, !=, parentheses and
# the status/string functions. Anything else raises Unsupported, which the
# caller turns into a loud failure with the offending expression, instead of
# guessing a value.
# ---------------------------------------------------------------------------

TOKEN_RE = re.compile(
    r"""
    (?P<space>\s+)
  | (?P<number>-?\d+(?:\.\d+)?)
  | (?P<string>'(?:[^']|'')*'|"(?:[^"\\]|\\.)*")
  | (?P<operator>==|!=|&&|\|\||!)
  | (?P<punct>[()\[\],.])
  | (?P<star>\*)
  | (?P<literal>(?:true|false|null)(?![A-Za-z0-9_-]))
  | (?P<ident>[A-Za-z_][A-Za-z0-9_-]*)
    """,
    re.VERBOSE,
)

LITERALS: Dict[str, Any] = {"true": True, "false": False, "null": None}

# A condition that calls none of these is implicitly `success() && (...)`.
STATUS_FUNCTIONS = {"always", "cancelled", "failure", "success"}

KNOWN_FUNCTIONS = {
    "always",
    "cancelled",
    "contains",
    "endsWith",
    "failure",
    "format",
    "fromJSON",
    "hashFiles",
    "join",
    "startsWith",
    "success",
    "toJSON",
}


@dataclasses.dataclass
class Token:
    kind: str
    value: str


def tokenize(expr: str) -> List[Token]:
    tokens: List[Token] = []
    pos = 0
    while pos < len(expr):
        match = TOKEN_RE.match(expr, pos)
        if not match:
            raise Unsupported(f"cannot parse expression near {expr[pos:pos + 20]!r}")
        pos = match.end()
        kind = match.lastgroup or ""
        if kind == "space":
            continue
        tokens.append(Token(kind, match.group()))
    return tokens


class Expression:
    """Parses and evaluates one ${{ ... }} expression body."""

    def __init__(self, source: str, context: "EvalContext") -> None:
        self.source = source
        self.context = context
        self.tokens = tokenize(source)
        self.index = 0

    # -- token helpers ----------------------------------------------------
    def peek(self) -> Optional[Token]:
        return self.tokens[self.index] if self.index < len(self.tokens) else None

    def take(self) -> Token:
        token = self.peek()
        if token is None:
            raise Unsupported(f"unexpected end of expression {self.source!r}")
        self.index += 1
        return token

    def accept(self, value: str) -> bool:
        token = self.peek()
        if (
            token is not None
            and token.value == value
            and token.kind in ("operator", "punct")
        ):
            self.index += 1
            return True
        return False

    def expect(self, value: str) -> None:
        if not self.accept(value):
            raise Unsupported(f"expected {value!r} in expression {self.source!r}")

    # -- grammar ----------------------------------------------------------
    def parse(self) -> Any:
        value = self.parse_or()
        if self.peek() is not None:
            raise Unsupported(f"trailing tokens in expression {self.source!r}")
        return value

    def parse_or(self) -> Any:
        value = self.parse_and()
        while self.accept("||"):
            right = self.parse_and()
            value = value if truthy(value) else right
        return value

    def parse_and(self) -> Any:
        value = self.parse_not()
        while self.accept("&&"):
            right = self.parse_not()
            value = right if truthy(value) else value
        return value

    def parse_not(self) -> Any:
        if self.accept("!"):
            return not truthy(self.parse_not())
        return self.parse_comparison()

    def parse_comparison(self) -> Any:
        left = self.parse_unary()
        token = self.peek()
        if token is not None and token.value in ("==", "!="):
            self.take()
            right = self.parse_unary()
            equal = equals(left, right)
            return equal if token.value == "==" else not equal
        return left

    def parse_unary(self) -> Any:
        token = self.take()
        if token.kind == "number":
            return float(token.value) if "." in token.value else int(token.value)
        if token.kind == "string":
            return unquote(token.value)
        if token.kind == "literal":
            return LITERALS[token.value]
        if token.kind == "operator" and token.value == "!":
            return not truthy(self.parse_unary())
        if token.kind == "punct" and token.value == "(":
            value = self.parse_or()
            self.expect(")")
            return value
        if token.kind == "star":
            raise Unsupported("object filters (*) are not supported")
        if token.kind == "ident":
            following = self.peek()
            if (
                following is not None
                and following.kind == "punct"
                and following.value == "("
            ):
                return self.parse_call(token.value)
            return self.parse_path(token.value)
        raise Unsupported(
            f"unexpected token {token.value!r} in expression {self.source!r}"
        )

    def parse_path(self, head: str) -> Any:
        parts: List[Any] = head.split(".")
        while True:
            if self.accept("."):
                token = self.take()
                if token.kind not in ("ident", "literal"):
                    raise Unsupported(
                        f"bad property access in expression {self.source!r}"
                    )
                parts.extend(token.value.split("."))
            elif self.accept("["):
                if self.accept("*"):
                    raise Unsupported("object filters (*) are not supported")
                token = self.take()
                if token.kind == "number":
                    parts.append(int(float(token.value)))
                elif token.kind == "string":
                    parts.append(unquote(token.value))
                elif token.kind in ("ident", "literal"):
                    parts.append(token.value)
                else:
                    raise Unsupported(f"bad index in expression {self.source!r}")
                self.expect("]")
            else:
                break
        return self.context.lookup(parts, self.source)

    def parse_call(self, name: str) -> Any:
        if name not in KNOWN_FUNCTIONS:
            raise Unsupported(
                f"unknown function {name}() in expression {self.source!r}"
            )
        if name == "hashFiles":
            raise Unsupported("hashFiles() is not supported by ci-local")
        self.expect("(")
        args: List[Any] = []
        if not self.accept(")"):
            args.append(self.parse_or())
            while self.accept(","):
                args.append(self.parse_or())
            self.expect(")")
        try:
            return call_function(name, args, self.context)
        except Unsupported:
            raise
        except (TypeError, ValueError, IndexError) as exc:
            raise Unsupported(f"{name}() failed: {exc}") from exc


def unquote(raw: str) -> str:
    if raw.startswith("'"):
        return raw[1:-1].replace("''", "'")
    return json.loads(raw)


def truthy(value: Any) -> bool:
    if value is None:
        return False
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        return value != ""
    return True


JSON_NUMBER_RE = re.compile(r"-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?")


def value_kind(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, (int, float)):
        return "number"
    if isinstance(value, str):
        return "string"
    return "array" if isinstance(value, list) else "object"


def to_number(value: Any) -> float:
    """GitHub's coercion for comparing mismatched types."""
    kind = value_kind(value)
    if kind == "null":
        return 0.0
    if kind in ("boolean", "number"):
        return float(value)
    if kind == "string":
        text = value.strip()
        if not text:
            return 0.0
        return float(text) if JSON_NUMBER_RE.fullmatch(text) else math.nan
    return math.nan


def equals(left: Any, right: Any) -> bool:
    """GitHub's loose equality: mismatched types compare as numbers (NaN never
    equals anything), strings ignore case, arrays and objects compare by
    identity."""
    kind = value_kind(left)
    if kind != value_kind(right):
        return to_number(left) == to_number(right)
    if kind == "string":
        return str(left).lower() == str(right).lower()
    if kind in ("array", "object"):
        return left is right
    return bool(left == right)


def to_string(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    if isinstance(value, (dict, list)):
        return json.dumps(value, separators=(",", ":"))
    return str(value)


def call_function(name: str, args: Sequence[Any], context: "EvalContext") -> Any:
    if name == "always":
        return True
    if name == "cancelled":
        return False
    if name == "success":
        return context.job_status == "success"
    if name == "failure":
        return context.job_status == "failure"
    if name == "contains":
        haystack, needle = args[0], args[1]
        if isinstance(haystack, list):
            return any(equals(item, needle) for item in haystack)
        return to_string(needle).lower() in to_string(haystack).lower()
    if name == "startsWith":
        return to_string(args[0]).lower().startswith(to_string(args[1]).lower())
    if name == "endsWith":
        return to_string(args[0]).lower().endswith(to_string(args[1]).lower())
    if name == "format":
        text = to_string(args[0])
        for index, value in enumerate(args[1:]):
            text = text.replace("{" + str(index) + "}", to_string(value))
        return text
    if name == "join":
        separator = to_string(args[1]) if len(args) > 1 else ","
        return separator.join(to_string(item) for item in args[0])
    if name == "toJSON":
        return json.dumps(args[0])
    if name == "fromJSON":
        return json.loads(to_string(args[0]))
    raise Unsupported(f"unknown function {name}()")


class EvalContext:
    """Contexts an expression can read, plus the job status so far."""

    def __init__(
        self,
        github: Dict[str, Any],
        env: Dict[str, str],
        runner: Dict[str, Any],
        matrix: Optional[Dict[str, Any]] = None,
    ) -> None:
        self.contexts: Dict[str, Any] = {
            "github": github,
            "env": env,
            "runner": runner,
            "job": {"status": "success"},
            "steps": {},
            "secrets": {},
            "vars": {},
            "matrix": matrix or {},
            "inputs": {},
            "strategy": {"fail-fast": True, "job-index": 0, "job-total": 1},
        }
        self.job_status = "success"
        self.warned: set = set()

    def lookup(self, parts: Sequence[Any], source: str) -> Any:
        if not parts:
            raise Unsupported(f"empty expression {source!r}")
        name = str(parts[0])
        if name in ("secrets", "vars") and len(parts) > 1:
            key = f"{name}.{parts[1]}"
            if key not in self.warned:
                self.warned.add(key)
                warn(f"{key} is empty: ci-local has no repository {name}")
        if name not in self.contexts:
            # GitHub rejects the workflow for an unrecognized named-value;
            # guessing a value here could silently flip a condition.
            raise Unsupported(
                f"expression {source!r} reads context {name!r}, which ci-local does not provide"
            )
        value: Any = self.contexts[name]
        for part in parts[1:]:
            if isinstance(value, dict):
                value = value.get(part)
            elif isinstance(value, list):
                value = (
                    value[part]
                    if isinstance(part, int) and -len(value) <= part < len(value)
                    else None
                )
            else:
                return None
        return value


EXPR_RE = re.compile(r"\$\{\{(.*?)\}\}", re.S)


def interpolate(text: str, context: EvalContext) -> str:
    """Replace every ${{ ... }} in text with its evaluated value."""

    def replace(match: re.Match) -> str:
        expression = match.group(1).strip()
        if not expression:
            return ""
        return to_string(Expression(expression, context).parse())

    return EXPR_RE.sub(replace, text)


def strip_expression(text: str) -> str:
    """The body of a whole-value `${{ ... }}`, or the text unchanged."""
    match = re.fullmatch(r"\$\{\{(.*)\}\}", text.strip(), re.S)
    return match.group(1).strip() if match else text.strip()


def calls_status_function(expression: Expression) -> bool:
    tokens = expression.tokens
    return any(
        token.kind == "ident"
        and token.value in STATUS_FUNCTIONS
        and index + 1 < len(tokens)
        and tokens[index + 1].value == "("
        for index, token in enumerate(tokens)
    )


def evaluate_condition(condition: Any, context: EvalContext) -> bool:
    """Evaluate a step/job `if`. A missing condition means `success()`, and a
    condition that calls no status function means `success() && (...)`."""
    succeeded = context.job_status == "success"
    if condition is None:
        return succeeded
    if isinstance(condition, bool):
        return succeeded and condition
    text = strip_expression(str(condition))
    if not text:
        return succeeded
    expression = Expression(text, context)
    if calls_status_function(expression):
        return truthy(expression.parse())
    # GitHub skips the step without evaluating the rest once the job failed.
    return succeeded and truthy(expression.parse())


def evaluate_flag(value: Any, key: str, context: EvalContext) -> bool:
    """A boolean step/job key that may also be an expression."""
    if value is None:
        return False
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        text = value.strip()
        if text.lower() in ("true", "false"):
            return text.lower() == "true"
        if text.startswith("${{"):
            return truthy(Expression(strip_expression(text), context).parse())
    raise Unsupported(f"{key} must be a boolean or an expression, not {value!r}")


def evaluate_minutes(value: Any, key: str, context: EvalContext) -> Optional[float]:
    """A timeout-minutes value that may also be an expression."""
    if value is None or value == "":
        return None
    if isinstance(value, str) and value.strip().startswith("${{"):
        value = Expression(strip_expression(value), context).parse()
    number = to_number(value) if not isinstance(value, bool) else math.nan
    if math.isnan(number) or number <= 0:
        raise Unsupported(f"{key} must be a positive number, not {value!r}")
    return number


# ---------------------------------------------------------------------------
# GitHub context
# ---------------------------------------------------------------------------


def git_output(args: Sequence[str]) -> str:
    try:
        result = subprocess.run(
            ["git", *args],
            capture_output=True,
            text=True,
            check=False,
            cwd=os.environ.get("GITHUB_WORKSPACE") or os.getcwd(),
        )
    except OSError:
        return ""
    return result.stdout.strip() if result.returncode == 0 else ""


def github_context(
    workflow_name: str, workflow_path: str, job_id: str
) -> Dict[str, Any]:
    workspace = os.environ.get("GITHUB_WORKSPACE") or os.getcwd()
    repository = os.environ.get("CI_LOCAL_REPOSITORY") or Path(workspace).name
    ref_name = os.environ.get("CI_LOCAL_REF_NAME") or "main"
    sha = (
        os.environ.get("CI_LOCAL_SHA") or git_output(["rev-parse", "HEAD"]) or "0" * 40
    )
    runner_temp = os.environ.get("RUNNER_TEMP") or tempfile.gettempdir()
    event_path = Path(runner_temp) / "event.json"
    try:
        event_path.parent.mkdir(parents=True, exist_ok=True)
        if not event_path.exists():
            event_path.write_text("{}\n", encoding="utf-8")
    except OSError:
        event_path = Path(tempfile.gettempdir()) / "ci-local-event.json"
    return {
        "action": "__run",
        "action_path": "",
        "action_ref": "",
        "action_repository": "",
        "actor": "ci-local",
        "api_url": "https://api.github.com",
        "base_ref": "",
        "event": {},
        "event_name": os.environ.get("CI_LOCAL_EVENT_NAME") or "workflow_dispatch",
        "event_path": str(event_path),
        "graphql_url": "https://api.github.com/graphql",
        "head_ref": "",
        "job": job_id,
        "ref": f"refs/heads/{ref_name}",
        "ref_name": ref_name,
        "ref_protected": False,
        "ref_type": "branch",
        "repository": repository,
        "repository_id": "0",
        "repository_owner": repository.split("/")[0] if "/" in repository else "local",
        "retention_days": "90",
        "run_attempt": "1",
        "run_id": os.environ.get("CI_LOCAL_RUN_ID") or str(int(time.time())),
        "run_number": "1",
        "server_url": "https://github.com",
        "sha": sha,
        "token": "",
        "triggering_actor": "ci-local",
        "workflow": workflow_name,
        "workflow_ref": f"{repository}/{workflow_path}",
        "workflow_sha": sha,
        "workspace": workspace,
    }


def runner_context() -> Dict[str, Any]:
    machine = os.uname().machine
    return {
        "name": os.environ.get("RUNNER_NAME") or "ci-local",
        "os": "Linux",
        "arch": {"x86_64": "X64", "aarch64": "ARM64", "riscv64": "RISCV64"}.get(
            machine, machine.upper()
        ),
        "temp": os.environ.get("RUNNER_TEMP") or tempfile.gettempdir(),
        "tool_cache": os.environ.get("RUNNER_TOOL_CACHE") or "/opt/hostedtoolcache",
        "workspace": os.environ.get("GITHUB_WORKSPACE") or os.getcwd(),
    }


def standard_env(github: Dict[str, Any], runner: Dict[str, Any]) -> Dict[str, str]:
    """The environment every GitHub-hosted step starts with."""
    env = {
        "CI": "true",
        "GITHUB_ACTIONS": "true",
        "GITHUB_ACTOR": str(github["actor"]),
        "GITHUB_API_URL": str(github["api_url"]),
        "GITHUB_BASE_REF": str(github["base_ref"]),
        "GITHUB_EVENT_NAME": str(github["event_name"]),
        "GITHUB_EVENT_PATH": str(github["event_path"]),
        "GITHUB_GRAPHQL_URL": str(github["graphql_url"]),
        "GITHUB_HEAD_REF": str(github["head_ref"]),
        "GITHUB_JOB": str(github["job"]),
        "GITHUB_REF": str(github["ref"]),
        "GITHUB_REF_NAME": str(github["ref_name"]),
        "GITHUB_REF_PROTECTED": "false",
        "GITHUB_REF_TYPE": str(github["ref_type"]),
        "GITHUB_REPOSITORY": str(github["repository"]),
        "GITHUB_REPOSITORY_ID": str(github["repository_id"]),
        "GITHUB_REPOSITORY_OWNER": str(github["repository_owner"]),
        "GITHUB_RETENTION_DAYS": str(github["retention_days"]),
        "GITHUB_RUN_ATTEMPT": str(github["run_attempt"]),
        "GITHUB_RUN_ID": str(github["run_id"]),
        "GITHUB_RUN_NUMBER": str(github["run_number"]),
        "GITHUB_SERVER_URL": str(github["server_url"]),
        "GITHUB_SHA": str(github["sha"]),
        "GITHUB_TRIGGERING_ACTOR": str(github["triggering_actor"]),
        "GITHUB_WORKFLOW": str(github["workflow"]),
        "GITHUB_WORKFLOW_REF": str(github["workflow_ref"]),
        "GITHUB_WORKFLOW_SHA": str(github["workflow_sha"]),
        "GITHUB_WORKSPACE": str(github["workspace"]),
        "RUNNER_ARCH": str(runner["arch"]),
        "RUNNER_NAME": str(runner["name"]),
        "RUNNER_OS": str(runner["os"]),
        "RUNNER_TEMP": str(runner["temp"]),
        "RUNNER_TOOL_CACHE": str(runner["tool_cache"]),
    }
    return env


# ---------------------------------------------------------------------------
# Toolcache (the layout actions/setup-* populate)
# ---------------------------------------------------------------------------


@dataclasses.dataclass
class ToolVersion:
    """One installed tool: the version string and its toolcache root."""

    version: str
    root: Path


def version_key(version: str) -> Tuple[int, ...]:
    parts = re.findall(r"\d+", version)
    return tuple(int(part) for part in parts) or (0,)


def toolcache_candidates(tool: str) -> List[ToolVersion]:
    """Every toolcache install for a tool, newest version first.

    actions/*-versions lay the toolcache out as <tool>/<version>/<arch>, and
    <arch> is the directory that holds bin/ -- Go, Node.js and Python alike.
    A layout without the arch level is accepted too, since a hand-built cache
    may skip it.
    """
    root = Path(os.environ.get("RUNNER_TOOL_CACHE") or "/opt/hostedtoolcache") / tool
    if not root.is_dir():
        return []
    versions = sorted(
        (entry for entry in root.iterdir() if entry.is_dir()),
        key=lambda path: version_key(path.name),
        reverse=True,
    )
    candidates: List[ToolVersion] = []
    for version in versions:
        arches = sorted(entry for entry in version.iterdir() if entry.is_dir())
        if arches:
            candidates.extend(ToolVersion(version.name, arch) for arch in arches)
        else:
            candidates.append(ToolVersion(version.name, version))
    return candidates


def select_toolcache(tool: str, spec: str) -> Optional[ToolVersion]:
    """Resolve a setup-* version spec against the toolcache."""
    spec = spec.strip().lstrip("v")
    candidates = toolcache_candidates(tool)
    if not spec or spec in ("latest", "stable", "current"):
        return candidates[0] if candidates else None
    if spec.endswith(".x"):
        prefix = spec[:-2]
        for candidate in candidates:
            if candidate.version == prefix or candidate.version.startswith(
                prefix + "."
            ):
                return candidate
    if re.fullmatch(r"\d+(\.\d+)*", spec):
        for candidate in candidates:
            if candidate.version == spec or candidate.version.startswith(spec + "."):
                return candidate
    for candidate in candidates:
        if candidate.version == spec:
            return candidate
    return None


def require_toolcache(tool: str, spec: str, env_name: str) -> ToolVersion:
    """Resolve a toolcache entry or fall back to the tool already on PATH."""
    selected = select_toolcache(tool, spec)
    if selected is not None:
        return selected
    fallback = shutil.which(env_name)
    if fallback:
        warn(
            f"toolcache holds no {tool} matching {spec!r} "
            f"(image built without that version?); falling back to {fallback}"
        )
        return ToolVersion(spec, Path(fallback).resolve().parent.parent)
    raise Unsupported(f"{tool} {spec!r} is in neither the toolcache nor PATH")


# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

SUPPORTED_STEP_KEYS = {
    "name",
    "uses",
    "with",
    "run",
    "env",
    "id",
    "working-directory",
    "shell",
    "timeout-minutes",
    "continue-on-error",
    "if",
}


@dataclasses.dataclass
class StepRecord:
    label: str
    status: str
    seconds: float
    detail: str = ""


class JobState:
    """Mutable state shared by the steps of one job."""

    def __init__(self, workspace: str, runner_temp: str) -> None:
        self.workspace = workspace
        self.runner_temp = runner_temp
        self.extra_env: Dict[str, str] = {}
        self.path_entries: List[str] = []
        self.steps: Dict[str, Dict[str, Any]] = {}
        self.notes: List[str] = []

    def note(self, message: str) -> None:
        self.notes.append(message)

    def prepend_path(self, entry: str) -> None:
        if entry and entry not in self.path_entries:
            self.path_entries.insert(0, entry)

    def command_file(self, name: str, index: int) -> Path:
        directory = Path(self.runner_temp) / "_runner_file_commands"
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / f"{name}_{os.getpid()}_{index}"
        path.write_text("", encoding="utf-8")
        return path

    def base_path(self, environ: Dict[str, str]) -> str:
        parts = [*self.path_entries, environ.get("PATH", "")]
        return os.pathsep.join(part for part in parts if part)


def step_label(step: Dict[str, Any], index: int) -> str:
    if step.get("name"):
        return str(step["name"])
    if step.get("uses"):
        return str(step["uses"])
    first_line = str(step.get("run", "")).strip().splitlines()
    return first_line[0] if first_line else f"step {index + 1}"


def parse_env(raw: Any, context: Optional[EvalContext]) -> Dict[str, str]:
    env: Dict[str, str] = {}
    for key, value in (raw or {}).items():
        text = to_string(value)
        if context is not None:
            text = interpolate(text, context)
        env[str(key)] = text
    return env


def setup_action(uses: str, with_map: Dict[str, str], state: JobState) -> List[str]:
    """Emulate actions/setup-*, returning environment variables to export."""
    action = uses.split("@", 1)[0]
    if action == "actions/checkout":
        if str(with_map.get("persist-credentials", "")).lower() == "true":
            warn(
                "checkout with persist-credentials: true has no token to persist in ci-local"
            )
        state.note("workspace already prepared by entrypoint.sh")
        return []
    if action == "actions/setup-go":
        spec = find_tool_spec(with_map, ("go-version", "version"))
        if "go-version-file" in with_map:
            path = Path(state.workspace) / with_map["go-version-file"]
            spec = parse_go_directive(path) or spec
        if not spec:
            raise Unsupported("actions/setup-go without go-version or go-version-file")
        go = require_toolcache("go", spec, "go")
        state.prepend_path(str(go.root / "bin"))
        state.note(f"Go {go.version} from {go.root}")
        return ["GOROOT=" + str(go.root)]
    if action == "actions/setup-node":
        spec = find_tool_spec(with_map, ("node-version", "version"))
        if not spec:
            raise Unsupported("actions/setup-node without node-version")
        node = require_toolcache("node", spec, "node")
        state.prepend_path(str(node.root / "bin"))
        state.note(f"Node.js {node.version} from {node.root}")
        return []
    if action == "actions/setup-python":
        spec = find_tool_spec(with_map, ("python-version", "version"))
        if not spec:
            raise Unsupported("actions/setup-python without python-version")
        python = require_toolcache("Python", spec, "python3")
        state.prepend_path(str(python.root / "bin"))
        ensure_python3_alias(python.root, state)
        state.note(f"Python {python.version} from {python.root}")
        return []
    raise Unsupported(
        f"action {action!r} is not emulated by ci-local; it implements only "
        "actions/checkout, actions/setup-go, actions/setup-node and actions/setup-python"
    )


def find_tool_spec(with_map: Dict[str, str], keys: Sequence[str]) -> str:
    for key in keys:
        if key in with_map:
            return str(with_map[key])
    return ""


def ensure_python3_alias(python_root: Path, state: JobState) -> None:
    """setup-python guarantees `python3`; the toolcache builds only ship `python`."""
    bin_dir = python_root / "bin"
    if (bin_dir / "python3").exists() or not (bin_dir / "python").exists():
        return
    shim = Path(state.runner_temp) / "_ci_local_shims"
    shim.mkdir(parents=True, exist_ok=True)
    link = shim / "python3"
    if not link.exists():
        link.symlink_to(bin_dir / "python")
    state.prepend_path(str(shim))


def parse_go_directive(path: Path) -> str:
    try:
        for line in path.read_text(encoding="utf-8").splitlines():
            match = re.match(r"^go\s+(\S+)", line.strip())
            if match:
                return match.group(1)
    except OSError:
        return ""
    return ""


# ---------------------------------------------------------------------------
# GitHub command files
# ---------------------------------------------------------------------------

KEY_VALUE_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
HEREDOC_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)<<(\S*)$")


def read_command_file(path: Path) -> List[Tuple[str, str]]:
    """Parse a GitHub command file: KEY=VALUE and KEY<<DELIM blocks."""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    entries: List[Tuple[str, str]] = []
    index = 0
    while index < len(lines):
        line = lines[index]
        index += 1
        heredoc = HEREDOC_RE.match(line)
        if heredoc:
            delimiter = heredoc.group(2)
            body: List[str] = []
            while index < len(lines) and lines[index] != delimiter:
                body.append(lines[index])
                index += 1
            index += 1  # skip the delimiter
            entries.append((heredoc.group(1), "\n".join(body)))
            continue
        simple = KEY_VALUE_RE.match(line)
        if simple and not simple.group(2).endswith("<<"):
            entries.append((simple.group(1), simple.group(2)))
    return entries


def apply_github_env(path: Path, state: JobState) -> List[str]:
    applied: List[str] = []
    for key, value in read_command_file(path):
        state.extra_env[key] = value
        applied.append(key)
    return applied


def apply_github_path(path: Path, state: JobState) -> List[str]:
    """GITHUB_PATH holds one literal directory per line, not KEY=VALUE."""
    try:
        entries = [
            line.strip() for line in path.read_text(encoding="utf-8").splitlines()
        ]
    except OSError:
        return []
    applied: List[str] = []
    for entry in reversed([entry for entry in entries if entry]):
        state.prepend_path(entry)
        applied.append(entry)
    return list(reversed(applied))


def apply_github_output(path: Path, state: JobState, step_id: str) -> List[str]:
    if not step_id:
        return []
    outputs = state.steps.setdefault(step_id, {}).setdefault("outputs", {})
    applied: List[str] = []
    for key, value in read_command_file(path):
        outputs[key] = value
        applied.append(key)
    return applied


# ---------------------------------------------------------------------------
# Process execution
# ---------------------------------------------------------------------------


def run_script(
    argv: Sequence[str],
    script_path: Path,
    cwd: str,
    env: Dict[str, str],
    timeout: Optional[float],
    label: str,
) -> Tuple[int, List[str], bool]:
    """Run a step script, streaming its output. Returns (code, tail, timed_out)."""
    process = subprocess.Popen(
        [*argv, str(script_path)],
        cwd=cwd,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
        start_new_session=True,
    )
    tail: deque = deque(maxlen=40)
    killed = False

    def kill() -> None:
        nonlocal killed
        killed = True
        try:
            os.killpg(os.getpgid(process.pid), signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            process.kill()

    timer = threading.Timer(timeout, kill) if timeout else None
    if timer:
        timer.start()
    try:
        assert process.stdout is not None
        for line in process.stdout:
            sys.stdout.write(line)
            sys.stdout.flush()
            tail.append(line.rstrip("\n"))
        code = process.wait()
    finally:
        if timer:
            timer.cancel()
    if killed:
        print(red(f"ci-local: {label} exceeded its timeout"), flush=True)
        return 124, list(tail), True
    return code, list(tail), False


# ---------------------------------------------------------------------------
# Workflow loading and execution
# ---------------------------------------------------------------------------


def workflow_files(workspace: str) -> List[Path]:
    directory = Path(workspace) / WORKFLOW_DIR
    if not directory.is_dir():
        return []
    return sorted([*directory.glob("*.yml"), *directory.glob("*.yaml")])


def load_workflow(workspace: str, name: str) -> Path:
    candidates = workflow_files(workspace)
    wanted = name.strip()
    for path in candidates:
        if wanted in (path.name, path.stem, str(path), WORKFLOW_DIR + "/" + path.name):
            return path
    available = ", ".join(path.name for path in candidates) or "none"
    raise Unsupported(f"no workflow named {name!r} (available: {available})")


def read_workflow(path: Path) -> Dict[str, Any]:
    try:
        import yaml  # pylint: disable=import-outside-toplevel
    except ImportError:  # pragma: no cover - the ci-local image installs python3-yaml
        sys.exit("ci-local: PyYAML is missing; it is installed in the ci-local image")
    workflow = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(workflow, dict):
        raise Unsupported(f"{path.name} does not contain a workflow mapping")
    # PyYAML reads the bare `on:` key as boolean True (YAML 1.1), so accept both.
    if not (workflow.get("on") or workflow.get(True)):
        warn(
            f"{path.name} declares no `on:` trigger; running it as a workflow_dispatch"
        )
    return workflow


def list_workflows(workspace: str, wanted: Sequence[str]) -> int:
    files = [
        path
        for path in workflow_files(workspace)
        if not wanted or path.name in wanted or path.stem in wanted
    ]
    if not files:
        print(
            f"ci-local: no matching workflows under {os.path.join(workspace, WORKFLOW_DIR)}"
        )
        return 1
    for path in files:
        workflow = read_workflow(path)
        print(f"{bold(path.name)}  {workflow.get('name', '')}")
        for job_id, job in (workflow.get("jobs") or {}).items():
            steps = job.get("steps") or []
            actions = sorted(
                {
                    str(step["uses"]).split("@", 1)[0]
                    for step in steps
                    if step.get("uses")
                }
            )
            detail = f"runs-on {job.get('runs-on', '?')}, {len(steps)} steps"
            if actions:
                detail += ", uses " + ", ".join(actions)
            print(f"  {cyan(f'{job_id:<24}')} {job.get('name', '')}")
            print(f"      {detail}")
    return 0


def run_job(path: Path, job_id: str, options: argparse.Namespace) -> int:
    workspace = os.environ.get("GITHUB_WORKSPACE") or os.getcwd()
    runner_temp = os.environ.get("RUNNER_TEMP") or tempfile.gettempdir()
    workflow = read_workflow(path)
    jobs = workflow.get("jobs") or {}
    if job_id not in jobs:
        raise Unsupported(
            f"{path.name} has no job {job_id!r} (jobs: {', '.join(jobs) or 'none'})"
        )
    job = jobs[job_id]

    matrix: Dict[str, Any] = {}
    strategy = job.get("strategy") or {}
    if strategy.get("matrix"):
        raw_matrix = strategy["matrix"]
        if any(
            isinstance(value, list) and len(value) > 1 for value in raw_matrix.values()
        ):
            raise Unsupported(
                "strategy.matrix with more than one combination is not emulated by ci-local"
            )
        matrix = {
            key: (value[0] if isinstance(value, list) and value else value)
            for key, value in raw_matrix.items()
        }
    for ignored in ("container", "services"):
        if job.get(ignored):
            raise Unsupported(f"job-level {ignored!r} is not emulated by ci-local")
    for ignored in ("needs", "outputs", "environment"):
        if job.get(ignored):
            warn(
                f"job key {ignored!r} is ignored by ci-local; run its dependencies yourself"
            )
    runs_on = str(job.get("runs-on", ""))
    if runs_on and "ubuntu" not in runs_on:
        warn(f"job runs-on {runs_on!r} is not ubuntu-latest; the image may not match")

    github = github_context(
        str(workflow.get("name") or path.stem), WORKFLOW_DIR + "/" + path.name, job_id
    )
    runner = runner_context()
    state = JobState(workspace, runner_temp)
    base_context = EvalContext(github, {}, runner, matrix)
    # No `needs` job ran here, so a job condition sees success(); one that
    # reads the needs context fails loudly as an unknown context.
    if not evaluate_condition(job.get("if"), base_context):
        print()
        print(f"{yellow('⤼')} {job.get('name', job_id)} {yellow('(job skipped: if)')}")
        return 0
    job_env = {
        **parse_env(workflow.get("env"), base_context),
        **parse_env(job.get("env"), base_context),
    }

    print()
    print(
        bold(
            f"── {workflow.get('name', path.name)} / {job.get('name', job_id)} "
        ).ljust(78, "─")
    )
    print(
        f"   {path.name}, job {job_id}, runs-on {runs_on or 'ubuntu-latest'} (ci-local image)"
    )

    default_shell = ""
    default_cwd = ""
    for source in (workflow.get("defaults") or {}, job.get("defaults") or {}):
        run_defaults = (source or {}).get("run", {}) or {}
        default_shell = run_defaults.get("shell", default_shell)
        default_cwd = run_defaults.get("working-directory", default_cwd)

    job_timeout = evaluate_minutes(
        job.get("timeout-minutes"), "job timeout-minutes", base_context
    )
    job_deadline = time.monotonic() + job_timeout * 60 if job_timeout else None
    records: List[StepRecord] = []
    failed = False

    for index, step in enumerate(job.get("steps") or []):
        unknown = set(step) - SUPPORTED_STEP_KEYS
        if unknown:
            raise Unsupported(
                f"step {step_label(step, index)!r} uses unsupported keys: {', '.join(sorted(unknown))}"
            )
        context = EvalContext(github, {**job_env, **state.extra_env}, runner, matrix)
        context.contexts["steps"] = state.steps
        context.job_status = "failure" if failed else "success"

        try:
            condition = evaluate_condition(step.get("if"), context)
            label = interpolate(step_label(step, index), context)
            continue_on_error = evaluate_flag(
                step.get("continue-on-error"), "continue-on-error", context
            )
        except Unsupported as exc:
            print(red(f"✗ {step_label(step, index)}: {exc}"), flush=True)
            records.append(
                StepRecord(step_label(step, index), "failure", 0.0, str(exc))
            )
            failed = True
            break
        if not condition:
            print(f"{yellow('⤼')} {label} {yellow('(skipped: if)')}", flush=True)
            records.append(StepRecord(label, "skipped", 0.0))
            continue
        # --step narrows the `run` steps only: the setup actions put the
        # toolchains on PATH, and every later step depends on them.
        if (
            options.step
            and not step.get("uses")
            and not any(pattern.lower() in label.lower() for pattern in options.step)
        ):
            print(f"{yellow('⤼')} {label} {yellow('(skipped: --step)')}", flush=True)
            records.append(StepRecord(label, "skipped", 0.0))
            continue

        print(flush=True)
        print(cyan(f"▶ {label}"), flush=True)
        started = time.monotonic()
        try:
            status, detail = execute_step(
                step,
                index,
                context,
                state,
                job_env,
                default_shell,
                default_cwd,
                job_deadline,
            )
        except Unsupported as exc:
            # An unemulated construct stops the job: nothing after it can be
            # trusted to mean what it says.
            elapsed = time.monotonic() - started
            print(red(f"ci-local: {exc}"), flush=True)
            records.append(StepRecord(label, "failure", elapsed, str(exc)))
            failed = True
            break
        elapsed = time.monotonic() - started
        records.append(StepRecord(label, status, elapsed, detail))
        if status != "failure":
            print(green(f"✓ {label}") + f" ({elapsed:.1f}s)", flush=True)
            continue
        if continue_on_error:
            print(
                yellow(
                    f"⚠ {label} failed but continue-on-error is set ({elapsed:.1f}s)"
                ),
                flush=True,
            )
            continue
        failed = True
        print(red(f"✗ {label} failed after {elapsed:.1f}s"), flush=True)
        if detail:
            print(red(f"  {detail}"), flush=True)
        if job_deadline is not None and time.monotonic() >= job_deadline:
            break

    print_summary(records)
    return 1 if failed else 0


def execute_step(
    step: Dict[str, Any],
    index: int,
    context: EvalContext,
    state: JobState,
    job_env: Dict[str, str],
    default_shell: str,
    default_cwd: str,
    job_deadline: Optional[float],
) -> Tuple[str, str]:
    with_map = {
        str(key): interpolate(to_string(value), context)
        for key, value in (step.get("with") or {}).items()
    }
    if step.get("uses"):
        extra_env = setup_action(str(step["uses"]), with_map, state)
        if state.notes:
            for note in state.notes:
                print(f"  {note}", flush=True)
            state.notes.clear()
        state.extra_env.update(
            {
                key: value
                for key, value in (
                    item.split("=", 1) for item in extra_env if "=" in item
                )
            }
        )
        return "success", ""
    if not step.get("run"):
        raise Unsupported(
            f"step {step_label(step, index)!r} has neither `run` nor a known `uses`"
        )

    step_env = parse_env(step.get("env"), context)
    script_context = EvalContext(
        context.contexts["github"],
        {**job_env, **state.extra_env, **step_env},
        context.contexts["runner"],
        context.contexts["matrix"],
    )
    script_context.job_status = context.job_status
    script = interpolate(str(step["run"]), script_context)

    shell = str(step.get("shell") or default_shell or "bash")
    if shell not in SHELLS:
        raise Unsupported(
            f"unsupported shell {shell!r}; ci-local implements {', '.join(sorted(SHELLS))}"
        )
    cwd = str(step.get("working-directory") or default_cwd or state.workspace)
    cwd = cwd if os.path.isabs(cwd) else os.path.join(state.workspace, cwd)
    if not os.path.isdir(cwd):
        return "failure", f"working-directory {cwd} does not exist"

    env = dict(os.environ)
    env.update(standard_env(context.contexts["github"], context.contexts["runner"]))
    env.update(job_env)
    env.update(state.extra_env)
    env.update(step_env)
    env["PATH"] = state.base_path(env)
    step_id = str(step.get("id") or "")
    files = {
        "GITHUB_ENV": state.command_file("set_env", index),
        "GITHUB_PATH": state.command_file("add_path", index),
        "GITHUB_OUTPUT": state.command_file("set_output", index),
        "GITHUB_STEP_SUMMARY": state.command_file("step_summary", index),
        "GITHUB_STATE": state.command_file("save_state", index),
    }
    env.update({key: str(value) for key, value in files.items()})

    script_path = Path(state.runner_temp) / f"_ci_local_step_{os.getpid()}_{index}.sh"
    script_path.write_text(script + "\n", encoding="utf-8")

    step_minutes = evaluate_minutes(
        step.get("timeout-minutes"), "timeout-minutes", context
    )
    timeouts = [step_minutes * 60] if step_minutes else []
    if job_deadline is not None:
        remaining = job_deadline - time.monotonic()
        if remaining <= 0:
            return "failure", "the job exceeded its timeout-minutes"
        timeouts.append(max(remaining, 1.0))
    timeout = min(timeouts) if timeouts else None

    code, tail, timed_out = run_script(
        SHELLS[shell], script_path, cwd, env, timeout, str(step.get("name") or "step")
    )
    script_path.unlink(missing_ok=True)

    for key in apply_github_env(files["GITHUB_ENV"], state):
        print(f"  {yellow('env')} {key}", flush=True)
    for entry in apply_github_path(files["GITHUB_PATH"], state):
        print(f"  {yellow('path')} {entry}", flush=True)
    for key in apply_github_output(files["GITHUB_OUTPUT"], state, step_id):
        print(f"  {yellow('output')} {step_id}.{key}", flush=True)
    if step_id:
        state.steps.setdefault(step_id, {})["conclusion"] = (
            "success" if code == 0 else "failure"
        )

    if timed_out:
        return "failure", "timed out"
    if code != 0:
        detail = f"exit code {code}"
        if tail:
            detail += "; last output: " + tail[-1].strip()
        return "failure", detail
    return "success", ""


def print_summary(records: Sequence[StepRecord]) -> None:
    print()
    print(bold("Summary"))
    for record in records:
        if record.status == "success":
            mark = green("✓")
        elif record.status == "skipped":
            mark = yellow("⤼")
        else:
            mark = red("✗")
        line = f"  {mark} {record.label} ({record.seconds:.1f}s)"
        if record.detail:
            line += f" — {record.detail}"
        print(line)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="run-workflow.py",
        description="Run the Kula GitHub Actions workflows inside the ci-local image.",
    )
    parser.add_argument(
        "--step",
        action="append",
        default=[],
        metavar="PATTERN",
        help="run only the `run` steps whose name contains PATTERN (repeatable); setup actions always run",
    )
    subparsers = parser.add_subparsers(dest="command")

    list_parser = subparsers.add_parser("list", help="list workflows and their jobs")
    list_parser.add_argument("workflows", nargs="*")

    run_parser = subparsers.add_parser("run", help="run one job of one workflow")
    run_parser.add_argument("workflow")
    run_parser.add_argument("job", nargs="?")

    subparsers.add_parser("run-all", help="run every job of every workflow")

    options = parser.parse_args(argv)
    workspace = os.environ.get("GITHUB_WORKSPACE") or os.getcwd()

    try:
        if options.command in (None, "list"):
            return list_workflows(workspace, getattr(options, "workflows", []))
        if options.command == "run":
            path = load_workflow(workspace, options.workflow)
            workflow = read_workflow(path)
            ids = list((workflow.get("jobs") or {}).keys())
            if options.job:
                return run_job(path, options.job, options)
            if len(ids) == 1:
                return run_job(path, ids[0], options)
            print(
                f"ci-local: {path.name} has {len(ids)} jobs; name one of: {', '.join(ids)}",
                file=sys.stderr,
            )
            return 2
        if options.command == "run-all":
            status = 0
            for path in workflow_files(workspace):
                workflow = read_workflow(path)
                for job_id in workflow.get("jobs") or {}:
                    status |= run_job(path, job_id, options)
            return status
        parser.error(f"unknown command {options.command!r}")
    except Unsupported as exc:
        print(red(f"ci-local: {exc}"), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(red("\nci-local: interrupted"), file=sys.stderr)
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
