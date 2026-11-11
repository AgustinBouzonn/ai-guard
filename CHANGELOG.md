# Changelog

## 0.3.0 - 2026-11-10

Changes made after wiring 0.2.0 into a team repository and watching it on a machine that was out of
RAM.

### Added

- **`ai-guard hook`**: a Claude Code `Stop` / `SubagentStop` hook. When a turn ends it checks the git
  delta and, if the code has errors, blocks the turn and hands the report to the agent. Deferred checks
  and tools that could not run are reported to the person and do not block. A delta that already passed
  is not checked again, and a subagent is only checked in a working tree of its own. Settings snippet in
  `configs/agents/claude-hook.settings.json`.

### Changed

- **The wait for RAM is spent once per run, not once per phase.** On a machine with no RAM to spare,
  0.2.0 waited `queue_wait_ms` for the type-check and then again for the tests: 30 s to reach
  `DEFERRED`. Now the second phase gets a single attempt.

## 0.2.0 - 2026-11-10

Changes made after measuring 0.1.0 on a real monorepo. With the 0.1.0 defaults, the type-check of a
real API died at 515 MB (fixed 512 MB cap) and the project had no fast linter, so AI-Guard returned
nothing useful. Without configuration, 0.2.0 returns a syntax error in 0.3-0.6 s, a type error in 3-5 s,
and a valid change with its unit tests in 19-34 s depending on machine load. See
[docs/benchmarks.md](docs/benchmarks.md).

### Added

- **Tier 0 without a fast linter.** If the project has neither Biome nor Oxlint, `ts.syntax` parses the
  delta with the esbuild already in `node_modules` (Vite, Vitest and tsx bring it): 40-80 ms, 13 MB.
- **Native compiler even when the project uses the JavaScript one.** `ai-guard tools install` puts
  `tsgo` (`@typescript/native-preview`) in a directory of its own and `ts.types` uses it to check the
  project's `tsconfig`. `"ts": {"types": "tsc"}` goes back to the project's compiler.
- **The project's ESLint on the delta, opt-in:** `ai-guard check --enable ts.eslint`.
- `--enable` / `--disable <step>` per run.
- `ai-guard run`, for what has to run in full, contained.

### Changed

- **The RAM cap is no longer a fixed number.** `tier1.mem_mb: 0` (the default): cap per step = RAM/8
  (between 1 and 4 GB), and an admission reservation learned from previous runs
  (`.aiguard/cache/stats.json`). An explicit value is still a strict cap.
- **Tests: unit tests only.** If the package has a `test:unit` script, its `--project` / `--config`
  flags are honoured; integration tests (database, containers) do not run in tier 1. Runner workers =
  the cores the step may use, up to 4.
- The Go module path is `github.com/AgustinBouzonn/ai-guard`.

### Fixed

Each with its test.

- The tier-2 snapshot lost an edit made in the same second as the last commit.
- The tier-2 worker ignored `--config` and fell back to the defaults (push to `origin`).
- `--tier2` with no tier 2 configured made that push instead of refusing.
- `git status` rewrote `.git/index`; it now runs with `--no-optional-locks`.
- Every plan searched the PATH for missing tools: 570 ms per plan on Windows, three plans per run.

## 0.1.0 - 2026-11-10

First version: tiered `check` for TypeScript, Python, Go, Rust and C/C++, process containment (RSS
watchdog, OS hard cap, timeout over the whole tree), tier-2 offloading through a git snapshot or rsync,
and the `bench` command. Fixed 512 MB cap and 30 s timeout in tier 1.
