# Tooling matrix and drop-in configs

AI-Guard does not reimplement linters or compilers. It picks the fastest tool that answers the question
of each tier, runs it on the smallest possible input, and contains it. This page lists what replaces
what, the exact command AI-Guard executes, and the ready-made config files in [`configs/`](../configs).

## What replaces what

Versions are the ones installed and executed on 2026-11-10.

| Stack | Layer | Legacy | Replacement | What AI-Guard runs |
|---|---|---|---|---|
| TS/Node | Lint + syntax (T0) | `eslint .` | Biome 2.6.6 or Oxlint 1.93.0 | `biome lint --reporter=github --diagnostic-level=error <delta>` · `oxlint -f unix --quiet <delta>` |
| | Syntax (T0), no fast linter | `tsc --noEmit` | esbuild 0.28.2, the one in `node_modules` | `esbuild --log-level=error --loader:.js=jsx --outbase=. --outdir=.aiguard/cache/esbuild-out <delta>` |
| | Types (T1) | `tsc --noEmit` (JS) | native TypeScript 7.0.2, or `tsgo` (`@typescript/native-preview`) next to TS 5/6: the project's, or the one from `ai-guard tools install` | `tsc -p tsconfig.fast-check.json --noEmit`; without that file: `-p tsconfig.json --noEmit --incremental --tsBuildInfoFile .aiguard/cache/... --skipLibCheck` |
| | Tests (T1) | whole suite | selection by import graph, unit tests only | `vitest related --run --maxWorkers=N [--project unit] <delta>` · `jest --findRelatedTests <delta>` |
| | Typed lint (T1, opt-in) | `eslint .` | the project's ESLint, on the delta only | `eslint --format json --no-warn-ignored [--max-warnings N] <delta>` with `--enable ts.eslint` |
| | Transpile | `tsc` emitting | SWC 1.16 / esbuild 0.28 (no type-check) | outside the check: `configs/ts/package.scripts.json` |
| | Typed lint | typescript-eslint | `oxlint --type-aware` (oxlint-tsgolint 7) | optional step in `configs/aiguard.example.json` (not executed here) |
| Python | Environment | pip/venv | uv 0.11 | AI-Guard calls `.venv` directly; by hand: `uv run --no-sync` |
| | Lint + syntax (T0) | flake8 + black | Ruff 0.17.4 | `ruff check --output-format=concise --no-fix --force-exclude <delta>` |
| | Types (T1) | cold `mypy` | `dmypy` (mypy 2.4.1) when there is a mypy config; otherwise ty 0.0.100; pyright 1.1.414 per file | `dmypy --status-file ... run --timeout 900 -- ...` · `ty check --output-format concise` · `pyright --outputjson <delta>` |
| | Tests (T1) | full `pytest` | pytest-testmon 2.3.0 | `pytest -q --tb=line -rfE --testmon` (`TESTMON_DATAFILE` inside `.aiguard/`) |
| Go | Syntax (T0) | `go build` | parser only | `gofmt -l -e <delta>` |
| | Semantics (T1) | `go build ./...` | type-check with no object file and no link | `go vet <changed packages + direct importers>` |
| | Tests (T1) | `go test ./...` | the same packages, capped | `go test -json -vet=off <...>`; more than `go.max_test_packages` (16) is deferred |
| Rust | Syntax (T0) | `cargo build` | parser only | `rustfmt --check --edition <ed> <delta>` (only parse errors count) |
| | Semantics (T1) | `cargo build` | no codegen, no link | `cargo check --message-format=short` |
| | Tests (T1) | `cargo test` | the touched crate only | `cargo test -p <crate> --lib --bins` |
| | Cache / link | — | sccache 0.16, LLD (default on Linux), `rust-lld.exe` on MSVC | `configs/rust/cargo-config.toml` |
| C/C++ | Syntax + semantics (T0) | compile | front end only | the command from `compile_commands.json` without `-o/-c/-MD` and without the launcher, plus `-fsyntax-only` (`/Zs` on MSVC) |
| | Cache / link | — | sccache + lld/mold, link pool | `configs/cpp/CMakePresets.json` |

### Start-up cost of each tool

Measured on a minimal project in a Linux sandbox with 2 vCPUs:

| Tool | Time | Peak RSS |
|---|---:|---:|
| Biome, native binary | 16 ms | 35 MB |
| Biome, through the Node shim | 92 ms | — |
| tsc 7 (native) | 80 ms | 42 MB |
| tsc 6 (JavaScript) | ~500 ms | 129 MB |
| Ruff | 7 ms | 14 MB |
| ty | 24 ms | 27 MB |
| mypy | 130-150 ms | ~55 MB |
| `dmypy`, warm | 99 ms | — |
| pyright | 787 ms | 126 MB |
| gofmt | 2 ms | — |
| `go vet` | 105 ms | 22 MB |
| rustfmt | 91 ms | 36 MB |
| `cargo check` | ~100 ms | — |
| `clang -fsyntax-only` with `<vector>` | 136 ms | 86 MB |

### Notes that change decisions

- TypeScript 7 (the native compiler) has been GA since July 2026 and `typescript@latest` is already
  7.0.2, but typescript-eslint 8.72 declares `typescript <6.1.0`. If you depend on its typed rules, on
  ts-jest or on the TypeScript API, keep `typescript@6` and install `@typescript/native-preview` next to
  it: AI-Guard uses `tsgo` for the check and everything else stays the same.
- `pyright` has no command-line daemon (`--watch` is meant for a terminal). It runs per file with the V8
  heap bounded. The truly persistent one is `dmypy`; ty does not need one.
- `go build ./...` links `main` packages (verified with `-x`). `go vet` neither compiles nor links the
  package it inspects.
- sccache does not cache incremental compilations. Locally it helps with dependencies; in tier 2 it is
  used with `CARGO_INCREMENTAL=0`.

## Drop-in configs

Every file in `configs/` was run with the tool it configures. The last column says how.

| File | Goes to | What it does | Verification |
|---|---|---|---|
| `ts/biome.json` | package root | per-file lint: the `project` and `types` domains are off (no repo indexing), VCS ignore, formatter kept separate | `biome lint` 2.6.6, no warnings |
| `ts/.oxlintrc.json` | root | alternative to Biome: `correctness` as error | `oxlint -c` 1.93.0 |
| `ts/tsconfig.fast-check.json` | next to `tsconfig.json` | `noEmit`, `incremental` + `tsBuildInfoFile` in `.aiguard/cache`, `skipLibCheck`, `isolatedModules` | exit 0 and tsbuildinfo created with tsc 6.0.3, tsc 7.0.2 and tsgo, over a base with `composite` |
| `ts/.swcrc`, `ts/package.scripts.json` | root / `package.json` | transpile separated from the check | `swc` and `esbuild` executed |
| `python/pyproject.toml` | sections to merge | uv (`dev` group), Ruff, pytest, mypy with SQLite cache, ty, pyright | `uv sync`, `ruff check/format`, `pytest --strict-config`, `pytest --testmon`, `mypy`, `dmypy`, `ty`, `pyright`: no config errors |
| `rust/cargo-config.toml` | `.cargo/config.toml` | `jobs`, sccache, `debug = "line-tables-only"`, no debug info in dependencies, linker | `cargo build` without warnings; sccache: 1 hit after `cargo clean`; LLD confirmed in the binary. mold and `rust-lld.exe` not executed |
| `go/go.env` | `go env -w` / shell | `GOFLAGS=-p=4`, `GOCACHE` location, commands that produce no binary | commands verified; the value of `-p` is a choice |
| `cpp/CMakePresets.json` | root | Ninja, `compile_commands.json`, sccache, lld, pool of 2 links | `cmake --preset` + build + `ai-guard` on the result |
| `aiguard.example.json` | `aiguard.json` | every control, with example overrides | loaded with `ai-guard doctor --config` |
| `tier2/ai-guard-tier2.yml` | `.github/workflows/` | tier-2 workflow | `actionlint` 1.7.12; **not executed on GitHub** |
| `tier2/remote-worker.sh` | the repo's `ci/` | worker for a machine reached over SSH | executed end to end against a local git remote |
| `agents/AGENTS.snippet.md` | `AGENTS.md` / `CLAUDE.md` | instructions for the agent | — |
