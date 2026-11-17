# Octop desktop (Wails v3 + green portable)

All desktop-client code lives here. This is **not** `src/octop/infra/desktop`
(remote desktop streaming).

| Path | Role |
|------|------|
| [`portable/`](portable/) | Green zip packaging (was `scripts/green/`) |
| [`src/`](src/) | Wails v3 shell: load bundled zip, spawn Octop, tray/settings |
| [`package-release.sh`](package-release.sh) | Native end-to-end portable + Wails release build |

## Data directory

Same as the Octop CLI/server default:

- `OCTOP_HOME` → `~/.octop` (or the existing `OCTOP_HOME` env)
- Green runtime extract → `~/.octop/portable/`
- Shell prefs → `~/.octop/desktop-settings.json`

## First-run gate, restart and remote failures

The shell treats the absence of `desktop-settings.json` as first run and blocks
`boot()` until an onboarding popover (the tray-attached settings window in
`?onboarding=1` mode) is completed. A lightweight build without a bundled runtime
(`RuntimeAvailable()` is false) requires a remote URL; a full build allows an
empty URL to select the local portable runtime.

- Changing the server URL in settings reveals a "Restart now" banner. The
  cross-platform restart lives in `restart.go` plus `restart_windows.go` /
  `restart_unix.go`: the local server is stopped, the current executable is
  relaunched in a detached process group/session, and the old instance quits.
- When a configured remote fails the health probe, the shell shows an explicit
  fatal screen (with "Open settings" and "Quit") instead of silently starting
  the local portable runtime.
- Server URLs are validated on both the frontend and the Go backend
  (`validateServerURL`, http/https only); invalid input is rejected, not
  persisted, and kept in the field for correction.

## Build green zip

From repo root:

```bash
make -f desktop/portable/Makefile green
```

CI: `.github/workflows/octop-desktop.yml` builds native platform/arch variants.
`v*` tags and `workflow_dispatch` (platforms=`all`) run all six; pull requests
that touch `desktop/` build `darwin-*` and `windows-*` (amd64 + arm64). Each job
first creates the green zip and then packages the matching Wails application.

## Build a complete desktop release

Run the end-to-end script on the matching native host. It builds the Dashboard,
creates and verifies the portable runtime, embeds it into Wails, and produces
the final native package:

```bash
desktop/package-release.sh
# Reuse an existing desktop/portable/release/Octop-portable-<plat>-<version>.zip:
desktop/package-release.sh darwin-arm64 --reuse-portable
```

Wails requires native packaging, so all six variants are produced by the CI
matrix on macOS, Linux, and Windows runners rather than cross-compiled locally.
Windows packaging also needs [NSIS](https://nsis.sourceforge.io/) (`makensis`)
so the `.exe` is an installer rather than a portable single-file binary.

## Build the Wails shell

Run these from **`desktop/src`** (that directory contains `Taskfile.yml` and
`build/config.yml`). Requires **Go 1.25+**, [Wails v3](https://v3.wails.io/)
`v3.0.0-beta.13`.

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.13
cd desktop/src
go mod tidy
wails3 build            # development binary under desktop/src/bin/
wails3 task package ARCH=arm64 VERSION=<version> \
  PORTABLE_ZIP=../portable/release/Octop-portable-darwin-arm64-<version>.zip
```

Dev against an already-running Octop (skips the bundled green zip):

```bash
cd desktop/src
OCTOP_DESKTOP_URL=http://127.0.0.1:8088 wails3 dev
```

Without `OCTOP_DESKTOP_URL`, first launch uses `~/.octop/portable/` if valid,
otherwise extracts the matching zip shipped with the desktop package (embedded
in the Windows and Linux binaries, under `Contents/Resources` on macOS). The
Wails shell never downloads Octop. For local runtime debugging, set
`OCTOP_DESKTOP_PORTABLE_ZIP=/absolute/path/Octop-portable-<plat>-<version>.zip`.
On later launches, a newer bundled portable version replaces the extracted
runtime after creating a consistent SQLite backup under `~/.octop/backups/`.
The upgraded Octop process then applies the normal database migrations during
startup. Newer extracted runtimes are never downgraded; PostgreSQL remains
externally managed and is not copied by the desktop shell.

GitHub Release names follow `Octop-<kind>-<os>-<arch>-<version>.<ext>`:

- Desktop GUI: `Octop-desktop-<plat>-<version>.dmg` (macOS; open and drag
  `Octop.app` into Applications), `.exe` (Windows NSIS installer — copies
  into `Program Files\Octop` and creates Start Menu + desktop shortcuts),
  `.tar.gz` (Linux)
- Green runtime zip: `Octop-portable-<plat>-<version>.zip`
- PyPI wheels stay `octop-<version>-py3-none-any.whl` (PEP 427)

The Linux tar.gz contains only the GUI binary; it has no separate portable zip or
server terminal process. Runtime upgrades remain owned by Octop:
the shell sets `OCTOP_GREEN_PACKAGES`, so `octop update` upgrades the extracted
`packages/` directory through Octop's existing `--target` logic.

Linux also needs GTK4 + WebKitGTK 6 to link. macOS 12+.

## Icons

| File | Used for | Rule |
|------|----------|------|
| `src/build/appicon.png` | Windows `.ico`, Linux | Full-bleed 512x512 artwork |
| `src/build/appicon-macos.png` | macOS `.icns` | 1024x1024 canvas, artwork 824x824 centred |
| `src/assets/tray-icon.png` | Tray + app icon on Windows/Linux | Full-bleed |
| `src/assets/tray-icon-template.png` | macOS menu bar | 88px canvas, 64px black-on-transparent glyph |

macOS sizes both surfaces to a fixed box, so the padding has to live in the
artwork: the Dock follows Apple's 824/1024 icon grid, and Wails scales the menu
bar image to the full `NSStatusBar` thickness (22pt) where the glyph should be
~16pt. Full-bleed sources on either surface render a size bigger than every
other app. On macOS the Dock icon comes from the bundle's `icons.icns` only —
see `applyAppIcon` in `src/icons_darwin.go`.
