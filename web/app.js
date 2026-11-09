'use strict';
// SwiiftXML user interface. All data comes from the local SwiiftXML server.

const $ = (sel, el = document) => el.querySelector(sel);
const token = new URLSearchParams(location.search).get('t') || '';
const LINE_H = 24;   // must match --lh in style.css
const PAGE = 100;    // rows per page in the row list
const nf = new Intl.NumberFormat();
const fmt = n => nf.format(n);
const esc = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const clip = (s, n) => { s = String(s).replace(/\s+/g, ' '); return s.length > n ? s.slice(0, n) + '…' : s; };
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const isInput = el => el && /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName);

function size(n) {
  if (n < 1024) return fmt(n) + ' B';
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
}

const S = {
  st: null,             // last status from the server
  doc: null,            // st.doc
  mode: 'auto',         // what the user picked as rows: 'auto', 'L2' or 'N:Name'
  view: null,           // resolved view key used for requests
  total: 0,             // rows indexed so far
  page: { from: 0, rows: [] },
  row: -1,              // selected row
  rowGen: 0,            // bumped on every row change, to drop stale responses
  root: null,           // tree of the selected row
  lines: [],            // visible tree lines: {n, kind: 'node' | 'more' | 'loading'}
  sel: 0,               // selected line
  selLine: null,
  selKey: null,         // key of the selected node, kept when moving to another row
  focus: 'rows',
  expanded: new Set(),  // name paths of opened nodes, remembered across rows
  defaulted: new Set(), // row element names that got the default expansion
  search: { matchCase: false, hit: null, ctrl: null, re: null },
  searchMsg: '',
};

// ---------- server access ----------

async function api(path, params = {}, init = {}) {
  const url = new URL(path, location.origin);
  for (const [k, v] of Object.entries(params)) if (v != null) url.searchParams.set(k, v);
  const res = await fetch(url, { ...init, headers: { 'X-Token': token, ...(init.headers || {}) } });
  let body = null;
  try { body = await res.json(); } catch { /* not JSON */ }
  if (!res.ok) throw new Error((body && body.error) || res.statusText || 'Request failed');
  return body;
}
const post = (path, data) => api(path, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } });

let pollTimer = null, polling = false, pollAgain = false, failures = 0;
async function poll() {
  clearTimeout(pollTimer);
  if (polling) { pollAgain = true; return; }
  polling = true;
  try {
    const view = S.view;
    const st = await api('/api/status', { v: view });
    failures = 0;
    $('#stopped').hidden = true;
    await applyStatus(st, view);
  } catch (e) {
    if (++failures >= 2) $('#stopped').hidden = false;
  }
  polling = false;
  if (pollAgain) { pollAgain = false; return poll(); }
  const busy = S.doc && (!S.doc.index.done || (S.doc.named && !S.doc.named.done));
  pollTimer = setTimeout(poll, busy ? 400 : 2500);
}

async function applyStatus(st, askedView) {
  S.st = st;
  if (st.notice) toast(st.notice, true);
  opener.renderRecent();
  const doc = st.doc || null;
  if (!doc) { S.doc = null; showWelcome(); renderStatusBar(); return; }
  if (!S.doc || S.doc.path !== doc.path) resetForFile();
  S.doc = doc;
  showMain();
  renderHeader();
  renderViewSelect();
  renderProgress();

  const view = resolveView();
  if (view !== S.view) {
    // When the automatic choice changes, stay on the same record if possible.
    const switched = S.view !== null && S.mode === 'auto';
    const prevStart = S.root ? S.root.s : -1;
    Object.assign(S, { view, total: 0, row: -1, root: null, selKey: null, page: { from: 0, rows: [] }, searchMsg: '' });
    S.search.hit = null;
    let start = 0;
    if (switched && prevStart >= 0) {
      try { start = (await api('/api/locate', { v: view, off: prevStart })).row; } catch { /* start at the top */ }
    }
    if (switched) toast(`Rows are now the <${rowName()}> elements (detected automatically)`);
    await loadPage(start);
    if (S.total > 0) await selectRow(start); else refresh();
  } else if (askedView === S.view && doc.rows >= 0 && doc.rows !== S.total) {
    const total = doc.rows;
    S.total = total;
    const p = S.page;
    if (p.rows.length < PAGE && p.from + p.rows.length < total) await loadPage(p.from);
    else renderPager();
    if (S.row < 0 && total > 0) await selectRow(0);
    renderRowTitle();
  }
  renderStatusBar();
}

function resolveView() {
  const d = S.doc;
  if (S.mode.startsWith('N:')) {
    if (d.named && 'N:' + d.named.name === S.mode) return S.mode;
    S.mode = 'auto';
  }
  return S.mode === 'auto' ? d.auto : S.mode;
}

// viewName returns the element name rows have in a view ('L2:Order', 'N:Order' or 'L2').
function viewName(key) {
  if (!key) return '';
  if (key.startsWith('N:')) return key.slice(2);
  const i = key.indexOf(':');
  if (i > 0) return key.slice(i + 1);
  const lv = (S.doc.levels || []).find(l => 'L' + l.level === key);
  return lv ? lv.name : '';
}
const rowName = () => viewName(S.view);

const indexing = () => {
  const d = S.doc;
  if (!d) return false;
  return S.view && S.view.startsWith('N:') ? !(d.named && d.named.done) : !d.index.done;
};

function resetForFile() {
  Object.assign(S, {
    mode: 'auto', view: null, total: 0, page: { from: 0, rows: [] }, row: -1, root: null, lines: [],
    sel: 0, selLine: null, selKey: null, searchMsg: '',
  });
  S.expanded.clear();
  S.defaulted.clear();
  S.search.hit = null;
  S.search.re = null;
  $('#viewSel').dataset.html = '';
}

// ---------- header, status ----------

function renderHeader() {
  const d = S.doc;
  $('#fileinfo').innerHTML = `<b>${esc(d.fileName)}</b> · ${size(d.size)}`;
  $('#fileinfo').title = d.path;
  document.title = d.fileName + ' – SwiiftXML';
}

function renderViewSelect() {
  const d = S.doc, sel = $('#viewSel');
  const levels = d.levels || [];
  let html = `<option value="auto">Automatic: &lt;${esc(viewName(d.auto) || '…')}&gt;</option>`;
  // Names found on the first levels are already indexed; deeper ones need a pass of their own.
  const names = (d.names || []).filter(n => n.depth >= 1).slice(0, 40)
    .map(n => ({ key: n.level ? `L${n.level}:${n.name}` : `N:${n.name}`, name: n.name, count: n.level ? n.levelCount : n.count }));
  if (S.mode !== 'auto' && !names.some(n => n.key === S.mode) && !/^L\d$/.test(S.mode)) {
    names.unshift({ key: S.mode, name: viewName(S.mode), count: S.total });
  }
  if (names.length) {
    html += '<optgroup label="By element name">' +
      names.map(n => `<option value="${esc(n.key)}">&lt;${esc(n.name)}&gt; – ${fmt(n.count)}</option>`).join('') + '</optgroup>';
  }
  html += '<optgroup label="Everything on one nesting level">' +
    levels.map(l => `<option value="L${l.level}">Level ${l.level}: ${fmt(l.count)} elements, mostly &lt;${esc(l.name)}&gt;</option>`).join('') + '</optgroup>';
  if (html !== sel.dataset.html && document.activeElement !== sel) {
    sel.innerHTML = html;
    sel.dataset.html = html;
  }
  if (document.activeElement !== sel) sel.value = S.mode;
}

function renderProgress() {
  const d = S.doc, bar = $('#progbar');
  let p = 0;
  if (!d.index.done) p = d.index.scanned / d.size;
  else if (d.named && !d.named.done) p = d.named.scanned / d.size;
  bar.style.width = p > 0 ? Math.max(1, p * 100).toFixed(1) + '%' : '0';
}

function renderStatusBar() {
  const el = $('#statusText'), d = S.doc;
  if (S.searchMsg) { el.innerHTML = S.searchMsg; return; }
  if (!d) { el.textContent = 'No file open'; return; }
  const pct = n => Math.floor(100 * n / Math.max(1, d.size)) + '%';
  let msg;
  if (!d.index.done) {
    msg = `<span class="spin"></span>Indexing… ${pct(d.index.scanned)} (${size(d.index.scanned)} of ${size(d.size)}) – you can already browse the rows found so far`;
  } else if (d.named && !d.named.done) {
    msg = `<span class="spin"></span>Finding &lt;${esc(d.named.name)}&gt; elements… ${pct(d.named.scanned)}`;
  } else {
    msg = `${fmt(S.total)} rows of &lt;${esc(rowName())}&gt; · root &lt;${esc(d.root)}&gt; · ${esc(d.encoding)} · indexed in ${d.index.seconds.toFixed(1)} s`;
  }
  if (d.index.error) msg += ` · <span style="color:var(--danger)">Read error: ${esc(d.index.error)}</span>`;
  el.innerHTML = msg;
}

function setSearchMsg(html) { S.searchMsg = html; renderStatusBar(); }

let toastTimer;
function toast(msg, err) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'show' + (err ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ''; }, err ? 5000 : 2800);
}

// ---------- row list ----------

async function loadPage(from) {
  from = Math.max(0, Math.floor(from / PAGE) * PAGE);
  const view = S.view;
  try {
    const res = await api('/api/rows', { v: view, from, n: PAGE });
    if (view !== S.view) return;
    S.page = { from, rows: res.rows || [] };
    if (res.total > S.total) S.total = res.total;
  } catch (e) { toast(e.message, true); }
  renderRows();
}

function renderRows() {
  const list = $('#rowList'), rows = S.page.rows;
  if (!rows.length) {
    list.innerHTML = `<div class="empty">${indexing() ? '<span class="spin"></span>Looking for rows…' : 'No rows found.'}</div>`;
  } else {
    list.innerHTML = rows.map(r =>
      `<div class="r${r.i === S.row ? ' sel' : ''}" data-i="${r.i}"><span class="ri">${fmt(r.i + 1)}</span>` +
      `<span class="rn">${esc(r.n)}</span><span class="rp">${hl(r.p) || '&nbsp;'}</span></div>`).join('');
  }
  renderPager();
  scrollRowIntoView();
}

function renderPager() {
  const { from, rows } = S.page, plus = indexing() ? '+' : '';
  $('#pgInfo').textContent = S.total ? `${fmt(from + 1)}–${fmt(from + rows.length)} of ${fmt(S.total)}${plus}` : '';
  $('#rowsCount').textContent = S.total ? fmt(S.total) + plus : '';
  $('#pgFirst').disabled = $('#pgPrev').disabled = from === 0;
  $('#pgNext').disabled = $('#pgLast').disabled = from + PAGE >= S.total;
}

function markRow() {
  for (const el of $('#rowList').children) el.classList.toggle('sel', +el.dataset.i === S.row);
  scrollRowIntoView();
}

function scrollRowIntoView() {
  const el = $(`#rowList .r[data-i="${S.row}"]`);
  if (el) el.scrollIntoView({ block: 'nearest' });
}

function renderRowTitle() {
  const t = $('#rowTitle');
  if (S.row < 0 || !S.total) { t.innerHTML = ''; return; }
  let html = `Row ${fmt(S.row + 1)} <span class="muted">of ${fmt(S.total)}${indexing() ? '+' : ''}</span>`;
  if (S.root) {
    html += ` <span class="muted">·</span> &lt;${esc(S.root.n)}&gt;`;
    if (S.root.e > 0) html += ` <span class="muted">· ${size(S.root.e - S.root.s)}</span>`;
  }
  t.innerHTML = html;
  $('#rowPrev').disabled = S.row <= 0;
  $('#rowNext').disabled = S.row >= S.total - 1;
}

async function selectRow(i) {
  if (!S.view || !S.total) return;
  i = clamp(i, 0, S.total - 1);
  S.row = i;
  const gen = ++S.rowGen;
  if (i < S.page.from || i >= S.page.from + S.page.rows.length) await loadPage(i);
  else markRow();
  if (gen !== S.rowGen) return;
  renderRowTitle();
  try {
    const res = await api('/api/row', { v: S.view, i });
    if (gen !== S.rowGen) return;
    await setTree(res.node, gen);
  } catch (e) {
    if (gen === S.rowGen) toast(e.message, true);
  }
}

// ---------- tree model ----------

const kidName = k => (k.k === 'e' ? k.n : k.k === 't' ? '#text' : '#comment');
const expandable = n => n.k === 'e' && (n.nc > 0 || !!n.np || (n.c && n.c.length > 0));

// adopt links loaded children to their parent and gives them keys: key is
// unique within the row (Name[2]) and pkey is the plain name path used to
// remember which parts of the tree are open.
function adopt(parent, kids, reset) {
  if (reset || !parent.seen) parent.seen = {};
  for (const k of kids) {
    const nm = kidName(k);
    const idx = parent.seen[nm] = (parent.seen[nm] || 0) + 1;
    k.p = parent;
    k.d = parent.d + 1;
    k.key = `${parent.key}/${nm}[${idx}]`;
    k.pkey = `${parent.pkey}/${nm}`;
    if (k.c) adopt(k, k.c, true);
  }
  return kids;
}

async function setTree(node, gen) {
  Object.assign(node, { p: null, d: 0, key: node.n, pkey: node.n });
  if (node.c) adopt(node, node.c, true);
  S.root = node;
  if (!S.defaulted.has(node.n)) { defaultExpand(node); S.defaulted.add(node.n); }
  else applyRemembered(node);
  rebuild();
  // Keep the same field selected as in the previous row, or its closest ancestor.
  let i = -1;
  for (let key = S.selKey; key && i < 0; key = key.includes('/') ? key.slice(0, key.lastIndexOf('/')) : null) {
    i = S.lines.findIndex(l => l.kind === 'node' && l.n.key === key);
  }
  S.sel = Math.max(0, i);
  S.selLine = S.lines[S.sel];
  ensureVisible(S.sel);
  renderTree();
  renderDetails();
  renderRowTitle();
  await loadRemembered(node, gen);
}

// defaultExpand opens the first levels of the first row shown, as long as
// the tree stays reasonably small.
function defaultExpand(root) {
  root.x = true;
  S.expanded.add(root.pkey);
  let frontier = [root], count = 1 + (root.c ? root.c.length : 0);
  for (;;) {
    const next = [];
    for (const n of frontier) for (const k of n.c || []) if (expandable(k) && k.l) next.push(k);
    const add = next.reduce((s, k) => s + (k.c ? k.c.length : 0), 0);
    if (!next.length || count + add > 300) break;
    for (const k of next) { k.x = true; S.expanded.add(k.pkey); }
    count += add;
    frontier = next;
  }
}

function applyRemembered(n) {
  if (!expandable(n)) return;
  n.x = n.d === 0 || S.expanded.has(n.pkey);
  if (n.x && n.c) n.c.forEach(applyRemembered);
}

function findPending(n) {
  if (!n.x) return null;
  if (!n.l && !n.loading && !n.failed && expandable(n)) return n;
  for (const k of n.c || []) { const f = findPending(k); if (f) return f; }
  return null;
}

async function loadRemembered(root, gen) {
  for (let fetches = 0; fetches < 40; fetches++) {
    const n = findPending(root);
    if (!n) return;
    await loadChildren(n);
    if (gen !== S.rowGen) return;
    (n.c || []).forEach(applyRemembered);
    refresh();
  }
}

async function loadChildren(n) {
  if (n.l || n.loading) return;
  n.loading = true;
  const gen = S.rowGen;
  try {
    const r = await api('/api/children', { s: n.s });
    if (gen !== S.rowGen) return;
    n.c = adopt(n, r.c || [], true);
    n.l = true;
    n.nx = r.nx || 0;
    n.np = !!r.np;
    n.nc = r.nc;
    if (n.e < 0 && r.e >= 0) n.e = r.e;
  } catch (e) {
    n.failed = true;
    n.x = false;
    toast(e.message, true);
  } finally {
    n.loading = false;
  }
}

async function loadMore(n) {
  if (!n.nx || n.loadingMore) return;
  n.loadingMore = true;
  refresh();
  const gen = S.rowGen;
  try {
    const r = await api('/api/children', { s: n.s, c: n.nx });
    if (gen !== S.rowGen) return;
    n.c = (n.c || []).concat(adopt(n, r.c || [], false));
    n.nx = r.nx || 0;
    if (!n.nx) {
      if (n.np) { n.np = false; n.nc = n.c.length; }
      if (n.e < 0 && r.e >= 0) n.e = r.e;
    }
  } catch (e) {
    toast(e.message, true);
  } finally {
    n.loadingMore = false;
    if (gen === S.rowGen) refresh();
  }
}

async function toggle(n, open = !n.x) {
  if (!expandable(n)) return;
  if (open) {
    n.x = true;
    S.expanded.add(n.pkey);
    if (!n.l) { refresh(); await loadChildren(n); }
  } else {
    n.x = false;
    if (n.d > 0) S.expanded.delete(n.pkey);
  }
  refresh();
}

async function expandAll(start) {
  if (!start) return;
  const gen = S.rowGen, queue = [start];
  let fetches = 0, lines = S.lines.length;
  while (queue.length) {
    const n = queue.shift();
    if (!expandable(n)) continue;
    if (!n.l) {
      if (fetches++ >= 100) break;
      await loadChildren(n);
      if (gen !== S.rowGen) return;
    }
    if (!n.x) { n.x = true; lines += (n.c || []).length; }
    S.expanded.add(n.pkey);
    queue.push(...(n.c || []));
    if (lines > 20000) { toast('Stopped expanding – this part is very large. Open the parts you need instead.'); break; }
  }
  refresh();
}

function collapseAll() {
  if (!S.root) return;
  const walk = n => { if (n.d > 0) n.x = false; (n.c || []).forEach(walk); };
  walk(S.root);
  S.expanded.clear();
  S.expanded.add(S.root.pkey);
  refresh();
}

function rebuild() {
  const out = [];
  const walk = n => {
    out.push({ n, kind: 'node' });
    if (!n.x) return;
    if (!n.l && n.loading) { out.push({ n, kind: 'loading' }); return; }
    for (const k of n.c || []) walk(k);
    if (n.nx) out.push({ n, kind: 'more' });
  };
  if (S.root) walk(S.root);
  S.lines = out;
}

// refresh rebuilds the visible lines and keeps the selection on the same
// node (or its closest visible ancestor).
function refresh() {
  const prev = S.selLine;
  rebuild();
  let i = prev ? S.lines.findIndex(l => l.n === prev.n && l.kind === prev.kind) : -1;
  if (i < 0 && prev && prev.kind !== 'node') i = Math.min(S.sel, S.lines.length - 1);
  for (let p = prev && prev.n.p; i < 0 && p; p = p.p) i = S.lines.findIndex(l => l.n === p && l.kind === 'node');
  S.sel = Math.max(0, i);
  S.selLine = S.lines[S.sel] || null;
  renderTree();
  renderDetails();
}

// ---------- tree view ----------

const ind = d => `<span class="ind" style="--d:${d}"></span>`;
const countLabel = n => (n.np ? fmt(n.c ? n.c.length : n.nc) + '+' : fmt(n.nc));

function hl(s) {
  s = String(s);
  const re = S.search.re;
  if (!re) return esc(s);
  let out = '', last = 0;
  for (const m of s.matchAll(re)) {
    out += esc(s.slice(last, m.index)) + '<mark>' + esc(m[0]) + '</mark>';
    last = m.index + m[0].length;
  }
  return out + esc(s.slice(last));
}

function lineHTML(l, i) {
  const n = l.n, cls = 'ln' + (i === S.sel ? ' sel' : '');
  if (l.kind === 'more') {
    const left = n.np ? '' : ` (${fmt(n.nc - (n.c || []).length)} more)`;
    return `<div class="${cls} more" data-i="${i}">${ind(n.d + 1)}<span class="tw">…</span>` +
      `<span class="lm">${n.loadingMore ? 'Loading…' : `Load more${left}`}</span></div>`;
  }
  if (l.kind === 'loading') {
    return `<div class="${cls}" data-i="${i}">${ind(n.d + 1)}<span class="tw"></span><span class="ld">Loading…</span></div>`;
  }
  let tw = '', body;
  if (n.k === 'e') {
    const attrs = n.a || [];
    if (expandable(n)) tw = n.x ? '▾' : '▸';
    body = `<span class="tag">${hl(n.n)}</span>`;
    for (const [k, v] of attrs.slice(0, 6)) body += `<span class="an">${hl(k)}</span>=<span class="av">"${hl(clip(v, 80))}"</span>`;
    if (attrs.length > 6) body += '<span class="an">…</span>';
    if (!expandable(n)) {
      const t = (n.t || '').trim();
      if (t) body += `<span class="val">${hl(clip(t, 400))}</span>`;
      else if (!attrs.length) body += '<span class="nil">empty</span>';
    } else if (!n.x) {
      body += `<span class="cnt" title="child nodes">${countLabel(n)}</span>`;
    }
  } else if (n.k === 't') {
    body = `<span class="txt">${hl(clip(n.t.trim(), 400))}</span>`;
  } else {
    body = `<span class="cmt">&lt;!-- ${hl(clip(n.t.trim(), 200))} --&gt;</span>`;
  }
  return `<div class="${cls}" data-i="${i}">${ind(n.d)}<span class="tw">${tw}</span>${body}</div>`;
}

// Only the lines in view are put in the page, so rows with huge numbers of
// nodes stay fast.
function renderTree() {
  const tree = $('#tree');
  $('#treeSpacer').style.height = S.lines.length * LINE_H + 'px';
  const top = tree.scrollTop, h = tree.clientHeight;
  const first = Math.max(0, Math.floor(top / LINE_H) - 8);
  const last = Math.min(S.lines.length, Math.ceil((top + h) / LINE_H) + 8);
  let html = '';
  for (let i = first; i < last; i++) html += lineHTML(S.lines[i], i);
  const rows = $('#treeRows');
  rows.style.transform = `translateY(${first * LINE_H}px)`;
  rows.innerHTML = html;
  $('#treeMsg').innerHTML = S.root ? '' : S.total ? '<span class="spin"></span>Loading…' : indexing() ? '<span class="spin"></span>Looking for rows…' : '';
}

function ensureVisible(i) {
  const tree = $('#tree'), y = i * LINE_H;
  if (y < tree.scrollTop) tree.scrollTop = y;
  else if (y + LINE_H > tree.scrollTop + tree.clientHeight) tree.scrollTop = y + LINE_H - tree.clientHeight;
}

function select(i) {
  if (!S.lines.length) return;
  S.sel = clamp(i, 0, S.lines.length - 1);
  S.selLine = S.lines[S.sel];
  if (S.selLine.kind === 'node') S.selKey = S.selLine.n.key;
  ensureVisible(S.sel);
  renderTree();
  renderDetails();
}

function selectNode(n) {
  const i = S.lines.findIndex(l => l.n === n && l.kind === 'node');
  if (i >= 0) select(i);
}

// ---------- details panel ----------

let textTimer;
function renderDetails() {
  const box = $('#details'), l = S.selLine;
  if (!l || !S.root) { box.innerHTML = ''; return; }
  const n = l.n;
  const chain = [];
  for (let p = n; p; p = p.p) chain.unshift(p);
  let html = '<div class="crumb">' + chain.map((p, i) =>
    (i ? '<span class="sep">›</span>' : '') +
    `<a data-crumb="${i}">${esc(p.k === 'e' ? p.n : p.k === 't' ? 'text' : 'comment')}${i === 0 ? ` <span class="muted">#${fmt(S.row + 1)}</span>` : ''}</a>`).join('') + '</div>';
  html += '<div class="dgrid">';
  if (n.k === 'e') {
    if (!expandable(n)) {
      html += `<span class="k">Value</span><div>${valueHTML(n)}</div>`;
    } else {
      html += `<span class="k">Children</span><span>${countLabel(n)}</span>`;
    }
    if (n.a && n.a.length) {
      html += `<span class="k">Attributes</span><table class="attrs">` +
        n.a.map(([k, v]) => `<tr><td>${esc(k)}</td><td>${esc(v)}</td></tr>`).join('') + '</table>';
    }
  } else {
    html += `<span class="k">${n.k === 't' ? 'Text' : 'Comment'}</span><div>${valueHTML(n)}</div>`;
  }
  html += `<span class="k">Size</span><span>${n.e > 0 ? size(n.e - n.s) : 'unknown (very large)'} <span class="muted">· starts at byte ${fmt(n.s)}</span></span>`;
  html += '</div><div class="dactions">' +
    '<button data-act="xml">View XML</button><button data-act="save">Save as file</button>' +
    (n.k !== 'e' || !expandable(n) ? '<button data-act="copy">Copy value</button>' : '') +
    '<button data-act="path">Copy path</button>' +
    (n.k === 'e' ? '<button data-act="agg">Summarize…</button>' : '') + '</div>';
  box.innerHTML = html;
  box.chain = chain;
  if (n.tt && !n.full) {
    clearTimeout(textTimer);
    textTimer = setTimeout(async () => {
      try {
        const r = await api('/api/text', { s: n.s });
        n.full = r;
        if (S.selLine && S.selLine.n === n) renderDetails();
      } catch (e) { /* keep the short value */ }
    }, 150);
  }
}

function valueHTML(n) {
  const text = n.full ? n.full.text : n.t || '';
  const cut = n.full ? n.full.trunc : n.tt;
  if (!text.trim()) return '<span class="muted">(empty)</span>';
  let note = '';
  if (cut) note = n.full ? '<div class="muted">Only the first 4 MB are shown – use “Save as file” for everything.</div>' : '<div class="muted"><span class="spin"></span>Loading the full value…</div>';
  return `<div class="dval">${hl(text)}</div>${note}`;
}

function xpath(n) {
  const parts = [];
  for (let p = n; p; p = p.p) {
    const nm = kidName(p).replace('#text', 'text()').replace('#comment', 'comment()');
    const idx = p.p ? +p.key.slice(p.key.lastIndexOf('[') + 1, -1) : 0;
    parts.unshift(p.p && p.p.seen && p.p.seen[kidName(p)] > 1 ? `${nm}[${idx}]` : nm);
  }
  return parts.join('/');
}

async function copy(text, what) {
  try { await navigator.clipboard.writeText(text); toast(`${what} copied`); }
  catch { toast('Could not copy to the clipboard', true); }
}

function download(n) {
  const name = (n.k === 'e' ? n.n : 'text') + (n === S.root ? `-row${S.row + 1}` : '');
  const url = new URL('/api/download', location.origin);
  url.search = new URLSearchParams({ s: n.s, e: n.e, name, t: token });
  const a = document.createElement('a');
  a.href = url;
  a.download = name + '.xml';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// ---------- XML dialog ----------

let xmlNode = null;
async function showXML(n) {
  if (!n) return;
  xmlNode = n;
  openModal('#xmlDlg');
  $('#xmlTitle').textContent = n.k === 'e' ? `<${n.n}>` : n.k === 't' ? 'Text' : 'Comment';
  const body = $('#xmlBody'), note = $('#xmlNote');
  body.innerHTML = '<span class="spin"></span>Loading…';
  note.hidden = true;
  try {
    const r = await api('/api/xml', { s: n.s, e: n.e, pretty: $('#xmlPretty').checked ? 1 : 0 });
    if (xmlNode !== n) return;
    body.innerHTML = highlightXML(r.xml);
    body.raw = r.xml;
    if (r.trunc) {
      note.textContent = 'Only the first 4 MB are shown. Use “Save as file” to get the complete XML.';
      note.hidden = false;
    }
  } catch (e) { body.textContent = e.message; }
}

function highlightXML(s) {
  const e = t => t.replace(/[&<>]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c]));
  if (s.length > 3e6) return e(s);
  return s.split(/(<!--[\s\S]*?-->|<[^>]*>)/g).map((part, i) => {
    if (i % 2 === 0) return e(part);
    if (part.startsWith('<!--')) return `<span class="xc">${e(part)}</span>`;
    const tag = e(part).replace(/(\s)([^\s=]+)=("[^"]*"|'[^']*')/g, '$1<span class="xa">$2</span>=<span class="xv">$3</span>');
    return `<span class="xt">${tag}</span>`;
  }).join('');
}

// ---------- search ----------

function makeRe(q, matchCase) {
  return new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), matchCase ? 'g' : 'gi');
}

async function findNext() {
  const q = $('#q').value;
  if (!q || !S.view || S.search.ctrl) return;
  const s = S.search;
  const again = s.hit && s.hit.q === q && s.hit.matchCase === s.matchCase && s.hit.row === S.row;
  const from = again ? s.hit.offset + 1 : S.root ? S.root.s : 0;
  s.ctrl = new AbortController();
  setSearchMsg(`<span class="spin"></span>Searching for “${esc(q)}”… <span class="muted">(Esc to stop)</span>`);
  try {
    const params = { v: S.view, q, case: s.matchCase ? 1 : 0 };
    let res = await api('/api/search', { ...params, from }, { signal: s.ctrl.signal });
    let wrapped = false;
    if (!res.found && from > 0) {
      res = await api('/api/search', { ...params, from: 0 }, { signal: s.ctrl.signal });
      wrapped = true;
    }
    const partial = res.partial ? ' in the part of the file indexed so far' : '';
    if (!res.found) {
      s.hit = null;
      setSearchMsg('');
      toast(`No matches for “${q}”${partial}`, true);
      return;
    }
    if (wrapped) toast(again && s.hit && res.offset === s.hit.offset ? 'This is the only match' : 'Reached the end of the file – continued from the top');
    s.hit = { q, matchCase: s.matchCase, row: res.row, offset: res.offset };
    s.re = makeRe(q, s.matchCase);
    if (res.row !== S.row || !S.root) await selectRow(res.row);
    else renderRows();
    await reveal(res.offset);
    setSearchMsg(`Found “${esc(q)}” in row ${fmt(res.row + 1)}${partial} · <span class="muted">Enter / F3 for the next match</span>`);
  } catch (e) {
    setSearchMsg('');
    if (e.name !== 'AbortError') toast(e.message, true);
  } finally {
    s.ctrl = null;
  }
}

// reveal opens the tree down to the node containing a file offset.
async function reveal(off) {
  let n = S.root;
  if (!n) return;
  const gen = S.rowGen;
  const findKid = p => (p.c || []).find(k => k.s <= off && (k.e < 0 || off < k.e));
  for (let guard = 0; guard < 500 && n.k === 'e'; guard++) {
    if (!n.l) { await loadChildren(n); if (gen !== S.rowGen) return; }
    let kid = findKid(n);
    for (let more = 0; !kid && n.nx && n.nx <= off && more < 40; more++) {
      await loadMore(n);
      if (gen !== S.rowGen) return;
      kid = findKid(n);
    }
    if (!kid) break;
    n.x = true;
    n = kid;
  }
  refresh();
  selectNode(n);
}

// ---------- open dialog ----------

const opener = (() => {
  const el = $('#openTpl').content.firstElementChild.cloneNode(true);
  const pathIn = $('.pathIn', el);
  let cur = null, shown = [], loaded = false;
  const isXml = name => /\.xml$/i.test(name);

  async function list(dir) {
    try {
      const r = await api('/api/ls', { dir: dir || '' });
      cur = r;
      loaded = true;
      $('.curDir', el).innerHTML = `<bdi>${esc(r.dir)}</bdi>`;
      $('.curDir', el).title = r.dir;
      $('.upBtn', el).disabled = !r.parent;
      const only = $('.onlyXml', el).checked;
      shown = r.entries.filter(e => e.dir || !only || isXml(e.name));
      $('.dirList', el).innerHTML = shown.length ? shown.map((e, i) =>
        `<div class="fl${!e.dir && isXml(e.name) ? ' xml' : ''}" data-i="${i}"><span class="ic">${e.dir ? '📁' : '📄'}</span>` +
        `<span class="nm">${esc(e.name)}</span><span class="sz">${e.dir ? '' : size(e.size)}</span></div>`).join('')
        : `<div class="empty">${only ? 'No XML files in this folder' : 'This folder is empty'}</div>`;
    } catch (e) { toast(e.message, true); }
  }

  async function open(path) {
    if (!path.trim()) { pathIn.focus(); return; }
    try {
      const r = await post('/api/open', { path });
      if (r.dir) { list(r.dir); return; }
      closeModal($('#openDlg'));
      pathIn.value = '';
      poll();
    } catch (e) { toast(e.message, true); }
  }

  function renderRecent() {
    const rec = (S.st && S.st.recent) || [];
    const box = $('.recent', el);
    box.innerHTML = rec.length ? '<h3>Recent files</h3>' + rec.map((p, i) =>
      `<div class="fl" data-recent="${i}" title="${esc(p)}"><span class="ic">🕘</span><span class="nm">${esc(p.split(/[\\/]/).pop())}</span>` +
      `<span class="pth"><bdi>${esc(p)}</bdi></span></div>`).join('') : '';
  }

  $('.pathGo', el).onclick = () => open(pathIn.value);
  pathIn.addEventListener('keydown', e => { if (e.key === 'Enter') open(pathIn.value); });
  $('.upBtn', el).onclick = () => cur && cur.parent && list(cur.parent);
  $('.homeBtn', el).onclick = () => cur && list(cur.home);
  $('.onlyXml', el).onchange = () => cur && list(cur.dir === 'This PC' ? '::drives' : cur.dir);
  $('.dirList', el).addEventListener('click', e => {
    const f = e.target.closest('.fl');
    if (!f) return;
    const entry = shown[+f.dataset.i];
    if (entry.dir) list(entry.path); else open(entry.path);
  });
  $('.recent', el).addEventListener('click', e => {
    const f = e.target.closest('.fl');
    if (f) open(S.st.recent[+f.dataset.recent]);
  });
  return { el, list, renderRecent, focus: () => pathIn.focus(), get loaded() { return loaded; }, get dir() { return cur && cur.dir; } };
})();

function showWelcome() {
  $('#main').hidden = true;
  $('#welcome').hidden = false;
  $('#viewWrap').hidden = $('#searchWrap').hidden = true;
  $('#fileinfo').textContent = '';
  if (opener.el.parentElement !== $('#welcomeOpen')) $('#welcomeOpen').appendChild(opener.el);
  if (!opener.loaded) opener.list('');
}

function showMain() {
  $('#main').hidden = false;
  $('#welcome').hidden = true;
  $('#viewWrap').hidden = $('#searchWrap').hidden = false;
}

function openDialog() {
  if (!S.doc) { opener.focus(); return; }
  $('#openHost').appendChild(opener.el);
  opener.renderRecent();
  opener.list(opener.dir || '');
  openModal('#openDlg');
  opener.focus();
}

function openModal(sel) { $(sel).hidden = false; }
function closeModal(m) {
  if (typeof m === 'string') m = $(m);
  m.hidden = true;
  if (m.id === 'xmlDlg') xmlNode = null;
}

// ---------- error box ----------

// errorBox explains why something is not possible. alt is an optional
// second button, e.g. {label: 'Count instead', run}.
function errorBox(title, message, alt) {
  $('#errTitle').textContent = title;
  $('#errMsg').textContent = message;
  const b = $('#errAlt');
  b.hidden = !alt;
  if (alt) {
    b.textContent = alt.label;
    b.onclick = () => { closeModal('#errDlg'); alt.run(); };
  }
  openModal('#errDlg');
  $('#errOk').focus();
}

// ---------- context menu ----------

// relPath is a node's element path below the row element, e.g. "Customer/City".
const relPath = n => n.pkey.split('/').slice(1).join('/');

function showMenu(x, y, items) {
  const m = $('#ctxMenu');
  m.items = items;
  m.innerHTML = items.map((it, i) => (it === '-' ? '<div class="sep"></div>'
    : `<button role="menuitem" data-i="${i}"><span class="mi">${esc(it.icon || '')}</span>${esc(it.label)}</button>`)).join('');
  m.hidden = false;
  const r = m.getBoundingClientRect();
  m.style.left = Math.max(4, Math.min(x, innerWidth - r.width - 8)) + 'px';
  m.style.top = Math.max(4, Math.min(y, innerHeight - r.height - 8)) + 'px';
  m.querySelector('button').focus();
}

function hideMenu() {
  const m = $('#ctxMenu');
  if (m.hidden) return;
  m.hidden = true;
  setFocus(S.focus);
}

function nodeMenu(n) {
  const items = [];
  if (n.k === 'e' && n.d === 0) {
    items.push({ icon: '#', label: 'Count rows…', run: () => openAgg({ fn: 'count', value: '' }) });
  } else if (n.k === 'e') {
    items.push({ icon: 'Σ', label: `Sum of ${n.n}…`, run: () => trySum(n) });
    items.push({ icon: '#', label: `Count of ${n.n}…`, run: () => openAgg({ fn: 'count', value: relPath(n) }) });
    const gs = S.agg.groups, also = gs.length && !gs.includes(relPath(n));
    items.push({ icon: '⊞', label: `${also ? 'Also group' : 'Group'} by ${n.n}…`, run: () => tryGroup(n) });
  } else {
    const what = n.k === 't' ? 'a piece of text' : 'a comment';
    const no = () => errorBox('This cannot be summarized', `You right-clicked ${what}. Summaries work on elements: right-click a field, for example one with a number in it.`);
    items.push({ icon: 'Σ', label: 'Sum…', run: no }, { icon: '#', label: 'Count…', run: no });
  }
  items.push('-');
  if (n.k !== 'e' || !expandable(n)) items.push({ icon: '⧉', label: 'Copy value', run: () => copy(nodeText(n), 'Value') });
  items.push({ icon: '⧉', label: 'Copy path', run: () => copy(xpath(n), 'Path') });
  items.push({ icon: '‹›', label: 'View XML', run: () => showXML(n) });
  items.push({ icon: '⤓', label: 'Save as file', run: () => download(n) });
  return items;
}

function openNodeMenu(i, x, y) {
  const l = S.lines[i];
  if (!l) return;
  select(i);
  if (l.kind !== 'node') return;
  if (x == null) {
    const el = $(`#treeRows .ln[data-i="${i}"]`);
    const r = el ? el.getBoundingClientRect() : $('#tree').getBoundingClientRect();
    x = r.left + 40;
    y = r.bottom;
  }
  showMenu(x, y, nodeMenu(l.n));
}

// nodeText is a field's value in the current row; it is used to check a
// request before reading the whole file.
const nodeText = n => (n.full ? n.full.text : n.t || '').trim();

function trySum(n) {
  const path = relPath(n);
  const countInstead = { label: `Count ${n.n} instead`, run: () => openAgg({ fn: 'count', value: path }) };
  if (expandable(n)) {
    return errorBox('This field cannot be summed',
      `<${n.n}> contains other elements, not a single value, so there is nothing to add up.\n\nRight-click one of the fields inside it, or count <${n.n}> instead.`, countInstead);
  }
  const t = nodeText(n);
  if (t && parseNumber(t) == null) {
    return errorBox('This field cannot be summed',
      `<${n.n}> contains text (“${clip(t, 40)}”), not a number, so it cannot be added up.\n\nYou can count it instead – grouped by <${n.n}> itself, that shows how often each value occurs.`, countInstead);
  }
  openAgg({ fn: 'sum', value: path });
}

function tryGroup(n) {
  if (expandable(n)) {
    return errorBox('Cannot group by this field',
      `<${n.n}> contains other elements, not a single value. Group by a field that has a value, such as a customer number.`);
  }
  openAgg({ addGroup: relPath(n) });
}

// ---------- summarize ----------

S.agg = { fn: 'sum', value: null, groups: [] }; // the choices in the dialog
const MAX_GROUP_FIELDS = 5;
let aggTimer = null, aggRun = null, aggData = null, aggSort = null;

// parseNumber mirrors ParseNumber on the server: 12, -3.5, 1,5 and 1 234,50 are numbers.
function parseNumber(s) {
  s = String(s).trim().replace(/[\s  ]/g, '');
  const comma = s.lastIndexOf(','), dot = s.lastIndexOf('.');
  if (comma >= 0 && dot >= 0) s = comma > dot ? s.replace(/\./g, '').replace(',', '.') : s.replace(/,/g, '');
  else if (comma >= 0) s = s.split(',').length === 2 ? s.replace(',', '.') : s.replace(/,/g, '');
  return /^[+-]?(\d+\.?\d*|\.\d+)$/.test(s) ? parseFloat(s) : null;
}

// rowFields lists the element paths found in the current row's tree.
function rowFields() {
  const out = [], seen = new Set();
  const walk = n => {
    for (const k of n.c || []) {
      if (k.k !== 'e') continue;
      const p = relPath(k);
      if (!seen.has(p)) { seen.add(p); out.push({ path: p, leaf: !expandable(k) }); }
      walk(k);
    }
  };
  if (S.root) walk(S.root);
  return out;
}

function fillAggSelects() {
  const fields = rowFields(), a = S.agg;
  const opt = (v, label) => `<option value="${esc(v)}">${esc(label)}</option>`;
  const withCurrent = (list, cur) => (cur && !list.some(f => f.path === cur) ? list.concat({ path: cur, leaf: true }) : list);
  const values = withCurrent(fields.filter(f => a.fn === 'count' || f.leaf), a.value);
  const valueSel = $('#aggValue');
  valueSel.innerHTML = (a.fn === 'count' ? opt('', `rows (<${rowName()}>)`) : '') + values.map(f => opt(f.path, f.path)).join('');
  if (a.value == null || ![...valueSel.options].some(o => o.value === a.value)) {
    a.value = valueSel.options.length ? valueSel.options[0].value : '';
  }
  valueSel.value = a.value;
  let leaves = fields.filter(f => f.leaf);
  for (const g of a.groups) leaves = withCurrent(leaves, g);
  const box = $('#aggGroups');
  box.innerHTML = a.groups.length ? a.groups.map((g, i) =>
    `<span class="gsel">${i ? '<span class="plus">+</span>' : ''}<select data-gi="${i}" title="Group by this field">` +
    leaves.map(f => opt(f.path, f.path)).join('') +
    `</select><button class="rm" data-rm="${i}" title="Stop grouping by this field">✕</button></span>`).join('')
    : '<span class="muted">— no grouping —</span>';
  box.querySelectorAll('select').forEach((sel, i) => { sel.value = a.groups[i]; });
  $('#aggAddGroup').disabled = a.groups.length >= MAX_GROUP_FIELDS || !leaves.length;
  $('#aggAddGroup').textContent = a.groups.length ? '＋ Add field' : '＋ Group by a field';
  $('#aggFn').value = a.fn;
  $('#aggScope').textContent = `Reads all ${fmt(S.total)}${indexing() ? '+' : ''} <${rowName()}> rows in the file.`;
}

function openAgg(opts) {
  if (!S.root) return errorBox('Nothing to summarize', 'Open a file and select a row first.');
  const { addGroup, ...rest } = opts;
  Object.assign(S.agg, rest);
  if (addGroup && !S.agg.groups.includes(addGroup)) {
    if (S.agg.groups.length >= MAX_GROUP_FIELDS) {
      errorBox('Too many group fields', `A summary can be grouped by at most ${MAX_GROUP_FIELDS} fields. Remove one in the Summarize window first.`);
    } else {
      S.agg.groups.push(addGroup);
    }
  }
  fillAggSelects();
  openModal('#aggDlg');
  $('#aggRun').focus();
  pollAgg();
}

async function runAgg() {
  const a = S.agg;
  a.fn = $('#aggFn').value;
  a.value = $('#aggValue').value;
  a.groups = [...new Set(a.groups)];
  if (a.fn === 'sum') {
    if (a.value === '') return errorBox('Rows cannot be summed', 'Choose a field with numbers to sum, or choose Count to count rows.');
    // Check the field in the row that is shown before reading the whole file.
    const n = S.lines.map(l => l.n).find(x => x.k === 'e' && x.d > 0 && relPath(x) === a.value);
    if (n) {
      const countInstead = { label: `Count ${n.n} instead`, run: () => openAgg({ fn: 'count', value: a.value }) };
      if (expandable(n)) return errorBox('This field cannot be summed', `<${n.n}> contains other elements, not a single value.`, countInstead);
      const t = nodeText(n);
      if (t && parseNumber(t) == null) return errorBox('This field cannot be summed', `<${n.n}> contains text (“${clip(t, 40)}”), not a number.`, countInstead);
    }
  }
  aggRun = { ...a, groups: [...a.groups], rowName: rowName() };
  try {
    await post('/api/aggregate', { view: S.view, value: a.value, groups: a.groups });
  } catch (e) {
    aggRun = null;
    return errorBox('The summary could not be started', e.message);
  }
  aggData = null;
  $('#aggHead').textContent = '';
  $('#aggNotes').innerHTML = '';
  $('#aggTable').innerHTML = '';
  $('#aggActions').hidden = true;
  pollAgg();
}

async function pollAgg() {
  clearTimeout(aggTimer);
  if (!aggRun) return;
  const prog = $('#aggProgress');
  let r;
  try {
    r = await api('/api/aggregate');
  } catch (e) {
    aggRun = null;
    prog.hidden = true;
    return errorBox('The summary failed', e.message);
  }
  const st = r.status;
  if (!st) { aggRun = null; prog.hidden = true; return; }
  if (!st.done) {
    prog.hidden = false;
    $('.fill', prog).style.width = (100 * st.scanned / Math.max(1, r.size)).toFixed(1) + '%';
    $('.txt', prog).textContent = `${fmt(st.rows)} rows read…`;
    if (!$('#aggDlg').hidden) aggTimer = setTimeout(pollAgg, 300);
    return;
  }
  prog.hidden = true;
  const run = aggRun;
  aggRun = null;
  if (st.error) return errorBox('The summary failed', st.error);
  const res = st.result, t = res.total;
  if (run.fn === 'sum' && t[3] === 0) {
    const why = t[2] > 0 ? `the ${fmt(t[2])} values found are text, not numbers` : `<${run.value}> is empty or missing in every row`;
    return errorBox('There is nothing to sum', `None of the ${fmt(res.rows)} rows has a number in <${run.value}>: ${why}.`,
      { label: 'Count instead', run: () => { S.agg.fn = 'count'; fillAggSelects(); runAgg(); } });
  }
  aggData = { run, res, seconds: st.seconds };
  aggSort = null;
  renderAgg();
}

function aggColumns(run) {
  const name = run.value || 'rows';
  const keys = (run.groups.length ? run.groups : ['']).map((g, i) => ({ k: 'key' + i, label: g, text: true }));
  if (run.fn === 'sum') {
    return [...keys, { k: 'sum', label: `Sum of ${name}` }, { k: 'rows', label: 'Rows' }, { k: 'avg', label: 'Average', extra: 2 },
      { k: 'min', label: 'Min' }, { k: 'max', label: 'Max' }];
  }
  if (!run.value) return [...keys, { k: 'rows', label: 'Rows' }, { k: 'share', label: 'Share' }];
  return [...keys, { k: 'count', label: `Count of ${name}` }, { k: 'share', label: 'Share' }, { k: 'rows', label: 'Rows' }];
}

// aggRows turns the server's [keys, rows, count, numbers, sum, min, max]
// arrays into objects with the table's columns (key0, key1, ... per group field).
function aggRows(run, res) {
  const countRows = run.fn === 'count' && !run.value;
  const base = countRows ? res.total[1] : res.total[2];
  const obj = g => {
    const o = {
      rows: g[1], count: g[2], sum: g[4],
      min: g[3] ? g[5] : null, max: g[3] ? g[6] : null, avg: g[3] ? g[4] / g[3] : null,
      share: base ? (countRows ? g[1] : g[2]) / base : null,
    };
    g[0].forEach((key, i) => { o['key' + i] = key; });
    return o;
  };
  const total = obj(res.total);
  total.key0 = run.groups.length ? 'All rows' : 'Total';
  return { total, groups: (res.groups || []).map(obj) };
}

const MAX_SHOWN = 2000;

function renderAgg() {
  if (!aggData) return;
  const { run, res } = aggData;
  const cols = aggColumns(run);
  const { total, groups } = aggRows(run, res);
  if (!aggSort) aggSort = { k: cols.find(c => !c.text).k, dir: -1 };
  const { k, dir } = aggSort;
  groups.sort((a, b) => {
    const x = a[k], y = b[k];
    if (x == null || y == null) return x == null ? 1 : -1;
    return (typeof x === 'string' ? x.localeCompare(y, undefined, { numeric: true }) : x - y) * dir;
  });
  const num = (v, c) => {
    if (v == null) return '';
    if (c.k === 'share') return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(v);
    const whole = c.k === 'rows' || c.k === 'count';
    const d = whole ? 0 : res.decimals;
    return new Intl.NumberFormat(undefined, { minimumFractionDigits: d, maximumFractionDigits: d + (c.extra || 0) }).format(v);
  };
  const cell = (g, c) => {
    if (!c.text) return `<td class="num">${num(g[c.k], c)}</td>`;
    const v = g[c.k];
    if (v == null) return '<td></td>';
    return `<td class="key">${v === '' ? '<span class="empty-key">(empty)</span>' : esc(v)}</td>`;
  };
  let html = '<table><thead><tr>' + cols.map(c =>
    `<th class="${c.text ? '' : 'num'}" data-k="${c.k}">${esc(c.label)}${c.k === k ? (dir < 0 ? ' ▼' : ' ▲') : ''}</th>`).join('') + '</tr></thead><tbody>';
  html += '<tr class="total">' + cols.map(c => cell(total, c)).join('') + '</tr>';
  for (const g of groups.slice(0, MAX_SHOWN)) html += '<tr>' + cols.map(c => cell(g, c)).join('') + '</tr>';
  $('#aggTable').innerHTML = html + '</tbody></table>';

  const what = run.fn === 'sum' ? `Sum of ${run.value}` : run.value ? `Count of ${run.value}` : 'Number of rows';
  const per = run.groups.join(' + ');
  $('#aggHead').textContent = `${what}${per ? ` per ${per}` : ''} – ${fmt(res.rows)} <${run.rowName}> rows` +
    (per ? `, ${fmt(res.groupCount)} groups` : '') + ` (${aggData.seconds.toFixed(1)} s)`;
  const notes = [], t = res.total;
  if (run.fn === 'sum' && t[2] > t[3]) notes.push(`${fmt(t[2] - t[3])} values of <${run.value}> are not numbers and were left out.`);
  if (run.value && res.missing) notes.push(`${fmt(res.missing)} rows have no <${run.value}>.`);
  run.groups.forEach((g, i) => {
    if (res.noGroup[i]) notes.push(`${fmt(res.noGroup[i])} rows have no <${g}>; they are counted under “(empty)”.`);
  });
  if (res.cut) notes.push(`There are too many groups to show them all; only the largest ${fmt(groups.length)} are included.`);
  if (groups.length > MAX_SHOWN) notes.push(`The table shows ${fmt(MAX_SHOWN)} of ${fmt(groups.length)} groups. “Copy table” and “Save as CSV” include all of them.`);
  $('#aggNotes').innerHTML = notes.map(n => `<div>${esc(n)}</div>`).join('');
  $('#aggActions').hidden = false;
}

// aggExport returns the table as rows of cells, with numbers written the
// way this computer's Excel expects them (decimal comma in Sweden).
function aggExport() {
  const { run, res } = aggData;
  const cols = aggColumns(run);
  const { total, groups } = aggRows(run, res);
  const decSep = (1.5).toLocaleString().includes(',') ? ',' : '.';
  const cellOf = (g, c) => {
    const v = g[c.k];
    if (v == null) return '';
    if (c.text) return v;
    const d = c.k === 'share' ? 4 : c.k === 'rows' || c.k === 'count' ? 0 : res.decimals + (c.extra || 0);
    return String(+v.toFixed(d)).replace('.', decSep);
  };
  const head = cols.map(c => c.label);
  return { decSep, rows: [head, cols.map(c => cellOf(total, c)), ...groups.map(g => cols.map(c => cellOf(g, c)))] };
}

function aggCopy() {
  copy(aggExport().rows.map(r => r.join('\t')).join('\n'), 'Table');
}

function aggCsv() {
  const { decSep, rows } = aggExport();
  const sep = decSep === ',' ? ';' : ',';
  const q = v => (/["\n\r]/.test(v) || v.includes(sep) ? `"${v.replace(/"/g, '""')}"` : v);
  const csv = '﻿' + rows.map(r => r.map(v => q(String(v))).join(sep)).join('\r\n') + '\r\n';
  const { run } = aggData;
  const name = [run.fn, run.value || 'rows', run.groups.length && 'per-' + run.groups.join('-')].filter(Boolean).join('-').replace(/[^\w.-]+/g, '_');
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([csv], { type: 'text/csv;charset=utf-8' }));
  a.download = `${name}.csv`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

// ---------- focus & keyboard ----------

function setFocus(which) {
  S.focus = which;
  $('#rowsPane').classList.toggle('focus', which === 'rows');
  $('#treePane').classList.toggle('focus', which === 'tree');
  (which === 'rows' ? $('#rowList') : $('#tree')).focus({ preventScroll: true });
}

function rowsKey(e) {
  const page = Math.max(1, Math.floor($('#rowList').clientHeight / 46));
  switch (e.key) {
    case 'ArrowDown': selectRow(S.row + 1); break;
    case 'ArrowUp': selectRow(S.row - 1); break;
    case 'PageDown': selectRow(S.row + page); break;
    case 'PageUp': selectRow(S.row - page); break;
    case 'Home': selectRow(0); break;
    case 'End': selectRow(S.total - 1); break;
    case 'Enter': case 'ArrowRight': setFocus('tree'); break;
    default: return false;
  }
  return true;
}

function treeKey(e) {
  const l = S.lines[S.sel];
  if (!l) return false;
  const n = l.n;
  const page = Math.max(1, Math.floor($('#tree').clientHeight / LINE_H) - 1);
  switch (e.key) {
    case 'ArrowDown': select(S.sel + 1); break;
    case 'ArrowUp': select(S.sel - 1); break;
    case 'PageDown': select(S.sel + page); break;
    case 'PageUp': select(S.sel - page); break;
    case 'Home': select(0); break;
    case 'End': select(S.lines.length - 1); break;
    case 'ArrowRight':
      if (l.kind !== 'node') break;
      if (expandable(n) && !n.x) toggle(n, true);
      else if (n.x) select(S.sel + 1);
      break;
    case 'ArrowLeft': {
      if (l.kind === 'node' && n.x && expandable(n)) { toggle(n, false); break; }
      const parent = l.kind === 'node' ? n.p : n;
      if (parent) selectNode(parent); else setFocus('rows');
      break;
    }
    case 'Enter': case ' ':
      if (l.kind === 'more') loadMore(n); else toggle(n);
      break;
    case '*': expandAll(l.kind === 'node' ? n : S.root); break;
    case '-': collapseAll(); break;
    case 'x': case 'X': showXML(n); break;
    default: return false;
  }
  return true;
}

document.addEventListener('keydown', e => {
  const menu = $('#ctxMenu');
  if (!menu.hidden) {
    const btns = [...menu.querySelectorAll('button')], i = btns.indexOf(document.activeElement);
    if (e.key === 'Enter' || e.key === ' ') return; // activates the focused item
    e.preventDefault();
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') btns[(i + (e.key === 'ArrowDown' ? 1 : btns.length - 1)) % btns.length].focus();
    else if (e.key === 'Escape' || e.key === 'Tab') hideMenu();
    return;
  }
  const modal = [...document.querySelectorAll('.modal')].reverse().find(m => !m.hidden && m.id !== 'stopped');
  if (e.key === 'Escape') {
    if (modal) closeModal(modal);
    else if (S.search.ctrl) S.search.ctrl.abort();
    else if (isInput(e.target)) { e.target.blur(); setFocus(S.focus); }
    return;
  }
  if (modal) return;
  const mod = e.ctrlKey || e.metaKey;
  if (mod && e.key.toLowerCase() === 'f' && S.doc) { e.preventDefault(); $('#q').focus(); $('#q').select(); return; }
  if (mod && e.key.toLowerCase() === 'o') { e.preventDefault(); openDialog(); return; }
  if (e.key === 'F3') { e.preventDefault(); findNext(); return; }
  if (isInput(e.target) || !S.doc) return;
  if (S.focus === 'tree' && (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10'))) {
    e.preventDefault();
    openNodeMenu(S.sel);
    return;
  }
  if (mod && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
    e.preventDefault();
    selectRow(S.row + (e.key === 'ArrowDown' ? 1 : -1));
    return;
  }
  if (mod || e.altKey) return;
  let handled = true;
  switch (e.key) {
    case 'n': case 'N': selectRow(S.row + 1); break;
    case 'p': case 'P': selectRow(S.row - 1); break;
    case 'g': case 'G': $('#gotoRow').focus(); break;
    case '/': $('#q').focus(); break;
    case '?': openModal('#helpDlg'); break;
    case 'Tab': setFocus(S.focus === 'rows' ? 'tree' : 'rows'); break;
    default: handled = S.focus === 'rows' ? rowsKey(e) : treeKey(e);
  }
  if (handled) e.preventDefault();
});

// ---------- wiring ----------

function wire() {
  $('#rowList').tabIndex = 0;
  $('#rowList').addEventListener('click', e => {
    const r = e.target.closest('.r');
    if (r) selectRow(+r.dataset.i);
  });
  $('#rowsPane').addEventListener('mousedown', e => { if (!isInput(e.target)) setTimeout(() => setFocus('rows')); });
  $('#treePane').addEventListener('mousedown', e => {
    if (!e.target.closest('#details')) setTimeout(() => setFocus('tree'));
  });
  $('#treeRows').addEventListener('mousedown', e => {
    const el = e.target.closest('.ln');
    if (!el) return;
    const i = +el.dataset.i, l = S.lines[i];
    select(i);
    if (l.kind === 'more') loadMore(l.n);
    else if (e.target.classList.contains('tw')) toggle(l.n);
  });
  $('#treeRows').addEventListener('dblclick', e => {
    const el = e.target.closest('.ln');
    if (el && !e.target.classList.contains('tw')) {
      const l = S.lines[+el.dataset.i];
      if (l && l.kind === 'node') toggle(l.n);
    }
  });
  $('#tree').addEventListener('scroll', () => requestAnimationFrame(renderTree));
  addEventListener('resize', () => renderTree());

  $('#details').addEventListener('click', e => {
    const crumb = e.target.closest('[data-crumb]');
    if (crumb) { selectNode($('#details').chain[+crumb.dataset.crumb]); return; }
    const act = e.target.closest('[data-act]');
    const n = S.selLine && S.selLine.n;
    if (!act || !n) return;
    switch (act.dataset.act) {
      case 'xml': showXML(n); break;
      case 'save': download(n); break;
      case 'copy': copy(n.full ? n.full.text : n.t || '', 'Value'); break;
      case 'path': copy(xpath(n), 'Path'); break;
      case 'agg':
        if (n.d === 0) openAgg({ fn: 'count', value: '' });
        else if (!expandable(n) && parseNumber(nodeText(n)) != null) openAgg({ fn: 'sum', value: relPath(n) });
        else openAgg({ fn: 'count', value: relPath(n) });
        break;
    }
  });

  // Right-click menu with Sum, Count and Group by.
  $('#treeRows').addEventListener('contextmenu', e => {
    const el = e.target.closest('.ln');
    if (!el) return;
    e.preventDefault();
    setFocus('tree');
    openNodeMenu(+el.dataset.i, e.clientX, e.clientY);
  });
  $('#ctxMenu').addEventListener('click', e => {
    const b = e.target.closest('button');
    if (!b) return;
    const it = $('#ctxMenu').items[+b.dataset.i];
    hideMenu();
    it.run();
  });
  addEventListener('mousedown', e => { if (!e.target.closest('#ctxMenu')) hideMenu(); }, true);
  addEventListener('blur', hideMenu);
  $('#tree').addEventListener('scroll', hideMenu);

  // Summarize dialog.
  $('#aggFn').onchange = () => { S.agg.fn = $('#aggFn').value; S.agg.value = $('#aggValue').value; fillAggSelects(); };
  $('#aggRun').onclick = runAgg;
  $('#aggGroups').addEventListener('change', e => {
    const sel = e.target.closest('select');
    if (sel) S.agg.groups[+sel.dataset.gi] = sel.value;
  });
  $('#aggGroups').addEventListener('click', e => {
    const rm = e.target.closest('[data-rm]');
    if (!rm) return;
    S.agg.groups.splice(+rm.dataset.rm, 1);
    fillAggSelects();
  });
  $('#aggAddGroup').onclick = () => {
    const a = S.agg, free = rowFields().filter(f => f.leaf && !a.groups.includes(f.path) && f.path !== a.value);
    const next = free[0] || rowFields().find(f => f.leaf && !a.groups.includes(f.path));
    if (!next) return errorBox('No more fields', 'All fields of this row are already used for grouping.');
    a.groups.push(next.path);
    fillAggSelects();
    const sels = $('#aggGroups').querySelectorAll('select');
    sels[sels.length - 1].focus();
  };
  $('#aggStop').onclick = async () => {
    try { await post('/api/aggregate/stop', {}); } catch { /* it may have finished */ }
    aggRun = null;
    clearTimeout(aggTimer);
    $('#aggProgress').hidden = true;
    toast('Summary stopped');
  };
  $('#aggTable').addEventListener('click', e => {
    const th = e.target.closest('th');
    if (!th || !aggSort) return;
    const k = th.dataset.k;
    aggSort = { k, dir: aggSort.k === k ? -aggSort.dir : k.startsWith('key') ? 1 : -1 };
    renderAgg();
  });
  $('#aggCopy').onclick = aggCopy;
  $('#aggCsv').onclick = aggCsv;
  $('#errOk').onclick = () => closeModal('#errDlg');

  $('#rowPrev').onclick = () => selectRow(S.row - 1);
  $('#rowNext').onclick = () => selectRow(S.row + 1);
  $('#expAll').onclick = () => expandAll(S.root);
  $('#colAll').onclick = () => collapseAll();
  $('#rowXml').onclick = () => showXML(S.root);
  $('#rowSave').onclick = () => S.root && download(S.root);
  $('#pgFirst').onclick = () => selectRow(0);
  $('#pgPrev').onclick = () => selectRow(S.page.from - PAGE);
  $('#pgNext').onclick = () => selectRow(S.page.from + PAGE);
  $('#pgLast').onclick = () => selectRow(Math.floor((S.total - 1) / PAGE) * PAGE);
  $('#gotoRow').addEventListener('keydown', e => {
    if (e.key !== 'Enter') return;
    const n = parseInt(e.target.value.replace(/[\s,.']/g, ''), 10);
    if (n > 0) {
      if (n > S.total) toast(`There are only ${fmt(S.total)} rows${indexing() ? ' indexed so far' : ''}`, true);
      selectRow(n - 1);
      e.target.value = '';
      setFocus('rows');
    }
  });

  $('#viewSel').addEventListener('change', async e => {
    const v = e.target.value;
    try {
      if (v.startsWith('N:')) await post('/api/index', { name: v.slice(2) });
      S.mode = v;
      S.view = null;
      e.target.blur();
      poll();
    } catch (err) { toast(err.message, true); }
  });

  $('#q').addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); findNext(); } });
  $('#q').addEventListener('input', () => {
    if (!$('#q').value && S.search.re) { S.search.re = null; S.search.hit = null; setSearchMsg(''); renderTree(); renderRows(); renderDetails(); }
  });
  $('#qgo').onclick = () => findNext();
  $('#qcase').onclick = () => {
    S.search.matchCase = !S.search.matchCase;
    $('#qcase').classList.toggle('on', S.search.matchCase);
  };

  $('#openBtn').onclick = openDialog;
  $('#helpBtn').onclick = () => openModal('#helpDlg');
  $('#quitBtn').onclick = async () => {
    if (!confirm('Quit SwiiftXML?')) return;
    try { await post('/api/quit', {}); } catch { /* already gone */ }
    clearTimeout(pollTimer);
    $('#stopped').hidden = false;
  };
  for (const m of document.querySelectorAll('.modal')) {
    m.addEventListener('mousedown', e => { if (e.target === m && m.id !== 'stopped') closeModal(m); });
  }
  for (const b of document.querySelectorAll('[data-close]')) b.onclick = () => closeModal(b.closest('.modal'));
  $('#xmlPretty').onchange = () => xmlNode && showXML(xmlNode);
  $('#xmlCopy').onclick = () => copy($('#xmlBody').raw || '', 'XML');
  $('#xmlSave').onclick = () => xmlNode && download(xmlNode);

  // Dropping a file on the page would make the browser try to load all of it.
  addEventListener('dragover', e => e.preventDefault());
  addEventListener('drop', e => {
    e.preventDefault();
    toast('Browsers cannot pass a file’s location on. Paste the file path in “Open file…”, or drag the file onto the SwiiftXML program icon.', true);
  });

  const split = $('#splitter');
  split.addEventListener('mousedown', e => {
    e.preventDefault();
    const startX = e.clientX, startW = $('#rowsPane').offsetWidth;
    const move = ev => { $('#rowsPane').style.width = clamp(startW + ev.clientX - startX, 220, innerWidth - 420) + 'px'; };
    const up = () => { removeEventListener('mousemove', move); removeEventListener('mouseup', up); renderTree(); };
    addEventListener('mousemove', move);
    addEventListener('mouseup', up);
  });
}

wire();
setFocus('rows');
poll();
