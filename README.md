streambox
==============

`streambox` is a minimal DLNA media server that makes your video files visible
to TVs, tablets and other UPnP/DLNA clients on your local network.
It is developed and tested primarily against **LG TVs**, but follows the
standard DLNA/UPnP spec and should work well on any compatible client.

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
  -d, --debug           Enable debug logging
  -h, --help            help for streambox
  -i, --iface string    Network interface for SSDP (default: auto-detect)
  -m, --media string    Directory to serve video files from
  -n, --name string     Friendly device name shown on the TV (default "StreamBox")
  -p, --port int        HTTP port (default 8080)
```

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

### LG TV: stale folder content

LG TVs aggressively cache DLNA folder listings and often ignore standard
UPnP cache-invalidation signals (`SystemUpdateID` changes, SSDP alive
notifications). This means newly added files may not appear even after the
library is rescanned.

Two workarounds are built in:

**1. Multiple Recent folders (`recent_buckets`)**

Set `recent_buckets = 3` (or any number ≥ 2) in the config. Streambox will
expose **Recent 1**, **Recent 2**, **Recent 3**, … — each showing the same
recently-added files. Because the LG TV fetches a folder fresh the first time
it is opened, you can force a live view of new files by navigating to whichever
bucket you haven't visited yet. No restart required.

**2. Regenerate UUID**

Open the streambox web UI (`http://<host>:8080/ui`) and click
**Regenerate UUID**. This assigns a new device identity, appends an
incrementing suffix to the friendly name (e.g. `StreamBox 2`), and restarts
the service. The TV sees a brand-new server and fetches all folders fresh.
The old cached entry (with the previous name) fades out on its own.

> This requires streambox to run as a systemd user service so it can restart
> itself. The name suffix distinguishes the new entry from the stale one while
> both are briefly visible on the TV's source list.

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
