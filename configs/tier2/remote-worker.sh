#!/usr/bin/env sh
# Tier-2 worker for a build box reached over SSH. It lives in a clone of the repository on that box.
#
#   aiguard.json:
#     "tier2": {
#       "trigger": "on_pass",
#       "shell": "ssh build-box \"cd /srv/ci/repo && sh ci/remote-worker.sh {sha} {ref}\""
#     }
#
# ai-guard has already pushed the snapshot of your working tree to <ref> on origin; this script
# fetches it, builds it with warm caches and removes the ref. Its exit status is the job's result
# (`ai-guard tier2 wait` returns it; the output ends up in .aiguard/tier2/<id>.log on your machine).
set -eu

sha="$1"
ref="${2:-}"

if [ -n "$ref" ]; then
  git fetch --quiet origin "$ref"
else
  git fetch --quiet origin "$sha"
fi
git checkout --quiet --force --detach "$sha"

cleanup() {
  if [ -n "$ref" ]; then
    git push --quiet origin --delete "$ref" || true
  fi
}
trap cleanup EXIT

# ---- the heavy pipeline: edit to match the project ------------------------------------------------
export CARGO_INCREMENTAL=0 RUSTC_WRAPPER="${RUSTC_WRAPPER:-sccache}"

if [ -f package.json ]; then
  npm ci --no-audit --no-fund
  npm run build
  npm test
fi
if [ -f pyproject.toml ]; then
  uv sync --frozen
  uv run --no-sync pytest -q
fi
if [ -f go.mod ]; then
  go build ./...
  go test ./...
fi
if [ -f Cargo.toml ]; then
  cargo build --release --locked
  cargo test --locked
fi
