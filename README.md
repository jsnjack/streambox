streambox
==============

`streambox` is a minimal DLNA media server that makes your video files visible
to TVs, tablets and other UPnP/DLNA clients on your local network.

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
 - serve two virtual folders: **All** (every video, flat list) and **Recent** (files modified within the last N days)
 - keep file IDs stable across restarts, so your TV can resume playback of the same file
 - generate clean, human-readable titles from filenames

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

# Write log output to this file in addition to stderr.
log_file = "/tmp/streambox.log"

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
