# analysis

`stats.py` turns a harness `results.csv` (columns: `scenario,condition,run,
t_start,t_end,duration_ms,outcome,detail`) into per-group latency summaries.

Only `outcome == "ok"` rows contribute to the duration statistics (median,
p95, min, max) — timeout/error rows are not valid latency samples. Their
counts are still reported per group (as `timeout`/`error` columns) so
failures are never silently dropped from the report.

Any group with fewer than 10 ok-outcome rows is refused outright: the tool
exits 2 and prints the offending group(s) and their actual `n` to stderr,
rather than printing stats for an underpowered sample.

## Install

```
pip install -r analysis/requirements.txt
```

## Usage

```
python3 analysis/stats.py results.csv --by scenario,condition,detail --emit markdown
python3 analysis/stats.py results.csv --by scenario,condition,detail --emit csv
```

`--by` defaults to `scenario,condition,detail` if omitted. `--emit` defaults
to `markdown` if omitted.

Markdown output prints one `#### <scenario>` heading and table per scenario
present in the input:

- `scale`: one row per `condition` (columns: condition, n, median (ms),
  p95 (ms), min (ms), max (ms), timeout, error).
- `drift`: one row per `condition, detail` pair, since `detail` (detect/repair)
  is a meaningful sub-grouping for drift (same columns as scale plus `detail`).
- any other scenario present in the data (fault, race, latency, verify,
  refConverge, refDrift): a generic fallback table using whatever `--by`
  grouping was requested, so the tool never crashes or drops data for a
  scenario without a hard-coded shape.

Markdown output requires `scenario` to be one of the `--by` columns (the
default satisfies this); the tool needs it to know which heading/table
shape each row belongs to.

CSV output (`--emit csv`) prints the raw group-level summary — the `--by`
key columns plus `n,median,p95,min,max,timeout,error` — as CSV to stdout.

Note: no companion table-shape spec document exists for this project; the
scale/drift markdown layouts above were designed from the CSV schema and
the stated requirements, not matched against an external spec.

## Tests

```
make check-analysis
```

runs `analysis/check_golden.py`, which regenerates markdown from
`analysis/testdata/fixture.csv` and compares it byte-for-byte against
`analysis/testdata/golden.md`, and separately checks that
`analysis/testdata/underpowered.csv` is refused with exit code 2. This
assumes `pandas` is already installed (see Install above); the Makefile
target does not provision dependencies itself.
