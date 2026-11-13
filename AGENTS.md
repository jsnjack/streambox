# AGENTS.md

> See [AGENTS.universal.md](./AGENTS.universal.md) and [AGENTS.go.md](./AGENTS.go.md) for universal conventions.
> Refresh: `make standards`

---

## Overview

`streambox` is a minimal DLNA/UPnP media server for local-network video
playback on TVs and tablets. It scans a directory, advertises itself via SSDP,
serves files over HTTP, and exposes a small web UI for managing the library
(watch history, delete, refresh, restart the systemd unit). The server holds
no persistent identity — every process start is a fresh device.

---

## Architecture

```
main.go                       Thin entry point — delegates to cmd.Execute.
cmd/
  root.go                     Root cobra command: loads config, wires server +
                              ssdp, handles signals.
  config.go                   `streambox config init` subcommand.
  logger.go                   slog setup; defines LevelTrace and initLogger.
                              Fan-out handler: stderr (INFO or DEBUG) and an
                              optional trace file at TRACE level, set
                              independently by --debug and --trace.
internal/
  config/
    config.go                 TOML schema, defaults, loader. DefaultConfig is
                              the template written by `config init`.
  loglevel/
    loglevel.go               Exports the custom slog `LevelTrace` constant so
                              any subsystem can log at trace level without
                              importing cmd.
  media/
    library.go                Recursive scan, in-memory object index, title
                              cleaner, fsnotify-based change watcher.
    flatten.go                Optional watcher that moves video files out of
                              newly-detected subfolders into the root once
                              the subfolder's contents stop changing.
    history.go                Bounded watch history with undo buffer.
  server/
    playback.go               Browser playback HTTP endpoints, embedded player
                              page and asset serving, conversion shutdown.
    transcode.go              ffprobe metadata cache, VA-API/software H.264
                              conversion, paced HLS sessions and buffer cleanup.
    static/                   Player JavaScript/CSS and vendored hls.js 1.7.3
                              with Apache 2.0 license and provenance.
    thumbnails.go             On-demand FFmpeg frame extraction, bounded
                              in-memory LRU cache, JPEG_TN HTTP endpoint.
    clients.go                Client family detection (Samsung by User-Agent),
                              remembered per IP for requests without one.
    diagnostics.go            Request logging middleware (trace: headers,
                              SOAP bodies, reply size and chunking) and the
                              root-Browse loop warning.
    watched.go                Decides when a client has played enough of a
                              file to record it in the watch history.
    server.go                 UPnP device + service descriptors,
                              ContentDirectory SOAP, file serving,
                              event subscriptions, web UI templates,
                              auto-regen state machine.
  ssdp/
    ssdp.go                   SSDP multicast discovery: NOTIFY broadcasts on
                              all physical IPv4 interfaces, M-SEARCH replies,
                              10s alive cadence, byebye-on-shutdown.
```

---

## Key Flows

1. **Startup** — `runServe` loads defaults, overlays the TOML file (if any),
   then overlays CLI flags. Scans the media directory. Generates a fresh
   random UUID for this run and seeds `SystemUpdateID` from
   `time.Now().Unix()` (spec-compliant baseline — a returning client that
   cached a value can't mistake it for "nothing changed"). Starts HTTP + SSDP
   goroutines, blocks on SIGINT/SIGTERM.
2. **DLNA browse** — TV sends SOAP `Browse` to `/contentdirectory/control`.
   `Server.browse` walks `media.Library`, builds a DIDL-Lite XML fragment, and
   returns it wrapped in a SOAP envelope. An empty ObjectID means the root.
   Samsung TVs also call `X_GetFeatureList`, answered with a
   `samsung.com_BASICVIEW` list that points every media type at the root,
   and `X_SetBookmark`, accepted and ignored.
3. **File playback** — TV fetches `/files/<id>`. `serveFile` resolves the ID
   and streams the file with `http.ServeContent` (range support, MIME, DLNA
   headers). It records the file in `WatchHistory` once one client has
   received 32 MiB of it, or half of a smaller file, within 10 minutes, so
   HEAD requests, browse previews and index reads do not count. File IDs are
   stable hashes of the on-disk path — survive restarts so partial playback
   can resume.
4. **Library invalidation + auto-regen** — fsnotify create/remove/rename
   events debounced 2 s. On fire: `Library.Reload` rescans, `BumpUpdateID`
   increments `SystemUpdateID` and (a) NOTIFYs subscribers, (b) marks an
   auto-regen pending. A background ticker (every 5 s) fires the pending
   regen once a 30 s cooldown has elapsed: byebye-old + alive-new over SSDP,
   new in-memory UUID. TVs then see a brand-new device on their next
   discovery and fetch a fresh state.
5. **Web UI delete** — `/ui/delete` removes the file from disk and reloads
   the library for an immediate UI refresh. The fsnotify-driven bump above
   handles TV notifications ~2 s later (deduplicated single event).
6. **UPnP eventing** — TVs SUBSCRIBE to `/contentdirectory/events` and
   `/connectionmanager/events`. Each SUBSCRIBE with a CALLBACK header creates
   a SID; an unknown-SID renewal returns 412 so the control point must
   re-SUBSCRIBE; failed NOTIFYs evict the subscription on first failure
   (no 30-min ghost callbacks).

---

## Build & Run

```bash
make check                            # full validation gate (fmt+vet+build+test+lint)
make build                            # multi-arch binaries under bin/
./streambox                           # INFO+ on stderr
./streambox --debug                   # DEBUG+ on stderr
./streambox --trace                   # INFO+ on stderr AND TRACE+ in /tmp/streambox.log
./streambox --debug --trace           # DEBUG+ on stderr AND TRACE+ in /tmp/streambox.log
./streambox --media ~/Videos          # override media dir
./streambox --version                 # print stamped version
./streambox config init               # write default config to UserConfigDir
```

`--debug` and `--trace` are independent: stderr level reflects `--debug`,
the trace file (truncated on every start) reflects `--trace`. The "ready"
line `streambox ready url=… name=…` is always emitted at INFO so the
terminal shows readiness regardless of where logs go.

`--trace` records every HTTP request with its headers, both SOAP bodies
(capped at 16 KB), the reply status and size, and whether the reply was
chunked, so one session with a new TV is enough to diagnose it. Media bodies
are not logged. A client that sends four root `BrowseMetadata` requests
within 10 s without browsing children logs a WARN at any level: that is how
a TV behaves when it cannot parse the Browse reply.

Smoke test: from another host on the same LAN, `curl http://<host>:8080/ui`
and verify the directory listing renders.

---

## Configuration

- File location: `os.UserConfigDir()/streambox/config.toml`
  (typically `~/.config/streambox/config.toml`, respects `$XDG_CONFIG_HOME`).
- Format: TOML. Schema in `internal/config/config.go` (`Config` struct).
- Override order (lowest → highest): defaults → TOML file → CLI flags.
- `--config` / `-c` overrides the auto-detected file location.

No persistent state files. Identity (UUID, SystemUpdateID) is regenerated
on every process start.

---

## Design Decisions

- **Browser playback preserves 1080p.** Browser-compatible MP4/WebM files play
  directly; compatible H.264 is remuxed, with AAC conversion only when needed.
  Other video uses VA-API hardware decoding/scaling/encoding when a Linux render
  device is present, then software decode + hardware encode, then two-thread
  `libx264` ultrafast. Encoding fits within 1920×1080 without upscaling. HLS uses
  fragmented MP4, a paced rolling buffer of 12 segments (~24 s for encodes),
  and locally embedded hls.js. Seeking starts a new session; copy seeks report
  the real keyframe position so the player can skip preroll accurately. Pause
  and page exit stop sessions; idle sessions expire after 1 minute. At most one
  video encode and two converted sessions run concurrently. `Server.Close`
  cancels conversions and removes buffers on process shutdown. Browser resume
  positions and volume live in localStorage, not in server state files.

- **Playback integration checks.** `go test -tags integration ./internal/server`
  requires FFmpeg/ffprobe and generates synthetic videos to check JPEG frames,
  codec copying, audio conversion, seeking, 1080p scaling, hardware fallback,
  HTTP streaming, and cleanup. Normal `make check` tests use isolated fixtures
  and do not require FFmpeg.

- **On-demand thumbnails.** If FFmpeg is on PATH, `/thumbnails/<id>` extracts
  a frame at five seconds (falling back to the start for short clips) as a
  JPEG_TN image bounded to 160×160. The web UI loads images lazily and DIDL
  advertises `upnp:albumArtURI`. One FFmpeg process runs at a time with a 20 s
  timeout including queueing. A 256-entry in-memory LRU is keyed by path, size,
  and modification time; failures are cached for one minute. No persistent
  cache files or watch history entries are created. Missing FFmpeg disables
  thumbnails without affecting browsing or playback.

- **No persistent identity.** The server has no `uuid` or `updateid` file.
  Every start gets a fresh random UUID; `SystemUpdateID` is seeded from
  `time.Now().Unix()`. This is paired with auto-regen, which churns the UUID
  on library changes anyway — persistent identity would be dead weight.
  TVs simply see "the StreamBox is a new device" each time, do fresh
  discovery + subscription, and end up with current state.
- **Auto-regen on library change.** A bump (fsnotify-driven) marks
  `regenPending=true`; a background ticker fires `OnAutoRegen` after a 30 s
  cooldown. `OnAutoRegen` calls `ssdp.UpdateIdentity`, which multicasts
  ssdp:byebye for the old UUID and ssdp:alive for the new one. SSDP alives
  also continue at the regular 10 s cadence, so a TV that wakes within
  ~10 s of the alive burst still picks up the new device.
- **fsnotify with 2 s debounce.** Bulk file operations fire many events;
  rescanning per event would thrash. The debounce coalesces bursts.
- **All physical IPv4 interfaces for SSDP.** Virtual interfaces (docker,
  veth, virbr, tun/tap) are filtered — they cause spurious NOTIFY traffic and
  occasional TV duplicate-device bugs.
- **LG TV cache workaround (`recent_buckets`).** LG TVs cache directory
  listings aggressively per-container-ID. Auto-regen helps via fresh device
  identity, but if a TV stays on a folder it has already cached, it won't
  re-Browse it. Setting `recent_buckets = N` creates `Recent 1`, `Recent 2`,
  …, each a distinct container ID — navigating to an unvisited bucket forces
  a fresh fetch.
- **Manual "Regenerate UUID" button.** Same flow as auto-regen; kept as an
  emergency lever for stuck clients.
- **Fan-out logging via `slog`.** A single multi-handler dispatches each
  record to stderr (INFO or DEBUG depending on `--debug`) and, if
  `--trace` is set, also to `/tmp/streambox.log` at TRACE level. Both
  destinations are filtered independently.

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
- Auto-regen uses `regenCheckInterval = 5 s` ticker + `regenCooldown = 30 s`;
  worst-case latency from a library change to actual regen is
  ~`debounce + cooldown + checkInterval` ≈ 37 s.
- The auto-regen `OnAutoRegen` callback is nil-checked rather than asserted —
  tests construct a `Server` without it.
- DLNA XML must never contain numeric character references such as `&#34;`:
  Samsung TVs then list the server but show its folders as empty. Escape
  with `escXML`, which writes the five named entities; never use
  `xml.EscapeText`. The replay tests in `samsung_test.go` check every reply.
