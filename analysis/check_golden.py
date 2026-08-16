#!/usr/bin/env python3
"""Verify stats.py output against checked-in fixtures.

Two things are checked:
  1. The happy-path fixture (testdata/fixture.csv) regenerates markdown
     that matches testdata/golden.md byte-for-byte.
  2. The underpowered fixture (testdata/underpowered.csv) is refused with
     exit code 2 and a stderr message naming the offending group.

Run with: python3 analysis/check_golden.py
"""

from __future__ import annotations

import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
STATS = HERE / "stats.py"
FIXTURE = HERE / "testdata" / "fixture.csv"
GOLDEN = HERE / "testdata" / "golden.md"
UNDERPOWERED = HERE / "testdata" / "underpowered.csv"


def check_golden_markdown() -> None:
    result = subprocess.run(
        [sys.executable, str(STATS), str(FIXTURE), "--by", "scenario,condition,detail", "--emit", "markdown"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        sys.exit(
            f"FAIL: stats.py exited {result.returncode} on the happy-path fixture, expected 0\nstderr:\n{result.stderr}"
        )

    want = GOLDEN.read_text()
    got = result.stdout
    if got != want:
        sys.exit(
            "FAIL: stats.py markdown output does not match testdata/golden.md\n"
            f"--- want ---\n{want}\n--- got ---\n{got}"
        )
    print("PASS: happy-path markdown output matches golden.md")


def check_refusal() -> None:
    result = subprocess.run(
        [sys.executable, str(STATS), str(UNDERPOWERED), "--by", "scenario,condition,detail", "--emit", "markdown"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 2:
        sys.exit(
            f"FAIL: stats.py exited {result.returncode} on the underpowered fixture, expected 2\nstderr:\n{result.stderr}"
        )
    if "scale=5;changes=1;events=on" not in result.stderr:
        sys.exit(f"FAIL: refusal message does not name the offending group\nstderr:\n{result.stderr}")
    if "n=5" not in result.stderr:
        sys.exit(f"FAIL: refusal message does not report the actual n\nstderr:\n{result.stderr}")
    print("PASS: underpowered fixture is refused with exit 2 and names the offending group")


def main() -> int:
    check_golden_markdown()
    check_refusal()
    return 0


if __name__ == "__main__":
    sys.exit(main())
