// darkbot frontend: vanilla JS, talks to Go via Wails bindings (window.go.main.App)
// and events (window.runtime). No network port is involved.
'use strict';

const App = () => window.go.main.App;
const $ = (id) => document.getElementById(id);
const IS_WINDOWS = navigator.userAgent.includes('Windows');
const SINK_SUPPORTED = typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
const TRACK_COUNT = 6;

let state = null;
let rewards = [];
let editing = null;
let obsPassDirty = false;

// ---------- helpers ----------

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const c of children) e.append(c);
  return e;
}

function pct(v) { return Math.round(v * 100) + ' %'; }
function basename(p) { return (p || '').split(/[\\/]/).pop(); }
function errText(e) { return (e && e.message) || String(e); }

async function call(fn, ...args) {
  try {
    return await App()[fn](...args);
  } catch (e) {
    alertInline(errText(e));
    throw e;
  }
}

// Non-blocking error banner (never use window.alert in a webview).
let toastTimer = null;
function alertInline(msg) {
  let t = $('toast');
  if (!t) {
    t = el('div', { id: 'toast', style: 'position:fixed;bottom:16px;left:50%;transform:translateX(-50%);background:#2a1418;color:#ffd6d8;border:1px solid #f2555a;padding:10px 14px;border-radius:8px;max-width:80vw;z-index:10' });
    document.body.append(t);
  }
  t.textContent = msg;
  t.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add('hidden'), 6000);
}

// ---------- tabs ----------

document.querySelectorAll('.tab').forEach((b) => b.addEventListener('click', () => {
  document.querySelectorAll('.tab').forEach((x) => x.classList.toggle('active', x === b));
  document.querySelectorAll('.panel').forEach((p) => p.classList.toggle('active', p.id === 'tab-' + b.dataset.tab));
  if (b.dataset.tab === 'log') renderLog();
}));

// ---------- state rendering ----------

async function refresh() {
  state = await App().GetState();
  render();
}

function render() {
  const c = state.config;
  $('pillTwitch').classList.toggle('on', !!state.twitchLogin);
  $('pillTwitch').textContent = state.twitchLogin ? 'Twitch: ' + state.twitchLogin : 'Twitch';
  $('pillEvents').classList.toggle('on', state.eventSubOnline);
  $('pillObs').classList.toggle('on', state.obsConnected);

  // Twitch
  if (document.activeElement !== $('clientId')) $('clientId').value = c.twitch_client_id || '';
  $('twitchStatus').textContent = state.twitchLogin
    ? 'Logged in as ' + state.twitchLogin + (state.eventSubOnline ? ', listening for redemptions' : ', connecting…')
    : 'Not logged in';
  $('login').classList.toggle('hidden', !!state.twitchLogin);
  $('logout').classList.toggle('hidden', !state.twitchLogin);
  $('loginBox').classList.toggle('hidden', !state.loginUserCode);
  $('loginCode').textContent = state.loginUserCode || '';

  // OBS connection form (don't overwrite while typing)
  const o = c.obs;
  if (!document.activeElement || !document.activeElement.id.startsWith('obs')) {
    $('obsHost').value = o.host;
    $('obsPort').value = o.port;
    $('obsAuto').checked = o.auto_connect;
    $('obsInput').value = o.input_name;
  }
  $('obsStatus').textContent = state.obsConnected ? 'Connected' : 'Not connected';
  $('obsConnect').classList.toggle('hidden', state.obsConnected);
  $('obsDisconnect').classList.toggle('hidden', !state.obsConnected);

  renderOutput();
  renderSounds();
}

// ---------- sounds ----------

function rewardLabel(s) {
  const r = rewards.find((x) => x.id === s.reward_id);
  return r ? r.title : (s.reward_title || '(unknown reward)');
}

function renderSounds() {
  const list = $('soundList');
  list.replaceChildren();
  const sounds = state.config.sounds || [];
  $('soundEmpty').classList.toggle('hidden', sounds.length > 0);
  for (const s of sounds) {
    const cd = [];
    if (s.cooldown_seconds) cd.push('cooldown ' + s.cooldown_seconds + ' s');
    if (s.user_cooldown_seconds) cd.push('per viewer ' + s.user_cooldown_seconds + ' s');
    const toggle = el('input', { type: 'checkbox', title: 'Enabled' });
    toggle.checked = s.enabled;
    toggle.addEventListener('change', () => call('SaveSound', { ...s, enabled: toggle.checked }).then(refresh));
    list.append(el('div', { class: 'sound' + (s.enabled ? '' : ' off') },
      toggle,
      el('div', {},
        el('div', { class: 'title' }, rewardLabel(s)),
        el('div', { class: 'meta', title: s.file }, basename(s.file) + ' · ' + pct(s.volume) + (cd.length ? ' · ' + cd.join(', ') : ''))),
      el('div', { class: 'btns' },
        el('button', { class: 'btn small', onclick: () => App().TestSound(s.id) }, '▶ Test'),
        el('button', { class: 'btn small', onclick: () => openEditor(s) }, 'Edit'),
        el('button', { class: 'btn small danger', onclick: () => deleteSound(s) }, 'Delete'))));
  }
}

async function deleteSound(s) {
  // Two-click confirm instead of window.confirm (dialogs block the webview).
  const key = 'del-' + s.id;
  if (deleteSound.armed !== key) {
    deleteSound.armed = key;
    alertInline('Click “Delete” again to remove “' + rewardLabel(s) + '”.');
    setTimeout(() => { if (deleteSound.armed === key) deleteSound.armed = null; }, 4000);
    return;
  }
  deleteSound.armed = null;
  await call('DeleteSound', s.id);
  refresh();
}

async function loadRewards() {
  $('edRewardHint').textContent = 'Loading rewards…';
  try {
    rewards = (await App().GetRewards()) || [];
    $('edRewardHint').textContent = rewards.length ? '' : 'No custom rewards on your channel yet. Create one on Twitch first.';
  } catch (e) {
    rewards = [];
    $('edRewardHint').textContent = errText(e);
  }
}

function fillRewardSelect(selectedId, selectedTitle) {
  const sel = $('edReward');
  sel.replaceChildren(el('option', { value: '' }, '— choose a reward —'));
  let found = false;
  for (const r of rewards) {
    const o = el('option', { value: r.id }, r.title + ' (' + r.cost + ')' + (r.is_enabled ? '' : ' – disabled on Twitch'));
    if (r.id === selectedId) { o.selected = true; found = true; }
    sel.append(o);
  }
  if (selectedId && !found) {
    // Keep a mapping to a reward we can't see right now (offline, deleted…).
    sel.append(el('option', { value: selectedId, selected: '' }, (selectedTitle || selectedId) + ' (not found)'));
  }
}

async function openEditor(s) {
  editing = s ? { ...s } : { id: '', reward_id: '', reward_title: '', file: '', volume: 1, cooldown_seconds: 0, user_cooldown_seconds: 0, enabled: true };
  $('editorTitle').textContent = s ? 'Edit sound' : 'Add sound';
  $('edError').classList.add('hidden');
  $('edFile').value = editing.file;
  $('edVolume').value = editing.volume;
  $('edVolumeVal').textContent = pct(editing.volume);
  $('edCooldown').value = editing.cooldown_seconds;
  $('edUserCooldown').value = editing.user_cooldown_seconds;
  $('edEnabled').checked = editing.enabled;
  fillRewardSelect(editing.reward_id, editing.reward_title);
  $('editor').showModal();
  await loadRewards();
  fillRewardSelect(editing.reward_id, editing.reward_title);
}

$('addSound').addEventListener('click', () => openEditor(null));
$('edCancel').addEventListener('click', () => $('editor').close());
$('edRefreshRewards').addEventListener('click', async () => { await loadRewards(); fillRewardSelect($('edReward').value); });
$('edVolume').addEventListener('input', () => { $('edVolumeVal').textContent = pct(+$('edVolume').value); });
$('edPick').addEventListener('click', async () => {
  const f = await call('PickSoundFile');
  if (f) $('edFile').value = f;
});
$('edSave').addEventListener('click', async () => {
  const rid = $('edReward').value;
  const r = rewards.find((x) => x.id === rid);
  const s = {
    ...editing,
    reward_id: rid,
    reward_title: r ? r.title : editing.reward_title,
    file: $('edFile').value,
    volume: +$('edVolume').value,
    cooldown_seconds: Math.max(0, parseInt($('edCooldown').value, 10) || 0),
    user_cooldown_seconds: Math.max(0, parseInt($('edUserCooldown').value, 10) || 0),
    enabled: $('edEnabled').checked,
  };
  try {
    await App().SaveSound(s);
    $('editor').close();
    refresh();
  } catch (e) {
    $('edError').textContent = errText(e);
    $('edError').classList.remove('hidden');
  }
});

// ---------- audio output ----------

let devices = [];

async function loadDevices(askPermission) {
  devices = [];
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return;
  let list = await navigator.mediaDevices.enumerateDevices();
  let outs = list.filter((d) => d.kind === 'audiooutput');
  // Chromium (WebView2 on Windows) hides device names until the page got
  // microphone permission once. We open and immediately close the mic only
  // to read the names — nothing is recorded. On macOS/Linux we never ask.
  if (askPermission && IS_WINDOWS && outs.length && outs.every((d) => !d.label)) {
    try {
      const s = await navigator.mediaDevices.getUserMedia({ audio: true });
      s.getTracks().forEach((t) => t.stop());
      list = await navigator.mediaDevices.enumerateDevices();
      outs = list.filter((d) => d.kind === 'audiooutput');
    } catch (e) { /* user declined: fall back to unnamed devices */ }
  }
  devices = outs.filter((d) => d.deviceId && d.deviceId !== 'default' && d.deviceId !== 'communications');
}

function renderOutput() {
  const c = state.config;
  document.querySelectorAll('input[name=mode]').forEach((r) => { r.checked = r.value === c.output_mode; });
  $('deviceCard').classList.toggle('hidden', c.output_mode !== 'device');
  $('obsTracksCard').classList.toggle('hidden', c.output_mode !== 'obs');
  $('master').value = c.master_volume;
  $('masterVal').textContent = pct(c.master_volume);

  const sel = $('device');
  sel.replaceChildren(el('option', { value: '' }, 'System default output'));
  let found = !c.audio_device_id;
  devices.forEach((d, i) => {
    const o = el('option', { value: d.deviceId }, d.label || 'Audio output ' + (i + 1));
    if (d.deviceId === c.audio_device_id) { o.selected = true; found = true; }
    sel.append(o);
  });
  if (!found) {
    sel.append(el('option', { value: c.audio_device_id, selected: '' }, (c.audio_device_name || 'Saved device') + ' (not available)'));
  }
  // Choosing a device needs setSinkId AND a visible device list. macOS/Linux
  // web views usually expose no output list, so there we play on the system
  // default and OBS captures the darkbot app itself.
  const canChoose = SINK_SUPPORTED && devices.length > 0;
  sel.disabled = !canChoose && !c.audio_device_id;
  $('refreshDevices').disabled = !SINK_SUPPORTED;
  $('deviceHint').textContent = canChoose
    ? 'Tip: pick a virtual cable (e.g. VB-CABLE) or a dedicated output and add it in OBS as “Audio Output Capture”. Or keep the default and capture darkbot in OBS with “Application Audio Capture”.'
    : 'This system does not let darkbot list output devices, so sounds play on the system default output. In OBS, capture darkbot directly (Windows: “Application Audio Capture”, macOS: “macOS Audio Capture” → Application), or switch to “OBS media source” to pick tracks.';

  const tracks = $('tracks');
  if (!tracks.children.length) {
    for (let i = 0; i < TRACK_COUNT; i++) {
      tracks.append(el('label', { class: 'track' }, el('input', { type: 'checkbox', 'data-track': i }), 'Track ' + (i + 1)));
    }
  }
  tracks.querySelectorAll('input').forEach((cb) => { cb.checked = !!c.obs.tracks[+cb.dataset.track]; });
}

document.querySelectorAll('input[name=mode]').forEach((r) => r.addEventListener('change', () => {
  $('deviceCard').classList.toggle('hidden', r.value !== 'device');
  $('obsTracksCard').classList.toggle('hidden', r.value !== 'obs');
}));
$('master').addEventListener('input', () => { $('masterVal').textContent = pct(+$('master').value); });
$('refreshDevices').addEventListener('click', async () => { await loadDevices(true); renderOutput(); });

$('saveOutput').addEventListener('click', async () => {
  const mode = document.querySelector('input[name=mode]:checked').value;
  const sel = $('device');
  const name = sel.value ? sel.options[sel.selectedIndex].textContent : '';
  await call('SaveOutput', mode, sel.value, name, +$('master').value);
  if (mode === 'obs') await saveOBS();
  await refresh();
  alertInline('Saved.');
});

// Play requests from Go (audio-device mode).
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
      if (!SINK_SUPPORTED) throw new Error('choosing an output device is not supported here');
      await audio.setSinkId(req.deviceId);
    }
    audio.volume = Math.min(1, Math.max(0, req.volume));
    audio.src = req.url;
    await audio.play();
  } catch (e) {
    done(errText(e));
  }
}

// ---------- connections ----------

$('saveClientId').addEventListener('click', async () => { await call('SetClientID', $('clientId').value); refresh(); alertInline('Saved.'); });
$('login').addEventListener('click', async () => {
  await call('SetClientID', $('clientId').value);
  await call('StartLogin');
  refresh();
});
$('cancelLogin').addEventListener('click', () => App().CancelLogin());
$('loginLink').addEventListener('click', (e) => { e.preventDefault(); if (state.loginUri) window.runtime.BrowserOpenURL(state.loginUri); });
$('logout').addEventListener('click', async () => { await call('Logout'); refresh(); });

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
$('obsSave').addEventListener('click', async () => { await saveOBS(); refresh(); alertInline('Saved.'); });
$('obsConnect').addEventListener('click', async () => {
  await saveOBS();
  $('obsStatus').textContent = 'Connecting…';
  try { await App().ConnectOBS(); } catch (e) { alertInline(errText(e)); }
  refresh();
});
$('obsDisconnect').addEventListener('click', async () => { await App().DisconnectOBS(); refresh(); });

// ---------- log ----------

function renderLog() {
  App().GetLogs().then((lines) => {
    const pre = $('log');
    const atBottom = pre.parentElement.scrollTop + pre.parentElement.clientHeight >= pre.parentElement.scrollHeight - 20;
    pre.textContent = (lines || []).join('\n');
    if (atBottom) pre.parentElement.scrollTop = pre.parentElement.scrollHeight;
  });
}

// ---------- boot ----------

window.addEventListener('DOMContentLoaded', async () => {
  window.runtime.EventsOn('state', refresh);
  window.runtime.EventsOn('play', playSound);
  window.runtime.EventsOn('log', () => { if ($('tab-log').classList.contains('active')) renderLog(); });
  await loadDevices(false);
  App().ReportAudioSupport(SINK_SUPPORTED, devices.length, devices.filter((d) => d.label).length);
  await refresh();
  // Pre-load reward names for the sound list (silently ignore when logged out).
  if (state.twitchLogin) { await loadRewards(); renderSounds(); }
});
