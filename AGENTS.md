# AGENTS.md

> See [AGENTS.universal.md](./AGENTS.universal.md) and [AGENTS.go.md](./AGENTS.go.md) for universal conventions.
> Refresh: `make standards`

---

## Overview

`streambox` is a minimal DLNA/UPnP media server for local-network video
playback on TVs and tablets. It scans a directory, advertises itself via SSDP,
serves files over HTTP, and exposes a small web UI for managing the library
(watch history, delete, refresh, restart the systemd unit).

---

## Architecture

```
main.go                       Thin entry point — delegates to cmd.Execute.
cmd/
  root.go                     Root cobra command: loads config, wires server +
                              ssdp, handles signals, persists update IDs.
  config.go                   `streambox config init` subcommand.
  logger.go                   slog setup; defines LevelTrace and initLogger
                              wired from --debug / --trace flags.
internal/
  config/
    config.go                 TOML schema, defaults, loader. DefaultConfig is
                              the template written by `config init`.
  media/
    library.go                Recursive scan, in-memory object index, title
                              cleaner, fsnotify-based change watcher.
    flatten.go                Optional watcher that moves video files out of
                              newly-detected subfolders into the root once
                              the subfolder's contents stop changing.
    history.go                Bounded watch history with undo buffer.
  server/
    server.go                 UPnP device + service descriptors,
                              ContentDirectory SOAP, file serving,
                              event subscriptions, web UI templates.
  ssdp/
    ssdp.go                   SSDP multicast discovery: NOTIFY broadcasts on
                              all physical IPv4 interfaces, M-SEARCH replies,
                              periodic alive + byebye-on-shutdown.
```

---

## Key Flows

1. **Startup** — `runServe` loads defaults, overlays the TOML file (if any),
   then overlays CLI flags. Scans the media directory, loads or generates a
   persistent UUID, bumps and saves SystemUpdateID, starts HTTP + SSDP
   goroutines, blocks on SIGINT/SIGTERM.
2. **DLNA browse** — TV sends SOAP `Browse` to `/contentdirectory/control`.
   `Server.browse` walks `media.Library`, builds a DIDL-Lite XML fragment, and
   returns it wrapped in a SOAP envelope.
3. **File playback** — TV fetches `/files/<id>`. `serveFile` resolves the ID,
   records it in `WatchHistory`, and streams the file with `http.ServeContent`
   (range support, MIME, DLNA headers).
4. **Library invalidation** — fsnotify create/remove/rename events debounced
   2s; on fire, `Library.Reload` rescans, `SystemUpdateID` is bumped and
   persisted, and `ssdp.Server.SendAlive` triggers a fresh NOTIFY burst so TVs
   refetch the directory.
5. **Web UI delete** — `/ui/delete` removes the file from disk, drops it from
   history, calls `OnFileDelete` to rescan and bump UpdateID.

---

## Build & Run

```bash
make check                            # full validation gate (fmt+vet+build+test+lint)
make build                            # multi-arch binaries under bin/
./streambox                           # run from project root after build
./streambox --trace                   # full diagnostic log to /tmp/streambox.log
./streambox --debug --media ~/Videos  # debug logs to stderr
./streambox --version                 # print stamped version
./streambox config init               # write default config to UserConfigDir
```

Smoke test: from another host on the same LAN, `curl http://<host>:8080/ui`
and verify the directory listing renders.

---

## Configuration

- File location: `os.UserConfigDir()/streambox/config.toml`
  (typically `~/.config/streambox/config.toml`, respects `$XDG_CONFIG_HOME`).
- Format: TOML. Schema in `internal/config/config.go` (`Config` struct).
- Override order (lowest → highest): defaults → TOML file → CLI flags.
- `--config` / `-c` overrides the auto-detected file location.

Persistent state lives separately under `os.UserConfigDir()` is **not** used
for state — see Design Decisions below.

---

## Design Decisions

- **State separate from config.** `uuid` and `updateid` live under
  `$XDG_DATA_HOME/streambox/` (default `~/.local/share/streambox/`), not the
  config dir. UUID must survive config-dir wipes so the TV keeps recognising
  the same server.
- **Stable UUID, bumped UpdateID.** UUID is generated once and reused so TVs
  remember the device across restarts. `SystemUpdateID` is bumped on every
  startup to invalidate stale TV-side directory caches.
- **fsnotify with 2s debounce.** Bulk file operations fire many events;
  rescanning per event would thrash. The debounce coalesces bursts.
- **All physical IPv4 interfaces for SSDP.** Virtual interfaces (docker,
  veth, virbr, tun/tap) are filtered — they cause spurious NOTIFY traffic and
  occasional TV duplicate-device bugs.
- **byebye+alive cache-bust, rate-limited.** Some TVs (notably LG) cache
  directory listings aggressively. On M-SEARCH we optionally send byebye then
  alive to force a refetch, but no more than once per 5 minutes to avoid
  disrupting active playback.
- **Logging via `slog`.** `--debug` writes structured logs to stderr at
  debug level. `--trace` writes the same at trace level (-8) to
  `/tmp/streambox.log`, truncated on each start. Both are persistent root
  flags inherited by subcommands.

---

## Gotchas

- `Library.Reload` constructs a fresh `Library` and copies its fields under
  the write lock — callers keep their `*Library` pointer.
- `Watch` returns once watchers are wired and runs the loop in a goroutine.
  An error from `fsnotify` after startup logs and continues (the function
  has already returned).
- The `Recent` virtual folder reuses item IDs from `All`. `parentCtx` in
  DIDL output is the *container being browsed*, not the item's real parent,
  so back-navigation from inside `Recent` works on TVs.
- `flatten` mode only watches direct children of the media root. Events for
  deeper paths are ignored on purpose.
- `Server.cfg.Debug` is retained as a switch for one HTTP-request middleware;
  general logging now flows through `slog` and is filtered by level.

---

## Known Issues

- Moving `uuid` / `updateid` to `~/.local/share/streambox/` is a one-time
  breaking change: existing installs that had them under `~/.config/streambox/`
  will appear as a new device until the old files are moved manually or the
  binary is rerun without the old data dir present.
