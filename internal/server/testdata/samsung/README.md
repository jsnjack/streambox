# Samsung TV request captures

Requests that real Samsung TVs sent to other DLNA servers, replayed by the
tests in `samsung_test.go`. Headers and bodies are verbatim except for the
request path, which is rewritten to streambox's control URL.

| File | TV | Source |
|---|---|---|
| `browse-root-metadata.http` | LS32AM700PRXEN smart monitor (2021). A 2024 QN55S90D sends the same body. | [rclone issue 9346](https://github.com/rclone/rclone/issues/9346), [rclone forum](https://forum.rclone.org/t/dlna-not-working-on-recent-samsung-tv/50480) |

rclone logged these through Go's request dump, so header names are
canonicalised and sorted; the body length matches the captured
Content-Length.
