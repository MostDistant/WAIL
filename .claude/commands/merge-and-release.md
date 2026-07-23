---
description: Merge a feature PR, then merge the knope "chore: prepare release" PR it triggers, and report once the GitHub release + artifact builds are done
allowed-tools: [Bash, Read, Edit, Write]
---

# Merge and release (WAIL)

Drive a feature PR through WAIL's full knope release pipeline and report when the
release is live. This is the superset of `/release` (which only merges the
prepare-release PR).

Releases are automated by knope via GitHub Actions (see `CLAUDE.md` → "Release pipeline"):

1. Push to `main` → **Prepare Release** (`auto-release.yml`) runs `knope prepare-release` → opens/updates a PR from the `release` branch titled **`chore: prepare release vX.Y.Z`**.
2. Merge that PR → **Release on Merge** (`release-on-merge.yml`) runs `knope release` → creates the GitHub release + tag and dispatches artifact builds.
3. **Release** (`release.yml`) builds and uploads the macOS / Windows / Linux artifacts.

Both merges are outward-facing and hard to reverse — only run this when the user asked to merge **and** release. Never run `knope` locally, never hand-edit `VERSION`, never create tags by hand (per `CLAUDE.md`).

## Instructions

### 1. Identify the feature PR

Determine which PR to ship from the conversation. If it's ambiguous (several open PRs, none named), run `gh pr list --state open` and ask which one.

If there's **no feature PR to merge** — e.g. a `chore: prepare release` PR is already open from previously-merged work and the user just wants to cut the release — skip straight to step 5 and merge it.

### 2. Preflight it

- `gh pr view <N> --json title,mergeable,mergeStateStatus,headRefName` and `gh pr checks <N>`.
- The **title must be a conventional commit** (`feat:` / `fix:` / `feat!:`): you'll squash-merge, so the title becomes the commit knope reads for the bump (`feat`→minor, `fix`→patch, `!`→major). Fix it with `gh pr edit <N> --title ...` if it's wrong.
- Know the type's release impact before merging: `feat:`→minor, `fix:`→patch, breaking (`!`)→major, and **`chore:`→patch too** — knope in this repo bumps a patch for `chore:` and lists it under "Fixes" (observed: a lone `chore:` cut v3.5.1). Only `docs:` is ignored (no bump, no release PR). So a `chore:` **will** open a release PR in step 4; don't expect it to skip. If you're merging a `chore:` you don't want a standalone release for (e.g. an internal tooling/doc tweak), bundle it into a feature PR or let it ride the next feature release instead of cutting a dedicated version for it.
- If `mergeStateStatus` is `DIRTY` / `mergeable` is `CONFLICTING`: resolve in a throwaway worktree — `git worktree add`, merge `origin/main`, fix the conflicts, run `cd wail-app && go build ./... && go test ./...` (plus `signaling-server` if it was touched), push the branch, re-check mergeability. Remove the worktree afterward.
- If checks are red: stop and report. Don't merge over failing CI.

### 3. Merge the feature PR (squash)

```sh
gh pr merge <N> --squash
gh api repos/MostDistant/WAIL/commits/main --jq '.sha[0:9] + " " + (.commit.message | split("\n")[0])'
```

Confirm `main` advanced and shows the conventional commit. Squash (not a merge commit) so multiple commits collapse into the single conventional commit knope needs.

### 4. Wait for the prepare-release PR

Poll every ~20–30s until the **Prepare Release** run succeeds and the release PR appears:

```sh
gh run list --limit 6 --json name,status,conclusion,headBranch,event \
  --jq '.[] | "\(.name) | \(.status)/\(.conclusion) | \(.headBranch) | \(.event)"'
gh pr list --state open --head release --base main --json number,title,url
```

If the merged commit was `docs:` (the only type knope ignores), "Prepare Release" opens no release PR — that's expected; you're done. A `chore:` **does** open one (patch bump, entry under "Fixes"), so proceed to step 5. Otherwise, give up and report if nothing shows after a few minutes (check `gh run view` for a failed prepare-release). Sanity-check the version bump matches the commit type you merged (`feat`→minor, `fix`/`chore`→patch, `!`→major).

### 5. Merge the release PR

- Confirm it touches only `CHANGELOG.md` + `VERSION`: `gh pr diff <RELEASE_PR> --name-only`. Anything else → stop and investigate.
- Let CI settle, then squash-merge and delete the branch (knope recreates `release` on the next prepare-release). **This repo disallows merge commits — use `--squash`, not `--merge`** (a `--merge` attempt fails with "Merge commits are not allowed on this repository"). `release-on-merge.yml` triggers on the merge regardless of method.

```sh
sleep 120 && gh pr merge <RELEASE_PR> --squash --delete-branch
```

### 6. Wait for the release to complete

"Completed" means the release/tag exists **and** the artifact build has uploaded binaries.

```sh
gh run list --limit 6 --json name,status,conclusion,headBranch \
  --jq '.[] | "\(.name) | \(.status)/\(.conclusion) | \(.headBranch)"'
gh release view vX.Y.Z --json tagName,url,assets \
  --jq '{tag: .tagName, url, assets: [.assets[].name]}'
```

Watch **Release on Merge** succeed (creates the release/tag), then the **Release** run (tag `vX.Y.Z`) finish and attach assets.

### 7. Report

Give the user: the version + release URL, whether the artifact build succeeded and which binaries are attached (call out a failed build plainly — the release exists but has no binaries), and the one-line changelog entry.
