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
const SPOTIFY_DASHBOARD = 'https://developer.spotify.com/dashboard';

let state = null;
let rewards = [];
let devices = [];
let editing = null;
let obsPassDirty = false;
let languages = ['en'];             // shipped UI languages (from Go)
const activities = [];              // feed entries, newest first, re-rendered on language change
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
const points = (n) => t('points', { n: Number(n).toLocaleString(I18N.lang || undefined) });
const basename = (p) => (p || '').split(/[\\/]/).pop();
const errText = (e) => (e && e.message) || String(e);
const clock = (d) => d.toLocaleTimeString(I18N.lang || undefined, { hour: '2-digit', minute: '2-digit' });

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
  if (state.language !== I18N.lang) {
    await loadLanguage(state.language);
    renderFeed();
  }
  render();
}

function render() {
  const c = state.config;
  const loggedIn = !!state.twitchLogin;

  setSignal('sigTwitch', loggedIn, loggedIn ? state.twitchLogin : t('signal.twitch'));
  setSignal('sigEvents', state.eventSubOnline, t('signal.redemptions'));
  setSignal('sigObs', state.obsConnected, t('signal.obs'));
  $('sigObs').classList.toggle('hidden', c.output_mode !== 'obs' && !state.obsConnected);

  const up = state.update;
  $('updateChip').classList.toggle('hidden', !(up && up.available));
  if (up && up.available) $('updateChip').textContent = state.updating ? t('update.chipBusy') : t('update.chip', { version: up.latest });

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
  return (r && r.title) || s.reward_title || t('pad.unnamed');
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
    const cdText = s.enabled && s.cooldown_seconds ? t('pad.seconds', { n: s.cooldown_seconds }) : '';
    const pad = el('div', {
      class: 'pad' + (s.enabled ? '' : ' off') + (playing.has(s.id) ? ' playing' : ''),
      role: 'button', tabindex: '0', 'data-id': s.id,
      'aria-label': t('pad.preview', { name: soundName(s) }),
      onclick: () => App().TestSound(s.id),
      onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); App().TestSound(s.id); } },
    },
    el('span', { class: 'led', 'aria-hidden': 'true' }),
    el('span', { class: 'cd' }, cdText),
    el('button', {
      class: 'edit', title: t('pad.edit'), 'aria-label': t('pad.editNamed', { name: soundName(s) }),
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

const redeemed = () => ' ' + t('feed.redeemed') + ' ';
const FEED_TEXT = {
  queued: (a) => [a.user, redeemed(), a.rewardTitle, null],
  cooldown: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.cooldown', { time: a.message })],
  disabled: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.disabled')],
  queue_full: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.queueFull')],
  error: (a) => [a.test ? t('feed.preview') : a.user, a.test ? ' ' + t('feed.previewOf') + ' ' : redeemed(), a.rewardTitle, a.message || t('feed.failed')],
  song_added: (a) => [a.user, ' ' + t('feed.requested') + ' ', a.title, null],
  song_rejected: (a) => [a.user, ' ' + t('feed.requested') + ' ', a.title, a.message],
  song_skipped: (a) => [a.user, ' ' + t('feed.skippedSong') + ' ', a.title, null],
};
const FEED_CLASS = { queued: 'played', song_added: 'played', error: 'error' };

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

  if (!FEED_TEXT[a.kind]) return;
  activities.unshift(a);
  if (activities.length > FEED_MAX) activities.length = FEED_MAX;
  const item = feedItem(a);
  item.classList.add('fresh');
  const feed = $('feed');
  feed.prepend(item);
  while (feed.children.length > FEED_MAX) feed.lastChild.remove();
  $('feedEmpty').classList.add('hidden');
  requestAnimationFrame(() => requestAnimationFrame(() => item.classList.remove('fresh')));
}

function feedItem(a) {
  const [who, verb, what, why] = FEED_TEXT[a.kind](a);
  const kindClass = FEED_CLASS[a.kind] || 'skipped';
  return el('li', { class: 'feed-item ' + kindClass },
    el('time', { datetime: a.at }, clock(new Date(a.at))),
    el('span', { class: 'what' }, el('b', {}, who), verb, el('b', {}, what)),
    why ? el('span', { class: 'why' }, why) : null);
}

// Rebuilds the feed, e.g. after the language changed.
function renderFeed() {
  $('feed').replaceChildren(...activities.map(feedItem));
  $('feedEmpty').classList.toggle('hidden', activities.length > 0);
}

// ---------- strip ----------

function outputLabel() {
  const c = state.config;
  if (c.output_mode === 'obs') {
    const tracks = c.obs.tracks.map((on, i) => (on ? i + 1 : null)).filter(Boolean);
    return t('strip.obsTracks', { tracks: tracks.join(', ') || t('strip.none') });
  }
  return c.audio_device_name || t('strip.default');
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
  renderGeneral();
  renderAudio();
  renderTwitch();
  renderOBS();
  renderSongs();
  renderUpdates();
}

// general
function systemLanguage() {
  for (const l of navigator.languages || [navigator.language]) {
    const base = String(l).toLowerCase().split(/[-_]/)[0];
    if (languages.includes(base)) return base;
  }
  return 'en';
}
function renderGeneral() {
  const sel = $('language');
  const chosen = state.config.language || '';
  sel.replaceChildren(
    el('option', { value: '', selected: chosen === '' }, t('general.languageSystem', { lang: t('lang.' + systemLanguage()) })),
    ...languages.map((l) => el('option', { value: l, selected: chosen === l }, t('lang.' + l))));
}
$('language').addEventListener('change', async () => {
  await call('SetLanguage', $('language').value);
  await refresh();
  toast(t('general.saved'));
});

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
  sel.replaceChildren(el('option', { value: '' }, t('strip.default')));
  let found = !keep;
  devices.forEach((d, i) => {
    sel.append(el('option', { value: d.deviceId, selected: d.deviceId === keep }, d.label || t('audio.deviceN', { n: i + 1 })));
    if (d.deviceId === keep) found = true;
  });
  if (!found) sel.append(el('option', { value: keep, selected: true, 'data-name': c.audio_device_name || '' }, t('audio.notConnected', { name: c.audio_device_name || t('audio.savedDevice') })));

  const canChoose = SINK_SUPPORTED && devices.length > 0;
  sel.disabled = !canChoose && !c.audio_device_id;
  $('refreshDevices').disabled = !SINK_SUPPORTED;
  $('deviceHint').textContent = canChoose ? t('audio.hintChoose') : t('audio.hintNoChoose');

  const tracks = $('tracks');
  if (!tracks.children.length) {
    for (let i = 0; i < TRACK_COUNT; i++) {
      tracks.append(el('label', { class: 'track' },
        el('input', { type: 'checkbox', 'data-track': i }), String(i + 1)));
    }
  }
  tracks.querySelectorAll('label').forEach((lb, i) => {
    lb.title = t('audio.track', { n: i + 1 });
    lb.querySelector('input').setAttribute('aria-label', lb.title);
  });
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
  const opt = sel.options[sel.selectedIndex];
  const name = !sel.value ? '' : opt.hasAttribute('data-name') ? opt.dataset.name : opt.textContent;
  await call('SaveOutput', mode, sel.value, name, state.config.master_volume);
  await saveOBS();
  delete $('sec-audio').dataset.touched;
  await refresh();
  toast(mode === 'obs' && !state.obsConnected ? t('audio.savedObsOffline') : t('audio.saved'));
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
  const login = state.twitchLogin;
  let status = t('twitch.statusOff');
  if (loggedIn) {
    status = state.needsReconnect ? t('twitch.statusReconnect', { login })
      : state.eventSubOnline ? t('twitch.statusListening', { login }) : t('twitch.statusConnecting', { login });
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
$('saveClientId').addEventListener('click', async () => { await call('SetClientID', $('clientId').value); refresh(); toast(t('twitch.clientIdSaved')); });

// obs
function renderOBS() {
  const o = state.config.obs;
  if (!$('sec-obs').contains(document.activeElement)) {
    $('obsHost').value = o.host;
    $('obsPort').value = o.port;
    $('obsAuto').checked = o.auto_connect;
  }
  $('obsStatus').textContent = state.obsConnected ? t('obs.connected') : t('obs.notConnected');
  $('obsConnect').textContent = state.obsConnected ? t('obs.reconnect') : t('obs.connect');
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
  $('obsStatus').textContent = t('obs.connecting');
  try { await App().ConnectOBS(); toast(t('obs.connectedToast')); } catch (e) { toast(errText(e), true); }
  refresh();
});
$('obsDisconnect').addEventListener('click', async () => { await App().DisconnectOBS(); refresh(); });

// song requests
function renderSongs() {
  const c = state.config.songs;
  const connected = !!state.spotifyUser;
  const sec = $('sec-songs');
  $('songsChatHint').classList.toggle('hidden', !state.twitchLogin || state.chatReady);
  $('spotifyStatus').textContent = connected ? t('songs.connectedAs', { user: state.spotifyUser }) : t('songs.notConnected');
  $('spotifyStatus').classList.toggle('on', connected);
  $('spotifyLogout').classList.toggle('hidden', !connected);
  $('spotifyRedirect').value = state.spotifyRedirectUri;
  $('spotifyPasteBox').classList.toggle('hidden', !state.spotifyLoginPending);
  $('spotifyLogin').textContent = connected ? t('songs.reconnect') : t('songs.connect');
  // Open the setup steps until Spotify is connected.
  if (!connected && !sec.dataset.setupShown) { $('spotifySetup').open = true; sec.dataset.setupShown = '1'; }
  if (sec.dataset.touched) return;
  $('spotifyClientId').value = c.spotify_client_id || '';
  $('songsEnabled').checked = c.enabled;
  $('songsCooldown').value = c.user_cooldown_seconds;
  $('songsMax').value = c.max_minutes;
  $('songsExplicit').checked = c.block_explicit;
  $('songsRequesterSkip').checked = c.requester_can_skip;
  $('songsReplies').checked = c.chat_replies;
}
for (const id of ['spotifyClientId', 'songsEnabled', 'songsCooldown', 'songsMax', 'songsExplicit', 'songsRequesterSkip', 'songsReplies']) {
  $(id).addEventListener('input', () => { $('sec-songs').dataset.touched = '1'; });
  $(id).addEventListener('change', () => { $('sec-songs').dataset.touched = '1'; });
}
function songSettings() {
  return {
    enabled: $('songsEnabled').checked,
    spotify_client_id: $('spotifyClientId').value.trim(),
    user_cooldown_seconds: Math.max(0, parseInt($('songsCooldown').value, 10) || 0),
    max_minutes: Math.max(0, parseInt($('songsMax').value, 10) || 0),
    block_explicit: $('songsExplicit').checked,
    chat_replies: $('songsReplies').checked,
    requester_can_skip: $('songsRequesterSkip').checked,
  };
}
async function saveSongs() {
  await call('SaveSongSettings', songSettings());
  delete $('sec-songs').dataset.touched;
}
$('saveSongs').addEventListener('click', async () => {
  await saveSongs();
  await refresh();
  toast(t('songs.saved'));
});
$('copyRedirect').addEventListener('click', async () => {
  const ok = await window.runtime.ClipboardSetText($('spotifyRedirect').value);
  toast(ok ? t('songs.copied') : t('songs.copyFailed'), !ok);
});
$('openSpotifyDashboard').addEventListener('click', () => window.runtime.BrowserOpenURL(SPOTIFY_DASHBOARD));
$('spotifyLogin').addEventListener('click', async () => {
  await saveSongs(); // the Client ID typed above is what the login uses
  await call('SpotifyStartLogin');
  $('spotifyPaste').value = '';
  await refresh();
  $('spotifyPaste').focus();
});
$('spotifyReopen').addEventListener('click', () => App().SpotifyOpenLogin());
$('spotifyCancel').addEventListener('click', async () => { await App().SpotifyCancelLogin(); refresh(); });
$('spotifyFinish').addEventListener('click', async () => {
  $('spotifyFinish').disabled = true;
  try {
    await call('SpotifyFinishLogin', $('spotifyPaste').value);
    $('spotifyPaste').value = '';
    $('spotifySetup').open = false;
    toast(t('songs.connectedToast'));
  } catch (e) { /* toast shown by call */ } finally {
    $('spotifyFinish').disabled = false;
    refresh();
  }
});
$('spotifyPaste').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('spotifyFinish').click(); });
$('spotifyLogout').addEventListener('click', async () => { await call('SpotifyLogout'); refresh(); });

// updates
function renderUpdates() {
  const up = state.update;
  const dev = state.version === 'dev';
  let text = dev ? t('updates.dev') : t('updates.current', { version: state.version });
  if (up && up.available) text = t('updates.available', { latest: up.latest, version: state.version });
  else if (up && !dev) text += ' ' + t('updates.latest');
  $('updateStatus').textContent = text;
  $('autoUpdate').checked = state.config.auto_update;
  $('installUpdate').classList.toggle('hidden', !(up && up.available));
  $('installUpdate').disabled = state.updating;
  $('installUpdate').textContent = state.updating ? t('updates.installing') : t('updates.install');
  $('updateNotes').classList.toggle('hidden', !(up && up.available && up.notes));
  $('updateNotes').textContent = (up && up.notes) || '';
}
$('autoUpdate').addEventListener('change', () => call('SetAutoUpdate', $('autoUpdate').checked).then(refresh));
$('checkUpdate').addEventListener('click', async () => {
  $('checkUpdate').disabled = true;
  try {
    const info = await App().CheckUpdate();
    toast(info.available ? t('updates.availableToast', { latest: info.latest }) : t('updates.upToDate'));
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
  $('edRewardHint').textContent = t('editor.loadingRewards');
  try {
    rewards = (await App().GetRewards()) || [];
    $('edRewardHint').textContent = rewards.length ? '' : t('editor.noRewards');
  } catch (e) {
    rewards = [];
    $('edRewardHint').textContent = errText(e);
  }
}

function fillRewardSelect(selectedId, selectedTitle) {
  const sel = $('edReward');
  const usedByOthers = new Set((state.config.sounds || []).filter((s) => !editing || s.id !== editing.id).map((s) => s.reward_id));
  sel.replaceChildren(el('option', { value: '' }, t('editor.chooseReward')));
  let found = false;
  for (const r of rewards) {
    const taken = usedByOthers.has(r.id);
    const label = t('editor.rewardOption', { title: r.title, points: points(r.cost) })
      + (r.is_enabled ? '' : t('editor.rewardHidden')) + (taken ? t('editor.rewardTaken') : '');
    sel.append(el('option', { value: r.id, selected: r.id === selectedId, disabled: taken }, label));
    if (r.id === selectedId) found = true;
  }
  if (selectedId && !found) sel.append(el('option', { value: selectedId, selected: true }, t('editor.notFound', { title: selectedTitle || t('editor.savedReward') })));
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
  $('editorTitle').textContent = isNew ? t('editor.add') : t('editor.edit');
  $('edSave').textContent = isNew ? t('editor.add') : t('editor.save');
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
  $('edDelete').textContent = t('editor.delete');
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
    toast(managed && !editing.reward_id ? t('editor.created', { title: s.reward_title }) : t('editor.saved'));
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
    $('edDelete').textContent = editing.managed ? t('editor.deleteConfirmReward') : t('editor.deleteConfirm');
    return;
  }
  deleteArmed = 0;
  try {
    await App().DeleteSound(editing.id);
    $('editor').close();
    toast(t('editor.deleted'));
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
  audio.addEventListener('error', () => done(t('play.cannotPlay')));
  try {
    if (req.deviceId) {
      if (!SINK_SUPPORTED) throw new Error(t('play.noSink'));
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
  languages = (await App().GetLanguages()) || languages;
  await App().ReportSystemLanguage([...(navigator.languages || [navigator.language])]);
  await loadDevices(false);
  App().ReportAudioSupport(SINK_SUPPORTED, devices.length, devices.filter((d) => d.label).length);
  await refresh();
  if (state.twitchLogin) {
    try { rewards = (await App().GetRewards()) || []; renderPads(); } catch (e) { /* shown when editing */ }
  }
});
