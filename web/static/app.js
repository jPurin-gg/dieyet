import { api, ApiError } from '/static/api.js';
import { startScanner } from '/static/scan.js';

const $ = (id) => document.getElementById(id);
const views = ['login', 'home', 'scan', 'product', 'record', 'manual'];

const state = {
  user: null,
  date: localDate(new Date()),
  stopScanner: null,
  // product being recorded / edited: { barcode, product }
  current: null,
};

// ---------------------------------------------------------------- helpers

function localDate(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function shiftDate(dateStr, days) {
  const [y, m, d] = dateStr.split('-').map(Number);
  const dt = new Date(y, m - 1, d + days);
  return localDate(dt);
}

function labelFor(dateStr) {
  const today = localDate(new Date());
  if (dateStr === today) return '今日';
  if (dateStr === shiftDate(today, -1)) return '昨日';
  const [y, m, d] = dateStr.split('-').map(Number);
  const w = '日月火水木金土'[new Date(y, m - 1, d).getDay()];
  return `${m}/${d} (${w})`;
}

function show(name) {
  for (const v of views) $(`view-${v}`).hidden = v !== name;
  if (name !== 'scan' && state.stopScanner) {
    state.stopScanner();
    state.stopScanner = null;
  }
  window.scrollTo(0, 0);
}

let toastTimer = null;
function toast(msg, ms = 2500) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), ms);
}

function fmt(n, digits = 1) {
  if (n === null || n === undefined) return '？';
  return Number(n).toFixed(digits).replace(/\.0$/, '');
}

function numOrNull(v) {
  const s = String(v ?? '').trim();
  if (s === '') return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

function readNutrients(form) {
  const f = new FormData(form);
  return {
    kcal: numOrNull(f.get('kcal')),
    protein_g: numOrNull(f.get('protein_g')),
    fat_g: numOrNull(f.get('fat_g')),
    carbs_g: numOrNull(f.get('carbs_g')),
    salt_g: numOrNull(f.get('salt_g')),
  };
}

function fillNutrients(form, n) {
  for (const k of ['kcal', 'protein_g', 'fat_g', 'carbs_g', 'salt_g']) {
    form.elements[k].value = n && n[k] !== null && n[k] !== undefined ? n[k] : '';
  }
}

async function guard(fn, errorEl) {
  try {
    if (errorEl) errorEl.textContent = '';
    await fn();
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      state.user = null;
      show('login');
      return;
    }
    const msg = e && e.message ? e.message : String(e);
    if (errorEl) errorEl.textContent = msg; else toast(msg, 4000);
  }
}

// ---------------------------------------------------------------- home

async function loadDay() {
  $('day-label').textContent = labelFor(state.date);
  const data = await api.logs(state.date);
  const t = data.totals;
  const setTotal = (key, id, digits) => {
    $(`t-${id}`).textContent = fmt(t[key].sum, digits);
    const u = $(`t-${id}-unknown`);
    u.hidden = t[key].unknown === 0;
    u.textContent = t[key].unknown ? `？${t[key].unknown}` : '';
  };
  setTotal('kcal', 'kcal', 0);
  setTotal('protein_g', 'protein', 1);
  setTotal('fat_g', 'fat', 1);
  setTotal('carbs_g', 'carbs', 1);
  setTotal('salt_g', 'salt', 2);
  $('t-note').hidden = t.complete;

  const ul = $('entries');
  ul.replaceChildren();
  for (const e of data.entries) {
    const li = document.createElement('li');
    const info = document.createElement('div');
    info.className = 'info';
    const name = document.createElement('div');
    name.className = 'name';
    name.textContent = e.name;
    const sub = document.createElement('div');
    sub.className = 'sub';
    const time = new Date(e.eaten_at);
    const hhmm = isNaN(time) ? '' : `${String(time.getHours()).padStart(2, '0')}:${String(time.getMinutes()).padStart(2, '0')} · `;
    sub.textContent = `${hhmm}${e.unit} × ${fmt(e.servings, 2)} · P ${fmt(e.protein_g)} / F ${fmt(e.fat_g)} / C ${fmt(e.carbs_g)} / 塩 ${fmt(e.salt_g, 2)}`;
    info.append(name, sub);
    const kcal = document.createElement('div');
    kcal.className = 'kcal';
    kcal.textContent = e.kcal === null ? '？ kcal' : `${fmt(e.kcal, 0)} kcal`;
    const del = document.createElement('button');
    del.className = 'del';
    del.setAttribute('aria-label', '削除');
    del.textContent = '×';
    del.addEventListener('click', () => guard(async () => {
      if (!confirm(`「${e.name}」の記録を消しますか？`)) return;
      await api.deleteLog(e.id);
      await loadDay();
    }));
    li.append(info, kcal, del);
    ul.append(li);
  }
  $('entries-empty').hidden = data.entries.length > 0;
}

async function goHome() {
  show('home');
  $('me-name').textContent = state.user ? state.user.nickname : '';
  await guard(loadDay);
}

$('day-prev').addEventListener('click', () => { state.date = shiftDate(state.date, -1); guard(loadDay); });
$('day-next').addEventListener('click', () => { state.date = shiftDate(state.date, 1); guard(loadDay); });
$('day-label').addEventListener('click', () => { state.date = localDate(new Date()); guard(loadDay); });
$('logout').addEventListener('click', () => guard(async () => {
  await api.logout();
  state.user = null;
  show('login');
}));

// ---------------------------------------------------------------- login

$('login-form').addEventListener('submit', (ev) => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  guard(async () => {
    const res = await api.login(f.get('nickname'), f.get('passphrase'));
    state.user = res.user;
    ev.target.reset();
    if (res.created) toast(`はじめまして、${res.user.nickname} として登録しました`, 4000);
    await goHome();
  }, $('login-error'));
});

// ---------------------------------------------------------------- scan

function openScanner() {
  show('scan');
  $('scan-status').textContent = 'カメラを起動しています…';
  state.stopScanner = startScanner($('scan-video'), {
    onStatus: (msg) => { $('scan-status').textContent = msg; },
    onCode: (code) => { state.stopScanner = null; onBarcode(code); },
  });
}

$('go-scan').addEventListener('click', openScanner);
$('scan-close').addEventListener('click', goHome);
$('scan-type').addEventListener('click', () => {
  const code = prompt('バーコードの番号（8桁か13桁）を入力してください');
  if (code && /^\d{8}$|^\d{12,14}$/.test(code.trim())) onBarcode(code.trim());
  else if (code) toast('番号の形式が違います');
});

async function onBarcode(code) {
  await guard(async () => {
    const res = await api.product(code);
    if (res.found) {
      state.current = { barcode: res.product.barcode, product: res.product };
      openRecord();
    } else {
      state.current = { barcode: code, product: null };
      openProductForm(res.suggestion);
    }
  });
}

// ---------------------------------------------------------------- product form

function openProductForm(suggestion) {
  const form = $('product-form');
  const p = state.current.product;
  form.reset();
  form.elements.barcode.value = state.current.barcode;
  form.elements.barcode.readOnly = true;
  if (p) {
    $('product-title').textContent = '商品を直す';
    $('product-note').textContent = '';
    form.elements.name.value = p.name;
    form.elements.brand.value = p.brand || '';
    form.elements.unit.value = p.unit;
    fillNutrients(form, p);
  } else if (suggestion) {
    $('product-title').textContent = '商品を登録';
    $('product-note').textContent = 'Open Food Facts のデータを入れておきました。パッケージの表示と見比べて直してください。';
    form.elements.name.value = suggestion.name || '';
    form.elements.brand.value = suggestion.brand || '';
    form.elements.unit.value = suggestion.unit || '';
    fillNutrients(form, suggestion);
  } else {
    $('product-title').textContent = '商品を登録';
    $('product-note').textContent = 'まだ登録のない商品です。パッケージの栄養成分表示を見て入力してください。';
    form.elements.unit.value = '1個';
  }
  show('product');
  form.elements.name.focus();
}

$('product-form').addEventListener('submit', (ev) => {
  ev.preventDefault();
  const form = ev.target;
  const f = new FormData(form);
  guard(async () => {
    const product = {
      name: f.get('name'),
      brand: f.get('brand'),
      unit: f.get('unit'),
      ...readNutrients(form),
      source: 'manual',
    };
    const saved = await api.putProduct(state.current.barcode, product);
    state.current = { barcode: saved.barcode, product: saved };
    toast('商品を保存しました');
    openRecord();
  }, $('product-error'));
});
$('product-cancel').addEventListener('click', goHome);

// ---------------------------------------------------------------- record

function updatePreview() {
  const p = state.current && state.current.product;
  if (!p) return;
  const s = Number($('record-form').elements.servings.value) || 0;
  const mul = (v) => (v === null || v === undefined ? '？' : fmt(v * s, 1));
  $('record-preview').textContent = `この量で: ${mul(p.kcal)} kcal · P ${mul(p.protein_g)} / F ${mul(p.fat_g)} / C ${mul(p.carbs_g)} / 塩 ${mul(p.salt_g)} g`;
}

function openRecord() {
  const p = state.current.product;
  $('record-name').textContent = p.name;
  $('record-sub').textContent = `${p.brand ? p.brand + ' · ' : ''}${p.unit} あたり ${fmt(p.kcal, 0)} kcal`;
  $('record-unit').textContent = p.unit;
  $('record-form').elements.servings.value = 1;
  updatePreview();
  show('record');
}

$('record-form').elements.servings.addEventListener('input', updatePreview);
$('record-form').addEventListener('submit', (ev) => {
  ev.preventDefault();
  guard(async () => {
    const servings = Number(ev.target.elements.servings.value);
    await api.addLog({ barcode: state.current.barcode, servings, date: state.date, eaten_at: new Date().toISOString() });
    toast('記録しました');
    await goHome();
  }, $('record-error'));
});
$('record-cancel').addEventListener('click', goHome);
$('record-edit').addEventListener('click', () => openProductForm(null));

// ---------------------------------------------------------------- manual entry

$('go-manual').addEventListener('click', () => {
  $('manual-form').reset();
  show('manual');
  $('manual-form').elements.name.focus();
});
$('manual-cancel').addEventListener('click', goHome);
$('manual-form').addEventListener('submit', (ev) => {
  ev.preventDefault();
  const form = ev.target;
  const f = new FormData(form);
  guard(async () => {
    await api.addLog({
      name: f.get('name'),
      unit: f.get('unit') || '1食',
      servings: Number(f.get('servings')) || 1,
      date: state.date,
      eaten_at: new Date().toISOString(),
      ...readNutrients(form),
    });
    toast('記録しました');
    await goHome();
  }, $('manual-error'));
});

// ---------------------------------------------------------------- boot

(async () => {
  try {
    const res = await api.me();
    state.user = res.user;
    await goHome();
  } catch (e) {
    show('login');
  }
})();
