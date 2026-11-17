// Tooltips: any element with data-tip="<i18n key>" explains itself on hover,
// keyboard focus or click/tap. One shared bubble (role=tooltip) is moved
// into the open <dialog> when needed, because modal dialogs sit in the top
// layer and would cover anything outside them.
'use strict';

const TIP_GAP = 8;     // px between trigger and bubble
const TIP_MARGIN = 8;  // px kept free at the window edges
const TIP_HOVER_MS = 250; // short delay so moving the mouse across doesn't flicker

const Tip = { bubble: null, owner: null, timer: 0 };

function tipBubble() {
  if (!Tip.bubble) {
    const b = document.createElement('div');
    b.id = 'tooltip';
    b.className = 'tooltip';
    b.setAttribute('role', 'tooltip');
    b.hidden = true;
    Tip.bubble = b;
  }
  return Tip.bubble;
}

function showTip(owner) {
  clearTimeout(Tip.timer);
  const b = tipBubble();
  const host = owner.closest('dialog[open]') || document.body;
  if (b.parentNode !== host) host.append(b);
  b.textContent = t(owner.dataset.tip);
  b.hidden = false;
  if (Tip.owner && Tip.owner !== owner) Tip.owner.removeAttribute('aria-describedby');
  Tip.owner = owner;
  owner.setAttribute('aria-describedby', b.id);
  placeTip(owner, b);
}

// Above the trigger when there is room, otherwise below; always inside the window.
function placeTip(owner, b) {
  const r = owner.getBoundingClientRect();
  const w = b.offsetWidth;
  const h = b.offsetHeight;
  const left = Math.min(Math.max(r.left + r.width / 2 - w / 2, TIP_MARGIN), window.innerWidth - w - TIP_MARGIN);
  let top = r.top - h - TIP_GAP;
  b.classList.toggle('below', top < TIP_MARGIN);
  if (top < TIP_MARGIN) top = r.bottom + TIP_GAP;
  b.style.left = Math.round(left) + 'px';
  b.style.top = Math.round(top) + 'px';
}

function hideTip() {
  clearTimeout(Tip.timer);
  if (Tip.bubble) Tip.bubble.hidden = true;
  if (Tip.owner) Tip.owner.removeAttribute('aria-describedby');
  Tip.owner = null;
}

const tipOwner = (e) => (e.target instanceof Element ? e.target.closest('[data-tip]') : null);

document.addEventListener('mouseover', (e) => {
  const o = tipOwner(e);
  if (!o || o === Tip.owner) return;
  clearTimeout(Tip.timer);
  Tip.timer = setTimeout(() => showTip(o), TIP_HOVER_MS);
});
document.addEventListener('mouseout', (e) => {
  const o = tipOwner(e);
  if (o && !o.contains(e.relatedTarget)) {
    clearTimeout(Tip.timer);
    if (o === Tip.owner && document.activeElement !== o) hideTip();
  }
});
document.addEventListener('focusin', (e) => { const o = tipOwner(e); if (o) showTip(o); });
document.addEventListener('focusout', (e) => { if (tipOwner(e) === Tip.owner) hideTip(); });

// Click/tap toggles (touch screens have no hover). The "?" sits inside
// <label>s: cancelling the click keeps it from ticking the checkbox or
// focusing the field next to it.
document.addEventListener('click', (e) => {
  const o = tipOwner(e);
  if (!o) { hideTip(); return; }
  if (o.classList.contains('tip')) { e.preventDefault(); e.stopPropagation(); }
  if (Tip.owner === o && !Tip.bubble.hidden && o.classList.contains('tip')) hideTip();
  else showTip(o);
}, true);

document.addEventListener('keydown', (e) => {
  const o = tipOwner(e);
  if (e.key === 'Escape' && Tip.owner) {
    // Close only the tooltip, not the dialog around it.
    e.preventDefault();
    e.stopPropagation();
    hideTip();
  } else if (o && o.classList.contains('tip') && (e.key === 'Enter' || e.key === ' ')) {
    e.preventDefault();
    showTip(o);
  }
}, true);

window.addEventListener('scroll', hideTip, true);
window.addEventListener('resize', hideTip);
