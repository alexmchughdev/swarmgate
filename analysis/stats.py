#!/usr/bin/env python3
"""Summarise swarmgate harness results.csv into per-group latency stats.

Input schema (fixed, produced by cmd/harness):
    scenario,condition,run,t_start,t_end,duration_ms,outcome,detail

Only outcome == "ok" rows contribute to the duration statistics (median,
p95, min, max); timeout/error rows are counted separately so a group's
failure rate stays visible instead of being silently dropped.

Groups with fewer than MIN_OK_N ok rows are refused outright (exit 2)
rather than printed with a misleadingly small sample size.
"""

from __future__ import annotations

import argparse
import sys

import pandas as pd

MIN_OK_N = 10
REQUIRED_COLUMNS = [
    "scenario",
    "condition",
    "run",
    "t_start",
    "t_end",
    "duration_ms",
    "outcome",
    "detail",
]
DEFAULT_BY = ["scenario", "condition", "detail"]


def load(path: str) -> pd.DataFrame:
    df = pd.read_csv(path, dtype={"detail": "object", "condition": "object"})
    missing = [c for c in REQUIRED_COLUMNS if c not in df.columns]
    if missing:
        raise SystemExit(f"results csv is missing required column(s): {', '.join(missing)}")
    # detail is legitimately empty for t1 rows; keep it a string rather than
    # letting pandas turn a blank column into NaN, which would fracture
    # group keys between "" and NaN.
    df["detail"] = df["detail"].fillna("")
    return df


def summarise(df: pd.DataFrame, by: list[str]) -> pd.DataFrame:
    """Return one row per --by group with ok-only duration stats plus
    failure counts, sorted by the group key for stable output."""
    rows = []
    for key, group in df.groupby(by, dropna=False, sort=True):
        key_tuple = key if isinstance(key, tuple) else (key,)
        ok = group[group["outcome"] == "ok"]
        n = len(ok)
        timeout_n = int((group["outcome"] == "timeout").sum())
        error_n = int((group["outcome"] == "error").sum())
        row = dict(zip(by, key_tuple))
        row["n"] = n
        if n > 0:
            row["median"] = ok["duration_ms"].median()
            row["p95"] = ok["duration_ms"].quantile(0.95)
            row["min"] = ok["duration_ms"].min()
            row["max"] = ok["duration_ms"].max()
        else:
            row["median"] = float("nan")
            row["p95"] = float("nan")
            row["min"] = float("nan")
            row["max"] = float("nan")
        row["timeout"] = timeout_n
        row["error"] = error_n
        rows.append(row)
    out = pd.DataFrame(rows, columns=by + ["n", "median", "p95", "min", "max", "timeout", "error"])
    return out.sort_values(by=by).reset_index(drop=True)


def enforce_min_n(summary: pd.DataFrame, by: list[str]) -> None:
    """Refuse to emit anything if any group has fewer than MIN_OK_N ok
    rows. This is a hard gate: printing partial stats for an underpowered
    group is worse than refusing outright."""
    short = summary[summary["n"] < MIN_OK_N]
    if short.empty:
        return
    print(f"refusing: {len(short)} group(s) have fewer than {MIN_OK_N} ok-outcome rows:", file=sys.stderr)
    for _, row in short.iterrows():
        key_desc = ", ".join(f"{col}={row[col]!r}" for col in by)
        print(f"  {key_desc}: n={int(row['n'])}", file=sys.stderr)
    raise SystemExit(2)


def fmt_ms(value: float) -> str:
    if pd.isna(value):
        return "-"
    return f"{value:.1f}"


def fmt_int(value: float) -> str:
    if pd.isna(value):
        return "-"
    return str(int(value))


def render_markdown_table(rows: pd.DataFrame, columns: list[str], headers: list[str]) -> str:
    lines = ["| " + " | ".join(headers) + " |", "| " + " | ".join("---" for _ in headers) + " |"]
    for _, row in rows.iterrows():
        cells = [str(row[c]) for c in columns]
        lines.append("| " + " | ".join(cells) + " |")
    return "\n".join(lines)


def emit_markdown(summary: pd.DataFrame, df: pd.DataFrame, by: list[str]) -> None:
    if "scenario" not in by:
        raise SystemExit("markdown output requires 'scenario' to be one of the --by columns")

    scenarios = sorted(df["scenario"].unique())
    blocks = []
    for scenario in scenarios:
        sc = summary[summary["scenario"] == scenario].copy()
        if sc.empty:
            continue

        if scenario == "t1" and "condition" in by:
            sc = sc.sort_values("condition")
            display = pd.DataFrame({
                "condition": sc["condition"],
                "n": sc["n"].map(fmt_int),
                "median (ms)": sc["median"].map(fmt_ms),
                "p95 (ms)": sc["p95"].map(fmt_ms),
                "min (ms)": sc["min"].map(fmt_int),
                "max (ms)": sc["max"].map(fmt_int),
                "timeout": sc["timeout"].map(fmt_int),
                "error": sc["error"].map(fmt_int),
            })
            headers = ["condition", "n", "median (ms)", "p95 (ms)", "min (ms)", "max (ms)", "timeout", "error"]
            table = render_markdown_table(display, headers, headers)
        elif scenario == "t2" and "condition" in by and "detail" in by:
            sc = sc.sort_values(["condition", "detail"])
            display = pd.DataFrame({
                "condition": sc["condition"],
                "detail": sc["detail"],
                "n": sc["n"].map(fmt_int),
                "median (ms)": sc["median"].map(fmt_ms),
                "p95 (ms)": sc["p95"].map(fmt_ms),
                "min (ms)": sc["min"].map(fmt_int),
                "max (ms)": sc["max"].map(fmt_int),
                "timeout": sc["timeout"].map(fmt_int),
                "error": sc["error"].map(fmt_int),
            })
            headers = ["condition", "detail", "n", "median (ms)", "p95 (ms)", "min (ms)", "max (ms)", "timeout", "error"]
            table = render_markdown_table(display, headers, headers)
        else:
            # generic fallback: whatever --by grouping was requested, minus
            # 'scenario' itself since it is fixed by the heading above.
            other_cols = [c for c in by if c != "scenario"]
            sc = sc.sort_values(other_cols) if other_cols else sc
            data = {}
            for c in other_cols:
                data[c] = sc[c]
            data["n"] = sc["n"].map(fmt_int)
            data["median (ms)"] = sc["median"].map(fmt_ms)
            data["p95 (ms)"] = sc["p95"].map(fmt_ms)
            data["min (ms)"] = sc["min"].map(fmt_int)
            data["max (ms)"] = sc["max"].map(fmt_int)
            data["timeout"] = sc["timeout"].map(fmt_int)
            data["error"] = sc["error"].map(fmt_int)
            display = pd.DataFrame(data)
            headers = other_cols + ["n", "median (ms)", "p95 (ms)", "min (ms)", "max (ms)", "timeout", "error"]
            table = render_markdown_table(display, headers, headers)

        blocks.append(f"#### {scenario}\n\n{table}")

    print("\n\n".join(blocks))


def emit_csv(summary: pd.DataFrame) -> None:
    out = summary.copy()
    out["median"] = out["median"].round(1)
    out["p95"] = out["p95"].round(1)
    print(out.to_csv(index=False), end="")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("csv_path", help="path to a results.csv file")
    parser.add_argument(
        "--by",
        default=",".join(DEFAULT_BY),
        help=f"comma-separated column list to group by (default: {','.join(DEFAULT_BY)})",
    )
    emit_group = parser.add_mutually_exclusive_group()
    emit_group.add_argument(
        "--emit",
        choices=["markdown", "csv"],
        default="markdown",
        help="output format (default: markdown)",
    )
    args = parser.parse_args(argv)

    by = [c.strip() for c in args.by.split(",") if c.strip()]
    if not by:
        raise SystemExit("--by must name at least one column")

    df = load(args.csv_path)
    unknown = [c for c in by if c not in df.columns]
    if unknown:
        raise SystemExit(f"--by names unknown column(s): {', '.join(unknown)}")

    summary = summarise(df, by)
    enforce_min_n(summary, by)

    if args.emit == "markdown":
        emit_markdown(summary, df, by)
    else:
        emit_csv(summary)

    return 0


if __name__ == "__main__":
    sys.exit(main())
