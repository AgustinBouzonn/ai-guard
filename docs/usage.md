# Usage

## Commands

```
ai-guard [check] [flags] [files...]   validate changed files (default command)
ai-guard run [flags] -- cmd args...   run any command inside a memory/time budget
ai-guard hook [check flags]           Claude Code Stop/SubagentStop hook: check the delta when
                                      a turn ends and hand the errors back to the agent
ai-guard doctor                       show machine limits and the tools that would run
ai-guard tools [install] [--dir DIR]  show or install the tools ai-guard can bring itself
                                      (the native TypeScript compiler, via npm)
ai-guard selftest                     verify process containment on this machine
ai-guard init                         write aiguard.json with the defaults
ai-guard tier2 run|list|status|wait   queue and follow remote jobs
ai-guard bench init|run|restore       measure against a legacy pipeline
ai-guard version
```

### `check`

Reads the delta from git (staged + unstaged + untracked), runs tier 0, then tier 1, and applies the
tier-2 policy.

| Flag | Effect |
|---|---|
| `--staged` | only staged files |
| `--since REF` | everything changed since REF (branch point), plus untracked files |
| `--files a,b` | explicit files (no git needed); also accepted as plain arguments |
| `--files-from FILE` | newline-separated list; `-` reads stdin |
| `--tier N` | highest local tier to run: `0` or `1` (default `1`) |
| `--enable ID` | also run an opt-in step for this run, e.g. `ts.eslint` |
| `--disable ID` | skip a step for this run, e.g. `ts.test` |
| `--tier2` / `--no-tier2` | queue the remote tier after a local pass / never queue it |
| `--format F` | `agent` (default) or `json` |
| `--warnings` | include warnings |
| `--max-errors N` | cap on reported diagnostics |
| `--config FILE` | config file (default `<root>/aiguard.json`) |
| `--root DIR` | repository root (default: found from the current directory) |

### `run`

Runs any command contained: RAM cap, time cap, low priority, and the whole process tree dies together.
Use it for what has to run in full, such as an integration suite or a production build.

```
ai-guard run --mem 6000 --timeout 3600 -- pnpm test:int
ai-guard run --mem 2000 --dir apps/api --shell "pnpm typecheck && pnpm lint"
```

| Flag | Effect |
|---|---|
| `--mem MB` | kill the whole process tree past this much RAM (`0` = no limit) |
| `--timeout SEC` | kill it after this long (`0` = no limit) |
| `--normal-priority` | do not lower the CPU priority |
| `--shell "LINE"` | run a shell command line instead of an argv |
| `--dir DIR` | working directory |

The exit status is the command's own; `137` when it was killed for memory, `124` for time.

### `hook`

A [Claude Code hook](https://code.claude.com/docs/en/hooks) for the `Stop` and `SubagentStop` events. It
reads the event from stdin, runs `check` on the git delta of the event's directory, and answers the
way Claude Code expects:

| `check` result | What the hook does |
|---|---|
| `OK` | exits `0` and remembers the delta |
| `FAIL` (errors in the code) | exits `2` with the report on stderr: Claude Code blocks the end of the turn and gives the report to the model |
| `DEFERRED` | exits `0` and prints a `systemMessage` naming the checks that were not validated |
| a tool could not run, or bad config | exits `0` and prints a `systemMessage`: it is not an error in the change |

It is built not to get in the way:

- **A delta that already passed is not checked again.** The hook stores a fingerprint of the
  uncommitted state in `.aiguard/hook-last-ok`. A turn that edits nothing costs a `git status`.
- **A clean tree, or a directory outside a repository, is a silent exit `0`.**
- **A subagent is only checked in a working tree of its own** (an isolated worktree). If it shares the
  session's tree, that tree is checked by the session's own `Stop` hook; blocking the subagent would
  set it to fixing work in progress that is not its own.
- **`AI_GUARD_HOOK=off`** switches it off.

Any `check` flag can follow: `ai-guard hook --disable ts.test` validates types but leaves the tests to
an explicit `ai-guard check`.

To install it, merge [`configs/agents/claude-hook.settings.json`](../configs/agents/claude-hook.settings.json)
into the project's `.claude/settings.json`. Each event gets this entry:

```json
{ "type": "command", "command": "ai-guard", "args": ["hook"], "timeout": 180 }
```

Claude Code ends the loop by itself after eight blocks in a row. If some people on the team do not
have the binary, Claude Code reports a hook that could not start on every turn and carries on; wrap
the call in a script that checks for the binary first if that noise matters.

To try it by hand: `echo '{"hook_event_name":"Stop"}' | ai-guard hook; echo $?`

### `doctor`, `selftest`, `tools`

- `ai-guard doctor` prints the RAM and CPU the scheduler sees, the budgets it derived from them, and the
  tool that would run for each group of files in this repository.
- `ai-guard selftest` proves on this machine that an over-budget process is killed: memory watchdog,
  timeout over a whole tree, and the OS hard cap.
- `ai-guard tools install` downloads the native TypeScript compiler (`@typescript/native-preview`) into
  a directory of its own. It never touches the project's `node_modules`. The directory is
  `AIGUARD_TOOLS` if set, otherwise `tools/` next to the binary, otherwise the user cache.

### `tier2`

`ai-guard tier2 run` queues a job by hand; `list` shows the last ten jobs; `status [id]` prints one
(the latest by default) plus the output of `tier2.status_command`; `wait [id]` blocks until the job
ends. `wait` exits `0` when the job succeeded, `1` when it failed, and `3` if it is still running after
`--timeout` seconds (default 1800).

### `bench`

See [benchmarks.md](benchmarks.md).

## Output

The `agent` format is what goes back into the model's prompt, so it is compact on purpose: one summary
line, then `file`, then `line:col rule message`.

```
FAIL t0 99ms files=1 peak=31MB errors=2
src/bad.ts
  2:3 noDebugger This is an unexpected use of the debugger statement.
  3:12 noDoubleEquals Using == may be unsafe if you are relying on type coercion.

FAIL t0 13ms t1 864ms files=1 peak=118MB errors=1
test/calc.test.ts
  5:21 add AssertionError: expected -1 to be 4 // Object.is equality

OK t0 17ms t1 611ms files=1 peak=97MB
```

`--format json` gives the same on one line with short keys (`f`, `l`, `c`, `r`, `m`) plus the detail of
each step.

## Exit status

| Code | Meaning |
|---:|---|
| `0` | pass |
| `1` | errors found in the code |
| `2` | usage or config error |
| `3` | a check could not complete (broken tool, timeout in tier 0) |
| `4` | nothing failed, but some checks were deferred to tier 2 |

## Wiring it into an agent

There are two ways, and they work together:

- **Instructions.** Paste [`configs/agents/AGENTS.snippet.md`](../configs/agents/AGENTS.snippet.md) into
  your `AGENTS.md` or `CLAUDE.md`. It tells the agent to run `ai-guard check` after every batch of
  edits, how to read each exit code, and not to run the project's full lint, type-check, build or test
  commands itself. This works with any agent, and depends on the agent following it.
- **A hook.** With Claude Code, [`ai-guard hook`](#hook) runs the check when a turn ends, whether or not
  the agent remembered to.

## Configuration

`aiguard.json` at the repository root is optional and only needs the keys that differ from the
defaults. Unknown keys are rejected, so a typo never silently becomes a default. `ai-guard init` writes
the file with every default; [`configs/aiguard.example.json`](../configs/aiguard.example.json) shows
each control with example overrides.

### Envelopes

| Key | Default | Meaning |
|---|---|---|
| `tier0.budget_ms` / `timeout_ms` / `mem_mb` | 300 / 10000 / 150 | target latency (reported when exceeded), per-step timeout, per-step RSS budget |
| `tier1.budget_ms` / `timeout_ms` / `mem_mb` | 3000 / 60000 / 0 | same; `mem_mb: 0` sizes the cap from the machine (RAM/8, between 1024 and 4096 MB) and learns the reservation from past runs |
| `max_workers` | 0 | concurrent steps; `0` = one per idle core, capped |
| `min_free_mb` | 0 | RAM to leave untouched; `0` scales with installed RAM |
| `queue_wait_ms` | 15000 | how long a step may wait for RAM before it is deferred |
| `hard_cap_factor` | 1.5 | OS cap = `mem_mb` x factor; `0` disables the OS cap |
| `low_priority` | true | run tier 1 below normal priority |
| `rlimit_as_mb` | 0 | Linux: `RLIMIT_AS` for tier-1 steps; `0` = off |
| `wrap` | — | prefix for tier-1 commands, e.g. `systemd-run --user --scope -p MemoryMax={mem_mb}M --` |

A tier-0 step that exceeds its timeout is an error; a tier-1 step that does is deferred.

### Steps

| Key | Meaning |
|---|---|
| `overrides` | change the envelope of one step: `"ts.types": {"mem_mb": 1536}` |
| `enable` | opt-in steps by id, e.g. `ts.eslint` |
| `disable` | steps to skip by id, e.g. `ts.test` |
| `steps` | your own steps: any command with `{files}` and a parser (see below) |

A custom step has `id`, `tier`, `phase` (tier 1 only: `0` runs with static analysis, `1` with tests),
`langs`, `cmd` or `shell`, `dir`, `parser`, `mem_mb` and `timeout_ms`. Available parsers: `biome`,
`oxlint`, `esbuild`, `eslint`, `tsc`, `ruff`, `gcc`, `go`, `gotest`, `pyright`, `pytest`, `cargotest`,
`rust`, `vitest`, `jest`, `generic`.

### Tool selection per stack

| Key | Values |
|---|---|
| `ts.lint` | `auto` \| `biome` \| `oxlint` \| `off` |
| `ts.syntax` | `auto` \| `off` (the esbuild fallback of tier 0) |
| `ts.types` | `auto` \| `native` \| `tsc` \| `off` |
| `ts.tests` | `auto` \| `vitest` \| `jest` \| `off` |
| `ts.tsconfig`, `ts.test_args` | preferred tsconfig file name; extra runner flags, e.g. `["--project", "unit"]` |
| `python.lint` | `auto` \| `ruff` \| `off` |
| `python.types` | `auto` \| `dmypy` \| `mypy` \| `ty` \| `pyright` \| `off` |
| `python.tests` | `auto` \| `testmon` \| `off` |
| `python.dmypy_idle_sec`, `python.daemon_max_rss_mb` | 900, 1024 |
| `go.vet`, `go.tests` | `auto` \| `off` |
| `go.rdeps`, `go.max_test_packages` | `true` (also check direct importers), 16 (wider is deferred; `0` = no cap) |
| `rust.check`, `rust.tests`, `rust.all_targets` | `auto` \| `off`, `auto` \| `off`, `false` |
| `cpp.syntax`, `cpp.compiler` | `auto` \| `off`; compiler used when `compile_commands.json` has no entry |

`ts.types: auto` prefers a native compiler (the project's TypeScript 7 or native preview, then the one
from `ai-guard tools install`) over the JavaScript one. `native` insists on it. `tsc` uses only the
project's own compiler.

When the package has a `test:unit` script, its `--project` / `--config` flags are honoured: integration
tests (database, containers) do not run in tier 1.

### Tier 2

| Key | Values |
|---|---|
| `tier2.trigger` | `off` (default) \| `manual` \| `on_defer` \| `on_pass` \| `always` |
| `tier2.sync` | `git-ref` (default) \| `rsync` \| `none` |
| `tier2.remote`, `tier2.ref_prefix` | `origin`, `refs/aiguard` |
| `tier2.rsync_dest`, `tier2.rsync_args` | `user@host:/path`, extra flags |
| `tier2.command` or `tier2.shell` | the remote trigger; `{id} {sha} {ref} {branch} {root}` expand |
| `tier2.status_command` | what `ai-guard tier2 status` runs to show remote progress |

Example: run a heavy integration suite on the same machine, in the background and contained, after a
local pass:

```json
"tier2": { "trigger": "manual", "sync": "none",
           "command": ["ai-guard", "run", "--mem", "6000", "--timeout", "3600",
                       "--dir", "apps/api", "--shell", "pnpm test:int"] }
```

`ai-guard check --tier2` queues it and `ai-guard tier2 wait` returns the result.
