# Contributing

## Build and test

```sh
make build   # dist/swarmgate, dist/harness
make check   # gofmt + go vet + go test ./... — what CI runs
```

`make check` must pass before a PR is merged; it's also the first CI job
(`.github/workflows/ci.yml`), run on both a standard Ubuntu container and
an Alpine (musl) one.

Other Makefile targets (`check-analysis`, `test-integration`) need
dependencies or infrastructure beyond `make check` — see their own
comments in the `Makefile` and `analysis/README.md` / `pipeline/README.md`
before running them.

## Branches and pull requests

Branch off `main`, open a PR against `main`. CI must be green before merge.
There's no separate release branch; tags on `main` drive `make release`.

## Commit style

Short, imperative, descriptive commit subjects — no conventional-commit
prefixes (`feat:`, `fix:`, etc.) and no trailers. Look at `git log` for the
tone to match.

## Configuration reference

`swarmgate.example.yaml` at the repo root is the canonical config schema —
if it and `README.md` ever disagree, the example file is right. Changing a
config field means updating that file (and its `SWARMGATE_*` env override
in `internal/config/config.go`) alongside the code.
