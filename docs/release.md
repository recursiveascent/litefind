# Release process

Releases are cut by tagging a commit on `main`. GoReleaser builds the
artifacts and publishes the GitHub release; the release workflow then updates
the Homebrew tap formula automatically.

## Prerequisites

- GoReleaser is pinned in `flake.nix` (devShell) — run commands through
  `nix develop`.
- A fine-grained PAT with **contents: write** on
  `recursiveascent/homebrew-tap` must exist as the repo secret `TAP_TOKEN` in
  `recursiveascent/litefind`. Create it at
  <https://github.com/settings/personal-access-tokens/new>, then:

  ```
  gh secret set TAP_TOKEN -R recursiveascent/litefind
  ```

  (paste the token at the prompt). The default `GITHUB_TOKEN` in Actions is
  scoped to the `litefind` repo and cannot push to the tap; `TAP_TOKEN` is the
  only secret the release workflow uses for that step.
- The gh token needs the `workflow` scope to push commits that touch
  `.github/workflows/`. If it's missing, refresh:

  ```
  gh auth refresh -h github.com -s workflow
  ```
- The Git remote must be pushable. If it's SSH and the agent isn't running,
  switch to HTTPS (gh's credential helper handles auth):

  ```
  git remote set-url origin https://github.com/recursiveascent/litefind.git
  ```

## Steps

1. **Edit `CHANGELOG.md`** — add a `## <version>` section above the last
   release. Keep entries user-facing and concrete.

2. **Bump `VERSION`** — the file contains the bare version (`0.1.1`, no `v`
   prefix). The release workflow verifies `v$(cat VERSION)` equals the tag
   name; a mismatch fails the run before any artifact is published.

3. **Validate the release config and build locally:**

   ```
   nix develop -c goreleaser check
   nix develop -c goreleaser release --clean --snapshot
   ```

   `--snapshot` builds every target and the source archive without
   publishing.

4. **Run checks:**

   ```
   nix develop -c make check
   ```

5. **Commit** the `VERSION` bump and any release-config changes (e.g.
   `.goreleaser.yaml`, `.github/workflows/release.yml`) with a
   `Prepare <version> release` message. Keep this a single atomic commit —
   the tag must point at a commit whose `VERSION` matches, or the workflow
   fails the verify step.

6. **Move `main` to the release commit and push:**

   ```
   jj bookmark set main -r @
   git push origin main
   ```

   The working-copy commit is not on `main` until you move the bookmark.

7. **Tag `main` and push the tag:**

   ```
   git tag v<version> main
   git push origin v<version>
   ```

   Tag `main` explicitly, not the working copy — the tag must point at the
   same commit `main` does. Pushing a `v*` tag triggers the `Release` workflow.

## What the workflow does

`.github/workflows/release.yml` runs on a pushed `v*` tag:

1. **Verify tag matches `VERSION`** — `test "v$(cat VERSION)" = "$GITHUB_REF_NAME"`.
2. **Publish the GitHub release** — `goreleaser release --clean` builds
   darwin/linux/windows (amd64, arm64) binaries, archives (tar.gz for Unix,
   zip for Windows), a source tarball, and a `checksums.txt`, then publishes
   them to a GitHub release for the tag.
3. **Update the Homebrew tap** — downloads the source archive for the tag,
   computes its SHA256, renders `Formula/litefind.rb` (a source-build
   formula using `std_go_args` and `depends_on "go" => :build`), and commits
   it to `recursiveascent/homebrew-tap` via the Contents API using
   `TAP_TOKEN`.

The formula is a source build, not a binary cask, so it works on both macOS
and Linux (Linuxbrew) and mirrors what `go install` does. GoReleaser's
`brews`/`homebrew_casks` integrations were intentionally not used: `brews` is
deprecated (goreleaser v2.16+) and `homebrew_casks` is macOS-only and ships
pre-compiled binaries, which would drop Linuxbrew support.

## GoReleaser configuration

`.goreleaser.yaml`:

- `source: enabled: true` — publishes a source tarball to the release; the
  Homebrew formula's `url` points at it.
- `ldflags` injects `main.versionOverride` so tagged binaries report the
  exact version (see `version()` in `main.go` for the full resolution order).
- `format_overrides` ships `.zip` archives for Windows.

## Verifying a release

After the workflow completes:

- The GitHub release at `https://github.com/recursiveascent/litefind/releases`
  should list six archives, the source tarball, and `checksums.txt`.
- The tap commit on `recursiveascent/homebrew-tap` should reference the new
  tag with the correct SHA256:
  `gh api repos/recursiveascent/homebrew-tap/contents/Formula/litefind.rb --jq .content | base64 -d`.
- Install and check version:
  ```
  brew reinstall recursiveascent/tap/litefind
  litefind --version
  ```
