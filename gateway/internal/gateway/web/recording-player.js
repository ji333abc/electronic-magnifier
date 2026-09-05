// HLS keeps a short playback buffer and requests the target segment on seek.
// Prefer bundled hls.js for predictable buffering/retries, with native fallback.
window.RecordingPlayback = class RecordingPlayback {
  constructor(video, { onError, onAutoplayBlocked, onAuthenticationRequired } = {}) {
    this.video = video;
    this.onError = onError || (() => {});
    this.onAutoplayBlocked = onAutoplayBlocked || (() => {});
    this.onAuthenticationRequired = onAuthenticationRequired || (() => {});
    this.generation = 0;
    this.hls = null;
    this.nativeError = null;
  }

  open(url) {
    this.close();
    const generation = this.generation;
    const current = () => this.generation === generation;
    const play = () => {
      if (!current()) return;
      this.video.play().catch(error => {
        if (!current() || error.name === 'AbortError') return;
        if (error.name === 'NotAllowedError') this.onAutoplayBlocked();
      });
    };
    const mseSupported = window.Hls?.isSupported();
    if (!mseSupported && this.video.canPlayType('application/vnd.apple.mpegurl')) {
      this.nativeError = () => { if (current()) this.onError('playback_failed'); };
      this.video.addEventListener('error', this.nativeError);
      this.video.src = url;
      play();
      return;
    }
    if (!mseSupported) {
      this.onError('unsupported_browser');
      return;
    }
    const hls = this.hls = new window.Hls({
      enableWorker: false,
      lowLatencyMode: false,
      maxBufferLength: 12,
      maxMaxBufferLength: 18,
      backBufferLength: 12,
      frontBufferFlushThreshold: 18,
      maxBufferSize: 12 * 1024 * 1024,
      fragLoadPolicy: { default: {
        maxTimeToFirstByteMs: 15000,
        maxLoadTimeMs: 60000,
        timeoutRetry: { maxNumRetry: 2, retryDelayMs: 1000, maxRetryDelayMs: 4000 },
        errorRetry: { maxNumRetry: 2, retryDelayMs: 1000, maxRetryDelayMs: 4000 },
      } },
    });
    let mediaRecoveries = 0;
    hls.on(window.Hls.Events.MEDIA_ATTACHED, () => {
      if (current()) hls.loadSource(url);
    });
    hls.on(window.Hls.Events.MANIFEST_PARSED, play);
    hls.on(window.Hls.Events.ERROR, (_, data) => {
      if (!current()) return;
      const status = data.response?.code || data.networkDetails?.status;
      if (status === 401) {
        this.close();
        this.onAuthenticationRequired();
        return;
      }
      if (!data.fatal) return; // hls.js retries individual fragments first.
      if (data.details === 'bufferAddCodecError') {
        this.close();
        this.onError('unsupported_codec');
        return;
      }
      if (data.type === window.Hls.ErrorTypes.MEDIA_ERROR && mediaRecoveries++ < 1) {
        hls.recoverMediaError();
        return;
      }
      this.close();
      this.onError(status === 404 ? 'recording_missing' : 'playback_failed');
    });
    hls.attachMedia(this.video);
  }

  close() {
    ++this.generation;
    this.hls?.destroy();
    this.hls = null;
    if (this.nativeError) this.video.removeEventListener('error', this.nativeError);
    this.nativeError = null;
    this.video.pause();
    this.video.removeAttribute('src');
    this.video.load();
  }
};
