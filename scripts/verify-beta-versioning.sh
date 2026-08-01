#!/usr/bin/env bash
# Verify the beta channel's version progression WITHOUT cutting a real release
# (docs/adr/0008). Runs the *actual* knope.toml `prepare-beta` workflow against a
# throwaway git repo in a temp dir — no remote, nothing pushed, deleted on exit —
# so it proves the numbering the pipeline will produce with zero mutation to the
# real repo. This is the hermetic, non-"test-in-prod" way to check beta numbering
# before the first live beta, and a CI gate against a knope upgrade silently
# changing it.
#
# Requires knope on PATH (CI installs it; locally, grab the binary from
# github.com/knope-dev/knope releases). Asserts invariants — each version is a
# `-beta.N` prerelease, betas increase monotonically within a cycle, and a new
# cycle re-seeds to a higher minor — rather than exact strings, so it survives
# knope's zero-vs-one indexing choice. It also prints the sequence it observed.
set -euo pipefail

if ! command -v knope >/dev/null 2>&1; then
  echo "verify-beta-versioning: knope not on PATH — install it from" >&2
  echo "  https://github.com/knope-dev/knope/releases  then re-run." >&2
  exit 2
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

git init -q -b main
git config user.email verify@wail.test
git config user.name "beta-verify"
git config commit.gpgsign false

# Verify against the real config, so a change to knope.toml is what this checks.
cp "$REPO_ROOT/knope.toml" .
printf '# Changelog\n' > CHANGELOG.md
echo "4.1.0" > VERSION
git add -A
git commit -qm "chore: seed"
git tag v4.1.0

fail() { echo "FAIL: $*" >&2; exit 1; }
is_beta() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+-beta\.[0-9]+$ ]]; }
# a < b under version order, and not equal
lt() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$1" ]; }

# Mirror beta.yml: preserve the beta branch's state (so knope sees the last beta
# version), merge main, run the real prepare-beta workflow, tag as beta.yml does.
betabuild() {
  if git show-ref -q --verify refs/heads/beta; then
    git checkout -q beta
    git merge -q --no-edit main
  else
    git checkout -q -B beta main
  fi
  GITHUB_TOKEN='' knope prepare-beta >/dev/null 2>&1 || fail "knope prepare-beta errored"
  local v; v=$(cat VERSION)
  git tag "v$v"
  echo "$v"
}

land() { git checkout -q main; git commit -q --allow-empty -m "$1"; }

# --- cycle 1: three releasable merges ---
land "feat: a"; C1_0=$(betabuild)
land "fix: b";  C1_1=$(betabuild)
land "fix: c";  C1_2=$(betabuild)

# --- stable ships, beta resets to main (the release-on-merge.yml step) ---
git checkout -q main
echo "4.2.0" > VERSION
git commit -qam "chore: prepare release"
git tag v4.2.0
git branch -qf beta main

# --- cycle 2: two releasable merges ---
land "feat: d"; C2_0=$(betabuild)
land "fix: e";  C2_1=$(betabuild)

echo "observed: cycle1 [$C1_0 $C1_1 $C1_2]  stable 4.2.0  cycle2 [$C2_0 $C2_1]"

# Invariant 1: every beta is a well-formed -beta.N prerelease.
for v in "$C1_0" "$C1_1" "$C1_2" "$C2_0" "$C2_1"; do
  is_beta "$v" || fail "'$v' is not a X.Y.Z-beta.N prerelease"
done

# Invariant 2: betas increase monotonically within each cycle.
lt "$C1_0" "$C1_1" || fail "cycle1 not increasing: $C1_0 !< $C1_1"
lt "$C1_1" "$C1_2" || fail "cycle1 not increasing: $C1_1 !< $C1_2"
lt "$C2_0" "$C2_1" || fail "cycle2 not increasing: $C2_0 !< $C2_1"

# Invariant 3: a new cycle re-seeds above the shipped stable — the feat: bumps
# the minor, so the stable part of cycle 2's first beta is 4.3.0, and leftover
# cycle-1 beta tags must not derail that.
lt "4.2.0" "$C2_0" || fail "cycle2 did not re-seed above stable 4.2.0: got $C2_0"
[ "${C2_0%%-*}" = "4.3.0" ] || fail "expected cycle2 to re-seed at 4.3.0-beta.N, got $C2_0"

echo "PASS: beta versioning is monotonic within a cycle and re-seeds across cycles"
