# Samsung TV request captures

Requests that real Samsung TVs sent to other DLNA servers, replayed by the
tests in `samsung_test.go`. Headers and bodies are verbatim except for the
request path, which is rewritten to streambox's control URL.

| File | TV | Source |
|---|---|---|
| `browse-root-metadata.http` | LS32AM700PRXEN smart monitor (2021). A 2024 QN55S90D sends the same body. | [rclone issue 9346](https://github.com/rclone/rclone/issues/9346), [rclone forum](https://forum.rclone.org/t/dlna-not-working-on-recent-samsung-tv/50480) |
| `x-getfeaturelist.http` | UE40ES8000 (2012) | [minidlna bug 311](https://sourceforge.net/p/minidlna/bugs/311/) |
| `browse-empty-objectid.http` | UE40ES8000 (2012) | [minidlna bug 311](https://sourceforge.net/p/minidlna/bugs/311/) |

rclone logged its capture through Go's request dump, so header names are
canonicalised and sorted; minidlna logged the raw request. In every file
the body length matches the captured Content-Length.
