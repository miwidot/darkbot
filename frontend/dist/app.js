// darkbot UI. Vanilla JS; talks to Go through Wails bindings
// (window.go.main.App) and events (window.runtime). No network port.
'use strict';

const App = () => window.go.main.App;
const $ = (id) => document.getElementById(id);
const IS_WINDOWS = navigator.userAgent.includes('Windows');
const SINK_SUPPORTED = typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
const TRACK_COUNT = 6;
const FEED_MAX = 200; // as many as darkbot keeps on disk (activity.MaxEntries)
const DEFAULT_COST = 100;
// Suggested reward colors (Twitch accepts any #RRGGBB). New rewards start
// with a random one so the reward list on Twitch stays easy to tell apart.
const REWARD_COLORS = ['#9147FF', '#1F69FF', '#00C8AF', '#57BEE6', '#FACD3B', '#FF6905', '#EB0400', '#F074C9'];
const TOAST_MS = 5000;
const CONFIRM_MS = 4000;
const SPOTIFY_DASHBOARD = 'https://developer.spotify.com/dashboard';
const PEAR_DOWNLOAD = 'https://github.com/pear-devs/pear-desktop/releases/latest';
const YTM_DEFAULT_PORT = 26538; // Pear Desktop's API Server default
const MAX_PORT = 65535;
const VIEW_KEY = 'darkbot.view';     // localStorage: last opened view
const BUMP_MS = 900;                 // how long a meter glows after it changed

let state = null;
let stats = null;                   // dashboard numbers from Go
let view = 'dash';
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

// call runs a backend method. A failure is shown as a toast, the UI is
// re-read (the change may have half happened), and the error is rethrown
// for callers that want to react; nobody has to catch it just to keep the
// console clean (see the unhandledrejection handler at boot).
const shownErrors = new WeakSet();
async function call(fn, ...args) {
  try {
    return await App()[fn](...args);
  } catch (e) {
    toast(errText(e), true);
    refresh();
    const err = e instanceof Error ? e : new Error(errText(e));
    shownErrors.add(err);
    throw err;
  }
}

// ---------- state ----------

// refresh fetches the state and renders it. Calls while one runs share it
// and cause one more pass at its end, so a burst of "state" events renders
// twice at most instead of once per event.
let refreshing = null;
let refreshAgain = false;
function refresh() {
  if (refreshing) {
    refreshAgain = true;
    return refreshing;
  }
  refreshing = (async () => {
    do {
      refreshAgain = false;
      try {
        state = await App().GetState();
        if (state.language !== I18N.lang) {
          await loadLanguage(state.language);
          renderFeed();
        }
        render();
      } catch (e) {
        console.error('refresh failed', e); // the next state event retries
      }
    } while (refreshAgain);
  })().finally(() => { refreshing = null; });
  return refreshing;
}

function render() {
  const c = state.config;
  const loggedIn = !!state.twitchLogin;
  if (npBuilt) npLabels();
  if (npOpen && npState) npUpdate(npState);

  setSignal('sigTwitch', loggedIn, loggedIn ? state.twitchLogin : t('signal.twitch'), loggedIn && state.needsReconnect ? 'attn' : '');
  // Red: it should run but doesn't (logged in without live events, OBS
  // output without OBS).
  setSignal('sigEvents', state.eventSubOnline, t('signal.redemptions'), loggedIn && !state.eventSubOnline ? 'fault' : '');
  setSignal('sigObs', state.obsConnected, t('signal.obs'), c.output_mode === 'obs' && !state.obsConnected ? 'fault' : '');
  $('sigObs').classList.toggle('hidden', c.output_mode !== 'obs' && !state.obsConnected);

  const up = state.update;
  $('updateChip').classList.toggle('hidden', !(up && up.available));
  if (up && up.available) $('updateChip').textContent = state.updating ? t('update.chipBusy') : t('update.chip', { version: up.latest });

  $('onboard').classList.toggle('hidden', loggedIn);
  $('reconnect').classList.toggle('hidden', !loggedIn || !state.needsReconnect);
  // Before login there is nothing to count or play: only the welcome card.
  document.querySelector('.views').classList.toggle('hidden', !loggedIn);
  $('dashBody').classList.toggle('hidden', !loggedIn);
  $('dashReset').classList.toggle('hidden', !loggedIn);
  $('dashSince').classList.toggle('hidden', !loggedIn);
  showView(loggedIn ? view : 'dash');
  renderDash();
  renderEvents();
  renderChat();
  renderLogins();

  renderPads();
  renderStrip();
  if ($('settings').open) renderSettings();
}

// setSignal: green when on; problem ('attn' amber, 'fault' red) wins.
function setSignal(id, on, text, problem) {
  const s = $(id);
  s.classList.toggle('on', on && !problem);
  s.classList.toggle('attn', problem === 'attn');
  s.classList.toggle('fault', problem === 'fault');
  s.querySelector('span').textContent = text;
}

// ---------- pads ----------

// rewardColor: the managed reward's saved color, or the color Twitch
// reports for the reward (existing rewards, older sounds).
function rewardColor(s) {
  if (s.reward_color) return s.reward_color;
  const r = rewards.find((x) => x.id === s.reward_id);
  return (r && r.background_color) || '';
}

// colorChip paints a swatch. Through CSSOM, because the CSP (style-src
// 'self') blocks style attributes.
function colorChip(node, color) {
  node.style.setProperty('--chip', color);
  return node;
}

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
    el('span', { class: 'file', title: s.file },
      rewardColor(s) ? colorChip(el('span', { class: 'chip', 'aria-hidden': 'true' }), rewardColor(s)) : '',
      s.managed ? points(s.reward_cost) : basename(s.file)));
    pads.append(pad);
  }
  tickCooldowns();
}

// Cooldown sweep: runs only while a pad is cooling down, and at most
// RING_STEPS steps per cooldown (between RING_MIN_MS and RING_MAX_MS apart)
// instead of every frame: darkbot runs next to OBS and a game for hours.
const RING_STEPS = 360;
const RING_MIN_MS = 100;
const RING_MAX_MS = 1000;
let ringTimer = 0;
function tickCooldowns() {
  clearTimeout(ringTimer);
  const now = Date.now();
  let active = false;
  let step = RING_MAX_MS;
  for (const pad of document.querySelectorAll('.pad')) {
    const cd = cooldowns.get(pad.dataset.id);
    if (cd && cd.until > now) {
      active = true;
      step = Math.min(step, Math.max(RING_MIN_MS, cd.total / RING_STEPS));
      pad.classList.add('cooling');
      pad.style.setProperty('--cd', ((cd.until - now) / cd.total).toFixed(4));
    } else {
      pad.classList.remove('cooling');
      if (cd) cooldowns.delete(pad.dataset.id);
    }
  }
  if (active) ringTimer = setTimeout(tickCooldowns, step);
}

// ---------- views ----------

function showView(name) {
  for (const b of document.querySelectorAll('.views button')) {
    const on = b.dataset.view === name;
    b.classList.toggle('active', on);
    b.setAttribute('aria-current', on ? 'page' : 'false');
  }
  $('viewDash').classList.toggle('hidden', name !== 'dash');
  $('viewSounds').classList.toggle('hidden', name !== 'sounds');
  $('viewEvents').classList.toggle('hidden', name !== 'events');
  $('viewChat').classList.toggle('hidden', name !== 'chat');
  $('viewNp').classList.toggle('hidden', name !== 'np');
  npSetOpen(name === 'np');
}
for (const b of document.querySelectorAll('.views button')) {
  b.addEventListener('click', () => {
    view = b.dataset.view;
    try { localStorage.setItem(VIEW_KEY, view); } catch (e) { /* storage blocked: the view just isn't remembered */ }
    showView(view);
  });
}
const VIEWS = ['dash', 'sounds', 'events', 'chat', 'np'];
try { const v = localStorage.getItem(VIEW_KEY); view = VIEWS.includes(v) ? v : 'dash'; } catch (e) { view = 'dash'; }

// ---------- now playing ----------

// The tab shows all six designs live: each preview frame runs the same
// page as OBS (overlay/nowplaying.html) and gets the same data by
// postMessage. While the tab is open darkbot asks the services.
const NP_THEMES = ['console', 'glass', 'band', 'vinyl', 'minimal', 'pill'];
const NP_PAGE_WIDTH = 800; // the overlay page's size in OBS
const NP_SAMPLE_DURATION_MS = 240000;
const NP_SAMPLE_PROGRESS_MS = 72000;
let npState = null;  // last NowPlayingState from Go
let npOpen = false;
let npBuilt = false;

function npSetOpen(open) {
  if (open === npOpen) return;
  npOpen = open;
  App().SetNowPlayingPreview(open);
  if (open) {
    npBuild();
    App().GetNowPlaying().then(npUpdate).catch((e) => console.error('now playing', e));
  }
}

// The data a preview gets: the real song, or a sample while nothing plays.
function npPreviewData() {
  const p = { ...npState.payload };
  if (!p.song) {
    p.song = { service: 'sample', serviceName: 'Spotify', id: 'sample', title: t('np.sampleTitle'), artist: t('np.sampleArtist'),
      cover: '', durationMs: NP_SAMPLE_DURATION_MS, progressMs: NP_SAMPLE_PROGRESS_MS, playing: true, requestedBy: t('np.sampleViewer') };
  }
  return p;
}

function npPost(frame) {
  if (!npState || !frame.contentWindow) return;
  frame.contentWindow.postMessage({ type: 'darkbot-nowplaying', data: npPreviewData() }, location.origin);
}

function npBuild() {
  if (npBuilt) return;
  npBuilt = true;
  $('npGallery').replaceChildren(...NP_THEMES.map((theme) => {
    const frame = el('iframe', { src: 'overlay/nowplaying.html?preview=1&theme=' + theme, tabindex: '-1', 'aria-hidden': 'true', title: theme });
    const box = el('div', { class: 'np-frame' }, frame);
    const tile = el('button', { class: 'np-tile', type: 'button', 'data-theme': theme },
      box, el('span', { class: 'np-caption' }, el('span', { class: 'np-name' }), el('span', { class: 'np-badge' })));
    tile.addEventListener('click', () => npSave({ theme }));
    // Scale the 800 px page down to the tile.
    new ResizeObserver(() => { frame.style.transform = 'scale(' + (box.clientWidth / NP_PAGE_WIDTH) + ')'; }).observe(box);
    return tile;
  }));
  npLabels();
}

// A frame says when it is ready; then it gets the current data.
window.addEventListener('message', (e) => {
  if (e.origin !== location.origin || !e.data || e.data.type !== 'darkbot-nowplaying-ready') return;
  for (const f of document.querySelectorAll('#npGallery iframe')) if (f.contentWindow === e.source) npPost(f);
});

function npLabels() {
  for (const tile of document.querySelectorAll('#npGallery .np-tile')) {
    tile.querySelector('.np-name').textContent = t('np.theme.' + tile.dataset.theme);
  }
}

function npUpdate(st) {
  if (!st) return;
  npState = st;
  const p = st.payload;
  const song = p.song;
  const now = $('npNow');
  now.classList.toggle('playing', !!song && song.playing);
  now.classList.toggle('paused', !!song && !song.playing);
  now.classList.toggle('problem', !song && !!st.problem);
  let text = t('np.nothing');
  if (song) text = t(song.playing ? 'np.playing' : 'np.paused', { title: song.title, artist: song.artist, service: song.serviceName });
  else if (st.problem) text = t('np.problem', { error: st.problem });
  else if (!npAnyService()) text = t('np.noService');
  $('npNowText').textContent = text;
  // Settings only when Go sent the OBS status (tab opened, saved).
  if (st.obsReady !== undefined) npRenderSettings(st);
  for (const tile of document.querySelectorAll('#npGallery .np-tile')) {
    const on = tile.dataset.theme === p.theme;
    tile.setAttribute('aria-pressed', on ? 'true' : 'false');
    tile.querySelector('.np-badge').textContent = on ? t('np.selected') : '';
  }
  for (const f of document.querySelectorAll('#npGallery iframe')) npPost(f);
}

function npAnyService() {
  return !!(state && (state.spotifyUser || (state.config.songs.ytmusic_enabled && (state.ytmusicHealth || {}).linked)));
}

function npRenderSettings(st) {
  const c = state ? state.config.now_playing : null;
  if (!c) return;
  $('npEnabled').checked = c.enabled;
  $('npCover').checked = c.show_cover;
  $('npProgress').checked = c.show_progress;
  $('npRequester').checked = c.show_requester;
  $('npHide').checked = c.hide_when_paused;
  let obsText = t('np.off');
  if (c.enabled) obsText = st.obsReady ? t('np.obsReady') : st.obsProblem;
  $('npObs').textContent = obsText;
  $('npObs').classList.toggle('warn-text', c.enabled && !st.obsReady);
}

async function npSave(change) {
  const c = state.config.now_playing;
  const next = {
    enabled: $('npEnabled').checked, theme: c.theme, show_cover: $('npCover').checked, show_progress: $('npProgress').checked,
    show_requester: $('npRequester').checked, hide_when_paused: $('npHide').checked, ...change,
  };
  try {
    const st = await call('SaveNowPlaying', next);
    await refresh();
    npUpdate(st);
  } catch (e) {
    await refresh();
    App().GetNowPlaying().then(npUpdate).catch(() => {});
  }
}
for (const id of ['npEnabled', 'npCover', 'npProgress', 'npRequester', 'npHide']) {
  $(id).addEventListener('change', () => npSave({}));
}

// ---------- dashboard ----------

const num = (n) => Number(n || 0).toLocaleString(I18N.lang || undefined);

// loss marks a count that went wrong (unfollows, refused, rejected) red
// once it is above zero.
const loss = (text, n) => (n > 0 ? el('span', { class: 'loss' }, text) : text);

// Four meters. The big number is this session; the lines below explain it.
// Channel meters: what Twitch reports about your channel.
// channelNow: Twitch reported this channel total (state.channel).
function channelNow(key) {
  return !!(state && state.channel && state.channel[key] != null);
}

const METERS_CHANNEL = [
  { id: 'follows', label: 'dash.follows', tip: 'tip.dashFollows',
    value: (s) => s.session.follows,
    detail: (s) => (state && state.config.unfollow_detection ? loss(t('dash.followsDetail', { n: num(s.session.unfollows) }), s.session.unfollows) : t('dash.followsNoDetect')),
    total: (s) => s.total.follows,
    // The channel as it is on Twitch now, when known; else darkbot's count.
    now: () => (channelNow('followers') ? t('dash.followersNow', { n: num(state.channel.followers) }) : '') },
  { id: 'subs', label: 'dash.subs', tip: 'tip.dashSubs',
    value: (s) => s.session.subs,
    detail: (s) => t('dash.subsDetail', { n: num(s.session.giftedSubs) }),
    total: (s) => s.total.subs,
    now: () => (channelNow('subs') ? t('dash.subsNow', { n: num(state.channel.subs), points: num(state.channel.subPoints) }) : '') },
  { id: 'bits', label: 'dash.bits', tip: 'tip.dashBits',
    value: (s) => s.session.bits,
    detail: () => t('dash.bitsDetail'),
    total: (s) => s.total.bits },
  { id: 'raids', label: 'dash.raids', tip: 'tip.dashRaids',
    value: (s) => s.session.raids,
    detail: (s) => t('dash.raidsDetail', { n: num(s.session.raidViewers) }),
    total: (s) => s.total.raids },
];

const METERS = [
  { id: 'redemptions', label: 'dash.redemptions', tip: 'tip.dashRedemptions',
    value: (s) => s.session.redemptions,
    detail: (s) => [t('dash.played', { n: num(s.session.played) }), ', ', loss(t('dash.refused', { n: num(s.session.refused) }), s.session.refused)],
    total: (s) => s.total.redemptions },
  { id: 'commands', label: 'dash.commands', tip: 'tip.dashCommands',
    value: (s) => s.session.commands,
    detail: (s) => t('dash.commandsDetail', { skips: num(s.session.skips) }),
    total: (s) => s.total.commands },
  { id: 'songs', label: 'dash.songs', tip: 'tip.dashSongs',
    value: (s) => s.session.songsAdded,
    detail: (s) => loss(t('dash.songsDetail', { rejected: num(s.session.songsRejected) }), s.session.songsRejected),
    total: (s) => s.total.songsAdded },
  { id: 'viewers', label: 'dash.viewers', tip: 'tip.dashViewers',
    value: (s) => s.viewers,
    detail: () => t('dash.viewersDetail'),
    total: null },
];
const lastValues = new Map();

function renderDash() {
  if (!stats) return;
  const totalSince = new Date(stats.totalSince);
  // The session survives restarts, so it may have begun on another day.
  const since = new Date(stats.since);
  const sameDay = since.toDateString() === new Date().toDateString();
  $('dashSince').textContent = t('dash.since', {
    time: sameDay ? clock(since) : since.toLocaleString(I18N.lang || undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }),
    date: totalSince.toLocaleDateString(I18N.lang || undefined, { day: 'numeric', month: 'short', year: 'numeric' }),
  });

  renderMeters('metersChannel', METERS_CHANNEL);
  renderMeters('meters', METERS);
  renderRank('topSounds', stats.topSounds || []);
  renderRank('topViewers', stats.topViewers || []);
}

function renderMeters(id, defs) {
  $(id).replaceChildren(...defs.map((m) => {
    const v = m.value(stats) || 0;
    const prev = lastValues.get(m.id);
    lastValues.set(m.id, v);
    const bump = prev !== undefined && v > prev;
    const tile = el('div', { class: 'meter' + (bump ? ' bump' : '') },
      el('div', { class: 'meter-head' },
        el('span', { class: 'meter-label' }, t(m.label)),
        el('span', { class: 'tip', tabindex: '0', role: 'button', 'data-tip': m.tip, 'aria-label': t('tip.more') })),
      el('div', { class: 'meter-value' }, num(v)),
      el('div', { class: 'meter-detail' }, ...[m.detail(stats)].flat()),
      m.now && m.now() ? el('div', { class: 'meter-total now' }, m.now())
        : m.total ? el('div', { class: 'meter-total' }, t('dash.total', { n: num(m.total(stats)) })) : null);
    if (bump) setTimeout(() => tile.classList.remove('bump'), BUMP_MS);
    return tile;
  }));
}

// Ranked list with a bar relative to the leader.
function renderRank(id, entries) {
  const max = entries.length ? entries[0].count : 0;
  $(id).replaceChildren(...entries.map((e) => {
    const bar = el('span', { class: 'rank-bar', 'aria-hidden': 'true' });
    bar.style.setProperty('--w', max ? (e.count / max).toFixed(3) : '0');
    return el('li', {}, el('span', { class: 'rank-name', title: e.name }, e.name), bar, el('span', { class: 'rank-count' }, num(e.count)));
  }));
  $(id + 'Empty').classList.toggle('hidden', entries.length > 0);
}

// Coalesce bursts of "stats" events into one fetch per frame.
let statsQueued = false;
function loadStats() {
  if (statsQueued) return;
  statsQueued = true;
  requestAnimationFrame(async () => {
    statsQueued = false;
    try { stats = await App().GetStats(); renderDash(); } catch (e) { /* the next event fetches again */ }
  });
}

// Two-step reset like delete: native confirm dialogs would block the web view.
let resetArmed = 0;
$('dashReset').addEventListener('click', async () => {
  if (Date.now() - resetArmed > CONFIRM_MS) {
    resetArmed = Date.now();
    $('dashReset').textContent = t('dash.resetConfirm');
    setTimeout(() => { if (Date.now() - resetArmed >= CONFIRM_MS) $('dashReset').textContent = t('dash.reset'); }, CONFIRM_MS);
    return;
  }
  resetArmed = 0;
  $('dashReset').textContent = t('dash.reset');
  lastValues.clear();
  await App().ResetSessionStats();
  toast(t('dash.resetDone'));
});

// ---------- logins (token health) ----------

const LOGIN_TICK_MS = 30000; // relative times ("in 12 minutes") refresh this often
const MINUTE_MS = 60000;

const isSet = (iso) => iso && !iso.startsWith('0001-'); // Go zero time = not set
const dayDate = (iso) => new Date(iso).toLocaleDateString(I18N.lang || undefined, { day: 'numeric', month: 'long', year: 'numeric' });

// "21:42 (in 12 minutes)" in the UI language.
function whenWithRelative(iso) {
  const d = new Date(iso);
  const mins = Math.round((d - Date.now()) / MINUTE_MS);
  let rel;
  try {
    const rtf = new Intl.RelativeTimeFormat(I18N.lang || undefined, { numeric: 'auto' });
    rel = Math.abs(mins) >= 120 ? rtf.format(Math.round(mins / 60), 'hour') : rtf.format(mins, 'minute');
  } catch (e) { rel = ''; }
  return clock(d) + (rel ? ' (' + rel + ')' : '');
}

function loginRow(label, value) {
  return [el('dt', {}, label), el('dd', {}, value)];
}

// A login's state: 'ok' green, 'attn' amber (works, but needs you soon),
// 'bad' red (a problem now), 'idle' grey (connected, not checked yet),
// 'off' grey. Every state also has a word, so color is never the only sign.
const LOGIN_SOON_DAYS = 14; // a login that ends sooner asks to reconnect
const DAY_MS = 86400000;
const STATE_WORD = { ok: 'logins.stateOk', attn: 'logins.stateAttn', bad: 'logins.stateBad', idle: 'logins.stateIdle', off: 'logins.off' };

function cardHead(service, st, who) {
  return el('div', { class: 'login-head' },
    el('span', { class: 'login-led', 'aria-hidden': 'true' }),
    el('b', {}, t('logins.' + service)),
    el('span', { class: 'login-who' }, who),
    el('span', { class: 'login-state' }, t(STATE_WORD[st])));
}

function loginCard(service, h, opts) {
  let st = 'off';
  if (h.loggedIn) {
    // Only a login that really ends (Spotify) warns; Twitch's extends itself.
    const endsSoon = opts.warnEnding && isSet(h.loginUntil) && new Date(h.loginUntil) - Date.now() < LOGIN_SOON_DAYS * DAY_MS;
    st = h.lastError ? 'bad' : (endsSoon || opts.attn ? 'attn' : 'ok');
  }
  const card = el('div', { class: 'login-card ' + st });
  card.append(cardHead(service, st, h.loggedIn ? opts.who : ''));
  // Amber says why: the reconnect it needs, or the login ending soon.
  if (st === 'attn') card.append(el('p', { class: 'login-attn' }, opts.attn || t('logins.endsSoon', { date: dayDate(h.loginUntil) })));
  if (!h.loggedIn) {
    card.append(el('button', { class: 'btn', onclick: () => openSettings(opts.section) }, t('logins.connect')));
    return card;
  }
  const expired = new Date(h.expiresAt) <= Date.now();
  const rows = [
    ...loginRow(t('logins.validUntil'), expired ? t('logins.expired') : whenWithRelative(h.expiresAt)),
    ...loginRow(t('logins.autoRenew'), opts.nextRefresh ? t('logins.autoAt', { time: clock(new Date(opts.nextRefresh)) }) : t('logins.autoOnUse')),
    ...loginRow(t('logins.lastRefresh'), isSet(h.lastRefresh) ? clock(new Date(h.lastRefresh)) : t('logins.notYet')),
  ];
  if (opts.withCheck) rows.push(...loginRow(t('logins.lastCheck'), isSet(h.lastValidate) ? clock(new Date(h.lastValidate)) : t('logins.checkNotYet')));
  rows.push(...loginRow(t('logins.loginUntil'), isSet(h.loginUntil) ? t(opts.untilKey, { date: dayDate(h.loginUntil) }) : t('logins.unknown')));
  card.append(el('dl', { class: 'login-rows' }, ...rows));
  if (h.lastError) card.append(el('p', { class: 'login-error' }, t('logins.error', { error: h.lastError })));
  const btn = el('button', { class: 'btn' }, t('logins.renew'));
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try {
      await call(opts.renew);
      toast(t('logins.renewed', { service: t('logins.' + service) }));
    } catch (e) { /* toast shown by call */ } finally { btn.disabled = false; refresh(); }
  });
  // Renewing doesn't help a login that ends or lacks permissions: offer
  // connecting again, first.
  const actions = el('div', { class: 'login-actions' }, btn);
  if (st === 'attn') actions.prepend(el('button', { class: 'btn primary', onclick: () => openSettings(opts.section) }, t('logins.reconnect')));
  card.append(actions);
  return card;
}

// Green only once Pear Desktop answered: darkbot doesn't poll it, so right
// after start nothing is known yet (grey "not checked" until "Check now"
// or the first song request).
const ytmWorks = (h) => h.linked && !h.lastError && isSet(h.lastOk);

// YouTube Music has no token that expires: Pear Desktop allowed darkbot
// once, so the card shows that and whether Pear Desktop answers.
function ytmCard(h, enabled) {
  let st = 'off';
  if (h.linked && enabled) st = h.lastError ? 'bad' : (ytmWorks(h) ? 'ok' : 'idle');
  const card = el('div', { class: 'login-card ' + st });
  card.append(cardHead('ytmusic', st, h.linked ? t('logins.ytmWho') : ''));
  if (!h.linked || !enabled) {
    card.append(el('button', { class: 'btn', onclick: () => openSettings('songs') }, t('logins.connect')));
    return card;
  }
  card.append(el('dl', { class: 'login-rows' },
    ...loginRow(t('logins.ytmAllowed'), isSet(h.authorizedAt) ? dayDate(h.authorizedAt) : t('logins.unknown')),
    ...loginRow(t('logins.ytmLastOK'), isSet(h.lastOk) ? clock(new Date(h.lastOk)) : t('logins.notYet'))));
  if (h.lastError) card.append(el('p', { class: 'login-error' }, t('logins.error', { error: h.lastError })));
  const btn = el('button', { class: 'btn' }, t('logins.ytmCheck'));
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try {
      await call('YTMusicCheck');
      toast(t('logins.ytmChecked'));
    } catch (e) { /* toast shown by call */ } finally { btn.disabled = false; refresh(); }
  });
  card.append(btn);
  return card;
}

function renderLogins() {
  if (!state) return;
  const th = state.twitchHealth || {};
  const sh = state.spotifyHealth || {};
  const cards = [
    loginCard('twitch', th, { who: state.twitchLogin, section: 'twitch', renew: 'RefreshTwitchToken',
      attn: state.needsReconnect ? t('logins.needsReconnect') : '',
      nextRefresh: isSet(th.nextRefresh) ? th.nextRefresh : '', withCheck: true, untilKey: 'logins.twitchUntil' }),
    loginCard('spotify', sh, { who: state.spotifyUser, section: 'songs', renew: 'RefreshSpotifyToken',
      nextRefresh: '', withCheck: false, untilKey: 'logins.spotifyUntil', warnEnding: true })];
  cards.push(ytmCard(state.ytmusicHealth || {}, state.config.songs.ytmusic_enabled));
  $('logins').replaceChildren(...cards);
}
setInterval(() => { if (!$('viewDash').classList.contains('hidden')) renderLogins(); }, LOGIN_TICK_MS);

// ---------- events (follows, subs, bits, ...) ----------

let eventKinds = [];   // [{kind, hasAmount, hasChat}] from Go
let evEditing = null;  // kind open in the editor

// powerUpName: a custom Power-up by its title, Twitch's own by name (same
// rule as powerUpName in events_app.go).
function powerUpName(ev) {
  if (ev.power_up_type === 'custom' && ev.power_up_title) return ev.power_up_title;
  const key = 'powerup.' + ev.power_up_type;
  return ev.power_up_type && Object.prototype.hasOwnProperty.call(I18N.dict, key) ? t(key) : t('powerup.other');
}

// "{user} cheered {amount} Bits" -> text nodes with the name in bold.
function eventLine(ev) {
  const key = 'feed.ev.' + ev.kind;
  const who = ev.anonymous || !ev.user ? t('event.anonymous') : ev.user;
  const parts = t(key, { amount: num(ev.amount), tier: ev.tier, months: ev.months, powerup: powerUpName(ev) }).split('{user}');
  const nodes = [];
  parts.forEach((p, i) => {
    if (i > 0) nodes.push(el('b', {}, who));
    if (p) nodes.push(p);
  });
  return nodes;
}

function actionSummary(act) {
  if (!act || !act.enabled) return t('events.off');
  const bits = [];
  if (act.sound) bits.push(t('events.sound'));
  if (act.chat) bits.push(t('events.chat'));
  if (act.visual || act.overlay_text) bits.push(t('events.alert'));
  if (!bits.length) return t('events.noAction');
  return bits.join(' + ') + (act.min_amount > 0 ? ', ' + t('events.from', { n: num(act.min_amount) }) : '');
}

function renderEvents() {
  if (!state) return;
  const actions = state.config.events || {};
  $('eventsReconnect').classList.toggle('hidden', !state.needsReconnect);
  $('eventList').replaceChildren(...eventKinds.map((k) => {
    const act = actions[k.kind];
    const on = !!(act && act.enabled && (act.sound || act.chat || act.visual || act.overlay_text));
    return el('li', { class: 'event-row' + (on ? ' on' : '') },
      el('span', { class: 'event-led', 'aria-hidden': 'true' }),
      el('div', { class: 'event-text' },
        el('b', {}, t('event.' + k.kind)),
        el('span', {}, t('eventdesc.' + k.kind))),
      el('span', { class: 'event-summary' }, actionSummary(act)),
      el('button', { class: 'btn quiet', onclick: () => testEvent(k.kind) }, t('events.test')),
      el('button', { class: 'btn', onclick: () => openEventEditor(k) }, t('events.edit')));
  }));

  $('unfollowDetection').checked = !!state.config.unfollow_detection;
  const fs = state.followerStatus;
  let status = t('events.unfollowOff');
  if (state.config.unfollow_detection) {
    if (fs && fs.tooMany) status = t('events.unfollowTooMany', { n: num(fs.total) });
    else if (fs && isSet(fs.lastCheck)) status = t('events.unfollowWatching', { n: num(fs.tracking), time: clock(new Date(fs.lastCheck)) });
    else status = t('events.unfollowStarting');
  }
  $('unfollowStatus').textContent = status;
}

$('unfollowDetection').addEventListener('change', async () => {
  try { await call('SetUnfollowDetection', $('unfollowDetection').checked); } catch (e) { /* toast shown */ }
  refresh();
});

async function testEvent(kind) {
  try { await call('TestEvent', kind); toast(t('events.tested')); } catch (e) { /* toast shown */ }
}

// suggestedChat: darkbot's message suggestion for a kind in the UI
// language ("" for kinds without one, like an ended sub).
function suggestedChat(kind) {
  return I18N.dict['evdefault.' + kind] || '';
}

function openEventEditor(k) {
  evEditing = k.kind;
  // Never set up: start from the suggestion. Nothing is sent before Save.
  const act = (state.config.events || {})[k.kind]
    || { enabled: true, sound: '', volume: 1, chat: suggestedChat(k.kind), min_amount: 0 };
  $('evTitle').textContent = t('event.' + k.kind);
  $('evDesc').textContent = t('eventdesc.' + k.kind);
  $('evEnabled').checked = act.enabled;
  $('evSound').value = act.sound || '';
  $('evVolume').value = act.volume || 1;
  $('evVolumeVal').textContent = pct(+$('evVolume').value);
  $('evChat').value = act.chat || '';
  $('evMin').value = act.min_amount || 0;
  $('evVisual').value = act.visual || '';
  $('evOverlayText').value = act.overlay_text || '';
  $('evSeconds').value = act.seconds || 0;
  $('evOverlayField').classList.toggle('hidden', !k.hasChat); // never name unfollowers on screen
  $('evAlertState').textContent = alertStateText();
  $('evChatField').classList.toggle('hidden', !k.hasChat);
  $('evSuggest').classList.toggle('hidden', !suggestedChat(k.kind));
  $('evMinField').classList.toggle('hidden', !k.hasAmount);
  $('evError').classList.add('hidden');
  $('evEditor').showModal();
}

function evAction() {
  return {
    enabled: $('evEnabled').checked,
    sound: $('evSound').value,
    volume: +$('evVolume').value,
    chat: $('evChat').value.trim(),
    min_amount: Math.max(0, parseInt($('evMin').value, 10) || 0),
    visual: $('evVisual').value,
    overlay_text: $('evOverlayText').value.trim(),
    seconds: Math.min(60, Math.max(0, parseInt($('evSeconds').value, 10) || 0)),
  };
}

// alertStateText: whether alerts can show right now, and what's missing.
function alertStateText() {
  if (!state.obsConnected) return t('evedit.alertNoObs');
  const sc = state.obsScene;
  if (sc && (!sc.exists || sc.missing.length)) return t('evedit.alertNoScene');
  return t('evedit.alertReady');
}

$('evVolume').addEventListener('input', () => { $('evVolumeVal').textContent = pct(+$('evVolume').value); });
$('evPick').addEventListener('click', async () => {
  const f = await call('PickSoundFile');
  if (f) $('evSound').value = f;
});
$('evClear').addEventListener('click', () => { $('evSound').value = ''; });
$('evPickVisual').addEventListener('click', async () => {
  const f = await call('PickVisualFile');
  if (f) $('evVisual').value = f;
});
$('evClearVisual').addEventListener('click', () => { $('evVisual').value = ''; });
// A chat message is one line: Enter doesn't add a line break.
$('evChat').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
$('evSuggest').addEventListener('click', () => { $('evChat').value = suggestedChat(evEditing); $('evChat').focus(); });
$('evCancel').addEventListener('click', () => $('evEditor').close());
$('evSave').addEventListener('click', async () => {
  $('evSave').disabled = true;
  try {
    await App().SaveEventAction(evEditing, evAction());
    $('evEditor').close();
    toast(t('evedit.saved'));
    refresh();
  } catch (e) {
    $('evError').textContent = errText(e);
    $('evError').classList.remove('hidden');
  } finally {
    $('evSave').disabled = false;
  }
});
// Test from the editor saves first, so the test uses what is on screen.
$('evTest').addEventListener('click', async () => {
  try {
    await App().SaveEventAction(evEditing, evAction());
    await refresh();
    await testEvent(evEditing);
  } catch (e) {
    $('evError').textContent = errText(e);
    $('evError').classList.remove('hidden');
  }
});

// ---------- chat: commands and timers ----------

const PERMISSIONS = ['everyone', 'subscriber', 'vip', 'moderator', 'broadcaster'];
const DEFAULT_TIMER_MINUTES = 15;
let cmdEditing = null;    // command open in the editor (null id = new)
let timerEditing = null;  // timer open in the editor

// A "!" command or a word at the start of a message (older settings have
// no trigger: "!" commands).
const isWord = (c) => c.trigger === 'word';
const cmdNames = (c) => {
  const p = isWord(c) ? '' : '!';
  return [p + c.name, ...(c.aliases || []).map((a) => p + a)].join(', ');
};
const cmdAnswer = (c) => c.response_file ? t('cmds.fileAnswer', { file: basename(c.response_file) }) : (c.response || t('cmds.soundOnly'));

function cmdSummary(c) {
  const bits = [t('perm.' + c.permission)];
  if (c.once_per_stream) bits.push(t('cmds.once'));
  if (c.cooldown_seconds > 0) bits.push(t('cmds.cooldown', { n: num(c.cooldown_seconds) }));
  if (c.sound) bits.push(t('events.sound'));
  return bits.join(', ');
}

function timerSummary(tm) {
  const bits = [t('cmds.every', { n: num(tm.interval_minutes) })];
  if (tm.min_lines > 0) bits.push(t('cmds.minLines', { n: num(tm.min_lines) }));
  return bits.join(', ');
}

function chatRow(on, title, sub, summary, open) {
  return el('li', { class: 'event-row' + (on ? ' on' : '') },
    el('span', { class: 'event-led', 'aria-hidden': 'true' }),
    el('div', { class: 'event-text' }, el('b', {}, title), el('span', {}, sub)),
    el('span', { class: 'event-summary' }, summary),
    el('button', { class: 'btn', onclick: open }, t('events.edit')));
}

function renderChat() {
  if (!state) return;
  const c = state.config.chat || {};
  const cmds = c.commands || [];
  const timers = c.timers || [];
  $('chatMissing').classList.toggle('hidden', !state.twitchLogin || state.chatReady);
  $('cmdsEnabled').checked = !!c.commands_enabled;
  $('timersEnabled').checked = !!c.timers_enabled;
  $('cmdEmpty').classList.toggle('hidden', cmds.length > 0);
  $('timerEmpty').classList.toggle('hidden', timers.length > 0);
  $('cmdList').replaceChildren(...cmds.map((cmd) => chatRow(
    c.commands_enabled && cmd.enabled, cmdNames(cmd), cmdAnswer(cmd), cmdSummary(cmd), () => openCommandEditor(cmd))));
  $('timerList').replaceChildren(...timers.map((tm) => chatRow(
    c.timers_enabled && tm.enabled, timerSummary(tm), tm.message, '', () => openTimerEditor(tm))));
  renderClips();
  let st = t('cmds.timersOff');
  if (c.timers_enabled) st = state.live ? t('cmds.timersLive') : t('cmds.timersWaiting');
  $('timersState').textContent = st;
}

// ---------- clip command ----------

function renderClips() {
  const cl = state.config.clips || {};
  $('clipsMissing').classList.toggle('hidden', !state.twitchLogin || state.clipsReady);
  const bits = [t('perm.' + cl.permission)];
  if (cl.cooldown_seconds > 0) bits.push(t('cmds.cooldown', { n: num(cl.cooldown_seconds) }));
  $('clipList').replaceChildren(chatRow(!!cl.enabled, '!' + cl.name, t('clips.summary'), bits.join(', '), openClipEditor));
}

$('clipPermission').replaceChildren(...PERMISSIONS.map((p) => el('option', { value: p }, '')));
function openClipEditor() {
  const cl = state.config.clips || {};
  for (const o of $('clipPermission').options) o.textContent = t('perm.' + o.value);
  $('clipEnabled').checked = !!cl.enabled;
  $('clipName').value = cl.name || '';
  $('clipPermission').value = cl.permission;
  $('clipCooldown').value = cl.cooldown_seconds || 0;
  $('clipTitleTpl').value = cl.title || '';
  $('clipError').classList.add('hidden');
  $('clipEditor').showModal();
  $('clipName').focus();
}
$('clipCancel').addEventListener('click', () => $('clipEditor').close());
$('clipSave').addEventListener('click', async () => {
  $('clipSave').disabled = true;
  try {
    await App().SaveClips({
      enabled: $('clipEnabled').checked,
      name: $('clipName').value,
      permission: $('clipPermission').value,
      cooldown_seconds: intIn('clipCooldown'),
      title: $('clipTitleTpl').value,
    });
    $('clipEditor').close();
    toast(t('clips.saved'));
    refresh();
  } catch (e) { editorFailed('clipError', e); } finally { $('clipSave').disabled = false; }
});

$('cmdsEnabled').addEventListener('change', async () => {
  try { await call('SetCommandsEnabled', $('cmdsEnabled').checked); } catch (e) { /* toast shown */ }
  refresh();
});
$('timersEnabled').addEventListener('change', async () => {
  try { await call('SetTimersEnabled', $('timersEnabled').checked); } catch (e) { /* toast shown */ }
  refresh();
});

$('cmdPermission').replaceChildren(...PERMISSIONS.map((p) => el('option', { value: p }, '')));
function fillPermissionLabels() {
  for (const o of $('cmdPermission').options) o.textContent = t('perm.' + o.value);
}

let cmdDeleteArmed = 0;
// The editor follows the trigger (the "!" before the name) and the answer
// source (text or file).
function cmdEditorMode() {
  const word = $('cmdTrigger').value === 'word';
  $('cmdBang').classList.toggle('hidden', word);
  $('cmdNameLabel').textContent = t(word ? 'cmdedit.word' : 'cmdedit.name');
  $('cmdNameTip').dataset.tip = word ? 'tip.cmdWord' : 'tip.cmdName';
  $('cmdName').placeholder = t(word ? 'cmdedit.wordPlaceholder' : 'cmdedit.namePlaceholder');
  const file = $('cmdAnswerMode').value === 'file';
  $('cmdTextField').classList.toggle('hidden', file);
  $('cmdFileField').classList.toggle('hidden', !file);
}

// cmdFileInfo shows how many answers the chosen file has (or why it can't
// be used). Only the newest check may write, the file can change fast.
let cmdFileCheck = 0;
async function showFileInfo() {
  const path = $('cmdFile').value;
  const mine = ++cmdFileCheck;
  $('cmdFileInfo').textContent = '';
  $('cmdFileInfo').classList.remove('warn-text');
  if (!path) return;
  try {
    const n = await App().CountLines(path);
    if (mine === cmdFileCheck) $('cmdFileInfo').textContent = t('cmdedit.fileLines', { n: num(n) });
  } catch (e) {
    if (mine !== cmdFileCheck) return;
    $('cmdFileInfo').textContent = errText(e);
    $('cmdFileInfo').classList.add('warn-text');
  }
}

function openCommandEditor(cmd) {
  cmdEditing = cmd || { id: '', name: '', aliases: [], trigger: 'command', response: '', response_file: '', once_per_stream: false, permission: 'everyone', cooldown_seconds: 0, user_cooldown_seconds: 0, sound: '', volume: 1, enabled: true };
  fillPermissionLabels();
  $('cmdTitle').textContent = cmd ? cmdNames({ ...cmd, aliases: [] }) : t('cmdedit.new');
  $('cmdEnabled').checked = cmdEditing.enabled;
  $('cmdTrigger').value = isWord(cmdEditing) ? 'word' : 'command';
  $('cmdAnswerMode').value = cmdEditing.response_file ? 'file' : 'text';
  $('cmdFile').value = cmdEditing.response_file || '';
  $('cmdOnce').checked = !!cmdEditing.once_per_stream;
  cmdEditorMode();
  showFileInfo();
  $('cmdName').value = cmdEditing.name;
  $('cmdAliases').value = (cmdEditing.aliases || []).join(', ');
  $('cmdResponse').value = cmdEditing.response || '';
  $('cmdPermission').value = cmdEditing.permission;
  $('cmdCooldown').value = cmdEditing.cooldown_seconds || 0;
  $('cmdUserCooldown').value = cmdEditing.user_cooldown_seconds || 0;
  $('cmdSound').value = cmdEditing.sound || '';
  $('cmdVolume').value = cmdEditing.volume || 1;
  $('cmdVolumeVal').textContent = pct(+$('cmdVolume').value);
  $('cmdDelete').classList.toggle('hidden', !cmd);
  $('cmdDelete').textContent = t('cmdedit.delete');
  cmdDeleteArmed = 0;
  $('cmdError').classList.add('hidden');
  $('cmdEditor').showModal();
  $('cmdName').focus();
}
const intIn = (id) => Math.max(0, parseInt($(id).value, 10) || 0);
function editedCommand() {
  return {
    id: cmdEditing.id,
    name: $('cmdName').value,
    aliases: $('cmdAliases').value.split(/[,\s]+/).filter(Boolean),
    trigger: $('cmdTrigger').value,
    // Only the chosen answer source is kept.
    response: $('cmdAnswerMode').value === 'text' ? $('cmdResponse').value : '',
    response_file: $('cmdAnswerMode').value === 'file' ? $('cmdFile').value : '',
    once_per_stream: $('cmdOnce').checked,
    permission: $('cmdPermission').value,
    cooldown_seconds: intIn('cmdCooldown'),
    user_cooldown_seconds: intIn('cmdUserCooldown'),
    sound: $('cmdSound').value,
    volume: +$('cmdVolume').value,
    enabled: $('cmdEnabled').checked,
  };
}
function editorFailed(id, e) {
  $(id).textContent = errText(e);
  $(id).classList.remove('hidden');
}
$('cmdAdd').addEventListener('click', () => openCommandEditor(null));
$('cmdVolume').addEventListener('input', () => { $('cmdVolumeVal').textContent = pct(+$('cmdVolume').value); });
$('cmdPick').addEventListener('click', async () => {
  const f = await call('PickSoundFile');
  if (f) $('cmdSound').value = f;
});
$('cmdClear').addEventListener('click', () => { $('cmdSound').value = ''; });
$('cmdTrigger').addEventListener('change', () => {
  // A new greeting word answers each viewer once by default.
  if (!cmdEditing.id && $('cmdTrigger').value === 'word') $('cmdOnce').checked = true;
  cmdEditorMode();
});
$('cmdAnswerMode').addEventListener('change', cmdEditorMode);
$('cmdFilePick').addEventListener('click', async () => {
  const f = await call('PickTextFile');
  if (f) { $('cmdFile').value = f; showFileInfo(); }
});
$('cmdResponse').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
$('cmdCancel').addEventListener('click', () => $('cmdEditor').close());
$('cmdSave').addEventListener('click', async () => {
  $('cmdSave').disabled = true;
  try {
    await App().SaveCommand(editedCommand());
    $('cmdEditor').close();
    toast(t('cmdedit.saved'));
    refresh();
  } catch (e) { editorFailed('cmdError', e); } finally { $('cmdSave').disabled = false; }
});
$('cmdDelete').addEventListener('click', async () => {
  if (Date.now() - cmdDeleteArmed > CONFIRM_MS) {
    cmdDeleteArmed = Date.now();
    $('cmdDelete').textContent = t('cmdedit.deleteConfirm');
    return;
  }
  try {
    await App().DeleteCommand(cmdEditing.id);
    $('cmdEditor').close();
    toast(t('cmdedit.deleted'));
    refresh();
  } catch (e) { editorFailed('cmdError', e); }
});

let timerDeleteArmed = 0;
function openTimerEditor(tm) {
  timerEditing = tm || { id: '', message: '', interval_minutes: DEFAULT_TIMER_MINUTES, min_lines: 0, enabled: true };
  $('timerTitle').textContent = tm ? t('timeredit.edit') : t('timeredit.new');
  $('timerEnabled').checked = timerEditing.enabled;
  $('timerMessage').value = timerEditing.message;
  $('timerInterval').value = timerEditing.interval_minutes;
  $('timerLines').value = timerEditing.min_lines || 0;
  $('timerDelete').classList.toggle('hidden', !tm);
  $('timerDelete').textContent = t('timeredit.delete');
  timerDeleteArmed = 0;
  $('timerError').classList.add('hidden');
  $('timerEditor').showModal();
  $('timerMessage').focus();
}
$('timerAdd').addEventListener('click', () => openTimerEditor(null));
$('timerMessage').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
$('timerCancel').addEventListener('click', () => $('timerEditor').close());
$('timerSave').addEventListener('click', async () => {
  $('timerSave').disabled = true;
  try {
    await App().SaveTimer({
      id: timerEditing.id,
      message: $('timerMessage').value,
      interval_minutes: intIn('timerInterval'),
      min_lines: intIn('timerLines'),
      enabled: $('timerEnabled').checked,
    });
    $('timerEditor').close();
    toast(t('timeredit.saved'));
    refresh();
  } catch (e) { editorFailed('timerError', e); } finally { $('timerSave').disabled = false; }
});
$('timerDelete').addEventListener('click', async () => {
  if (Date.now() - timerDeleteArmed > CONFIRM_MS) {
    timerDeleteArmed = Date.now();
    $('timerDelete').textContent = t('timeredit.deleteConfirm');
    return;
  }
  try {
    await App().DeleteTimer(timerEditing.id);
    $('timerEditor').close();
    toast(t('timeredit.deleted'));
    refresh();
  } catch (e) { editorFailed('timerError', e); }
});

// ---------- activity feed ----------

const redeemed = () => ' ' + t('feed.redeemed') + ' ';
const FEED_TEXT = {
  queued: (a) => [a.user, redeemed(), a.rewardTitle, null],
  cooldown: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.cooldown', { time: a.message })],
  disabled: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.disabled')],
  queue_full: (a) => [a.user, redeemed(), a.rewardTitle, t('feed.queueFull')],
  error: (a) => a.event ? [t('feed.eventSound'), ' ', a.rewardTitle, a.message || t('feed.failed')] : [a.test ? t('feed.preview') : a.user, a.test ? ' ' + t('feed.previewOf') + ' ' : redeemed(), a.rewardTitle, a.message || t('feed.failed')],
  song_added: (a) => [a.user, ' ' + t('feed.requested') + ' ', a.title, null],
  song_rejected: (a) => [a.user, ' ' + t('feed.requested') + ' ', a.title, a.message],
  song_skipped: (a) => [a.user, ' ' + t('feed.skippedSong') + ' ', a.title, null],
  clip_made: (a) => [a.user, ' ' + t('feed.clipped') + ' ', clipLink(a), null],
};

// clipLink: the clip opens in the browser; only Twitch https links count.
function clipLink(a) {
  let u;
  try { u = new URL(a.url); } catch (e) { return a.title || a.url || ''; }
  const host = u.hostname;
  if (u.protocol !== 'https:' || !(host === 'twitch.tv' || host.endsWith('.twitch.tv'))) return a.title || '';
  return el('a', { href: u.href, title: t('feed.clipOpen'), onclick: (e) => { e.preventDefault(); window.runtime.BrowserOpenURL(u.href); } }, a.title || u.href);
}
const FEED_CLASS = { queued: 'played', song_added: 'played', clip_made: 'played', error: 'error' };

function onActivity(a) {
  if (a.kind === 'event') { addFeed(a); return; }
  if (a.kind === 'playing') playing.add(a.soundId);
  if (a.kind === 'done' || a.kind === 'error') playing.delete(a.soundId);
  const until = Date.parse(a.cooldownUntil);
  if (a.cooldownSeconds > 0 && until > Date.now()) {
    cooldowns.set(a.soundId, { until, total: a.cooldownSeconds * 1000 });
  }
  const pad = document.querySelector('.pad[data-id="' + CSS.escape(a.soundId) + '"]');
  if (pad) pad.classList.toggle('playing', playing.has(a.soundId));
  tickCooldowns();

  if (FEED_TEXT[a.kind]) addFeed(a);
}

function addFeed(a) {
  // Usually the newest; a follow found after darkbot was off is older and
  // goes where its time belongs.
  const at = Date.parse(a.at);
  const pos = activities.findIndex((o) => Date.parse(o.at) <= at);
  if (pos !== 0 && activities.length > 0) {
    if (pos < 0) {
      if (activities.length >= FEED_MAX) return; // older than everything kept
      activities.push(a);
    } else {
      activities.splice(pos, 0, a);
      if (activities.length > FEED_MAX) activities.length = FEED_MAX;
    }
    renderFeedSoon();
    return;
  }
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

// feedTime: the time today, the date on earlier days (full time as tooltip).
function feedTime(at) {
  const d = new Date(at);
  const today = d.toDateString() === new Date().toDateString();
  const full = d.toLocaleString(I18N.lang || undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
  return el('time', { datetime: at, title: full },
    today ? clock(d) : d.toLocaleDateString(I18N.lang || undefined, { day: 'numeric', month: 'numeric' }));
}

function feedItem(a) {
  if (a.kind === 'event') {
    const tag = a.test ? el('span', { class: 'test-tag' }, t('feed.test'))
      : a.event.missed ? el('span', { class: 'test-tag', title: t('feed.missedTitle') }, t('feed.missed')) : null;
    return el('li', { class: 'feed-item event ev-' + a.event.kind },
      feedTime(a.at),
      el('span', { class: 'what' }, ...(tag ? [tag, ' '] : []), ...eventLine(a.event)));
  }
  const text = FEED_TEXT[a.kind];
  if (!text) return null; // kept by a newer darkbot; nothing to show
  const [who, verb, what, why] = text(a);
  const kindClass = FEED_CLASS[a.kind] || 'skipped';
  return el('li', { class: 'feed-item ' + kindClass },
    feedTime(a.at),
    el('span', { class: 'what' }, el('b', {}, who), verb, el('b', {}, what)),
    why ? el('span', { class: 'why' }, why) : null);
}

// renderFeedSoon batches rebuilds: several follows found after darkbot was
// off arrive at once and are one rebuild.
let feedQueued = false;
function renderFeedSoon() {
  if (feedQueued) return;
  feedQueued = true;
  requestAnimationFrame(() => { feedQueued = false; renderFeed(); });
}

// Rebuilds the feed, e.g. after the language changed.
function renderFeed() {
  $('feed').replaceChildren(...activities.map(feedItem).filter(Boolean));
  $('feedEmpty').classList.toggle('hidden', activities.length > 0);
}

// loadFeed shows what darkbot kept (it outlives restarts and updates).
async function loadFeed() {
  try {
    activities.length = 0;
    activities.push(...((await App().GetActivity()) || []).slice(0, FEED_MAX));
  } catch (e) { toast(errText(e), true); }
  renderFeed();
}

// Two-step like deleting a sound.
let feedClearArmed = 0;
$('feedClear').addEventListener('click', async () => {
  const btn = $('feedClear');
  if (Date.now() - feedClearArmed > CONFIRM_MS) {
    feedClearArmed = Date.now();
    btn.textContent = t('feed.clearConfirm');
    setTimeout(() => { if (Date.now() - feedClearArmed >= CONFIRM_MS) btn.textContent = t('feed.clear'); }, CONFIRM_MS);
    return;
  }
  feedClearArmed = 0;
  btn.textContent = t('feed.clear');
  try {
    await App().ClearActivity();
    activities.length = 0;
    renderFeed();
  } catch (e) { toast(errText(e), true); }
});

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
// External links open in the system browser; the app window never navigates away.
$('credit').addEventListener('click', (e) => { e.preventDefault(); window.runtime.BrowserOpenURL($('credit').href); });

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
  $('autostartRow').classList.toggle('hidden', !state.autostartSupported);
  $('autostart').checked = state.autostart;
  $('closeToTrayRow').classList.toggle('hidden', !state.traySupported);
  $('closeToTray').checked = !state.config.quit_on_close;
  const sel = $('language');
  const chosen = state.config.language || '';
  sel.replaceChildren(
    el('option', { value: '', selected: chosen === '' }, t('general.languageSystem', { lang: t('lang.' + systemLanguage()) })),
    ...languages.map((l) => el('option', { value: l, selected: chosen === l }, t('lang.' + l))));
}
$('autostart').addEventListener('change', async () => {
  const on = $('autostart').checked;
  try { await call('SetAutostart', on); toast(t(on ? 'general.autostartOn' : 'general.autostartOff')); } catch (e) { /* toast shown */ }
  refresh();
});
$('closeToTray').addEventListener('change', async () => {
  try { await call('SetQuitOnClose', !$('closeToTray').checked); } catch (e) { /* toast shown */ }
  refresh();
});
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
}
$('login').addEventListener('click', async () => {
  await call('StartLogin');
  refresh();
});
$('cancelLogin').addEventListener('click', () => App().CancelLogin());
$('loginLink').addEventListener('click', () => { if (state.loginUri) window.runtime.BrowserOpenURL(state.loginUri); });
$('logout').addEventListener('click', async () => { await call('Logout'); refresh(); });

// obs
function renderOBS() {
  const o = state.config.obs;
  if (!$('sec-obs').contains(document.activeElement)) {
    $('obsHost').value = o.host;
    $('obsPort').value = o.port;
    $('obsAuto').checked = o.auto_connect;
  }
  $('obsStatus').textContent = state.obsConnected ? t('obs.connected')
    : state.obsAuthFailed ? t('obs.authFailed')
    : state.obsWaiting ? t('obs.waiting') : t('obs.notConnected');
  $('obsConnect').textContent = state.obsConnected ? t('obs.reconnect') : t('obs.connect');
  // While waiting, "Disconnect" stops the retries.
  $('obsDisconnect').classList.toggle('hidden', !state.obsConnected && !state.obsWaiting);
  renderOBSScene();
}
// The darkbot scene: one nested scene in every scene of the streamer.
function renderOBSScene() {
  const sc = state.obsScene;
  $('obsSceneBox').classList.toggle('hidden', !state.obsConnected);
  if (!sc) { $('obsSceneState').textContent = t('obs.sceneChecking'); $('obsSetup').classList.add('hidden'); return; }
  let text;
  if (!sc.exists) text = t('obs.sceneMissing');
  else if (sc.missing.length) text = t('obs.sceneMissingIn', { scenes: sc.missing.join(', ') });
  else text = t('obs.sceneReady', { n: num(sc.scenes) });
  $('obsSceneState').textContent = text;
  $('obsSetup').classList.toggle('hidden', sc.exists && !sc.missing.length);
  $('obsRemove').classList.toggle('hidden', !sc.exists);
}
$('obsSetup').addEventListener('click', async () => {
  $('obsSetup').disabled = true;
  try {
    const added = await App().SetupOBSScene();
    toast(added ? t('obs.setupAdded', { n: num(added) }) : t('obs.setupDone'));
  } catch (e) { toast(errText(e), true); }
  $('obsSetup').disabled = false;
  refresh();
});
// Two-step like deleting a sound: the first click asks, a second one
// within CONFIRM_MS removes.
let obsRemoveArmed = 0;
$('obsRemove').addEventListener('click', async () => {
  const btn = $('obsRemove');
  if (Date.now() - obsRemoveArmed > CONFIRM_MS) {
    obsRemoveArmed = Date.now();
    btn.textContent = t('obs.removeConfirm');
    setTimeout(() => { if (Date.now() - obsRemoveArmed >= CONFIRM_MS) btn.textContent = t('obs.remove'); }, CONFIRM_MS);
    return;
  }
  obsRemoveArmed = 0;
  btn.textContent = t('obs.remove');
  btn.disabled = true;
  try {
    await App().RemoveOBSScene();
    toast(t('obs.removed'));
  } catch (e) { toast(errText(e), true); }
  btn.disabled = false;
  refresh();
});
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
  const yh = state.ytmusicHealth || {};
  $('ytmStatus').textContent = yh.linked ? t('songs.ytmConnected') : t('songs.ytmNotConnected');
  $('ytmStatus').classList.toggle('on', ytmWorks(yh));
  $('ytmDisconnect').classList.toggle('hidden', !yh.linked);
  $('ytmProblem').textContent = yh.lastError ? t('songs.ytmProblem', { error: yh.lastError }) : '';
  $('ytmProblem').classList.toggle('hidden', !yh.lastError);
  $('ytmWaitBox').classList.toggle('hidden', !state.ytmusicConnecting);
  $('ytmConnect').disabled = !!state.ytmusicConnecting;
  $('ytmConnect').textContent = yh.linked ? t('songs.reconnect') : t('songs.ytmConnect');
  if (sec.dataset.touched) return;
  $('ytmEnabled').checked = c.ytmusic_enabled;
  $('ytmPort').value = c.ytmusic_port || YTM_DEFAULT_PORT;
  $('songsSearch').value = c.search_service;
  // Open the YouTube Music steps once while it is on but not connected.
  if (c.ytmusic_enabled && !yh.linked && !sec.dataset.ytmSetupShown) { $('ytmSetup').open = true; sec.dataset.ytmSetupShown = '1'; }
  $('spotifyClientId').value = c.spotify_client_id || '';
  $('songsEnabled').checked = c.enabled;
  $('songsCooldown').value = c.user_cooldown_seconds;
  $('songsMax').value = c.max_minutes;
  $('songsExplicit').checked = c.block_explicit;
  $('songsRequesterSkip').checked = c.requester_can_skip;
  $('songsReplies').checked = c.chat_replies;
  $('songsTitles').checked = !c.hide_titles;
}
for (const id of ['spotifyClientId', 'songsEnabled', 'songsCooldown', 'songsMax', 'songsExplicit', 'songsRequesterSkip', 'songsReplies', 'songsTitles', 'ytmEnabled', 'ytmPort', 'songsSearch']) {
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
    hide_titles: !$('songsTitles').checked,
    requester_can_skip: $('songsRequesterSkip').checked,
    ytmusic_enabled: $('ytmEnabled').checked,
    ytmusic_port: portValue($('ytmPort').value),
    search_service: $('songsSearch').value,
  };
}
// A port outside 1..65535 falls back to Pear Desktop's default.
function portValue(v) {
  const n = parseInt(v, 10);
  return n >= 1 && n <= MAX_PORT ? n : YTM_DEFAULT_PORT;
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
// Switching YouTube Music on opens its steps right away.
$('ytmEnabled').addEventListener('change', () => { if ($('ytmEnabled').checked && !(state.ytmusicHealth || {}).linked) $('ytmSetup').open = true; });
$('ytmDownload').addEventListener('click', () => window.runtime.BrowserOpenURL(PEAR_DOWNLOAD));
$('ytmConnect').addEventListener('click', async () => {
  // Connecting means using it: switch it on and save the port first.
  $('ytmEnabled').checked = true;
  await saveSongs();
  try {
    if (await call('YTMusicConnect')) {
      $('ytmSetup').open = false;
      toast(t('songs.ytmConnectedToast'));
    }
  } catch (e) { /* toast shown by call */ } finally { refresh(); }
});
$('ytmCancel').addEventListener('click', () => App().YTMusicCancelConnect());
$('ytmDisconnect').addEventListener('click', async () => { await call('YTMusicDisconnect'); refresh(); });

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
  $('updateNotes').textContent = updateNotesText((up && up.notes) || '');
}
// The release notes are Markdown for GitHub; the panel shows the "What's
// new" part as plain text (no downloads list, no # marks).
function updateNotesText(md) {
  return md.split(/^## Download/m)[0]
    .replace(/^## What's new\s*/m, '')
    .replace(/^#+\s*/gm, '')
    .trim();
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

// ---------- reward color ----------
// colorChosen: send the color to Twitch. Off only for an older sound whose
// color darkbot doesn't know yet, so saving never changes it by accident.
let colorChosen = false;
function setRewardColor(c, chosen, fromHex) {
  $('edColor').value = c.toLowerCase();
  if (!fromHex) $('edColorHex').value = chosen ? c.toUpperCase() : '';
  $('edColorHex').classList.remove('invalid');
  colorChosen = chosen;
  for (const b of document.querySelectorAll('#edSwatches .swatch')) {
    b.setAttribute('aria-pressed', String(chosen && b.dataset.color === c.toUpperCase()));
  }
}
for (const c of REWARD_COLORS) {
  $('edSwatches').append(colorChip(el('button', {
    type: 'button', class: 'swatch', 'data-color': c, title: c, 'aria-label': c,
    onclick: () => setRewardColor(c, true),
  }), c));
}
$('edColor').addEventListener('input', () => setRewardColor($('edColor').value.toUpperCase(), true));
// Typed hex: applied as soon as it is a full color; Go validates again on save.
const HEX_COLOR = /^#?[0-9a-f]{6}$/i;
$('edColorHex').addEventListener('input', () => {
  const v = $('edColorHex').value.trim();
  if (HEX_COLOR.test(v)) setRewardColor('#' + v.replace('#', '').toUpperCase(), true, true);
  else $('edColorHex').classList.toggle('invalid', v !== '');
});
$('edColorHex').addEventListener('blur', () => {
  if (colorChosen) $('edColorHex').value = $('edColor').value.toUpperCase();
});

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
  if (isNew) setRewardColor(REWARD_COLORS[Math.floor(Math.random() * REWARD_COLORS.length)], true);
  else if (rewardColor(editing)) setRewardColor(rewardColor(editing), true);
  else setRewardColor(REWARD_COLORS[0], false);
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
    reward_color: managed && colorChosen ? $('edColor').value.toUpperCase() : '',
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

// Errors from call() were shown already; anything else is a bug worth a
// console line instead of disappearing.
window.addEventListener('unhandledrejection', (ev) => {
  if (ev.reason && typeof ev.reason === 'object' && shownErrors.has(ev.reason)) ev.preventDefault();
  else console.error('unhandled', ev.reason);
});

window.addEventListener('DOMContentLoaded', async () => {
  window.runtime.EventsOn('state', refresh);
  window.runtime.EventsOn('nowplaying', npUpdate);
  window.runtime.EventsOn('play', playSound);
  window.runtime.EventsOn('activity', onActivity);
  window.runtime.EventsOn('stats', loadStats);
  window.runtime.EventsOn('log', () => { if ($('settings').open && $('sec-log').classList.contains('active')) renderLog(); });
  languages = (await App().GetLanguages()) || languages;
  eventKinds = (await App().GetEventKinds()) || [];
  await App().ReportSystemLanguage([...(navigator.languages || [navigator.language])]);
  await loadDevices(false);
  App().ReportAudioSupport(SINK_SUPPORTED, devices.length, devices.filter((d) => d.label).length);
  stats = await App().GetStats();
  await loadFeed();
  await refresh();
  if (state.twitchLogin) {
    try { rewards = (await App().GetRewards()) || []; renderPads(); } catch (e) { /* shown when editing */ }
  }
});
