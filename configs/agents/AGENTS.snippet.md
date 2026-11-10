## Validation loop (AI-Guard)

After every batch of edits, and before saying the work is done, run:

```
ai-guard check
```

- Exit `0` and a line starting with `OK`: the changed files pass lint, type-check and their affected tests.
- Exit `1` and `FAIL`: fix exactly what is listed (`file`, then `line:col rule message`) and run it again.
  Do not run the project's full lint, type-check, build or test commands yourself: they saturate this machine.
- Exit `4` and `DEFERRED`: nothing failed locally, but a check was too heavy to run here and went to the
  remote tier. Follow it with `ai-guard tier2 wait` if the task needs the full build.
- Exit `3` and `ERROR`: a tool could not run (see the lines starting with `!`). Report it; do not work around it.

Useful variants: `ai-guard check --tier 0` (lint and syntax only, milliseconds), `ai-guard check --staged`,
`ai-guard check --since main`, `ai-guard check --format json`, `ai-guard check --disable ts.test` (types
only). Before a commit, add the project's own ESLint on the changed files: `ai-guard check --enable ts.eslint`.

A command that must run in full (an integration suite, a production build) goes through
`ai-guard run --mem 6000 --timeout 3600 -- <command>`: same command, but it cannot exhaust the machine.
