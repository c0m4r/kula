#!/usr/bin/env python3
"""
Tests for run-workflow.py, the addons/ci-local.sh step runner.

    python3 -m unittest discover -s addons/ci-local -v

The expression tables pin GitHub's documented semantics (literals, loose
equality, implicit success()); a runner that disagrees with GitHub about a
condition can silently skip a check or run one that should not. The job tests
execute small workflows with bash and need PyYAML, like the runner itself.
"""

import argparse
import contextlib
import importlib.util
import io
import os
import tempfile
import textwrap
import unittest
from pathlib import Path
from typing import Any, Dict, List, Optional
from unittest import mock

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("run_workflow", HERE / "run-workflow.py")
assert SPEC is not None and SPEC.loader is not None
rw: Any = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(rw)

try:
    import yaml  # noqa: F401  pylint: disable=unused-import

    HAVE_YAML = True
except ImportError:  # pragma: no cover - the ci-local image installs python3-yaml
    HAVE_YAML = False


def make_context(status: str = "success") -> Any:
    context = rw.EvalContext(
        {"repository": "c0m4r/kula", "event": {}},
        {"SET": "yes", "EMPTY": "", "NUM": "3"},
        {"os": "Linux"},
    )
    context.contexts["steps"] = {"build-go": {"outputs": {"version": "1.26"}}}
    context.job_status = status
    return context


def evaluate(expression: str, status: str = "success") -> Any:
    return rw.Expression(expression, make_context(status)).parse()


class ExpressionTest(unittest.TestCase):
    def test_values(self) -> None:
        cases = [
            ("true", True),
            ("false", False),
            ("null", None),
            ("42", 42),
            ("'it''s'", "it's"),
            ("true == false", False),
            ("false == true", False),
            ("true == true", True),
            ("true != false", True),
            # Mismatched types compare as numbers: null -> 0, false -> 0.
            ("env.UNSET == false", True),
            ("env.UNSET == true", False),
            ("env.UNSET == null", True),
            ("env.UNSET == ''", True),
            ("env.UNSET == 0", True),
            # 'true' is not a number, and NaN equals nothing.
            ("true == 'true'", False),
            ("'abc' == 0", False),
            ("'' == 0", True),
            ("env.NUM == 3", True),
            ("env.NUM == '3.0'", False),
            ("env.SET == 'YES'", True),
            ("!env.UNSET", True),
            ("!true", False),
            ("env.SET && env.EMPTY", ""),
            ("env.EMPTY || 'fallback'", "fallback"),
            ("(false || true) && 'x'", "x"),
            ("steps.build-go.outputs.version", "1.26"),
            ("github.event.true", None),
            ("contains('Hello', 'ELL')", True),
            ("startsWith(github.repository, 'c0m4r/')", True),
            ("endsWith('kula.yml', '.YML')", True),
            ("format('{0}-{1}', 'a', 1)", "a-1"),
            ("fromJSON('true')", True),
        ]
        for expression, want in cases:
            with self.subTest(expression=expression):
                self.assertEqual(evaluate(expression), want)

    def test_unknown_context_fails_loudly(self) -> None:
        for expression in ("needs.build.result", "foo", "foo == false"):
            with self.subTest(expression=expression):
                with self.assertRaises(rw.Unsupported):
                    evaluate(expression)

    def test_unsupported_syntax_fails_loudly(self) -> None:
        for expression in ("1 < 2", "hashFiles('go.sum')", "github.*.x", "nope()"):
            with self.subTest(expression=expression):
                with self.assertRaises(rw.Unsupported):
                    evaluate(expression)

    def test_interpolate_formats_literals(self) -> None:
        self.assertEqual(
            rw.interpolate("${{ true }}/${{ null }}/${{ 1.5 }}", make_context()),
            "true//1.5",
        )


class ConditionTest(unittest.TestCase):
    def check(self, condition: Any, status: str, want: bool) -> None:
        with self.subTest(condition=condition, status=status):
            self.assertIs(rw.evaluate_condition(condition, make_context(status)), want)

    def test_default_is_success(self) -> None:
        self.check(None, "success", True)
        self.check(None, "failure", False)
        self.check("", "failure", False)

    def test_literal_conditions(self) -> None:
        self.check("${{ true }}", "success", True)
        self.check("${{ false }}", "success", False)
        self.check("${{ true == false }}", "success", False)
        self.check(True, "success", True)
        self.check(True, "failure", False)

    def test_implicit_success(self) -> None:
        # No status function: GitHub evaluates success() && (...).
        self.check("env.SET == 'yes'", "success", True)
        self.check("env.SET == 'yes'", "failure", False)

    def test_status_functions(self) -> None:
        self.check("always()", "failure", True)
        self.check("${{ failure() }}", "failure", True)
        self.check("failure()", "success", False)
        self.check("success()", "failure", False)
        self.check("!cancelled()", "failure", True)
        self.check("always() && env.UNSET == false", "failure", True)


class FlagTest(unittest.TestCase):
    def test_continue_on_error(self) -> None:
        context = make_context()
        cases = [
            (None, False),
            (True, True),
            (False, False),
            ("true", True),
            ("${{ true }}", True),
            ("${{ env.UNSET == 'x' }}", False),
        ]
        for value, want in cases:
            with self.subTest(value=value):
                self.assertIs(
                    rw.evaluate_flag(value, "continue-on-error", context), want
                )
        for value in ("maybe", 1):
            with self.subTest(value=value):
                with self.assertRaises(rw.Unsupported):
                    rw.evaluate_flag(value, "continue-on-error", context)

    def test_timeout_minutes(self) -> None:
        context = make_context()
        self.assertIsNone(rw.evaluate_minutes(None, "timeout-minutes", context))
        self.assertEqual(rw.evaluate_minutes(5, "timeout-minutes", context), 5.0)
        self.assertEqual(
            rw.evaluate_minutes("${{ 2 }}", "timeout-minutes", context), 2.0
        )
        for value in ("soon", 0, True):
            with self.subTest(value=value):
                with self.assertRaises(rw.Unsupported):
                    rw.evaluate_minutes(value, "timeout-minutes", context)


@unittest.skipUnless(HAVE_YAML, "PyYAML is not installed")
class JobTest(unittest.TestCase):
    WORKFLOW = textwrap.dedent("""
        name: Test
        on: workflow_dispatch
        jobs:
          failing:
            runs-on: ubuntu-latest
            steps:
              - uses: actions/checkout@v4
              - name: first
                run: echo first > "$MARKS/first"
              - name: tolerated
                continue-on-error: ${{ true }}
                run: exit 3
              - name: breaks
                run: exit 1
              - name: skipped after failure
                run: echo ran > "$MARKS/skipped"
              - name: on failure
                if: failure()
                run: echo ran > "$MARKS/failure"
              - name: always
                if: ${{ always() }}
                run: echo ran > "$MARKS/always"
          disabled:
            if: ${{ false }}
            runs-on: ubuntu-latest
            steps:
              - run: echo ran > "$MARKS/off"
          guarded:
            runs-on: ubuntu-latest
            steps:
              - name: never
                if: ${{ true == false }}
                run: echo ran > "$MARKS/never"
              - name: second
                run: echo ran > "$MARKS/second"
        """)

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()  # pylint: disable=consider-using-with
        root = Path(self.tmp.name)
        self.workspace = root / "work"
        self.temp = root / "temp"
        self.marks = root / "marks"
        (self.workspace / rw.WORKFLOW_DIR).mkdir(parents=True)
        self.temp.mkdir()
        self.marks.mkdir()
        (self.workspace / rw.WORKFLOW_DIR / "test.yml").write_text(
            self.WORKFLOW, encoding="utf-8"
        )
        self.env = mock.patch.dict(
            os.environ,
            {
                "GITHUB_WORKSPACE": str(self.workspace),
                "RUNNER_TEMP": str(self.temp),
                "MARKS": str(self.marks),
            },
        )
        self.env.start()

    def tearDown(self) -> None:
        self.env.stop()
        self.tmp.cleanup()

    def run_job(self, job: str, step: Optional[List[str]] = None) -> Dict[str, Any]:
        output = io.StringIO()
        path = self.workspace / rw.WORKFLOW_DIR / "test.yml"
        options = argparse.Namespace(step=step or [])
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            code = rw.run_job(path, job, options)
        ran = {name.name for name in self.marks.iterdir()}
        return {"code": code, "ran": ran, "output": output.getvalue()}

    def test_failure_runs_only_failure_and_always_steps(self) -> None:
        result = self.run_job("failing")
        self.assertEqual(result["code"], 1)
        self.assertEqual(result["ran"], {"first", "failure", "always"})

    def test_job_if_skips_the_job(self) -> None:
        result = self.run_job("disabled")
        self.assertEqual(result["code"], 0)
        self.assertEqual(result["ran"], set())
        self.assertIn("job skipped: if", result["output"])

    def test_false_literal_condition_skips_the_step(self) -> None:
        result = self.run_job("guarded")
        self.assertEqual(result["code"], 0)
        self.assertEqual(result["ran"], {"second"})


if __name__ == "__main__":
    unittest.main()
