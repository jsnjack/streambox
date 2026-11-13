# Samsung TVs

Read this before changing anything that Samsung TVs see: Browse replies,
SOAP actions, file serving or SSDP timing.

## Status

Samsung support is built from bugs that other DLNA servers found and fixed
on Samsung TVs. It has not yet been confirmed on a real Samsung TV. The target
TV is a 2024-or-newer model that listed StreamBox but could not browse it.

One streambox instance serves LG and Samsung TVs on the same network. Each
Samsung change is therefore either standard UPnP/XML behaviour that LG also
accepts, or it applies only to requests identified as Samsung.

## What streambox does for Samsung

| Behaviour | Why | Source |
|---|---|---|
| XML replies use only named entities (`&quot;`, `&apos;`), never `&#34;` | Samsung's parser rejects numeric references, keeps re-requesting the root's metadata and shows folders as empty. Titles with apostrophes showed as "39". | rclone [issue 9346](https://github.com/rclone/rclone/issues/9346), [commit 8502532c22](https://github.com/rclone/rclone/commit/8502532c22); anacrolix/dms [PR 110](https://github.com/anacrolix/dms/pull/110) |
| `X_GetFeatureList` returns `samsung.com_BASICVIEW` with every media type pointing at the root | Samsung TVs call it; a 2013 TV failed against a server that answered with an error | anacrolix/dms [issue 73](https://github.com/anacrolix/dms/issues/73); minidlna, Gerbera, rclone and Jellyfin all answer it |
| `X_SetBookmark` is accepted and ignored | Samsung TVs report resume positions with it; streambox keeps no state | Gerbera's ContentDirectory SCPD |
| An empty Browse `ObjectID` means the root | A 2012 ES8000 browses the root that way | [minidlna bug 311](https://sourceforge.net/p/minidlna/bugs/311/) |
| A file counts as watched only after 32 MiB, or half a smaller file, reaches one client within 10 minutes | Samsung TVs fetch the start of files to animate previews while browsing | UMS issues [5414](https://github.com/UniversalMediaServer/UniversalMediaServer/issues/5414), [6444](https://github.com/UniversalMediaServer/UniversalMediaServer/issues/6444) |
| Requests are labelled `client=samsung` when the User-Agent contains `SEC_HHP_`, `samsung-agent` or `Samsung UPnP SDK`; the label is remembered per IP | Control requests carry `SEC_HHP_` plus the owner's editable TV name; media requests often carry no User-Agent | minidlna `clients.c`, Gerbera `client_manager.cc` |
| A WARN when a client sends four root `BrowseMetadata` requests within 10 s without browsing children | That loop is how a Samsung TV behaves when it cannot parse the reply | rclone issue 9346 |

The client label is used only in logs so far. Samsung-only behaviour, if it
is ever needed, should key off it.

## Not done yet

Add these only if the field test shows the need.

| Change | Add it when | Notes |
|---|---|---|
| Send MKV as `video/x-mkv` to Samsung, in the DIDL and `Content-Type` | MKV files are missing from folders or refused | Sources disagree: minidlna sends `x-mkv` to every Samsung; the Universal Media Server profile for 2019+ TVs keeps `video/x-matroska` |
| Add `xmlns:sec`, `res@duration` and `resolution` (from the ffprobe cache in `transcode.go`), and a JPEG_SM thumbnail `<res>` | There is no seek bar, or thumbnails are missing | Most servers send these; none is proven necessary |
| Delay auto-regen while a client streams a file | Playback stops when a file is added | One 2017 report of Tizen playback dropping at each SSDP announcement; streambox announces every 10 s |
| External `.srt` subtitles: `sec:CaptionInfoEx` in the DIDL and a `CaptionInfo.sec` header when the TV sends `getCaptionInfo.sec: 1` | External subtitles are wanted | minidlna serves caption files as `smi/caption` |
| Replace `"` in titles with `”` | A title containing `"` breaks the listing | A 2021 Samsung monitor still failed on this after the entity fix |

Samsung TVs from 2024 do not play DTS audio, and 2025 models also drop WMV,
VC-1 and WMA ([Samsung 2024 TV video specifications](https://developer.samsung.com/smarttv/develop/specifications/media-specifications/2024-tv-video-specifications.html)).
Such files fail on the TV whatever the server does.

## Field test

The TV caches the server's description, so power-cycle it fully before
testing.

1. Note the model code: Settings → Support → About This TV.
2. Prepare test files: H.264/AAC MP4; HEVC MKV; MKV with DTS audio; AVI; a file named `Ocean's "Eleven".mp4`; a folder with more than 20 files.
3. Start `streambox --debug --trace` on the TV's network.
4. Unplug the TV for 30 seconds, then turn it on.
5. Open Sources → Connected Devices → StreamBox, then All and Recent.
6. Play each file; pause, seek forward and seek back.
7. While a video plays, copy a new file into the media folder. Auto-regen follows about 37 s later; note whether playback continues.
8. In the web UI, check that browsed but unplayed files are not under Recently Watched.
9. Keep `/tmp/streambox.log`. Optionally capture `sudo tcpdump -i any -s0 -w samsung.pcap host <TV-IP>`.

| Observation | Next step |
|---|---|
| The root-Browse WARN appears, and the log shows no `BrowseDirectChildren` | Compare the reply with rclone's working output; next suspects are `xmlns:sec` and the root container's class |
| Folders open, but MKV files are missing or refused | Send `video/x-mkv` to Samsung |
| A file is refused or plays without sound | Check its codecs with `ffprobe`; DTS is not supported |
| Playback stops when a file is added, or every ~10 s | Delay auto-regen while streaming |
| No seek bar, or seeking fails | Add `res@duration`; look for `TimeSeekRange` requests in the trace |
| Everything works | Record the model in this file and in the README |

## Testing without a TV

`internal/server/testdata/samsung/` holds requests captured from real Samsung
TVs, with their sources. `samsung_test.go` replays them and fails if any reply
contains a numeric character reference. VLC, Kodi and `gupnp-av-cp` accept
numeric references, so a passing check with them says nothing about Samsung.
Requests from the LG, taken from a `--trace` log, can be added the same way
to guard the TV that already works.
