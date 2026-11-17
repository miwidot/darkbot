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
  // Before login there is nothing to count or play: only the welcome card.
  document.querySelector('.views').classList.toggle('hidden', !loggedIn);
  $('dashBody').classList.toggle('hidden', !loggedIn);
  $('dashReset').classList.toggle('hidden', !loggedIn);
  $('dashSince').classList.toggle('hidden', !loggedIn);
  showView(loggedIn ? view : 'dash');
  renderDash();
  renderEvents();
  renderLogins();

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
}
for (const b of document.querySelectorAll('.views button')) {
  b.addEventListener('click', () => {
    view = b.dataset.view;
    try { localStorage.setItem(VIEW_KEY, view); } catch (e) { /* storage blocked: the view just isn't remembered */ }
    showView(view);
  });
}
const VIEWS = ['dash', 'sounds', 'events'];
try { const v = localStorage.getItem(VIEW_KEY); view = VIEWS.includes(v) ? v : 'dash'; } catch (e) { view = 'dash'; }

// ---------- dashboard ----------

const num = (n) => Number(n || 0).toLocaleString(I18N.lang || undefined);

// Four meters. The big number is this session; the lines below explain it.
// Channel meters: what Twitch reports about your channel.
const METERS_CHANNEL = [
  { id: 'follows', label: 'dash.follows', tip: 'tip.dashFollows',
    value: (s) => s.session.follows,
    detail: (s) => (state && state.config.unfollow_detection ? t('dash.followsDetail', { n: num(s.session.unfollows) }) : t('dash.followsNoDetect')),
    total: (s) => s.total.follows },
  { id: 'subs', label: 'dash.subs', tip: 'tip.dashSubs',
    value: (s) => s.session.subs,
    detail: (s) => t('dash.subsDetail', { n: num(s.session.giftedSubs) }),
    total: (s) => s.total.subs },
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
    detail: (s) => t('dash.redemptionsDetail', { played: num(s.session.played), refused: num(s.session.refused) }),
    total: (s) => s.total.redemptions },
  { id: 'commands', label: 'dash.commands', tip: 'tip.dashCommands',
    value: (s) => s.session.commands,
    detail: (s) => t('dash.commandsDetail', { skips: num(s.session.skips) }),
    total: (s) => s.total.commands },
  { id: 'songs', label: 'dash.songs', tip: 'tip.dashSongs',
    value: (s) => s.session.songsAdded,
    detail: (s) => t('dash.songsDetail', { rejected: num(s.session.songsRejected) }),
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
  $('dashSince').textContent = t('dash.since', {
    time: clock(new Date(stats.since)),
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
      el('div', { class: 'meter-detail' }, m.detail(stats)),
      m.total ? el('div', { class: 'meter-total' }, t('dash.total', { n: num(m.total(stats)) })) : null);
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

function loginCard(service, h, opts) {
  const ok = h.loggedIn && !h.lastError;
  const card = el('div', { class: 'login-card' + (h.loggedIn ? '' : ' off') + (h.lastError ? ' warn' : '') });
  const head = el('div', { class: 'login-head' },
    el('span', { class: 'login-led' + (ok ? ' on' : '') , 'aria-hidden': 'true' }),
    el('b', {}, t('logins.' + service)),
    el('span', { class: 'login-who' }, h.loggedIn ? opts.who : t('logins.off')));
  card.append(head);
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
  card.append(btn);
  return card;
}

function renderLogins() {
  if (!state) return;
  const th = state.twitchHealth || {};
  const sh = state.spotifyHealth || {};
  $('logins').replaceChildren(
    loginCard('twitch', th, { who: state.twitchLogin, section: 'twitch', renew: 'RefreshTwitchToken',
      nextRefresh: isSet(th.nextRefresh) ? th.nextRefresh : '', withCheck: true, untilKey: 'logins.twitchUntil' }),
    loginCard('spotify', sh, { who: state.spotifyUser, section: 'songs', renew: 'RefreshSpotifyToken',
      nextRefresh: '', withCheck: false, untilKey: 'logins.spotifyUntil' }));
}
setInterval(() => { if (!$('viewDash').classList.contains('hidden')) renderLogins(); }, LOGIN_TICK_MS);

// ---------- events (follows, subs, bits, ...) ----------

let eventKinds = [];   // [{kind, hasAmount, hasChat}] from Go
let evEditing = null;  // kind open in the editor

// "{user} cheered {amount} Bits" -> text nodes with the name in bold.
function eventLine(ev) {
  const key = ev.kind === 'sub' && ev.gift ? 'feed.ev.subGift' : 'feed.ev.' + ev.kind;
  const who = ev.anonymous || !ev.user ? t('event.anonymous') : ev.user;
  const parts = t(key, { amount: num(ev.amount), tier: ev.tier, months: ev.months }).split('{user}');
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
  if (!bits.length) return t('events.noAction');
  return bits.join(' + ') + (act.min_amount > 0 ? ', ' + t('events.from', { n: num(act.min_amount) }) : '');
}

function renderEvents() {
  if (!state) return;
  const actions = state.config.events || {};
  $('eventsReconnect').classList.toggle('hidden', !state.needsReconnect);
  $('eventList').replaceChildren(...eventKinds.map((k) => {
    const act = actions[k.kind];
    const on = !!(act && act.enabled && (act.sound || act.chat));
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

function openEventEditor(k) {
  evEditing = k.kind;
  const act = (state.config.events || {})[k.kind] || { enabled: true, sound: '', volume: 1, chat: '', min_amount: 0 };
  $('evTitle').textContent = t('event.' + k.kind);
  $('evDesc').textContent = t('eventdesc.' + k.kind);
  $('evEnabled').checked = act.enabled;
  $('evSound').value = act.sound || '';
  $('evVolume').value = act.volume || 1;
  $('evVolumeVal').textContent = pct(+$('evVolume').value);
  $('evChat').value = act.chat || '';
  $('evMin').value = act.min_amount || 0;
  $('evChatField').classList.toggle('hidden', !k.hasChat);
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
  };
}

$('evVolume').addEventListener('input', () => { $('evVolumeVal').textContent = pct(+$('evVolume').value); });
$('evPick').addEventListener('click', async () => {
  const f = await call('PickSoundFile');
  if (f) $('evSound').value = f;
});
$('evClear').addEventListener('click', () => { $('evSound').value = ''; });
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
};
const FEED_CLASS = { queued: 'played', song_added: 'played', error: 'error' };

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
  if (a.kind === 'event') {
    return el('li', { class: 'feed-item event ev-' + a.event.kind },
      el('time', { datetime: a.at }, clock(new Date(a.at))),
      el('span', { class: 'what' }, ...(a.test ? [el('span', { class: 'test-tag' }, t('feed.test')), ' '] : []), ...eventLine(a.event)));
  }
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
  window.runtime.EventsOn('stats', loadStats);
  window.runtime.EventsOn('log', () => { if ($('settings').open && $('sec-log').classList.contains('active')) renderLog(); });
  languages = (await App().GetLanguages()) || languages;
  eventKinds = (await App().GetEventKinds()) || [];
  await App().ReportSystemLanguage([...(navigator.languages || [navigator.language])]);
  await loadDevices(false);
  App().ReportAudioSupport(SINK_SUPPORTED, devices.length, devices.filter((d) => d.label).length);
  stats = await App().GetStats();
  await refresh();
  if (state.twitchLogin) {
    try { rewards = (await App().GetRewards()) || []; renderPads(); } catch (e) { /* shown when editing */ }
  }
});
