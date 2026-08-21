const state = {
  config: null, socket: null, clientId: '', leases: {}, cards: new Map(),
  activeJog: new Map(), stream: 'main', mainFailures: 0, reconnectDelay: 500,
  statusTimer: null, fpsTimer: null, measuredVideo: null,
  lastFrameCount: 0, lastFrameTime: 0,
  recordingRequest: 0,
};

const elements = {
  app: document.querySelector('#app'), loginLayer: document.querySelector('#loginLayer'),
  loginForm: document.querySelector('#loginForm'), loginError: document.querySelector('#loginError'),
  motorList: document.querySelector('#motorList'), template: document.querySelector('#motorTemplate'),
  videoFrame: document.querySelector('#videoFrame'), placeholder: document.querySelector('#videoPlaceholder'),
  streamLabel: document.querySelector('#streamLabel'), videoNote: document.querySelector('#videoNote'),
  videoStats: document.querySelector('#videoStats'),
  socketState: document.querySelector('#socketState'), toast: document.querySelector('#toast'),
  snapshotDialog: document.querySelector('#snapshotDialog'), snapshotImage: document.querySelector('#snapshotImage'),
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
  switchStream('main', false);
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
    card.querySelector('.motor-index').textContent = `MOTOR ${String(motor.id).padStart(2, '0')}`;
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
    elements.motorList.append(card);
    state.cards.set(motor.id, card);
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
    const busy = Boolean(owner && !mine);
    card.classList.toggle('busy', busy);
    const status = card.querySelector('.motor-status');
    if (busy) status.textContent = '其他用户操作中';
    else if (mine) status.textContent = '由你控制';
    else if (!status.dataset.running) status.textContent = '空闲';
  }
}

async function refreshStatus() {
  if (elements.app.hidden) return;
  try {
    const status = await api('/api/status');
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
      else if (!state.leases[motor.motor]) label.textContent = motor.coilsHeld ? '线圈保持' : '空闲';
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
        notify('主码流未就绪，已自动切换到流畅模式');
      }
    }
    if (!status.ipc) elements.videoNote.textContent = 'IPC 离线，请检查网线、电源或地址';
    else if (!status.go2rtc) elements.videoNote.textContent = '视频网关离线，请检查 go2rtc 服务';
    else elements.videoNote.textContent = `WebRTC · H.264 · 无音频 · ${state.stream === 'main' ? '主码流' : '子码流'}`;
    state.leases = status.leases || state.leases;
    updateLeaseUI();
  } catch (_) {}
}

function switchStream(kind, announce = true) {
  if (!state.config) return;
  state.stream = kind;
  state.mainFailures = 0;
  document.querySelectorAll('[data-stream]').forEach(button => button.classList.toggle('active', button.dataset.stream === kind));
  elements.streamLabel.textContent = kind === 'main' ? '主码流' : '子码流';
  elements.placeholder.classList.remove('hidden');
  resetVideoStats();
  const source = encodeURIComponent(state.config.streams[kind]);
  elements.videoFrame.src = `/stream/stream.html?src=${source}&mode=webrtc&media=video`;
  if (announce) notify(kind === 'main' ? '已切换高清主码流' : '已切换流畅子码流');
}

function decodedFrameCount(video) {
  const quality = video.getVideoPlaybackQuality?.();
  if (Number.isFinite(quality?.totalVideoFrames)) return quality.totalVideoFrames;
  if (Number.isFinite(video.webkitDecodedFrameCount)) return video.webkitDecodedFrameCount;
  return null;
}

function resetVideoStats() {
  state.measuredVideo = null;
  state.lastFrameCount = 0;
  state.lastFrameTime = 0;
  elements.videoStats.textContent = '等待视频帧…';
}

function updateVideoStats() {
  let video = null;
  try {
    video = elements.videoFrame.contentDocument?.querySelector('video');
  } catch (_) {}

  const frameCount = video ? decodedFrameCount(video) : null;
  if (!video || frameCount === null || video.readyState < 2) {
    resetVideoStats();
    return;
  }

  const now = performance.now();
  if (state.measuredVideo !== video || state.lastFrameTime === 0 || frameCount < state.lastFrameCount) {
    state.measuredVideo = video;
    state.lastFrameCount = frameCount;
    state.lastFrameTime = now;
    return;
  }

  const elapsed = (now - state.lastFrameTime) / 1000;
  const fps = elapsed > 0 ? (frameCount - state.lastFrameCount) / elapsed : 0;
  const resolution = video.videoWidth && video.videoHeight ? `${video.videoWidth}×${video.videoHeight} · ` : '';
  elements.videoStats.textContent = `${resolution}${fps.toFixed(1)} FPS`;
  state.lastFrameCount = frameCount;
  state.lastFrameTime = now;
}

document.querySelectorAll('[data-stream]').forEach(button => button.addEventListener('click', () => switchStream(button.dataset.stream)));
elements.videoFrame.addEventListener('load', () => setTimeout(() => elements.placeholder.classList.add('hidden'), 700));
document.querySelector('#fullscreenButton').addEventListener('click', () => document.querySelector('#videoShell').requestFullscreen?.());
document.querySelector('#snapshotButton').addEventListener('click', () => {
  elements.snapshotImage.src = `/api/snapshot?t=${Date.now()}`;
  elements.snapshotDialog.showModal();
});
document.querySelector('#closeSnapshot').addEventListener('click', () => elements.snapshotDialog.close());

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
    elements.recordingList.innerHTML = '<div class="recording-list-message">无法读取录像，请检查 MediaMTX 和录像硬盘。</div>';
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

document.addEventListener('visibilitychange', () => { if (document.hidden) stopEveryJog(); });
window.addEventListener('pagehide', stopEveryJog);
startApp();
