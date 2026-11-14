# Packaging & Release

All packaging is driven by Bash scripts in [`addons/`](../../addons/). Output goes to `dist/`.
The version comes from the [`VERSION`](../../VERSION) file, so the release flow is "bump VERSION,
run the builders."

## Release orchestration

[`addons/release.sh`](../../addons/release.sh) builds the full release set:

1. Reads `VERSION`.
2. Wipes `dist/` (so checksums reflect exactly this release).
3. Cross-compiles all architectures (`./build.sh cross` → amd64, arm64, riscv64).
4. For each binary, assembles a `kula/` bundle containing the binary plus `CHANGELOG.md`,
   `VERSION`, `LICENSE`, `README.md`, `config.example.yaml`, `scripts/`, `bash-completion`,
   `init/`, and `man/`, and produces both a `.tar.gz` (its members owned by uid/gid 0, so a root
   extraction does not hand the files to the builder's uid) and a gzipped single binary.
5. Builds `.deb` and `.rpm` packages for amd64, arm64 and riscv64 from those binaries, then deletes
   the uncompressed binaries (only the `.gz`/`.tar.gz`/`.deb`/`.rpm` files ship). When `snapcraft`
   is installed, the `.snap` set is cross-built from source; a failed snap build only warns.
6. Generates `CHECKSUMS.sha256.txt` over the artifacts (used by the guided installer).

AUR files are deliberately not built there (the PKGBUILD pins the published GitHub source archive):
run [`addons/build_aur.sh`](../../addons/build_aur.sh) after the release exists and re-upload
`CHECKSUMS.sha256.txt`, which it updates.

## Per-format builders

| Script | Output | Notes |
|--------|--------|-------|
| [`build.sh`](../../addons/build.sh) | `kula` (+ `kula-scan`, `gen-mock-data`) in the repo root, or `dist/kula-linux-<ver>-<arch>` with `cross` | `./build.sh cross` for all arches |
| [`build_deb.sh`](../../addons/build_deb.sh) | `dist/kula-*.deb` | installs systemd unit + `kula` user |
| [`build_rpm.sh`](../../addons/build_rpm.sh) | `dist/kula-*.rpm` | RHEL/Fedora family |
| [`build_aur.sh`](../../addons/build_aur.sh) | `dist/kula-<ver>-aur/` (+ `.tar.gz`) | then `makepkg -si` |
| [`build_snap.sh`](../../addons/build_snap.sh) | `dist/kula-*.snap` | needs snapcraft + LXD; `cross` for multi-arch, `--remote` via Launchpad |
| [`build_appimage.sh`](../../addons/build_appimage.sh) | `dist/kula-<ver>-<arch>.AppImage` | portable single-file; without `appimagetool` only the AppDir is prepared |
| [`docker/build.sh`](../../addons/docker/build.sh) | Docker image | multi-arch via buildx |

### Examples

```bash
./addons/build_deb.sh && ls -1 dist/kula-*.deb
./addons/build_rpm.sh && ls -1 dist/kula-*.rpm
./addons/build_aur.sh && (cd "dist/kula-$(cat VERSION)-aur" && makepkg -si)
./addons/build_snap.sh            # host arch
./addons/build_snap.sh cross      # amd64/arm64/riscv64 locally
./addons/build_appimage.sh        # host arch (or pass amd64/arm64/riscv64)
```

`build_snap.sh` reuses snapcraft's managed LXD instances across runs. Before a local build it
strips any orphaned `disk-/tmp/craft-state` mount left behind by an interrupted build (crash,
OOM kill, power loss): its source is a per-run state dir under `XDG_RUNTIME_DIR` that disappears
on reboot, after which LXD refuses to start the instance with "Missing source path".
`--remote` builds skip this and run on Launchpad.

`build_aur.sh` first asks whether to package the local checkout or the published GitHub source
archive; only the remote path appends its checksums to `CHECKSUMS.sha256.txt`.

## Docker

The image is a two-stage build ([`addons/docker/Dockerfile`](../../addons/docker/Dockerfile)):

- **Builder:** pinned `golang:1.26.11-trixie` (by digest), `CGO_ENABLED=0`,
  `-trimpath -ldflags="-s -w" -buildvcs=false`, building only `./cmd/kula/`. It runs on the
  build machine's platform (`--platform=$BUILDPLATFORM`) and cross-compiles with
  `GOARCH=$TARGETARCH`, so the Go image does not need a variant for every target (the pinned
  one has no `linux/riscv64`) and no Go runs under emulation. The runtime stage still runs
  `RUN` steps for the target, so a multi-arch push needs QEMU binfmt handlers on the builder
  host.
- **Runtime:** pinned `alpine:3.24.3` (by digest) with an unprivileged `kula:kula` user. Its uid
  100 and gid 101 are pinned to what `adduser -S` gave earlier images, so their data volumes
  stay writable; changing them would leave every existing volume owned by someone else. Root
  owns `/app`, the binary and `/app/config.yaml` (`root:kula 0640`), so Kula cannot replace
  them; only `/app/data` (`kula:kula 0750`) is Kula's.

The build context is the repository root (`build.sh` and the push scripts send `.`), and
[`.dockerignore`](../../.dockerignore) keeps out what the build does not need: `.git`,
`.claude`, `.scratch`, `dist`, `reviews`, `landing`, `node_modules`, local data, configs and
binaries. The image needs only the Go module, `VERSION` and `config.example.yaml`.

```bash
./addons/docker/build.sh
docker compose -f addons/docker/docker-compose.yml up -d
./addons/docker/push.sh        # Docker Hub
./addons/docker/push_ghcr.sh   # GHCR
```

The image expects host PID/network and read-only `/proc` at runtime — see the user
[Installation → Docker](../user/02-installation.md#docker) section. The compose file runs it
with `cap_drop: [ALL]`, `no-new-privileges` and a read-only root filesystem, and needs no tmpfs:
Kula writes only to the data volume (tier files, their seek indexes, `sessions.json`, backups,
the custom-metrics socket). With `pid: host`, uid 100 is shared with any host account of that
uid.

## Snap specifics

Building needs [snapcraft](https://documentation.ubuntu.com/snapcraft/stable/howto/setup/) and
a build backend (LXD recommended):

```bash
snap install snapcraft --classic
snap install lxd
lxd init --auto
```

The snap uses a **strict sandbox**, so capabilities are limited by default and extended with
`snap connect`. The snap packaging lives under [`snap/`](../../snap/). See the
[Snap wiki page](https://github.com/c0m4r/kula/wiki/Snap).

## Ansible

A deployment role is provided under [`addons/ansible/`](../../addons/ansible/) for fleet
rollouts (`deploy.sh`, `kula.yaml`, the `roles/kula` role with a `config.yaml.j2` template).

## Install scripts

- [`addons/install_v3.sh`](../../addons/install_v3.sh) — current guided installer (auto-detects
  distro, verifies checksums; flags `-y`/`--yes`, `--skip-verify`). README, the user docs and
  the landing page pin its sha256.
- [`addons/install_v2.sh`](../../addons/install_v2.sh) and
  [`addons/install.sh`](../../addons/install.sh) — earlier installers, kept byte for byte as
  published, so a copy of the instructions that pins their checksums still verifies them.
  Later fixes are not carried back: `install.sh` verifies none of its downloads, and both leave
  `/etc/kula` to the `kula` user and keep a tarball's stored owners when extracting as root.

A published installer never changes. To change the installer, copy the current one to the next
`install_vN.sh`, change the copy, and move the URL and the pinned checksum in
[`README.md`](../../README.md),
[`docs/user/02-installation.md`](../user/02-installation.md#verifying-the-installer-first),
[`landing/landing.js`](../../landing/landing.js) and
[`landing/index.html`](../../landing/index.html) to it (`grep -rn` the old name and hash).

## Packaging helpers

[`addons/packaging/`](../../addons/packaging/) holds optional helpers for distro packagers and
constrained builds, run against the source tree before building: `remove_fonts.sh` drops the
bundled fonts in favor of system fonts, `remove_game.sh` strips the Space Invaders easter egg
along with its routes, tests and font, and `replace_chartjs.sh` swaps the vendored Chart.js
bundle for the distro-provided `chart.umd.min.js` +
`chartjs-adapter-date-fns.bundle.min.js` + `chartjs-plugin-zoom.min.js` trio (found via
`--from DIR`, `$CHARTJS_DIST_DIR` or the usual distro paths, or fetched and sha256-verified
with `--download`). Each exits non-zero if its patterns no longer match the code.
`check_helpers.sh` runs the two removal helpers on scratch copies, alone and combined, and
requires `go vet ./...` and `go test ./internal/web/` to pass there; CI runs it on every change.
Tests that depend on the fonts or the game must therefore skip when a helper has removed
them. When the Chart.js wiring changes, also run `replace_chartjs.sh` on a scratch copy and
pass `./addons/check.sh` there.

## Release checklist

1. Update [`CHANGELOG.md`](../../CHANGELOG.md).
2. Bump [`VERSION`](../../VERSION).
3. `./addons/check.sh` — all green.
4. `./addons/release.sh` — build all artifacts + checksums.
5. Build/push the Docker image and any distro packages needed.
6. Publish the GitHub release with `dist/` artifacts and `CHECKSUMS.sha256.txt`.
7. If the installer changed, it is a new `install_vN.sh` (see
   [Install scripts](#install-scripts)); check that every pinned URL and checksum names it, and
   that the earlier installers still hash to what they were published with.

Next: [Contributing](16-contributing.md).
