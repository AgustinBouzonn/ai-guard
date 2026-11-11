# Benchmarks

Two measurements back the claims in the README: a real private monorepo, and a synthetic multi-language
fixture anyone can regenerate. Both are reported with the conditions they were taken under, including
the cases where AI-Guard does not win.

- [Protocol](#protocol)
- [Real project: the API of a pnpm monorepo](#real-project-the-api-of-a-pnpm-monorepo)
- [Synthetic fixture](#synthetic-fixture)
- [Reproducing](#reproducing)

## Protocol

1. `ai-guard bench init` writes `aiguard.bench.json`: three patches per language (valid, syntax error,
   type error) on a medium-sized file, and, as the baseline, the legacy commands it detected. **Edit the
   baseline until it is what you actually run today**, and add patches on your hot files.
2. `ai-guard bench run [--reps N] [--warmup N] [--only ts,py]`. For every patch and repetition it
   applies the patch (with an identifier unique to the run, so nothing comes out of a cache from the
   previous run), measures one pipeline, restores the file, and repeats with the other pipeline. The
   order alternates.
3. Metrics per run: wall-clock until the verdict, peak RSS of the process tree (plus persistent daemons
   in AI-Guard's case), and CPU-seconds / (wall x cores). Medians are reported; the first run, with
   cold caches, goes in a separate table.
4. Output: a Markdown table on stdout, `.aiguard/bench/report.md`, and `results.json` with every sample.
5. The original file is saved and written to a journal before it is touched. If the process dies,
   `ai-guard bench restore` (or the next run) puts it back.

## Real project: the API of a pnpm monorepo

Measured on 2026-11-10 with AI-Guard 0.2.0.

**Subject.** The API package (`apps/api`) of a private monorepo: pnpm 9 + turbo, TypeScript 5.9 (the
JavaScript compiler), ESLint 9 with typed rules, Vitest 5 with two projects (`unit`, 37 files; `int`,
against a real PostgreSQL), and neither Biome nor Oxlint. The workspace packages export their source
code, so the API's TypeScript program includes all 16 packages. That is why its type-check weighs
2.5 GB.

**Conditions.** A Windows 11 laptop with 16 logical cores and 32 GB, **with its normal work running**
(other agents and dev servers), on a separate clone of the repository, editing one source file that 8
unit-test files import. AI-Guard ran without an `aiguard.json`, with `tsgo` installed by
`ai-guard tools install`.

**One run per cell, not medians.** The "quiet" and "loaded" columns are two moments of the same machine,
and the difference between them is its real variation. "Today" is what the repository runs:
`pnpm typecheck` (`tsc --noEmit`), `pnpm test:unit`, and `pnpm lint` (`eslint . --max-warnings 0`),
measured with `ai-guard run`.

| Scenario | Today, quiet machine | AI-Guard, quiet | Today, loaded machine | AI-Guard, loaded |
|---|---:|---:|---:|---:|
| Syntax error | — | **0.32 s** / 12 MB | 12.3 s / 646 MB | **0.57 s** / 12 MB |
| Type error | — | **3.2 s** / 1.3 GB | 43.8 s / 2.3 GB | **5.0 s** / 1.4 GB |
| Valid: types only (`--disable ts.test`) | 29.4 s / 2.5 GB | **3.2 s** / 1.3 GB | 40.5 s / 2.5 GB | — |
| Valid: types + unit tests | 52.9 s / 2.5 GB | **19.4 s** / 1.4 GB | 86.4 s / 2.5 GB | **34.4 s** / 1.4 GB |
| Valid: the above + lint | 105.4 s / 2.5 GB | **34.9 s** / ≤2.2 GB | 164.6 s / 2.5 GB | — |
| Unchanged tree | — | 0.20 s | — | 0.40 s |

Quiet: 4-45% CPU busy, 11-12 GB free. Loaded: 45-100%, 5-11 GB free. "Today" is the sum of the commands
run in sequence (types 29.4/40.5 s; `test:unit` 23.6/45.8 s; lint 52.5/78.2 s); its RAM is the peak of
the largest one. With lint, types and ESLint run at the same time: 2.2 GB is the sum of their peaks.
Cells marked "—" were not measured.

Cold, without `.aiguard/`, the valid change took 22.6 s with the final binary and 43-45 s in two
earlier series under heavier load: the first type-check with `tsgo` uses 2.3-2.4 GB, the same as `tsc`.

### Per step

Warm, with a real edit between runs, three series:

| Step | What runs | Time | Peak RSS |
|---|---|---:|---:|
| `ts.syntax` (T0) | esbuild on the changed file | 36-80 ms (175-290 ms the first time) | 13 MB |
| `ts.types@apps/api` (T1) | `tsgo -p tsconfig.json --noEmit --incremental` | 2.6-4.2 s (cold: 5.6-16.4 s) | 1.3-1.4 GB (cold: 2.3-2.4 GB) |
| `ts.test@apps/api` (T1) | `vitest related --project unit`, 8 files | 16.0-20.7 s | 1.1-1.3 GB |
| `ts.eslint@apps/api` (T1, opt-in) | ESLint with types on 1 file | 18.0-30.1 s | 0.9-1.0 GB |

### Before and after 0.2.0

With the same valid change, 0.1.0 ended in `DEFERRED` after 11 s
(`ts.types@apps/api: rss 515MB > 512MB budget`) and had no tier 0
(`ts.lint skipped: no linter found`). 0.2.0 returns `OK`. In that 0.1.0 run the tests were disabled on
purpose: without the `--project unit` filter the integration tests would have started.

### How to read it

- The large gain is where an agent's loop spends most of its time: on broken changes. Syntax errors go
  from 12.3 s to 0.3-0.6 s, and type errors from 44 s to 3-5 s. On the valid path the gain is 2.5-3x.
- The 3 s target of tier 1 is borderline for types (2.6-4.2 s warm) and far off for tests: the affected
  ones take 16-21 s and are reported as `over_budget`. According to Vitest itself, 73-81% of that time
  is importing modules, not running tests. The full `pnpm test:unit` takes a similar time (23.6 s)
  because it uses all 16 threads (153 s of CPU, 2.5 GB); the selection spends about 40 s of CPU and
  1.1-1.3 GB.
- Peak RAM drops about 45% when warm (1.3-1.4 GB against 2.5 GB). Cold, it does not drop.
- Fixed overhead of AI-Guard in this repository: 0.2 s, which is the `git status`. It was about 2 s
  before it stopped searching the PATH for tools that are not there.
- ESLint with types builds the whole TypeScript program even when it is given a single file: 18-30 s.
  That is why it is opt-in and not part of the default loop.

### Options measured for the test step

One run each, same change (`ts.test_args` in `aiguard.json`):

| Vitest flags | Time | Peak RSS | CPU |
|---|---:|---:|---:|
| `--maxWorkers=2` (what 0.1.0 did) | 28.1 s | 1020 MB | 39.9 s |
| `--maxWorkers=4` (what 0.2.0 picks when cores are free) | 16.0 s | 1333 MB | 43.8 s |
| `--maxWorkers=2 --pool=threads` | 22.1 s | 1048 MB | 39.2 s |
| `--maxWorkers=2 --pool=threads --no-isolate` | 14.4 s | 1115 MB | 24.0 s |

`--no-isolate` shares modules between test files. It passed on these 8 files, but it can break tests
that rely on isolation (mocks, module state). It is not the default.

### On a machine that is out of RAM

The same repository on the same laptop, with 3 GB free of 32 and the CPU at 100% (2026-11-10). A type
error was injected. Tier 1 could not be admitted, so only the syntax was validated:

| Version | Result | Time until the verdict |
|---|---|---:|
| 0.2.0 | `DEFERRED`: `ts.types` and `ts.test`, "no free RAM for its 2005MB budget" | 30.1 s |

0.2.0 waited `queue_wait_ms` (15 s) once for the type-check and again for the tests. From 0.3.0 the
allowance is spent once per run, so the same situation ends after the first 15 s. That is covered by
a test with a simulated machine. **It was not re-measured on the laptop**, which had 8 GB free by the
time 0.3.0 was built.

With 8 GB free the injected type error is caught: 21.2 s cold (the first type-check of that worktree,
peak 2.5 GB) and 3.7 s warm (peak 0.5 GB).

### What this does not solve

- **The integration suite** (`test:int`, 135 files run serially against PostgreSQL). In the
  repository's own log from the early hours of 2026-11-10, the run took 38.8 minutes and only 72 of the
  135 files ran: the other 63 workers died at start-up (`Worker exited unexpectedly with exit code
  3221225794`, 0xC0000142). At the per-file cost of the ones that did run (about 22 s of tests plus
  about 10 s of imports) the full suite would be around 70 minutes; that was not measured. It does not
  run in tier 1 by design. Without CI, the way to take it out of the loop is tier 2 on the same machine,
  in the background and contained (see the example in [usage.md](usage.md#tier-2)). That mechanism was
  tried in the repository with `pnpm test:unit` in place of `test:int` (37 files, 283 tests,
  `done exit=0` after 24 s, peak 2.3 GB). **It was not executed with `test:int`**, which needs Docker.
  It runs on the working tree as it is: if editing continues meanwhile, it sees the changes. The time it
  takes belongs to the suite, not to AI-Guard: a new process per file, files in series, and, according
  to Vitest, 35% of the time importing modules.
- **Dependents inside the workspace.** A change in `packages/x` checks `packages/x`, not the apps that
  import it. To check those, include one of their files:
  `ai-guard check --files apps/api/src/server.ts`.
- **TypeScript 7 semantics on a 5.9 project.** `tsgo` reported 0 errors on the clean tree and reported
  the injected error exactly like `tsc` (TS2322, same line). That is an observation, not a proof of
  equivalence: the canonical check is still `pnpm typecheck` before pushing.

## Synthetic fixture

Measured with AI-Guard **0.1.0** (fixed 512 MB cap, 30 s timeout). These tables were not repeated with
0.2.0.

**Subject.** A fixture generated with `gen_fixture.py --ts 1200 --py 900 --go 120 --rs 120` (3,025
files), in a Linux sandbox with **2 vCPUs** and 8 GB. Two measured runs per patch after one warm-up.

**Baseline.** ESLint 10 + `tsc --noEmit` (TS 6.0.3) + `vitest run` · flake8 + black + `pytest` ·
`go build` + `go test ./...` · `cargo build` + `cargo test`. AI-Guard used its defaults.

| Patch | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB | RAM saved | Verdict (baseline / AI-Guard) |
|---|---:|---:|---:|---:|---:|---:|:---:|
| ts-valid | 38703 | 1333 | 29.0x | 426 | 141 | 66.9% | pass / pass |
| ts-syntax | 4690 | 19 | 253.5x | 433 | 36 | 91.8% | FAIL / FAIL |
| ts-type | 7107 | 339 | 21.0x | 503 | 116 | 76.9% | FAIL / FAIL |
| ts-valid-shared | 38590 | 17263 | 2.2x | 432 | 195 | 55.0% | pass / pass |
| py-valid | 3012 | 550 | 5.5x | 74 | 62 | 15.6% | pass / pass |
| py-syntax | 1787 | 10 | 188.1x | 55 | 13 | 76.1% | FAIL / FAIL |
| py-type | 3266 | 261 | 12.5x | 74 | 62 | 16.2% | pass / FAIL |
| py-valid-shared | 2997 | 2065 | 1.5x | 73 | 65 | 11.0% | pass / pass |
| go-valid | 1509 | 485 | 3.1x | 116 | 110 | 5.6% | pass / pass |
| go-syntax | 142 | 5 | 28.3x | 22 | 9 | 59.1% | FAIL / FAIL |
| go-type | 146 | 284 | 0.5x | 23 | 31 | -34.8% | FAIL / FAIL |
| go-valid-shared | 11236 | 1867 | 6.0x | 123 | 46 | 62.4% | pass / defer |
| rs-valid | 625 | 592 | 1.1x | 136 | 141 | -3.3% | pass / pass |
| rs-syntax | 84 | 50 | 1.7x | 53 | 34 | 35.8% | FAIL / FAIL |
| rs-type | 112 | 199 | 0.6x | 107 | 107 | 0.5% | FAIL / FAIL |
| rs-valid-shared | 639 | 643 | 1.0x | 138 | 142 | -3.3% | pass / pass |
| **TS** (geometric mean) | 89090 | 18953 | **24.2x** | 503 | 195 | **61.3%** | |
| **Python** | 11061 | 2885 | **11.7x** | 74 | 65 | **12.2%** | |
| **Go** | 13032 | 2641 | **4.1x** | 123 | 110 | **10.6%** | |
| **Rust** | 1459 | 1483 | **1.0x** | 138 | 142 | **-3.3%** | |
| **All** | 114640 | 25961 | **5.8x** | 503 | 195 | **61.3%** | |

Group rows add the patch medians; their speedup is the geometric mean of the patch speedups, and their
RSS is the highest peak. `*-shared` is a valid edit to the module that everything else imports.

The full reports, including CPU and the cold first run, are committed as generated:

- [`bench/results/fixture-baseline-with-tests.md`](../bench/results/fixture-baseline-with-tests.md): the
  table above.
- [`bench/results/fixture-baseline-literal.md`](../bench/results/fixture-baseline-literal.md): the same
  fixture against a baseline without tests.

### How to read it

- Where the legacy pipeline is whole-repo and non-incremental (ESLint + tsc + suite; flake8 + black +
  pytest) the difference is large: 5.5x and 29x on localised valid edits (1.5x and 2.2x when the whole
  repository imports the file), 12x to 253x when the edit is broken (it stops earlier), and in
  TypeScript peak RAM drops between 55% and 92%.
- Where the legacy tool is already incremental (Go, Rust), this fixture shows no gain in Rust, and in
  Go only when the baseline runs tests. On Go and Rust type errors AI-Guard is *slower* (0.5-0.6x,
  about 100-150 ms more): it pays for tier 0 and `go list` before reaching the same error.
- **Against a baseline without tests** in TS/Go/Rust (`eslint` + `tsc`, `go build`, `cargo build`),
  AI-Guard does more work than the baseline, because it also runs the affected tests: TS 10.1x, Python
  11.5x, Go 1.0x, Rust 0.6x geometric mean. Adding up all 16 patches AI-Guard still takes less (37.2 s
  against the baseline's 41.6 s), but in absolute time it loses in Go (12.4 s against 1.6 s) and Rust
  (1.4 s against 0.6 s), and its peak RAM is higher there (126 MB against 72 MB in Go). That run used
  three measured repetitions per patch.
- Hot file in TypeScript: `vitest related` selects all 240 test files and takes 17 s on 2 vCPUs, far
  above the 3 s target. To bound it, `"overrides": {"ts.test": {"timeout_ms": 8000}}` cuts it and defers
  it to tier 2. In Go the 16-package cap already defers it (`pass / defer`): that row went from 11.6 s
  to 1.9 s when the cap was added, after seeing the first run.
- `py-type`: the baseline has no type-checker, so it accepts the patch. AI-Guard rejects it.
- In Python the baseline was already light (74 MB), so the RAM saving is small. The 3 s budget of
  tier 1 is exceeded when four languages change at once on 2 vCPUs (5.4 s cold), and the 300 ms budget
  of tier 0 with C++ that includes heavy headers (484 ms). Both cases are flagged `over_budget`.

## Reproducing

On the synthetic fixture, without touching a real repository:

```
python bench/gen_fixture.py DEST --ts 1200 --py 900 --go 120 --rs 120
cd DEST && npm install && uv sync
ai-guard bench init      # then edit the baseline in aiguard.bench.json
ai-guard bench run --reps 2 --warmup 1
```

On your own repository: `ai-guard bench init`, edit the baseline until it matches what you run today,
then `ai-guard bench run`. The report lands in `.aiguard/bench/report.md`.
