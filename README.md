streambox
==============

`streambox` is a minimal DLNA media server that makes your video files visible
to TVs, tablets and other UPnP/DLNA clients on your local network.
It is developed and tested primarily against **LG TVs**, but follows the
standard DLNA/UPnP spec and should work well on any compatible client.
It also includes fixes for Samsung TVs, based on bugs that other DLNA servers
found on them; these have not yet been confirmed on a Samsung TV. See
[docs/samsung.md](docs/samsung.md).

### Description
```
Minimal DLNA media server for video files

Usage:
  streambox [flags]
  streambox [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  config      Manage streambox configuration
  help        Help about any command

Flags:
  -c, --config string   Path to TOML config file
  -d, --debug           DEBUG-level logging on stderr
  -h, --help            help for streambox
  -i, --iface string    Network interface for SSDP (default: auto-detect)
  -m, --media string    Directory to serve video files from
  -n, --name string     Friendly device name shown on the TV (default "StreamBox")
  -p, --port int        HTTP port (default 8080)
      --trace           TRACE-level logs to /tmp/streambox.log (truncated each run)
```

`--debug` and `--trace` can be combined — they control stderr and the trace
file independently.

Streambox generates clean, human-readable titles from filenames automatically:
- `The.Dark.Knight.2008.1080p.BluRay.x264.mkv` → **The Dark Knight**
- `Breaking.Bad.S03E07.720p.mkv` → **Breaking Bad S03E07**
- `some.movie.name.mkv` → **Some Movie Name**

### Details
When you run `streambox` it will:
 - scan the media directory recursively for video files
 - advertise itself on the local network via SSDP/UPnP so DLNA clients discover it automatically
 - serve virtual folders: **All** (every video, flat list) and one or more **Recent** folders (files modified within the last N days)
 - keep file IDs stable across restarts, so your TV can resume playback of the same file
 - generate clean, human-readable titles from filenames
 - regenerate its UPnP UUID automatically when files change, so the TV picks up new content without manual intervention

### Browser playback

Open `http://<host>:8080/ui` and select a video. The browser player includes a
full-video seek bar, volume controls, fullscreen, keyboard shortcuts, and resume
positions saved in that browser. Press Space to pause, the arrow keys to skip
10 seconds, M to mute, and F for fullscreen.

Install **FFmpeg and ffprobe** on the server for automatic format conversion.
Browser-compatible MP4 and WebM files play directly. H.264 video in other
containers is copied into an HLS stream without video encoding; incompatible
audio is converted to AAC. Other video formats are converted on demand to
H.264/AAC at **up to 1920×1080**, preserving smaller source resolutions.

On Linux, Streambox automatically tries a `/dev/dri/renderD*` graphics device
for VA-API hardware decoding, scaling, and H.264 encoding. This uses the Intel
media hardware on N200 systems when the appropriate drivers are installed and
the service has access to the render device. If hardware decoding fails, it
tries software decoding with hardware encoding, then falls back to a fast
software encoder limited to two threads. Actual real-time performance depends
on the source video and server hardware.

Converted playback uses locally bundled hls.js; no CDN or JavaScript package
installation is needed. Streams run at playback speed with a small rolling
buffer under the system temporary directory. Seeking starts a new stream at
the chosen position. Pausing, leaving the page, or shutting down the server
stops conversion and removes its buffer; abandoned sessions expire after one
minute without requests. One video transcode and up to two converted playback
sessions may be active at a time. Direct browser and DLNA playback do not use
these conversion slots.

### Thumbnails

Install [FFmpeg](https://ffmpeg.org/download.html) on the server and ensure
`ffmpeg` is on its PATH. Streambox automatically shows video frame thumbnails
in the web UI and advertises them to DLNA clients that display album art.

Thumbnails are generated when requested, using a frame five seconds into the
video (or the first frame for short clips). Generation runs one video at a time
with a 20-second timeout. Up to 256 thumbnails are cached in memory; replacing
or modifying a video invalidates its cached frame. No images are written into
your media directory, and thumbnail requests do not record watch history.

Without FFmpeg, browsing and playback continue with text listings. A video that
cannot be decoded shows an empty thumbnail in the web UI; failed generation is
retried after one minute. Whether a TV displays the supplied thumbnail depends
on its DLNA client.

### LG TV: stale folder content

LG TVs aggressively cache DLNA folder listings and often ignore standard
UPnP cache-invalidation signals (`SystemUpdateID` changes, SSDP alive
notifications). Streambox handles this in two ways:

**1. Auto-regen on library change (built in, automatic)**

Whenever a file is added or removed, streambox waits a brief cooldown then
regenerates its UPnP device UUID. It multicasts `ssdp:byebye` for the old
identity and `ssdp:alive` for the new one. The TV treats this as a brand-new
device on its next discovery, so it fetches a fresh listing instead of using
its cache. No restart, no manual action, no daemon required.

**2. Multiple Recent folders (`recent_buckets`)**

If the TV stays on a folder it has already cached (e.g. it's currently
displaying `Recent` when content changes), it may not re-fetch even after
auto-regen. Setting `recent_buckets = 3` in the config exposes **Recent 1**,
**Recent 2**, **Recent 3**, … — each a distinct container ID showing the
same recently-added files. Because LG fetches each container fresh on first
open, you can force a live view by navigating to a bucket you haven't
visited yet.

**3. Manual "Regenerate UUID" button**

Same effect as auto-regen but on demand. Open the web UI
(`http://<host>:8080/ui`) and click **Regenerate UUID**. Useful when a TV
is stuck and you want to force the change immediately rather than waiting
for the next library event.

### Configuration
Create a default config file with:
```bash
streambox config init
```

Then edit `~/.config/streambox/config.toml`:

```toml
# Path to the directory containing your video files (required).
media_dir = "~/Videos"

# HTTP port the server listens on.
port = 8080

# Friendly name shown on the TV's media source list.
name = "StreamBox"

# Files modified within this many days appear in the "Recent" folder.
# Set to 0 to disable the Recent folder.
recent_days = 14

# Number of "Recent" folders exposed via DLNA.
# Set to 2 or more to work around LG TV folder caching (see above).
recent_buckets = 1

# Enable verbose debug logging (HTTP requests, SSDP activity).
debug = false
```

### Installation
 - Using [grm](https://github.com/jsnjack/grm)
    ```bash
    grm install jsnjack/streambox
    ```
 - Download binary from [Releases](https://github.com/jsnjack/streambox/releases/latest/) page
 - One liner:
   ```bash
   curl -s https://api.github.com/repos/jsnjack/streambox/releases/latest | jq -r .assets[0].browser_download_url | xargs curl -LOs && chmod +x streambox && sudo mv streambox /usr/local/bin/
   ```
