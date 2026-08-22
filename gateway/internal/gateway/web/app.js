const VIDEO_STALL_WARNING_MS = 3000;
const VIDEO_STALL_RECOVERY_MS = 8000;
const VIDEO_START_RECOVERY_MS = 12000;
const VIDEO_RECOVERY_COOLDOWN_MS = 8000;
const VIDEO_STABLE_RESET_MS = 30000;

const state = {
  config: null, socket: null, clientId: '', leases: {}, cards: new Map(),
  activeJog: new Map(), stream: 'main', mainFailures: 0, reconnectDelay: 500,
  statusTimer: null, fpsTimer: null, measuredVideo: null,
  lastFrameCount: 0, lastFrameTime: 0, lastFrameAdvanceTime: 0, streamStartedAt: 0,
  lastVideoRecoveryTime: 0, videoRecoveryCount: 0, videoStableSince: 0,
  recordingRequest: 0, autoFocus: null, espOnline: false, orientationLocked: false,
};

const elements = {
  app: document.querySelector('#app'), loginLayer: document.querySelector('#loginLayer'),
  loginForm: document.querySelector('#loginForm'), loginError: document.querySelector('#loginError'),
  motorList: document.querySelector('#motorList'), template: document.querySelector('#motorTemplate'),
  videoFrame: document.querySelector('#videoFrame'), placeholder: document.querySelector('#videoPlaceholder'),
  placeholderText: document.querySelector('#videoPlaceholderText'),
  videoNote: document.querySelector('#videoNote'),
  videoStats: document.querySelector('#videoStats'),
  videoShell: document.querySelector('.video-shell'), controlFullscreen: document.querySelector('#controlFullscreen'),
  exitControlFullscreen: document.querySelector('#exitControlFullscreen'), fullscreenAutofocus: document.querySelector('#fullscreenAutofocus'),
  socketState: document.querySelector('#socketState'), toast: document.querySelector('#toast'),
  recordingDate: document.querySelector('#recordingDate'), recordingList: document.querySelector('#recordingList'),
  recordingSummary: document.querySelector('#recordingSummary'), recordingPlayer: document.querySelector('#recordingPlayer'),
  recordingPlayerEmpty: document.querySelector('#recordingPlayerEmpty'), recordingNow: document.querySelector('#recordingNow'),
};

async function api(path, options = {}) {
  const response = await fetch(path, { cache: 'no-store', ...options });
  if (response.status === 401) {
    showLogin();
    throw new Error('authentication_required');
  }
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.headers.get('content-type')?.includes('json') ? response.json() : response;
}

function showLogin() {
  if (controlFullscreenActive()) exitControlFullscreen();
  elements.loginLayer.classList.remove('hidden');
  elements.app.hidden = true;
  state.socket?.close();
}

async function startApp() {
  try {
    state.config = await api('/api/config');
  } catch (error) {
    if (error.message !== 'authentication_required') showLogin();
    return;
  }
  elements.loginLayer.classList.add('hidden');
  elements.app.hidden = false;
  renderMotors();
  switchStream('main');
  connectControl();
  await refreshStatus();
  clearInterval(state.statusTimer);
  state.statusTimer = window.setInterval(refreshStatus, 2000);
  clearInterval(state.fpsTimer);
  state.fpsTimer = window.setInterval(updateVideoStats, 1000);
  initializeRecordingDate();
  loadRecordings();
}

elements.loginForm.addEventListener('submit', async event => {
  event.preventDefault();
  elements.loginError.textContent = '';
  const button = elements.loginForm.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: elements.loginForm.elements.username.value, password: elements.loginForm.elements.password.value }),
    });
    if (response.status === 429) throw new Error('尝试次数过多，请十分钟后再试');
    if (!response.ok) throw new Error('用户名或密码错误');
    elements.loginForm.elements.password.value = '';
    await startApp();
  } catch (error) {
    elements.loginError.textContent = error.message;
  } finally {
    button.disabled = false;
  }
});

document.querySelector('#logoutButton').addEventListener('click', async () => {
  stopEveryJog();
  try { await api('/api/logout', { method: 'POST' }); } catch (_) {}
  showLogin();
});

function renderMotors() {
  elements.motorList.replaceChildren();
  state.cards.clear();
  for (const motor of state.config.motors) {
    const card = elements.template.content.firstElementChild.cloneNode(true);
    card.dataset.motor = motor.id;
    card.dataset.role = motor.role || 'motor';
    card.querySelector('.motor-index').textContent = `镜头 ${String(motor.id).padStart(2, '0')}`;
    card.querySelector('h3').textContent = motor.name;
    card.querySelector('.min-limit b').textContent = motor.minLimitLabel || motor.negative || '反向限位';
    card.querySelector('.max-limit b').textContent = motor.maxLimitLabel || motor.positive || '正向限位';
    const speed = card.querySelector('.speed');
    const speedValue = card.querySelector('.speed-value');
    speed.value = motor.defaultSpeed;
    speedValue.textContent = `${motor.defaultSpeed} 步/秒`;
    speed.addEventListener('input', () => { speedValue.textContent = `${speed.value} 步/秒`; });
    card.querySelector('.mode').value = motor.defaultMode;

    const negative = card.querySelector('.negative');
    const positive = card.querySelector('.positive');
    negative.querySelector('span').textContent = `◀ ${motor.negative}`;
    positive.querySelector('span').textContent = `${motor.positive} ▶`;
    bindJog(negative, motor.id, 'ccw');
    bindJog(positive, motor.id, 'cw');
    card.querySelector('.motor-stop').addEventListener('click', () => stopMotor(motor.id));

    const nudge = card.querySelector('.nudge-row');
    for (const signedSteps of [-100, -10, -1, 1, 10, 100]) {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = signedSteps > 0 ? `+${signedSteps}` : String(signedSteps);
      button.title = `${signedSteps < 0 ? motor.negative : motor.positive} ${Math.abs(signedSteps)} 步`;
      button.addEventListener('click', () => sendMove(motor.id, signedSteps < 0 ? 'ccw' : 'cw', Math.abs(signedSteps)));
      nudge.append(button);
    }
    if (motor.role === 'focus') {
      const autoFocusBlock = card.querySelector('.autofocus-block');
      autoFocusBlock.hidden = false;
      autoFocusBlock.querySelector('.autofocus-button').addEventListener('click', () => toggleAutoFocus(motor.id));
    }
    elements.motorList.append(card);
    state.cards.set(motor.id, card);
  }
  updateFullscreenControlLabels();
  updateAutoFocusUI();
}

function motorByRole(role) {
  return state.config?.motors.find(motor => motor.role === role);
}

function updateFullscreenControlLabels() {
  for (const button of document.querySelectorAll('.fullscreen-jog')) {
    const motor = motorByRole(button.dataset.role);
    const direction = button.dataset.direction;
    const label = direction === 'ccw' ? motor?.negative : motor?.positive;
    const fallback = button.dataset.role === 'focus' ? (direction === 'ccw' ? '近焦' : '远焦') : (direction === 'ccw' ? '广角' : '长焦');
    button.querySelector('small').textContent = label || fallback;
    button.disabled = !motor;
    button.setAttribute('aria-label', `${motor?.name || (button.dataset.role === 'focus' ? '对焦' : '变焦')} ${label || fallback}，按住运动`);
  }
}

function motorSettings(motor) {
  const card = state.cards.get(motor);
  return {
    speed: Number(card.querySelector('.speed').value),
    mode: card.querySelector('.mode').value,
    hold: card.querySelector('.hold').checked,
  };
}

function bindJog(button, motor, direction) {
  const begin = event => {
    if (event.button !== undefined && event.button !== 0) return;
    event.preventDefault();
    button.setPointerCapture?.(event.pointerId);
    startJog(motor, direction, button);
  };
  const end = event => { event.preventDefault(); endJog(motor, true); };
  button.addEventListener('pointerdown', begin);
  button.addEventListener('pointerup', end);
  button.addEventListener('pointercancel', end);
  button.addEventListener('lostpointercapture', () => endJog(motor, true));
  button.addEventListener('contextmenu', event => event.preventDefault());
}

function startJog(motor, direction, button) {
  endJog(motor, false);
  if (!socketReady()) return notify('控制连接尚未就绪');
  const settings = motorSettings(motor);
  const commandId = newCommandId();
  sendControl({ motor, action: 'jog', direction, commandId, ...settings });
  button.classList.add('active');
  const timer = window.setInterval(() => sendControl({ motor, action: 'heartbeat', commandId: newCommandId() }), 250);
  state.activeJog.set(motor, { button, timer });
}

function endJog(motor, sendStop) {
  const active = state.activeJog.get(motor);
  if (!active) return;
  clearInterval(active.timer);
  active.button.classList.remove('active');
  state.activeJog.delete(motor);
  if (sendStop && socketReady()) {
    const settings = motorSettings(motor);
    sendControl({ motor, action: 'stop', hold: settings.hold, commandId: newCommandId() });
  }
}

function stopMotor(motor) {
  endJog(motor, false);
  if (socketReady()) sendControl({ motor, action: 'stop', hold: motorSettings(motor).hold, commandId: newCommandId() });
}

function sendMove(motor, direction, steps) {
  endJog(motor, false);
  if (!socketReady()) return notify('控制连接尚未就绪');
  sendControl({ motor, action: 'move', direction, steps, commandId: newCommandId(), ...motorSettings(motor) });
}

function bindFullscreenJog(button) {
  const begin = event => {
    if (event.button !== undefined && event.button !== 0) return;
    event.preventDefault();
    const motor = motorByRole(button.dataset.role);
    if (!motor) return notify('当前没有对应的镜头电机');
    button.setPointerCapture?.(event.pointerId);
    startJog(motor.id, button.dataset.direction, button);
  };
  const end = event => {
    event.preventDefault();
    const motor = motorByRole(button.dataset.role);
    if (motor) endJog(motor.id, true);
  };
  button.addEventListener('pointerdown', begin);
  button.addEventListener('pointerup', end);
  button.addEventListener('pointercancel', end);
  button.addEventListener('lostpointercapture', () => {
    const motor = motorByRole(button.dataset.role);
    if (motor) endJog(motor.id, true);
  });
  button.addEventListener('contextmenu', event => event.preventDefault());
}

async function toggleAutoFocus(motor) {
  const active = state.autoFocus?.active && state.autoFocus.motor === motor;
  const card = state.cards.get(motor);
  const button = card?.querySelector('.autofocus-button');
  if (button) button.disabled = true;
  try {
    if (active) {
      state.autoFocus = await api('/api/autofocus', { method: 'DELETE' });
      notify('正在取消自动精调…');
    } else {
      endJog(motor, true);
      state.autoFocus = await api('/api/autofocus', { method: 'POST' });
      notify('已开始自动精调，请保持画面稳定');
    }
  } catch (error) {
    if (error.message !== 'authentication_required') {
      notify(error.message === 'HTTP 409' ? '对焦电机正在使用中' : '无法启动自动精调');
    }
  } finally {
    updateAutoFocusUI();
  }
}

function updateAutoFocusUI() {
  const focus = state.autoFocus;
  for (const [motor, card] of state.cards) {
    if (card.dataset.role !== 'focus') continue;
    const block = card.querySelector('.autofocus-block');
    const button = block?.querySelector('.autofocus-button');
    const label = block?.querySelector('.autofocus-state');
    if (!block || !button || !label) continue;
    const active = Boolean(focus?.active && focus.motor === motor);
    const available = state.config?.capabilities?.autoFocus !== false && focus?.available !== false;
    card.classList.toggle('autofocus-running', active);
    button.disabled = !available || (!active && !state.espOnline);
    button.querySelector('b').textContent = active ? '取消精调' : '自动精调';
    label.textContent = focus?.motor === motor ? focus.message : (available ? '等待启动' : '当前不可用');
    card.querySelectorAll('.jog,.nudge-row button,.speed,.mode,.hold').forEach(control => { control.disabled = active; });
  }
  const active = Boolean(focus?.active);
  const available = state.config?.capabilities?.autoFocus !== false && focus?.available !== false;
  elements.fullscreenAutofocus.disabled = !available || (!active && !state.espOnline);
  elements.fullscreenAutofocus.classList.toggle('running', active);
  elements.fullscreenAutofocus.querySelector('small').textContent = active ? '取消' : '自动';
  elements.fullscreenAutofocus.setAttribute('aria-label', active ? '取消自动精调' : '启动自动精调');
  document.querySelectorAll('.fullscreen-jog').forEach(control => {
    control.disabled = active || !state.espOnline || !motorByRole(control.dataset.role);
  });
}

function controlFullscreenActive() {
  return document.fullscreenElement === elements.videoShell || document.webkitFullscreenElement === elements.videoShell || elements.videoShell.classList.contains('fullscreen-fallback');
}

function portraitTouchscreen() {
  return matchMedia('(orientation: portrait)').matches && navigator.maxTouchPoints > 0;
}

function updateFallbackLandscape() {
  elements.videoShell.classList.toggle('fullscreen-landscape-fallback', elements.videoShell.classList.contains('fullscreen-fallback') && portraitTouchscreen());
}

function enterFallbackFullscreen() {
  elements.videoShell.classList.add('fullscreen-fallback');
  document.body.classList.add('control-fullscreen-active');
  updateFallbackLandscape();
}

async function lockControlLandscape() {
  if (!portraitTouchscreen()) return true;
  if (!screen.orientation?.lock) return false;
  try {
    await screen.orientation.lock('landscape');
    state.orientationLocked = true;
    return true;
  } catch (_) {
    return false;
  }
}

function unlockControlOrientation() {
  if (!state.orientationLocked) return;
  try { screen.orientation?.unlock?.(); } catch (_) {}
  state.orientationLocked = false;
}

async function enterControlFullscreen() {
  const needsLandscape = portraitTouchscreen();
  if (needsLandscape && !screen.orientation?.lock) {
    enterFallbackFullscreen();
    return;
  }
  try {
    if (elements.videoShell.requestFullscreen) await elements.videoShell.requestFullscreen();
    else if (elements.videoShell.webkitRequestFullscreen) elements.videoShell.webkitRequestFullscreen();
    else enterFallbackFullscreen();
    if (needsLandscape && !await lockControlLandscape()) {
      if (document.fullscreenElement && document.exitFullscreen) await document.exitFullscreen();
      else if (document.webkitFullscreenElement && document.webkitExitFullscreen) document.webkitExitFullscreen();
      enterFallbackFullscreen();
    }
  } catch (_) {
    enterFallbackFullscreen();
  }
}

async function exitControlFullscreen() {
  stopEveryJog();
  try {
    if (document.fullscreenElement && document.exitFullscreen) await document.exitFullscreen();
    else if (document.webkitFullscreenElement && document.webkitExitFullscreen) document.webkitExitFullscreen();
  } catch (_) {}
  unlockControlOrientation();
  elements.videoShell.classList.remove('fullscreen-fallback');
  elements.videoShell.classList.remove('fullscreen-landscape-fallback');
  document.body.classList.remove('control-fullscreen-active');
}

function handleFullscreenChange() {
  state.lastFrameAdvanceTime = performance.now();
  if (!controlFullscreenActive()) {
    stopEveryJog();
    unlockControlOrientation();
    elements.videoShell.classList.remove('fullscreen-fallback');
    elements.videoShell.classList.remove('fullscreen-landscape-fallback');
    document.body.classList.remove('control-fullscreen-active');
  }
}

function hideEmbeddedPlayerMode() {
  try {
    const frameDocument = elements.videoFrame.contentDocument;
    if (!frameDocument?.head || frameDocument.querySelector('#lens-player-style')) return;
    const style = frameDocument.createElement('style');
    style.id = 'lens-player-style';
    style.textContent = 'video-stream .info .mode{display:none!important}';
    frameDocument.head.append(style);
  } catch (_) {}
}

function stopEveryJog() {
  for (const motor of [...state.activeJog.keys()]) endJog(motor, true);
}

document.querySelector('#stopAll').addEventListener('click', () => {
  for (const motor of state.config?.motors || []) stopMotor(motor.id);
});
document.querySelector('#releaseAll').addEventListener('click', () => {
  stopEveryJog();
  if (socketReady()) {
    for (const motor of state.config?.motors || []) sendControl({ motor: motor.id, action: 'release', commandId: newCommandId() });
  }
});

function connectControl() {
  state.socket?.close();
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const socket = new WebSocket(`${protocol}//${location.host}/api/control`);
  state.socket = socket;
  setSocketState(false, '连接中');
  socket.addEventListener('open', () => {
    state.reconnectDelay = 500;
    setSocketState(true, '控制在线');
  });
  socket.addEventListener('message', event => {
    const message = JSON.parse(event.data);
    if (message.clientId) state.clientId = message.clientId;
    if (message.leases) {
      state.leases = message.leases;
      updateLeaseUI();
    }
    if (!message.ok && message.message) notify(message.message);
  });
  socket.addEventListener('close', () => {
    stopEveryJog();
    setSocketState(false, '控制断开');
    if (!elements.loginLayer.classList.contains('hidden')) return;
    const delay = state.reconnectDelay;
    state.reconnectDelay = Math.min(5000, delay * 1.7);
    setTimeout(connectControl, delay);
  });
}

function socketReady() { return state.socket?.readyState === WebSocket.OPEN; }
function sendControl(value) { if (socketReady()) state.socket.send(JSON.stringify(value)); }
function newCommandId() { return crypto.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`; }
function setSocketState(online, text) {
  elements.socketState.classList.toggle('online', online);
  elements.socketState.lastChild.textContent = text;
}

function updateLeaseUI() {
  for (const [motor, card] of state.cards) {
    const owner = state.leases[motor];
    const mine = owner?.endsWith(`@${state.clientId}`);
    const autoFocusActive = Boolean(state.autoFocus?.active && state.autoFocus.motor === motor);
    const busy = Boolean(owner && !mine && !autoFocusActive);
    card.classList.toggle('busy', busy);
    const status = card.querySelector('.motor-status');
    if (busy) status.textContent = '其他用户操作中';
    else if (autoFocusActive) status.textContent = '自动精调中';
    else if (mine) status.textContent = '由你控制';
    else if (!status.dataset.running) status.textContent = '空闲';
  }
  updateAutoFocusUI();
}

async function refreshStatus() {
  if (elements.app.hidden) return;
  try {
    const status = await api('/api/status');
    state.espOnline = Boolean(status.esp);
    state.autoFocus = status.autoFocus || state.autoFocus;
    for (const name of ['ipc', 'esp', 'go2rtc']) {
      const online = Boolean(status[name]);
      document.querySelector(`[data-health="${name}"]`)?.classList.toggle('online', online);
      document.querySelector(`[data-health-copy="${name}"]`)?.classList.toggle('online', online);
    }
    for (const motor of status.motors || []) {
      const card = state.cards.get(motor.motor);
      if (!card) continue;
      const label = card.querySelector('.motor-status');
      label.dataset.running = motor.running ? '1' : '';
      label.classList.toggle('active', motor.running);
      if (motor.running) label.textContent = motor.continuous ? '连续运动' : `剩余 ${motor.remainingSteps} 步`;
      else if (!state.leases[motor.motor]) label.textContent = motor.coilsHeld ? '位置保持' : '空闲';
      card.querySelector('.min-limit')?.classList.toggle('triggered', Boolean(motor.minLimit));
      card.querySelector('.max-limit')?.classList.toggle('triggered', Boolean(motor.maxLimit));
      const position = card.querySelector('.position-state b');
      if (position) position.textContent = motor.position === null || motor.position === undefined ? '--' : String(motor.position);
    }
    for (const card of state.cards.values()) card.classList.toggle('offline', !status.esp);
    if (state.stream === 'main' && status.go2rtc) {
      state.mainFailures = status.streams?.main?.online ? 0 : state.mainFailures + 1;
      if (state.mainFailures >= 3) {
        switchStream('sub');
        notify('视频连接不稳定，已自动恢复');
      }
    }
    if (!status.ipc) {
      elements.videoNote.hidden = false;
      elements.videoNote.textContent = '摄像头离线，请检查网络、电源或设备地址';
    } else if (!status.go2rtc) {
      elements.videoNote.hidden = false;
      elements.videoNote.textContent = '视频服务暂时不可用';
    } else {
      elements.videoNote.hidden = true;
      elements.videoNote.textContent = '';
    }
    state.leases = status.leases || state.leases;
    updateLeaseUI();
  } catch (_) {}
}

function switchStream(kind) {
  if (!state.config) return;
  state.videoRecoveryCount = 0;
  state.lastVideoRecoveryTime = 0;
  loadVideoStream(kind);
}

function loadVideoStream(kind) {
  state.stream = kind;
  state.mainFailures = 0;
  resetVideoStats('正在建立视频连接…');
  const source = encodeURIComponent(state.config.streams[kind]);
  elements.videoFrame.src = `/stream/stream.html?src=${source}&mode=webrtc,mse&media=video&_=${Date.now()}`;
}

function videoFrameSample(video) {
  const quality = video.getVideoPlaybackQuality?.();
  if (Number.isFinite(quality?.totalVideoFrames)) return { value: quality.totalVideoFrames, countsFrames: true };
  if (Number.isFinite(video.webkitDecodedFrameCount)) return { value: video.webkitDecodedFrameCount, countsFrames: true };
  if (Number.isFinite(video.currentTime)) return { value: video.currentTime, countsFrames: false };
  return null;
}

function showVideoPlaceholder(message) {
  elements.placeholderText.textContent = message;
  elements.placeholder.classList.remove('hidden');
}

function resetVideoStats(message = '正在等待视频帧…') {
  const now = performance.now();
  state.measuredVideo = null;
  state.lastFrameCount = 0;
  state.lastFrameTime = 0;
  state.lastFrameAdvanceTime = now;
  state.streamStartedAt = now;
  state.videoStableSince = 0;
  elements.videoStats.textContent = '等待视频帧…';
  showVideoPlaceholder(message);
}

function markVideoHealthy(now) {
  elements.placeholder.classList.add('hidden');
  elements.placeholderText.textContent = '正在建立视频连接…';
  if (!state.videoStableSince) state.videoStableSince = now;
  if (now - state.videoStableSince >= VIDEO_STABLE_RESET_MS) state.videoRecoveryCount = 0;
}

function recoverVideo(reason) {
  const now = performance.now();
  if (document.hidden || now - state.lastVideoRecoveryTime < VIDEO_RECOVERY_COOLDOWN_MS) return;
  state.lastVideoRecoveryTime = now;
  state.videoRecoveryCount += 1;
  showVideoPlaceholder('画面卡住，正在重新连接…');
  elements.videoStats.textContent = reason;
  if (state.stream === 'main' && state.videoRecoveryCount >= 2 && state.config?.streams?.sub) {
    notify('画面持续卡顿，正在尝试备用连接');
    switchStream('sub');
    return;
  }
  notify('检测到画面卡住，正在重新连接');
  loadVideoStream(state.stream);
}

function updateVideoStats() {
  if (document.hidden || elements.app.hidden || !state.streamStartedAt) return;
  let video = null;
  try {
    video = elements.videoFrame.contentDocument?.querySelector('video');
  } catch (_) {}

  const now = performance.now();
  const sample = video ? videoFrameSample(video) : null;
  if (!video || sample === null || video.readyState < 2) {
    elements.videoStats.textContent = '等待视频帧…';
    if (now - state.streamStartedAt >= VIDEO_START_RECOVERY_MS) recoverVideo('视频连接超时');
    return;
  }

  if (video.paused) video.play().catch(() => {});
  if (state.measuredVideo !== video || state.lastFrameTime === 0 || sample.value < state.lastFrameCount) {
    state.measuredVideo = video;
    state.lastFrameCount = sample.value;
    state.lastFrameTime = now;
    state.lastFrameAdvanceTime = sample.value > 0 ? now : state.streamStartedAt;
    if (sample.value > 0) markVideoHealthy(now);
    return;
  }

  const elapsed = (now - state.lastFrameTime) / 1000;
  const resolution = video.videoWidth && video.videoHeight ? `${video.videoWidth}×${video.videoHeight} · ` : '';
  const advanced = sample.value > state.lastFrameCount + (sample.countsFrames ? 0 : 0.001);
  if (advanced) {
    const fps = sample.countsFrames && elapsed > 0 ? (sample.value - state.lastFrameCount) / elapsed : null;
    state.lastFrameAdvanceTime = now;
    markVideoHealthy(now);
    elements.videoStats.textContent = fps === null ? `${resolution}播放中` : `${resolution}${fps.toFixed(1)} FPS`;
  } else {
    state.videoStableSince = 0;
    const stalledFor = now - state.lastFrameAdvanceTime;
    if (stalledFor >= VIDEO_STALL_WARNING_MS) {
      const seconds = (stalledFor / 1000).toFixed(0);
      elements.videoStats.textContent = `${resolution}画面停顿 ${seconds} 秒`;
      showVideoPlaceholder(`画面停顿 ${seconds} 秒，正在检测…`);
    }
    if (stalledFor >= VIDEO_STALL_RECOVERY_MS) recoverVideo('画面长时间未更新');
  }
  state.lastFrameCount = sample.value;
  state.lastFrameTime = now;
}

elements.videoFrame.addEventListener('load', () => {
  state.streamStartedAt = performance.now();
  state.lastFrameAdvanceTime = state.streamStartedAt;
  showVideoPlaceholder('正在等待视频帧…');
  hideEmbeddedPlayerMode();
});
document.querySelectorAll('.fullscreen-jog').forEach(bindFullscreenJog);
elements.controlFullscreen.addEventListener('click', enterControlFullscreen);
elements.exitControlFullscreen.addEventListener('click', exitControlFullscreen);
elements.fullscreenAutofocus.addEventListener('click', () => {
  const focusMotor = motorByRole('focus');
  if (focusMotor) toggleAutoFocus(focusMotor.id);
  else notify('当前没有对焦电机');
});
document.addEventListener('fullscreenchange', handleFullscreenChange);
document.addEventListener('webkitfullscreenchange', handleFullscreenChange);
window.addEventListener('resize', updateFallbackLandscape);
window.addEventListener('orientationchange', updateFallbackLandscape);
function localDateValue(date) {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function initializeRecordingDate() {
  if (!elements.recordingDate.value) elements.recordingDate.value = localDateValue(new Date());
  elements.recordingDate.max = localDateValue(new Date());
  updateRecordingDateButtons();
}

function updateRecordingDateButtons() {
  document.querySelector('#nextRecordingDay').disabled = elements.recordingDate.value >= localDateValue(new Date());
}

function changeRecordingDay(offset) {
  const date = new Date(`${elements.recordingDate.value}T12:00:00`);
  date.setDate(date.getDate() + offset);
  const today = localDateValue(new Date());
  elements.recordingDate.value = localDateValue(date) > today ? today : localDateValue(date);
  updateRecordingDateButtons();
  loadRecordings();
}

async function loadRecordings() {
  if (!elements.recordingDate.value || elements.app.hidden) return;
  const requestId = ++state.recordingRequest;
  const start = new Date(`${elements.recordingDate.value}T00:00:00`);
  const end = new Date(start);
  end.setDate(end.getDate() + 1);
  elements.recordingSummary.textContent = '正在读取录像…';
  elements.recordingList.innerHTML = '<div class="recording-list-message">加载中…</div>';
  try {
    const query = new URLSearchParams({ start: start.toISOString(), end: end.toISOString() });
    const result = await api(`/api/recordings?${query}`);
    if (requestId !== state.recordingRequest) return;
    renderRecordings(result.recordings || []);
  } catch (error) {
    if (requestId !== state.recordingRequest || error.message === 'authentication_required') return;
    elements.recordingSummary.textContent = '录像服务离线';
    elements.recordingList.innerHTML = '<div class="recording-list-message">无法读取录像，请检查录像服务和存储设备。</div>';
  }
}

function renderRecordings(recordings) {
  elements.recordingList.replaceChildren();
  elements.recordingSummary.textContent = recordings.length ? `${recordings.length} 个录像片段` : '当天暂无录像';
  if (!recordings.length) {
    const message = document.createElement('div');
    message.className = 'recording-list-message';
    message.textContent = '这个日期没有可回放的录像。';
    elements.recordingList.append(message);
    return;
  }
  const timeFormat = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  for (const recording of recordings) {
    const start = new Date(recording.start);
    const end = new Date(start.getTime() + recording.duration * 1000);
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'recording-item';
    button.innerHTML = `<span>${timeFormat.format(start)} – ${timeFormat.format(end)}</span><small>${Math.ceil(recording.duration / 60)} 分钟</small>`;
    button.addEventListener('click', () => playRecording(recording, button));
    elements.recordingList.append(button);
  }
}

function playRecording(recording, button) {
  document.querySelectorAll('.recording-item').forEach(item => item.classList.toggle('active', item === button));
  const query = new URLSearchParams({ start: recording.start, duration: String(recording.duration) });
  elements.recordingPlayer.src = `/api/recordings/play?${query}`;
  elements.recordingPlayerEmpty.classList.add('hidden');
  const start = new Date(recording.start);
  elements.recordingNow.textContent = `正在回放：${start.toLocaleString('zh-CN', { hour12: false })}`;
  elements.recordingPlayer.play().catch(() => {});
}

elements.recordingDate.addEventListener('change', () => { updateRecordingDateButtons(); loadRecordings(); });
document.querySelector('#previousRecordingDay').addEventListener('click', () => changeRecordingDay(-1));
document.querySelector('#nextRecordingDay').addEventListener('click', () => changeRecordingDay(1));
document.querySelector('#refreshRecordings').addEventListener('click', loadRecordings);

function notify(message) {
  elements.toast.textContent = message;
  elements.toast.classList.add('show');
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => elements.toast.classList.remove('show'), 2600);
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) stopEveryJog();
  else if (!elements.app.hidden) resetVideoStats('正在恢复视频…');
});
window.addEventListener('pagehide', stopEveryJog);
startApp();
