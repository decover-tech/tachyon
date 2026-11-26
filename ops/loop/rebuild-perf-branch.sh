#!/usr/bin/env bash
# perf-only: rebuilds perf/instrumentation = base + perf-only commits +
# candidate branches, from ops/loop/perf-branch.manifest.
#
#   ops/loop/rebuild-perf-branch.sh            build perf/rebuild, report
#   ops/loop/rebuild-perf-branch.sh --apply    ...then move perf/instrumentation to it
#   ops/loop/rebuild-perf-branch.sh --check    ...and run make check in the dev container
#
# PERF_MANIFEST=<file> reads the manifest from a file instead of the
# committed one, to try a candidate before committing the manifest.
#
# Never pushes. Stops at the first conflict, names the commit or candidate
# and the files, and leaves perf/instrumentation untouched: a conflict is a
# semantic question for a person, not something to resolve by guessing.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

PERF=perf/instrumentation
WORK=perf/rebuild
MANIFEST=ops/loop/perf-branch.manifest
apply=0 check=0
for a in "$@"; do
  case "$a" in
  --apply) apply=1 ;;
  --check) check=1 ;;
  *) echo "usage: $0 [--apply] [--check]" >&2; exit 2 ;;
  esac
done

[ -z "$(git status --porcelain)" ] || { echo "working tree not clean" >&2; exit 1; }
start=$(git rev-parse --abbrev-ref HEAD)

base="" candidates=()
while read -r kind ref _; do
  case "$kind" in
  "" | \#*) ;;
  base) base=$ref ;;
  candidate) candidates+=("$ref") ;;
  *) echo "$MANIFEST: unknown line: $kind $ref" >&2; exit 1 ;;
  esac
done < <(if [ -n "${PERF_MANIFEST:-}" ]; then cat "$PERF_MANIFEST"; else git show "$PERF:$MANIFEST"; fi)
[ -n "$base" ] || { echo "$MANIFEST: no base" >&2; exit 1; }

git fetch -q origin
base_sha=$(git rev-parse --verify "$base^{commit}")

# perf-only commits on the current perf branch, oldest first, merges and
# commits already in base excluded.
perf_commits=()
while read -r sha subject; do
  case "$subject" in perf-only:*) perf_commits+=("$sha") ;; esac
done < <(git log --reverse --no-merges --format='%H %s' "$base_sha..$PERF")

fail() { # what files
  echo
  echo "STOPPED: $1"
  echo "conflicting files:"
  git diff --name-only --diff-filter=U | sed 's/^/  /'
  git cherry-pick --abort 2>/dev/null || git merge --abort 2>/dev/null || true
  git checkout -q "$start"
  echo "$PERF is unchanged; $WORK is left at the last good step."
  exit 1
}

git checkout -q -B "$WORK" "$base_sha"
echo "base       $base = $(git rev-parse --short "$base_sha")"
for c in ${perf_commits[@]+"${perf_commits[@]}"}; do
  echo "perf-only  $(git log -1 --format='%h %s' "$c")"
  git cherry-pick --allow-empty "$c" >/dev/null 2>&1 || fail "perf-only commit $(git log -1 --format='%h %s' "$c") does not apply on $base"
done
for ref in ${candidates[@]+"${candidates[@]}"}; do
  sha=$(git rev-parse --verify "$ref^{commit}")
  echo "candidate  $ref = $(git rev-parse --short "$sha") ($(git rev-list --count "$base_sha..$sha") commits not in base)"
  git merge -q --no-ff --no-edit -m "perf: merge candidate $ref ($(git rev-parse --short "$sha"))" "$sha" >/dev/null 2>&1 ||
    fail "candidate $ref conflicts with base + perf-only commits"
done
echo "result     $WORK = $(git rev-parse --short HEAD)"

if [ "$check" = 1 ]; then
  ops/dev/run.sh make check
fi
if [ "$apply" = 1 ]; then
  # Still on $WORK, so $PERF can be moved even if it was checked out.
  git branch -f "$PERF" "$WORK"
  echo "$PERF -> $(git rev-parse --short "$PERF")"
fi
git checkout -q "$start"
