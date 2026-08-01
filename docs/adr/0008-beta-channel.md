# A beta channel: main is beta, stable is a promotion

WAIL had exactly one release channel: every push to `main` auto-opened a knope
release PR whose merge tagged and shipped a stable release (15 releases in the
six days before this ADR). There was no way to get a build in front of a musician
for real-DAW, real-WAN testing *before* it became the version everyone runs. This
ADR adds a beta channel for a small invite-only circle (you plus 2–6
collaborators on macOS and Windows), redefining the existing pipeline: **`main`
becomes the beta channel** — every releasable merge is cut as a prerelease — and
**stable becomes a deliberate promotion** you make by merging the standing
release PR when a beta has survived real use. Promotion is by judgment, not a
gate.

## Decisions

- **Beta version identity: knope-native semver prereleases** (`v4.2.0-beta.N`,
  zero-indexed — the first beta of a cycle is `beta.0`), promoted by dropping the
  suffix to `v4.2.0`. The number pins exactly which build a tester is on. The
  base version is still computed from conventional commits, so a `feat!:` on main
  switches betas to `v5.0.0-beta.0`. Numbering is verified without a live release
  by `scripts/verify-beta-versioning.sh` (see below).
- **Beta state lives on a long-lived `beta` branch, never on main.** On each push
  to main, `beta.yml` merges `origin/main` into `beta` and runs `knope
  prepare-beta` (`prerelease_label = beta`). Because `VERSION` on the branch
  already carries the prerelease, knope increments `beta.N` by itself — that
  round-trip is the branch's entire justification. Main's history stays exactly
  as clean as before; `CHANGELOG.md` only ever records stable releases.
- **`beta` is cycle-scoped: reset to main at each stable release.**
  `release-on-merge.yml` force-updates `beta` to the just-released main. Within a
  cycle main never touches `VERSION`/`CHANGELOG.md`, so `beta.yml`'s merge is
  always clean; a stable release moves both, so the reset returns `beta` to main
  before the next beta and re-seeds knope from the fresh stable `VERSION`.
- **A docs/chore/test-only merge cuts no beta.** The beta path *inverts*
  `auto-release.yml`'s fallback-changeset trick: with no `feat:`/`fix:`/changeset
  since the last beta, `beta.yml` exits cleanly instead of fabricating a release,
  so a README typo doesn't burn a Windows+Linux artifact build. The stable path
  keeps the fallback, so a docs-only cycle can still be promoted.
- **Betas and stable share the production relay and real rooms.** A beta is just
  a client; mixed beta/stable rooms are the normal case, which is exactly the
  interop we want proven before promotion (users upgrade at their own pace
  regardless). The relay stays manual-deploy from main and must stay backward
  compatible for the oldest supported client; a beta may depend on new relay
  behavior only once that relay is live.
- **macOS: a second `wail-beta` Homebrew formula in the same tap, generated at
  release time** from `homebrew/wail.rb` (only the class name differs). It
  installs the *same* paths as stable — `bin/wail` and `lib/wail-*.clap` — so the
  two coexist by `brew link`, not by a renamed binary: `brew unlink wail && brew
  link --overwrite wail-beta` flips channels with no rebuild, and both kegs stay
  on disk so falling back mid-session is a link, not a reinstall. The beta
  formula must **not** declare `conflicts_with "wail"` (that would force an
  uninstall + from-source rebuild to switch back).
- **Windows: the existing prerelease zip**, unzipped alongside a stable copy.
- **Shared data dir (`~/.wail`).** Beta runs with the tester's real identity,
  stream names, and capture selections, so they test their actual setup and the
  room sees their usual peer identity (channel affinity behaves normally). The
  three persisted files are JSON with graceful fallback; `-instance N` still
  gives isolation for anyone who wants it.
- **Distribution stays invite-only** (documented in `DEVELOPMENT.md`, not the
  README), which keeps unvetted builds out of other people's jams without a
  technical gate, consistent with sharing the production relay.

## Consequences

- **Wire/protocol changes must stay backward compatible for at least one cycle.**
  A beta that changes the WAIF wire format or a sync message and lands in a room
  with a stable peer degrades a real jam. This is the price of testing interop on
  the production relay, and it is the intended trade.
- **The `beta` branch is CI-owned; a hand commit will be lost or conflict.** A
  manual push either conflicts with the next `beta.yml` merge or is discarded at
  the next reset. Recorded in `CLAUDE.md` as a rule for agents.
- **There is a narrow race at the cycle boundary.** If a normal merge lands on
  main during the seconds `release-on-merge.yml` takes to reset `beta`, that
  `beta.yml` run can conflict on `VERSION`/`CHANGELOG.md` and fail loudly; the
  next merge recovers once the reset has completed (`beta.yml` recreates the
  branch from main if needed).
- **The in-tree `VERSION` on main lags reality within a cycle** (says `4.1.0`
  while `4.2.0-beta.5` exists on the `beta` branch). The honest beta version
  lives on `beta`, where the plugin stamp (`plugins/CMakeLists.txt` reads
  `VERSION`) and `appVersion` pick it up — a property a tag-only scheme would
  have lost.
- **Prerequisites this ADR's pipeline PR carries:** the `knope.toml` version
  regex had to accept a `-suffix` (it re-reads `VERSION` through that regex, so
  without it knope regenerates `beta.0` forever); `release.yml` marks
  prerelease-suffixed tags as GitHub prereleases (so a beta isn't labelled
  "Latest") and branches its Homebrew-tap step to write `wail-beta.rb` rather
  than repoint `wail.rb`.
- **Beta numbering is verified without cutting a release, not "watched in
  prod".** `scripts/verify-beta-versioning.sh` drives the real `knope.toml`
  `prepare-beta` workflow against a throwaway git repo (no remote, nothing
  pushed) and asserts the invariants: each version is a `-beta.N` prerelease,
  betas increase monotonically within a cycle, and a new cycle re-seeds to the
  next minor above the shipped stable (leftover cycle-1 beta tags do not derail
  it). `verify-release-config.yml` runs it in CI on any change to the release
  config, so a knope upgrade that alters numbering fails a check instead of a
  live beta. Confirmed sequence: `4.2.0-beta.0 → beta.1 → beta.2`, ship `4.2.0`,
  then `4.3.0-beta.0`.
- **Follow-up (a separate app/relay PR), not in the pipeline PR:** remove
  Honeybadger (main-goroutine-only coverage, `ReportError` never called, key in
  git history — rotate it) and redirect fd 2 to `~/.wail/logs/crash.log` so a
  beta panic leaves evidence; add a forced plugin-reinstall control, since
  `InstallPluginsIfMissing` skips bundles already present and beta plugin changes
  otherwise reach no one; and have the relay forward `client_version` in the peer
  list so a mixed-room report names both builds. Deferred by choice: making
  `semverLess` prerelease-aware — harmless at `minVersion = "0.0.0"`, a footgun
  the day it is bumped to lock out a bad build.
