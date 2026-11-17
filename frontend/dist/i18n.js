// UI translations. The strings live in internal/i18n/locales/*.json (shared
// with the Go side) and arrive through App.GetTranslations, so the app works
// offline and both sides can never disagree.
'use strict';

const I18N = {
  lang: '', // empty until the first loadLanguage, so boot always loads
  dict: {},
};

// t('points', { n: 5 }) -> "5 points". A missing key shows the key itself,
// so a forgotten translation is visible instead of an empty label.
function t(key, vars) {
  let s = Object.prototype.hasOwnProperty.call(I18N.dict, key) ? I18N.dict[key] : key;
  if (vars) {
    for (const [k, v] of Object.entries(vars)) s = s.split('{' + k + '}').join(String(v));
  }
  return s;
}

// Static markup carries data-i18n (text) and data-i18n-<attr> (attributes).
const I18N_ATTRS = ['title', 'placeholder', 'aria-label'];

function applyStaticText(root = document) {
  for (const node of root.querySelectorAll('[data-i18n]')) node.textContent = t(node.dataset.i18n);
  for (const attr of I18N_ATTRS) {
    const data = 'data-i18n-' + attr;
    for (const node of root.querySelectorAll('[' + data + ']')) node.setAttribute(attr, t(node.getAttribute(data)));
  }
  document.documentElement.lang = I18N.lang;
}

async function loadLanguage(lang) {
  I18N.dict = (await window.go.main.App.GetTranslations(lang)) || {};
  I18N.lang = lang;
  applyStaticText();
}
