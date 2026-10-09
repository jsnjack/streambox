(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const video = $('video'), player = $('player'), timeline = $('timeline');
  const id = document.body.dataset.id;
  const storageKey = `streambox-position-${id}`;
  let info, direct = false, offset = 0, position = 0, token = null, hls = null;
  let loading = false, playing = false, stale = false, force = false, scrubbing = false;
  let generation = 0, request = null, recovery = 0, awakeTimer, hlsScript;
  let releasing = Promise.resolve();
  let preroll = 0;

  function readStorage(key) {
    try { return localStorage.getItem(key); } catch (error) { console.debug('Read player preference', error); return null; }
  }
  function writeStorage(key, value) {
    try { localStorage.setItem(key, value); } catch (error) { console.debug('Save player preference', error); }
  }
  function seconds(value) {
    const time = Math.max(0, Math.floor(value || 0));
    return time >= 3600 ? `${Math.floor(time / 3600)}:${String(Math.floor(time / 60) % 60).padStart(2, '0')}:${String(time % 60).padStart(2, '0')}` : `${Math.floor(time / 60)}:${String(time % 60).padStart(2, '0')}`;
  }
  function currentPosition() {
    if (playing && !loading && video.readyState > 0) return offset + video.currentTime;
    return position;
  }
  function savePosition() {
    position = currentPosition();
    writeStorage(storageKey, video.ended || (info && position >= info.duration - 1) ? '0' : String(position));
  }
  function render() {
    const duration = info ? info.duration : 0;
    const at = scrubbing ? Number(timeline.value) : currentPosition();
    if (!scrubbing) timeline.value = Math.min(duration, at);
    timeline.max = duration;
    timeline.disabled = !duration;
    timeline.setAttribute('aria-valuetext', `${seconds(at)} of ${seconds(duration)}`);
    $('time').textContent = `${seconds(at)} / ${seconds(duration)}`;
    $('toggle').textContent = playing ? 'Ⅱ' : '▶';
    $('toggle').setAttribute('aria-label', playing ? 'Pause' : 'Play');
    player.classList.toggle('playing', playing && !loading);
    $('mute').textContent = video.muted || video.volume === 0 ? '×♪' : '♪';
    $('mute').setAttribute('aria-label', video.muted ? 'Unmute' : 'Mute');
  }
  function showStatus(message, busy = false, error = false) {
    $('overlay').hidden = false;
    $('status').textContent = message;
    $('start').disabled = busy;
    $('start').textContent = busy ? '◌' : '▶';
    $('retry').hidden = !error;
    render();
  }
  function awake() {
    player.classList.add('awake');
    clearTimeout(awakeTimer);
    awakeTimer = setTimeout(() => player.classList.remove('awake'), 2500);
  }
  async function releaseToken(value) {
    if (!value) return;
    const response = await fetch(`/ui/playback/stop?token=${encodeURIComponent(value)}`, { method: 'POST', keepalive: true });
    if (!response.ok) throw new Error('Could not stop the previous playback session.');
  }
  async function releaseStream() {
    const oldToken = token;
    token = null;
    if (hls) hls.stopLoad();
    releasing = releasing.catch(error => console.warn('Stop previous playback', error)).then(() => releaseToken(oldToken));
    await releasing;
  }
  function resetMedia() {
    if (hls) { hls.destroy(); hls = null; }
    video.pause();
    video.removeAttribute('src');
    video.load();
  }
  function loadHLS() {
    if (window.Hls) return Promise.resolve();
    if (!hlsScript) hlsScript = new Promise((resolve, reject) => {
      const script = document.createElement('script');
      script.src = '/ui/assets/hls.light.min.js';
      script.onload = resolve;
      script.onerror = () => { hlsScript = null; script.remove(); reject(new Error('Could not load the video player. Please retry.')); };
      document.head.appendChild(script);
    });
    return hlsScript;
  }
  async function playMedia() {
    try { await video.play(); }
    catch (error) {
      if (error.name === 'AbortError') return;
      if (error.name === 'NotAllowedError') showStatus('Ready. Press play to begin.');
      else fail(error.message);
    }
  }
  function fail(message) {
    savePosition();
    playing = false;
    loading = false;
    stale = !direct;
    video.pause();
    releaseStream().catch(error => console.warn('Stop playback', error));
    showStatus(message, false, true);
  }
  async function startPlayback(at = position) {
    if (!info) return;
    const version = ++generation;
    if (request) request.abort();
    request = new AbortController();
    const signal = request.signal;
    position = Math.max(0, Math.min(at, Math.max(0, info.duration - 0.05)));
    loading = true;
    playing = true;
    stale = false;
    showStatus('Preparing your video…', true);
    try {
      await releaseStream();
      if (version !== generation) return;
      resetMedia();
      if (direct) {
        offset = 0;
        video.src = info.directURL;
        // Browsers ignore seeking before loadedmetadata.
        return;
      }
      if (!info.canConvert) throw new Error('This video needs conversion. Install FFmpeg and ffprobe on the server, then retry.');
      await loadHLS();
      if (version !== generation) return;
      const mediaSource = window.Hls.isSupported();
      const nativeHLS = !mediaSource && !!video.canPlayType('application/vnd.apple.mpegurl');
      if (!mediaSource && !nativeHLS) throw new Error('This browser cannot play the streaming format. Try a current version of Firefox, Chrome, or Safari.');
      const response = await fetch('/ui/playback/start', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, signal,
        body: JSON.stringify({ id, start: position, force })
      });
      if (!response.ok) throw new Error((await response.text()).trim());
      const session = await response.json();
      if (version !== generation) { await releaseToken(session.token); return; }
      token = session.token;
      offset = session.start;
      preroll = Math.max(0, position - offset);
      if (nativeHLS) {
        video.src = session.url;
      } else {
        hls = new window.Hls({
          startPosition: preroll, liveSyncDurationCount: 1, liveDurationInfinity: true,
          maxBufferLength: 8, maxMaxBufferLength: 12, backBufferLength: 4,
          maxBufferSize: 16 * 1024 * 1024, enableWorker: true
        });
        hls.on(window.Hls.Events.MANIFEST_PARSED, () => {
          if (version !== generation) return;
          loading = false;
          playMedia();
        });
        hls.on(window.Hls.Events.ERROR, (_, data) => {
          if (!data.fatal || version !== generation || !playing) return;
          if (recovery++ < 1) {
            force = true;
            startPlayback(currentPosition());
          } else fail('Playback was interrupted. Retry, or download the original video.');
        });
        hls.attachMedia(video);
        hls.loadSource(session.url);
      }
    } catch (error) {
      if (version !== generation || error.name === 'AbortError') return;
      fail(error.message || 'Could not start playback. Please retry.');
    }
  }
  function pause() {
    savePosition();
    generation++;
    if (request) request.abort();
    playing = false;
    loading = false;
    stale = !direct;
    video.pause();
    releaseStream().catch(error => console.warn('Stop paused playback', error));
    showStatus('Paused');
  }
  function toggle() {
    if (!info) return;
    if (playing && !video.paused) { pause(); return; }
    if (loading) { pause(); return; }
    if (!stale && video.getAttribute('src') && video.readyState > 0) {
      playing = true;
      playMedia();
    } else if (!stale && hls && video.readyState > 0) {
      playing = true;
      playMedia();
    } else startPlayback(position);
  }
  function seek(at) {
    if (!info || !info.duration) return;
    position = Math.max(0, Math.min(at, info.duration - 0.05));
    writeStorage(storageKey, String(position));
    if (direct && video.readyState > 0) video.currentTime = position;
    else if (playing) startPlayback(position);
    else stale = true;
    render();
  }
  async function fullscreen() {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else if (player.requestFullscreen) await player.requestFullscreen();
      else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
    } catch (error) { console.debug('Fullscreen unavailable', error); }
  }

  $('start').addEventListener('click', toggle);
  $('toggle').addEventListener('click', toggle);
  video.addEventListener('click', toggle);
  video.addEventListener('dblclick', fullscreen);
  $('fullscreen').addEventListener('click', fullscreen);
  $('rewind').addEventListener('click', () => seek(currentPosition() - 10));
  $('forward').addEventListener('click', () => seek(currentPosition() + 10));
  $('retry').addEventListener('click', () => { recovery = 0; if (info) startPlayback(position); else init(); });
  timeline.addEventListener('input', () => { scrubbing = true; render(); });
  timeline.addEventListener('change', () => { scrubbing = false; seek(Number(timeline.value)); });
  $('volume').addEventListener('input', () => {
    video.volume = Number($('volume').value);
    video.muted = false;
    writeStorage('streambox-volume', String(video.volume));
    render();
  });
  $('mute').addEventListener('click', () => { video.muted = !video.muted; render(); });
  video.addEventListener('loadedmetadata', () => {
    if (!playing) return;
    if (direct) {
      if (Number.isFinite(video.duration)) info.duration = video.duration;
      video.currentTime = position;
    } else if (!hls) video.currentTime = preroll;
    loading = false;
    playMedia();
    render();
  });
  video.addEventListener('playing', () => {
    playing = true;
    loading = false;
    $('overlay').hidden = true;
    $('retry').hidden = true;
    awake(); render();
  });
  video.addEventListener('waiting', () => {
    if (playing && !loading) showStatus('Buffering…', true);
  });
  video.addEventListener('timeupdate', render);
  video.addEventListener('ended', () => {
    savePosition();
    position = 0;
    playing = false;
    stale = !direct;
    releaseStream().catch(error => console.warn('Stop finished playback', error));
    showStatus('Finished. Play again?');
  });
  video.addEventListener('error', () => {
    if (!playing || hls || !video.getAttribute('src')) return;
    if (direct && info.canConvert) {
      position = currentPosition();
      direct = false;
      startPlayback(position);
    } else if (recovery++ < 1 && info.canConvert) {
      force = true;
      startPlayback(currentPosition());
    } else fail('This video could not be played. Retry, or download the original.');
  });
  player.addEventListener('pointermove', awake);
  player.addEventListener('pointerdown', awake);
  document.addEventListener('keydown', event => {
    if (event.ctrlKey || event.altKey || event.metaKey || event.target.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(event.target.tagName)) return;
    if ((event.key === ' ' || event.key === 'Enter') && /^(BUTTON|A)$/.test(event.target.tagName)) return;
    switch (event.key.toLowerCase()) {
      case ' ': case 'k': event.preventDefault(); toggle(); break;
      case 'arrowleft': event.preventDefault(); seek(currentPosition() - 10); break;
      case 'arrowright': event.preventDefault(); seek(currentPosition() + 10); break;
      case 'f': fullscreen(); break;
      case 'm': video.muted = !video.muted; render(); break;
    }
    awake();
  });
  window.addEventListener('pagehide', () => {
    savePosition();
    generation++;
    if (request) request.abort();
    if (token) {
      const url = `/ui/playback/stop?token=${encodeURIComponent(token)}`;
      if (!navigator.sendBeacon(url, '')) fetch(url, { method: 'POST', keepalive: true }).catch(error => console.debug('Stop abandoned playback', error));
    }
  });
  setInterval(() => { if (playing && !loading) savePosition(); }, 5000);

  async function init() {
    showStatus('Preparing your video…', true);
    try {
      const response = await fetch(`/ui/playback?id=${encodeURIComponent(id)}`);
      if (!response.ok) throw new Error('Could not find this video. Return to the library and refresh.');
      info = await response.json();
      direct = !!info.directType && !!video.canPlayType(info.directType);
      const saved = Number(readStorage(storageKey));
      position = Number.isFinite(saved) && saved > 0 && saved < info.duration - 5 ? saved : 0;
      const volume = readStorage('streambox-volume');
      if (volume !== null && Number.isFinite(Number(volume))) video.volume = Math.max(0, Math.min(1, Number(volume)));
      $('volume').value = video.volume;
      showStatus(position > 0 ? `Resume from ${seconds(position)}` : 'Ready when you are.');
    } catch (error) { showStatus(error.message, false, true); }
  }
  init();
})();
