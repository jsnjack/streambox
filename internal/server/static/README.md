Player assets are embedded into the StreamBox binary and served locally.

`hls.light.min.js` is the light build of hls.js **1.7.3**, downloaded from
https://cdn.jsdelivr.net/npm/hls.js@1.7.3/dist/hls.light.min.js.
Its Apache 2.0 license notice is preserved in `hls.LICENSE`, with the full
license text in `hls.Apache-2.0.txt`.

Only the HLS playback path loads this dependency. Direct playback uses the
browser's video element with StreamBox's controls and no third-party player.
