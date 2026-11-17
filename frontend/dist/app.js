// darkbot UI. Vanilla JS; talks to Go through Wails bindings
// (window.go.main.App) and events (window.runtime). No network port.
'use strict';

const App = () => window.go.main.App;
const $ = (id) => document.getElementById(id);
const IS_WINDOWS = navigator.userAgent.includes('Windows');
const SINK_SUPPORTED = typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
const TRACK_COUNT = 6;
const FEED_MAX = 120;
const DEFAULT_COST = 100;
const TOAST_MS = 5000;
const CONFIRM_MS = 4000;

let state = null;
let rewards = [];
let devices = [];
let editing = null;
let obsPassDirty = false;
const playing = new Set();          // sound IDs currently playing
const cooldowns = new Map();        // sound ID -> { until: ms, total: ms }

// ---------- helpers ----------

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children) if (c != null) e.append(c);
  return e;
}
function icon(name) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const u = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  u.setAttribute('href', '#i-' + name);
  s.append(u);
  return s;
}
const pct = (v) => Math.round(v * 100) + ' %';
const points = (n) => Number(n).toLocaleString() + ' points';
const basename = (p) => (p || '').split(/[\\/]/).pop();
const errText = (e) => (e && e.message) || String(e);
const clock = (d) => d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

let toastTimer = null;
function toast(msg, bad = false) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.toggle('bad', bad);
  t.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add('hidden'), TOAST_MS);
}

async function call(fn, ...args) {
  try {
    return await App()[fn](...args);
  } catch (e) {
    toast(errText(e), true);
    throw e;
  }
}

// ---------- state ----------

async function refresh() {
  state = await App().GetState();
  render();
}

function render() {
  const c = state.config;
  const loggedIn = !!state.twitchLogin;

  setSignal('sigTwitch', loggedIn, loggedIn ? state.twitchLogin : 'Twitch');
  setSignal('sigEvents', state.eventSubOnline, 'Redemptions');
  setSignal('sigObs', state.obsConnected, 'OBS');
  $('sigObs').classList.toggle('hidden', c.output_mode !== 'obs' && !state.obsConnected);

  const up = state.update;
  $('updateChip').classList.toggle('hidden', !(up && up.available));
  if (up && up.available) $('updateChip').textContent = state.updating ? 'Updating…' : 'Update to v' + up.latest;

  $('onboard').classList.toggle('hidden', loggedIn);
  $('reconnect').classList.toggle('hidden', !loggedIn || !state.needsReconnect);

  renderPads();
  renderStrip();
  if ($('settings').open) renderSettings();
}

function setSignal(id, on, text) {
  const s = $(id);
  s.classList.toggle('on', on);
  s.querySelector('span').textContent = text;
}

// ---------- pads ----------

function soundName(s) {
  const r = rewards.find((x) => x.id === s.reward_id);
  return (r && r.title) || s.reward_title || 'Unnamed reward';
}

function renderPads() {
  const sounds = state.config.sounds || [];
  const loggedIn = !!state.twitchLogin;
  $('soundCount').textContent = sounds.length ? String(sounds.length) : '';
  $('padsEmpty').classList.toggle('hidden', sounds.length > 0 || !loggedIn);
  $('boardHint').classList.toggle('hidden', sounds.length === 0);
  $('addSound').classList.toggle('hidden', !loggedIn && sounds.length === 0);

  const pads = $('pads');
  pads.replaceChildren();
  for (const s of sounds) {
    const cdText = s.enabled && s.cooldown_seconds ? s.cooldown_seconds + ' s' : '';
    const pad = el('div', {
      class: 'pad' + (s.enabled ? '' : ' off') + (playing.has(s.id) ? ' playing' : ''),
      role: 'button', tabindex: '0', 'data-id': s.id,
      'aria-label': 'Preview ' + soundName(s),
      onclick: () => App().TestSound(s.id),
      onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); App().TestSound(s.id); } },
    },
    el('span', { class: 'led', 'aria-hidden': 'true' }),
    el('span', { class: 'cd' }, cdText),
    el('button', {
      class: 'edit', title: 'Edit sound', 'aria-label': 'Edit ' + soundName(s),
      onclick: (e) => { e.stopPropagation(); openEditor(s); },
      onkeydown: (e) => e.stopPropagation(),
    }, icon('edit')),
    el('span', { class: 'name' }, soundName(s)),
    el('span', { class: 'file', title: s.file }, s.managed ? points(s.reward_cost) : basename(s.file)));
    pads.append(pad);
  }
  tickCooldowns();
}

// Cooldown sweep: animate only while at least one pad is cooling down.
let rafId = 0;
function tickCooldowns() {
  cancelAnimationFrame(rafId);
  const now = Date.now();
  let active = false;
  for (const pad of document.querySelectorAll('.pad')) {
    const cd = cooldowns.get(pad.dataset.id);
    if (cd && cd.until > now) {
      active = true;
      pad.classList.add('cooling');
      pad.style.setProperty('--cd', ((cd.until - now) / cd.total).toFixed(4));
    } else {
      pad.classList.remove('cooling');
      if (cd) cooldowns.delete(pad.dataset.id);
    }
  }
  if (active) rafId = requestAnimationFrame(tickCooldowns);
}

// ---------- activity feed ----------

const FEED_TEXT = {
  queued: (a) => [a.user, ' redeemed ', a.rewardTitle, null],
  cooldown: (a) => [a.user, ' redeemed ', a.rewardTitle, 'On cooldown for ' + a.message + ', not played'],
  disabled: (a) => [a.user, ' redeemed ', a.rewardTitle, 'Sound is off, not played'],
  queue_full: (a) => [a.user, ' redeemed ', a.rewardTitle, 'Too many sounds waiting, not played'],
  error: (a) => [a.test ? 'Preview' : a.user, a.test ? ' of ' : ' redeemed ', a.rewardTitle, a.message || 'Playback failed'],
};

function onActivity(a) {
  if (a.kind === 'playing') playing.add(a.soundId);
  if (a.kind === 'done' || a.kind === 'error') playing.delete(a.soundId);
  const until = Date.parse(a.cooldownUntil);
  if (a.cooldownSeconds > 0 && until > Date.now()) {
    cooldowns.set(a.soundId, { until, total: a.cooldownSeconds * 1000 });
  }
  const pad = document.querySelector('.pad[data-id="' + CSS.escape(a.soundId) + '"]');
  if (pad) pad.classList.toggle('playing', playing.has(a.soundId));
  tickCooldowns();

  const fmt = FEED_TEXT[a.kind];
  if (!fmt) return;
  const [who, verb, what, why] = fmt(a);
  const kindClass = a.kind === 'queued' ? 'played' : a.kind === 'error' ? 'error' : 'skipped';
  const item = el('li', { class: 'feed-item fresh ' + kindClass },
    el('time', { datetime: a.at }, clock(new Date(a.at))),
    el('span', { class: 'what' }, el('b', {}, who), verb, el('b', {}, what)),
    why ? el('span', { class: 'why' }, why) : null);
  const feed = $('feed');
  feed.prepend(item);
  while (feed.children.length > FEED_MAX) feed.lastChild.remove();
  $('feedEmpty').classList.add('hidden');
  requestAnimationFrame(() => requestAnimationFrame(() => item.classList.remove('fresh')));
}

// ---------- strip ----------

function outputLabel() {
  const c = state.config;
  if (c.output_mode === 'obs') {
    const tracks = c.obs.tracks.map((on, i) => (on ? i + 1 : null)).filter(Boolean);
    return 'OBS, track ' + (tracks.join(', ') || 'none');
  }
  return c.audio_device_name || 'System default output';
}

function renderStrip() {
  $('outputName').textContent = outputLabel();
  if (document.activeElement !== $('master')) $('master').value = state.config.master_volume;
  $('masterVal').textContent = pct(+$('master').value);
}

$('master').addEventListener('input', () => { $('masterVal').textContent = pct(+$('master').value); });
$('master').addEventListener('change', () => call('SetMasterVolume', +$('master').value));

// ---------- settings sheet ----------

function openSettings(section) {
  showSection(section || 'audio');
  renderSettings();
  if (!$('settings').open) $('settings').showModal();
}
function showSection(name) {
  document.querySelectorAll('.sheet-nav button').forEach((b) => b.classList.toggle('active', b.dataset.sec === name));
  document.querySelectorAll('.sec').forEach((s) => s.classList.toggle('active', s.id === 'sec-' + name));
  if (name === 'log') renderLog();
}
document.querySelectorAll('.sheet-nav button').forEach((b) => b.addEventListener('click', () => showSection(b.dataset.sec)));
$('openSettings').addEventListener('click', () => openSettings());
$('stripOutput').addEventListener('click', () => openSettings('audio'));
$('closeSettings').addEventListener('click', () => $('settings').close());
$('onboardConnect').addEventListener('click', () => openSettings('twitch'));
$('reconnectBtn').addEventListener('click', () => openSettings('twitch'));
$('updateChip').addEventListener('click', () => openSettings('updates'));

function renderSettings() {
  renderAudio();
  renderTwitch();
  renderOBS();
  renderUpdates();
}

// audio
function renderAudio() {
  const c = state.config;
  const touched = !!$('sec-audio').dataset.touched;
  if (!touched) {
    document.querySelectorAll('input[name=mode]').forEach((r) => { r.checked = r.value === c.output_mode; });
  }
  showModeBlocks();

  const sel = $('device');
  const keep = touched ? sel.value : c.audio_device_id;
  sel.replaceChildren(el('option', { value: '' }, 'System default output'));
  let found = !keep;
  devices.forEach((d, i) => {
    sel.append(el('option', { value: d.deviceId, selected: d.deviceId === keep }, d.label || 'Audio output ' + (i + 1)));
    if (d.deviceId === keep) found = true;
  });
  if (!found) sel.append(el('option', { value: keep, selected: true }, (c.audio_device_name || 'Saved device') + ' (not connected)'));

  const canChoose = SINK_SUPPORTED && devices.length > 0;
  sel.disabled = !canChoose && !c.audio_device_id;
  $('refreshDevices').disabled = !SINK_SUPPORTED;
  $('deviceHint').textContent = canChoose
    ? 'To get it into OBS, pick a virtual cable or spare output and add it as Audio Output Capture. Or keep the default and add darkbot itself with Application Audio Capture.'
    : 'This system doesn’t let darkbot list outputs, so sounds play on the system default. In OBS add darkbot with Application Audio Capture (Windows) or macOS Audio Capture, or choose “Inside OBS”.';

  const tracks = $('tracks');
  if (!tracks.children.length) {
    for (let i = 0; i < TRACK_COUNT; i++) {
      tracks.append(el('label', { class: 'track', title: 'Track ' + (i + 1) },
        el('input', { type: 'checkbox', 'data-track': i, 'aria-label': 'Track ' + (i + 1) }), String(i + 1)));
    }
  }
  if (!touched) {
    tracks.querySelectorAll('input').forEach((cb) => { cb.checked = !!c.obs.tracks[+cb.dataset.track]; });
    $('obsInput').value = c.obs.input_name;
  }
}
function showModeBlocks() {
  const mode = (document.querySelector('input[name=mode]:checked') || {}).value;
  $('deviceBlock').classList.toggle('hidden', mode !== 'device');
  $('obsBlock').classList.toggle('hidden', mode !== 'obs');
}
$('sec-audio').addEventListener('input', () => { $('sec-audio').dataset.touched = '1'; });
$('sec-audio').addEventListener('change', () => { $('sec-audio').dataset.touched = '1'; });
document.querySelectorAll('input[name=mode]').forEach((r) => r.addEventListener('change', showModeBlocks));
$('refreshDevices').addEventListener('click', async () => { await loadDevices(true); renderAudio(); });

$('saveAudio').addEventListener('click', async () => {
  const mode = document.querySelector('input[name=mode]:checked').value;
  const sel = $('device');
  const name = sel.value ? sel.options[sel.selectedIndex].textContent.replace(/ \(not connected\)$/, '') : '';
  await call('SaveOutput', mode, sel.value, name, state.config.master_volume);
  await saveOBS();
  delete $('sec-audio').dataset.touched;
  await refresh();
  toast(mode === 'obs' && !state.obsConnected
    ? 'Audio output saved. Connect OBS in the OBS section to play sounds there.'
    : 'Audio output saved.');
});

async function loadDevices(askPermission) {
  devices = [];
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return;
  let outs = (await navigator.mediaDevices.enumerateDevices()).filter((d) => d.kind === 'audiooutput');
  // WebView2 (Windows) hides device names until microphone access was
  // allowed once. The stream is closed immediately; nothing is recorded.
  // On macOS/Linux we never ask.
  if (askPermission && IS_WINDOWS && outs.length && outs.every((d) => !d.label)) {
    try {
      const s = await navigator.mediaDevices.getUserMedia({ audio: true });
      s.getTracks().forEach((t) => t.stop());
      outs = (await navigator.mediaDevices.enumerateDevices()).filter((d) => d.kind === 'audiooutput');
    } catch (e) { /* declined: unnamed devices stay usable */ }
  }
  devices = outs.filter((d) => d.deviceId && d.deviceId !== 'default' && d.deviceId !== 'communications');
}

// twitch
function renderTwitch() {
  const loggedIn = !!state.twitchLogin;
  let status = 'Not connected. Connect to let viewers play sounds with channel points.';
  if (loggedIn) {
    status = state.needsReconnect
      ? 'Connected as ' + state.twitchLogin + '. Disconnect and connect again so darkbot can create rewards.'
      : 'Connected as ' + state.twitchLogin + (state.eventSubOnline ? ', listening for redemptions.' : ', connecting to redemptions…');
  }
  $('twitchStatus').textContent = status;
  $('login').classList.toggle('hidden', loggedIn);
  $('logout').classList.toggle('hidden', !loggedIn);
  $('loginBox').classList.toggle('hidden', !state.loginUserCode);
  $('loginCode').textContent = state.loginUserCode || '';
  if (document.activeElement !== $('clientId')) $('clientId').value = state.config.twitch_client_id || '';
  if (!state.config.twitch_client_id) $('sec-twitch').querySelector('details').open = true;
}
$('login').addEventListener('click', async () => {
  if ($('clientId').value.trim() !== (state.config.twitch_client_id || '')) await call('SetClientID', $('clientId').value);
  await call('StartLogin');
  refresh();
});
$('cancelLogin').addEventListener('click', () => App().CancelLogin());
$('loginLink').addEventListener('click', () => { if (state.loginUri) window.runtime.BrowserOpenURL(state.loginUri); });
$('logout').addEventListener('click', async () => { await call('Logout'); refresh(); });
$('saveClientId').addEventListener('click', async () => { await call('SetClientID', $('clientId').value); refresh(); toast('Client ID saved.'); });

// obs
function renderOBS() {
  const o = state.config.obs;
  if (!$('sec-obs').contains(document.activeElement)) {
    $('obsHost').value = o.host;
    $('obsPort').value = o.port;
    $('obsAuto').checked = o.auto_connect;
  }
  $('obsStatus').textContent = state.obsConnected ? 'Connected' : 'Not connected';
  $('obsConnect').textContent = state.obsConnected ? 'Save and reconnect' : 'Save and connect';
  $('obsDisconnect').classList.toggle('hidden', !state.obsConnected);
}
$('obsPass').addEventListener('input', () => { obsPassDirty = true; });
async function saveOBS() {
  const tracks = [...$('tracks').querySelectorAll('input')].map((cb) => cb.checked);
  await call('SaveOBS', {
    host: $('obsHost').value,
    port: parseInt($('obsPort').value, 10) || 0,
    password: $('obsPass').value,
    passwordChanged: obsPassDirty,
    autoConnect: $('obsAuto').checked,
    inputName: $('obsInput').value,
    tracks,
  });
  obsPassDirty = false;
  $('obsPass').value = '';
}
$('obsConnect').addEventListener('click', async () => {
  await saveOBS();
  $('obsStatus').textContent = 'Connecting…';
  try { await App().ConnectOBS(); toast('Connected to OBS.'); } catch (e) { toast(errText(e), true); }
  refresh();
});
$('obsDisconnect').addEventListener('click', async () => { await App().DisconnectOBS(); refresh(); });

// updates
function renderUpdates() {
  const up = state.update;
  const dev = state.version === 'dev';
  let text = dev ? 'Development build. Builds made on this computer don’t update themselves.' : 'You have version ' + state.version + '.';
  if (up && up.available) text = 'Version ' + up.latest + ' is available. You have ' + state.version + '.';
  else if (up && !dev) text += ' This is the latest version.';
  $('updateStatus').textContent = text;
  $('autoUpdate').checked = state.config.auto_update;
  $('installUpdate').classList.toggle('hidden', !(up && up.available));
  $('installUpdate').disabled = state.updating;
  $('installUpdate').textContent = state.updating ? 'Installing…' : 'Install and restart';
  $('updateNotes').classList.toggle('hidden', !(up && up.available && up.notes));
  $('updateNotes').textContent = (up && up.notes) || '';
}
$('autoUpdate').addEventListener('change', () => call('SetAutoUpdate', $('autoUpdate').checked).then(refresh));
$('checkUpdate').addEventListener('click', async () => {
  $('checkUpdate').disabled = true;
  try {
    const info = await App().CheckUpdate();
    toast(info.available ? 'Version ' + info.latest + ' is available.' : 'darkbot is up to date.');
  } catch (e) {
    toast(errText(e), true);
  } finally {
    $('checkUpdate').disabled = false;
    refresh();
  }
});
$('installUpdate').addEventListener('click', () => call('InstallUpdate').catch(() => refresh()));

// log
function renderLog() {
  App().GetLogs().then((lines) => {
    const pre = $('log');
    const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent = (lines || []).join('\n');
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  });
}

// ---------- sound editor ----------

async function loadRewards() {
  $('edRewardHint').textContent = 'Loading rewards…';
  try {
    rewards = (await App().GetRewards()) || [];
    $('edRewardHint').textContent = rewards.length ? '' : 'Your channel has no custom rewards yet.';
  } catch (e) {
    rewards = [];
    $('edRewardHint').textContent = errText(e);
  }
}

function fillRewardSelect(selectedId, selectedTitle) {
  const sel = $('edReward');
  const usedByOthers = new Set((state.config.sounds || []).filter((s) => !editing || s.id !== editing.id).map((s) => s.reward_id));
  sel.replaceChildren(el('option', { value: '' }, 'Choose a reward'));
  let found = false;
  for (const r of rewards) {
    const taken = usedByOthers.has(r.id);
    const label = r.title + ' (' + points(r.cost) + ')' + (r.is_enabled ? '' : ', hidden on Twitch') + (taken ? ', already has a sound' : '');
    sel.append(el('option', { value: r.id, selected: r.id === selectedId, disabled: taken }, label));
    if (r.id === selectedId) found = true;
  }
  if (selectedId && !found) sel.append(el('option', { value: selectedId, selected: true }, (selectedTitle || 'Saved reward') + ' (not found)'));
}

function editorSource() { return document.querySelector('input[name=edSource]:checked').value; }
function showSourceBlocks() {
  const isNew = editorSource() === 'new';
  $('edNewBlock').classList.toggle('hidden', !isNew);
  $('edExistingBlock').classList.toggle('hidden', isNew);
}
document.querySelectorAll('input[name=edSource]').forEach((r) => r.addEventListener('change', async () => {
  showSourceBlocks();
  if (editorSource() === 'existing' && !rewards.length) {
    await loadRewards();
    fillRewardSelect(editing.reward_id, editing.reward_title);
  }
}));

async function openEditor(s) {
  const isNew = !s;
  editing = s ? { ...s } : {
    id: '', reward_id: '', reward_title: '', managed: true, reward_cost: DEFAULT_COST,
    file: '', volume: 1, cooldown_seconds: 0, user_cooldown_seconds: 0, enabled: true,
  };
  $('editorTitle').textContent = isNew ? 'Add sound' : 'Edit sound';
  $('edSave').textContent = isNew ? 'Add sound' : 'Save sound';
  $('edError').classList.add('hidden');
  document.querySelector('input[name=edSource][value=' + (editing.managed ? 'new' : 'existing') + ']').checked = true;
  // A saved sound keeps its reward type; delete and re-add to switch.
  document.querySelectorAll('input[name=edSource]').forEach((r) => { r.disabled = !isNew; });
  showSourceBlocks();
  $('edTitle').value = editing.managed ? editing.reward_title : '';
  $('edCost').value = editing.reward_cost || DEFAULT_COST;
  $('edFile').value = editing.file;
  $('edVolume').value = editing.volume;
  $('edVolumeVal').textContent = pct(editing.volume);
  $('edCooldown').value = editing.cooldown_seconds;
  $('edUserCooldown').value = editing.user_cooldown_seconds;
  $('edEnabled').checked = editing.enabled;
  $('edDelete').classList.toggle('hidden', isNew);
  $('edDelete').textContent = 'Delete sound';
  deleteArmed = 0;
  fillRewardSelect(editing.reward_id, editing.reward_title);
  $('editor').showModal();
  (editing.managed ? $('edTitle') : $('edReward')).focus();
  if (!editing.managed && state.twitchLogin) {
    await loadRewards();
    fillRewardSelect(editing.reward_id, editing.reward_title);
  }
}

$('addSound').addEventListener('click', () => openEditor(null));
$('emptyAdd').addEventListener('click', () => openEditor(null));
$('edCancel').addEventListener('click', () => $('editor').close());
$('edRefreshRewards').addEventListener('click', async () => { await loadRewards(); fillRewardSelect($('edReward').value); });
$('edVolume').addEventListener('input', () => { $('edVolumeVal').textContent = pct(+$('edVolume').value); });
$('edPick').addEventListener('click', async () => {
  const f = await call('PickSoundFile');
  if (!f) return;
  $('edFile').value = f;
  // Suggest a reward name from the file name.
  if (editorSource() === 'new' && !$('edTitle').value) {
    $('edTitle').value = basename(f).replace(/\.[^.]+$/, '').replace(/[_-]+/g, ' ').trim().slice(0, 45);
  }
});

$('edSave').addEventListener('click', async () => {
  const managed = editorSource() === 'new';
  const rid = managed ? editing.reward_id : $('edReward').value;
  const r = rewards.find((x) => x.id === rid);
  const s = {
    ...editing,
    managed,
    reward_id: rid,
    reward_title: managed ? $('edTitle').value.trim() : (r ? r.title : editing.reward_title),
    reward_cost: managed ? Math.max(1, parseInt($('edCost').value, 10) || 0) : 0,
    file: $('edFile').value,
    volume: +$('edVolume').value,
    cooldown_seconds: Math.max(0, parseInt($('edCooldown').value, 10) || 0),
    user_cooldown_seconds: Math.max(0, parseInt($('edUserCooldown').value, 10) || 0),
    enabled: $('edEnabled').checked,
  };
  $('edSave').disabled = true;
  try {
    await App().SaveSound(s);
    $('editor').close();
    toast(managed && !editing.reward_id ? 'Reward “' + s.reward_title + '” created on Twitch.' : 'Sound saved.');
    refresh();
  } catch (e) {
    $('edError').textContent = errText(e);
    $('edError').classList.remove('hidden');
  } finally {
    $('edSave').disabled = false;
  }
});

// Two-step delete instead of window.confirm (native dialogs block the webview).
let deleteArmed = 0;
$('edDelete').addEventListener('click', async () => {
  if (Date.now() - deleteArmed > CONFIRM_MS) {
    deleteArmed = Date.now();
    $('edDelete').textContent = editing.managed ? 'Click again, this also deletes the reward on Twitch' : 'Click again to delete';
    return;
  }
  deleteArmed = 0;
  try {
    await App().DeleteSound(editing.id);
    $('editor').close();
    toast('Sound deleted.');
    refresh();
  } catch (e) {
    $('edError').textContent = errText(e);
    $('edError').classList.remove('hidden');
  }
});

// ---------- playback (audio-device mode) ----------

async function playSound(req) {
  const audio = new Audio();
  let finished = false;
  const done = (err) => {
    if (finished) return;
    finished = true;
    App().SoundDone(req.token, err || '');
  };
  audio.addEventListener('ended', () => done(''));
  audio.addEventListener('error', () => done('the app window could not play this file'));
  try {
    if (req.deviceId) {
      if (!SINK_SUPPORTED) throw new Error('choosing an output device is not supported on this system');
      await audio.setSinkId(req.deviceId);
    }
    audio.volume = Math.min(1, Math.max(0, req.volume));
    audio.src = req.url;
    await audio.play();
  } catch (e) {
    done(errText(e));
  }
}

// ---------- boot ----------

window.addEventListener('DOMContentLoaded', async () => {
  window.runtime.EventsOn('state', refresh);
  window.runtime.EventsOn('play', playSound);
  window.runtime.EventsOn('activity', onActivity);
  window.runtime.EventsOn('log', () => { if ($('settings').open && $('sec-log').classList.contains('active')) renderLog(); });
  await loadDevices(false);
  App().ReportAudioSupport(SINK_SUPPORTED, devices.length, devices.filter((d) => d.label).length);
  await refresh();
  if (state.twitchLogin) {
    try { rewards = (await App().GetRewards()) || []; renderPads(); } catch (e) { /* shown when editing */ }
  }
});
