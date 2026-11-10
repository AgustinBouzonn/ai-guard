# Limits, and what was not verified

This page lists what was not run or not measured, and the known limits of the design. Read it next to
the numbers in [benchmarks.md](benchmarks.md).

## Measurements

- **Real project.** One package (`apps/api`) of one private monorepo was measured, with edits to a
  single file, one run per case, and the machine in use. There are no medians, and neither the web app
  nor the other packages were measured.
- **Synthetic fixture.** Its tables are from 0.1.0; they were not repeated with the new defaults of
  0.2.0.
- The CPU of persistent daemons is not included in the CPU column of the benchmark.

## Platforms

- **Real checks on Windows.** Run end to end: esbuild, `tsgo`, Vitest and ESLint, on the clone of the
  monorepo. Biome, Oxlint, Ruff, mypy, Go, Rust and C++ checks were executed on Linux only.
- **WSL2.** Not executed there; it is the same Linux binary that was tested on Ubuntu 24.04.
- **cgroup v2.** The hard cap through cgroup v2 is implemented, but only the v1 path was exercised (the
  sandbox is v1). The watchdog does not depend on it.
- **macOS.** It compiles. There is no RSS watchdog and no RAM admission.
- **CI.** `.github/workflows/ci.yml` runs `go vet` and the test suite on GitHub's Ubuntu and Windows
  runners and cross-builds the other targets. It does not run `-race`, and nothing is executed on
  macOS (see [architecture.md](architecture.md#tests)).

## Budgets

- **Automatic budget.** The cap (RAM/8) and the learned reservation were exercised on a 32 GB machine
  (cap 4011 MB) and in the 8 GB sandbox (1024 MB). On a real 8 GB machine, a type-check that needs
  2.3 GB cold is killed and deferred: there you need `overrides` or tier 2.
- **`ai-guard doctor`** takes about 10 s in a repository with 3,000 source files on Windows, because it
  plans all of them.

## Tier 2

- **On GitHub.** The workflow template passes `actionlint` but was not executed, and the push of
  `refs/aiguard/*` was tested against a local git remote, not against GitHub. `rsync` was tested with a
  substitute binary.

## Tools

- **Not executed:** mold, `rust-lld.exe` on MSVC, MSVC's `/Zs` (only the argument rewriting),
  `oxlint --type-aware`, and the jest and pyright paths end to end (their parsers come from captured
  real output).
- **TypeScript project references.** A package that references another needs the referenced package's
  `.d.ts` files already built (`tsc -b` once); otherwise tsc reports TS6305.
- **testmon** runs the whole suite the first time, to build its database.
- **Workspace dependents.** A change in `packages/x` checks `packages/x`, not the apps that import it.
- **`tsgo` on an older TypeScript project.** It matched `tsc` on the cases tried, which is an
  observation and not a proof of equivalence. Keep the project's own type-check before pushing.
