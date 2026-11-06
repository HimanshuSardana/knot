// ============================================================ bsp tree
//
// A node is either a leaf (one window) or a split with two children and a
// ratio. This is the dwindle scheme bspwm uses: a new window splits the
// focused one along its longer axis.

const GAP = 6;

function leaf(id) { return { t: 'l', id }; }
function split(dir, a, b, ratio) { return { t: 's', dir, a, b, ratio: ratio ?? 0.5 }; }

function findLeaf(node, id) {
  if (!node) return null;
  if (node.t === 'l') return node.id === id ? node : null;
  return findLeaf(node.a, id) || findLeaf(node.b, id);
}

// insert replaces the target leaf with a split of target + new window.
function insertNode(node, targetId, newId, dir) {
  if (!node) return leaf(newId);
  if (node.t === 'l') {
    if (node.id !== targetId) return node;
    return split(dir, leaf(targetId), leaf(newId));
  }
  node.a = insertNode(node.a, targetId, newId, dir);
  node.b = insertNode(node.b, targetId, newId, dir);
  return node;
}

function lastLeafId(node) {
  if (!node) return null;
  if (node.t === 'l') return node.id;
  return lastLeafId(node.b) ?? lastLeafId(node.a);
}

// pruneTree drops leaves whose window no longer exists, so a restored layout
// cannot leave dead holes in the tiling.
function pruneTree(node, valid) {
  if (!node) return null;
  if (node.t === 'l') return valid.has(node.id) ? node : null;
  node.a = pruneTree(node.a, valid);
  node.b = pruneTree(node.b, valid);
  if (!node.a) return node.b;
  if (!node.b) return node.a;
  return node;
}

// removeNode deletes a leaf and collapses any split left with one child.
function removeNode(node, id) {
  if (!node) return null;
  if (node.t === 'l') return node.id === id ? null : node;
  node.a = removeNode(node.a, id);
  node.b = removeNode(node.b, id);
  if (!node.a) return node.b;
  if (!node.b) return node.a;
  return node;
}

// replaceLeaf swaps a leaf for an arbitrary subtree, which is how a dragged
// window is re-inserted next to its drop target.
function replaceLeaf(node, targetId, make) {
  if (!node) return null;
  if (node.t === 'l') return node.id === targetId ? make(node) : node;
  node.a = replaceLeaf(node.a, targetId, make);
  node.b = replaceLeaf(node.b, targetId, make);
  return node;
}

// moveWindow re-tiles: pull the window out of the tree, then put it back as a
// sibling of the target on the requested side. "swap" exchanges the two leaves
// in place instead of re-splitting.
function moveWindow(id, targetId, side) {
  if (id === targetId) return false;
  if (!findLeaf(wm.root, id) || !findLeaf(wm.root, targetId)) return false;

  if (side === 'swap') {
    const a = findLeaf(wm.root, id);
    const b = findLeaf(wm.root, targetId);
    if (!a || !b) return false;
    a.id = targetId;
    b.id = id;
    return true;
  }

  wm.root = removeNode(wm.root, id);
  if (!findLeaf(wm.root, targetId)) return false;

  wm.root = replaceLeaf(wm.root, targetId, (t) => {
    const moved = leaf(id);
    switch (side) {
      case 'left':   return split('h', moved, t);
      case 'right':  return split('h', t, moved);
      case 'top':    return split('v', moved, t);
      default:       return split('v', t, moved);
    }
  });
  return true;
}

// layout walks the tree assigning pixel rects to leaves and collecting the
// divider positions that become drag handles.
function computeLayout(node, rect, out, handles) {
  if (!node) return;

  if (node.t === 'l') {
    out[node.id] = rect;
    return;
  }

  const { x, y, w, h } = rect;
  if (node.dir === 'h') {
    const cut = Math.round(w * node.ratio);
    computeLayout(node.a, { x, y, w: cut, h }, out, handles);
    computeLayout(node.b, { x: x + cut, y, w: w - cut, h }, out, handles);
    handles.push({ node, dir: 'h', parent: rect, at: x + cut });
  } else {
    const cut = Math.round(h * node.ratio);
    computeLayout(node.a, { x, y, w, h: cut }, out, handles);
    computeLayout(node.b, { x, y: y + cut, w, h: h - cut }, out, handles);
    handles.push({ node, dir: 'v', parent: rect, at: y + cut });
  }
}

// ============================================================ state

const ICON = {
  dir:     '\uf07b',
  file:    '\uf15b',
  text:    '\uf15c',
  code:    '\uf121',
  image:   '\uf1c5',
  audio:   '\uf1c7',
  video:   '\uf1c8',
  archive: '\uf1c6',
  pdf:     '\uf1c1',
  config:  '\ue615',
  lock:    '\uf023',
  binary:  '\uf471',
};

const EXT_ICON = {
  txt: 'text', md: 'text', rst: 'text', log: 'text', org: 'text', csv: 'text',
  go: 'code', py: 'code', js: 'code', ts: 'code', sh: 'code', rs: 'code',
  c: 'code', h: 'code', cpp: 'code', java: 'code', rb: 'code', lua: 'code',
  json: 'config', yaml: 'config', yml: 'config', toml: 'config', ini: 'config',
  conf: 'config', env: 'config',
  png: 'image', jpg: 'image', jpeg: 'image', gif: 'image', webp: 'image',
  svg: 'image', bmp: 'image', ico: 'image', avif: 'image',
  mp3: 'audio', flac: 'audio', wav: 'audio', ogg: 'audio', m4a: 'audio',
  mp4: 'video', mkv: 'video', webm: 'video', mov: 'video', avi: 'video',
  zip: 'archive', tar: 'archive', gz: 'archive', xz: 'archive', zst: 'archive',
  bz2: 'archive', '7z': 'archive', rar: 'archive',
  pdf: 'pdf', pem: 'lock', key: 'lock', gpg: 'lock',
  so: 'binary', o: 'binary', bin: 'binary', exe: 'binary', a: 'binary',
};

function iconFor(entry) {
  if (entry.isDir) return ICON.dir;
  const dot = entry.name.lastIndexOf('.');
  if (dot < 0) return ICON.file;
  return ICON[EXT_ICON[entry.name.slice(dot + 1).toLowerCase()]] || ICON.file;
}

const $ = (id) => document.getElementById(id);
const workspace = $('workspace');
const handleLayer = $('handles');
const enc = encodeURIComponent;

const wm = {
  root: null,
  focused: null,
  nextId: 1,
  windows: new Map(),   // id -> window state
  els: new Map(),       // id -> element
  nodes: [],            // devices from the API
  rects: {},            // last computed rects, used to pick split direction
};

// ============================================================ helpers

function setStatus(text, isErr) {
  $('msg').textContent = text;
  $('status').classList.toggle('err', !!isErr);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function human(n) {
  if (!n) return '0 B';
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(1) + ' ' + units[i];
}

function age(t) {
  if (!t || String(t).startsWith('0001')) return '—';
  const d = (Date.now() - new Date(t).getTime()) / 1000;
  if (d < 60) return 'now';
  if (d < 3600) return Math.floor(d / 60) + 'm';
  if (d < 86400) return Math.floor(d / 3600) + 'h';
  if (d < 31536000) return Math.floor(d / 86400) + 'd';
  return Math.floor(d / 31536000) + 'y';
}

function join(dir, name) {
  return dir === '/' ? '/' + name : dir.replace(/\/+$/, '') + '/' + name;
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let body;
  try { body = text ? JSON.parse(text) : {}; } catch { body = { error: text }; }
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function needsAuth(msg) {
  return /unable to authenticate|no supported methods|password|auth/i.test(msg || '');
}

// ============================================================ windows

function createWindow(nodeName, path, focusId) {
  const id = wm.nextId++;
  const st = {
    id,
    node: nodeName,
    path: path || '/',
    parent: '',
    entries: [],
    selected: new Set(),
  };
  wm.windows.set(id, st);

  const el = document.createElement('div');
  el.className = 'win';
  el.dataset.id = id;
  el.innerHTML = windowMarkup();

  workspace.appendChild(el);
  wm.els.set(id, el);
  bindWindow(st, el);

  // Dwindle: split the focused window along its longer axis. If the focused id
  // is stale the new window would never enter the tree, so fall back to the
  // last leaf and finally to the root.
  let target = focusId;
  if (target == null || !findLeaf(wm.root, target)) target = lastLeafId(wm.root);

  let dir = 'h';
  const rect = target != null ? wm.rects[target] : null;
  if (rect) dir = rect.w >= rect.h ? 'h' : 'v';
  wm.root = insertNode(wm.root, target, id, dir);

  wm.focused = id;
  render();
  refreshWindow(st);
  save();
  return st;
}

function closeWindow(id) {
  if (!wm.windows.has(id)) return;
  wm.windows.delete(id);
  const el = wm.els.get(id);
  if (el) el.remove();
  wm.els.delete(id);
  wm.root = removeNode(wm.root, id);

  if (wm.focused === id) {
    const remaining = [...wm.windows.keys()];
    wm.focused = remaining[remaining.length - 1] ?? null;
  }
  render();
  save();
  if (!wm.windows.size) setStatus('all windows closed — press n for a new one');
}

function focusWindow(id) {
  if (wm.focused === id) return;
  wm.focused = id;
  for (const [wid, el] of wm.els) el.classList.toggle('focused', wid === id);
  save();
}

// ============================================================ layout

function render() {
  const wsRect = workspace.getBoundingClientRect();
  const full = { x: 0, y: 0, w: wsRect.width, h: wsRect.height };
  const rects = {};
  const handles = [];
  computeLayout(wm.root, full, rects, handles);
  wm.rects = rects;

  for (const [id, el] of wm.els) {
    const r = rects[id];
    if (!r) { el.style.display = 'none'; continue; }
    el.style.display = 'flex';
    el.style.left = (r.x + GAP / 2) + 'px';
    el.style.top = (r.y + GAP / 2) + 'px';
    el.style.width = Math.max(0, r.w - GAP) + 'px';
    el.style.height = Math.max(0, r.h - GAP) + 'px';
    el.classList.toggle('focused', id === wm.focused);
  }

  handleLayer.innerHTML = '';
  for (const h of handles) {
    const d = document.createElement('div');
    d.className = 'handle ' + h.dir;
    if (h.dir === 'h') {
      d.style.left = (h.at - 4) + 'px';
      d.style.top = h.parent.y + 'px';
      d.style.width = '8px';
      d.style.height = h.parent.h + 'px';
    } else {
      d.style.top = (h.at - 4) + 'px';
      d.style.left = h.parent.x + 'px';
      d.style.height = '8px';
      d.style.width = h.parent.w + 'px';
    }
    d.addEventListener('pointerdown', (ev) => beginResize(ev, h, d));
    handleLayer.appendChild(d);
  }
}

function beginResize(ev, h, el) {
  ev.preventDefault();
  ev.stopPropagation();
  el.classList.add('active');
  el.setPointerCapture?.(ev.pointerId);

  const wsRect = workspace.getBoundingClientRect();
  const isH = h.dir === 'h';
  const base = isH ? h.parent.x : h.parent.y;
  const total = isH ? h.parent.w : h.parent.h;
  if (total <= 0) return;

  const onMove = (e) => {
    const pos = isH ? e.clientX - wsRect.left : e.clientY - wsRect.top;
    let r = (pos - base) / total;
    r = Math.min(0.88, Math.max(0.12, r));
    h.node.ratio = r;
    render();
  };
  const onUp = () => {
    el.classList.remove('active');
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', onUp);
    save();
  };
  window.addEventListener('pointermove', onMove);
  window.addEventListener('pointerup', onUp);
}

new ResizeObserver(() => render()).observe(workspace);

// ============================================================ window ui

function bindWindow(st, el) {
  const sel = el.querySelector('.dev-sel');
  const body = el.querySelector('.win-body');

  el.addEventListener('pointerdown', () => focusWindow(st.id));
  el.querySelector('.win-head').addEventListener('pointerdown', (ev) => startWindowDrag(ev, st));

  sel.onchange = () => {
    const n = wm.nodes.find((x) => x.name === sel.value);
    st.node = sel.value;
    st.path = (n && n.home) || '/';
    st.selected.clear();
    refreshWindow(st);
    save();
  };

  el.querySelectorAll('.win-tools button').forEach((b) => {
    b.onclick = (ev) => {
      ev.stopPropagation();
      focusWindow(st.id);
      onTool(st, b.dataset.act);
    };
  });

  // Cross-window drag and drop. The payload is read on drop only, because
  // dragover is not allowed to inspect dataTransfer contents.
  body.addEventListener('dragover', (e) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = e.shiftKey ? 'move' : 'copy';
    el.classList.add('drag-over');
    body.dataset.dropHint = st.node + ':' + st.path;
  });
  body.addEventListener('dragleave', (e) => {
    if (!el.contains(e.relatedTarget)) el.classList.remove('drag-over');
  });
  body.addEventListener('drop', (e) => {
    e.preventDefault();
    el.classList.remove('drag-over');
    onDrop(e, st);
  });
}

function onTool(st, act) {
  switch (act) {
    case 'up':
      if (st.parent) { st.path = st.parent; st.selected.clear(); refreshWindow(st); save(); }
      break;
    case 'close':
      closeWindow(st.id);
      break;
    case 'split':
      createWindow(nextNodeName(), homeOf(nextNodeName()), st.id);
      break;
    case 'mkdir':
      askName('new folder', '', async (name) => {
        await api('/api/mkdir', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ node: st.node, path: st.path, name }),
        });
        await refreshWindow(st);
      });
      break;
    case 'delete': {
      const names = [...st.selected];
      if (!names.length) { setStatus('nothing selected', true); return; }
      showDialog(`delete ${names.length} item(s)?`,
        `<div class="sel-list">${names.map((n) => `<div>${escapeHtml(n)}</div>`).join('')}</div>`,
        async () => {
          await api('/api/delete', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ node: st.node, path: st.path, names }),
          });
          st.selected.clear();
          await refreshWindow(st);
        });
      break;
    }
    case 'upload':
      uploadTarget = st;
      $('file').click();
      break;
  }
}

function nextNodeName() {
  if (!wm.nodes.length) return '';
  const used = [...wm.windows.values()].map((w) => w.node);
  const fresh = wm.nodes.find((n) => !used.includes(n.name));
  return (fresh || wm.nodes[wm.windows.size % wm.nodes.length]).name;
}

// homeOf returns a device's start directory. It is empty until the device has
// been contacted, so fall back to / and let refreshWindow follow the real home
// reported by the list call.
function homeOf(name) {
  const n = wm.nodes.find((x) => x.name === name);
  return (n && n.home) || '/';
}

// ============================================================ window drag
//
// Dragging a window header re-tiles it. Dropping on an outer band of another
// window splits that window on that side; dropping in the middle swaps the two.

let zoneEl = null;

function zoneNode() {
  if (!zoneEl) {
    zoneEl = document.createElement('div');
    zoneEl.id = 'zone';
    zoneEl.style.display = 'none';
    workspace.appendChild(zoneEl);
  }
  return zoneEl;
}

function showZone(hit) {
  const z = zoneNode();
  if (!hit) { z.style.display = 'none'; return; }

  const ws = workspace.getBoundingClientRect();
  const r = hit.rect;
  const x = r.left - ws.left;
  const y = r.top - ws.top;
  const w = r.width;
  const h = r.height;

  let zr;
  switch (hit.side) {
    case 'left':   zr = { x, y, w: w / 2, h }; break;
    case 'right':  zr = { x: x + w / 2, y, w: w / 2, h }; break;
    case 'top':    zr = { x, y, w, h: h / 2 }; break;
    case 'bottom': zr = { x, y: y + h / 2, w, h: h / 2 }; break;
    default:       zr = { x, y, w, h };
  }

  z.className = hit.side === 'swap' ? 'swap' : '';
  z.style.display = 'block';
  z.style.left = zr.x + 'px';
  z.style.top = zr.y + 'px';
  z.style.width = Math.max(0, zr.w) + 'px';
  z.style.height = Math.max(0, zr.h) + 'px';
}

function hideZone() {
  if (zoneEl) zoneEl.style.display = 'none';
}

// hitTest reports which window is under the pointer and which region of it.
function hitTest(x, y, selfId) {
  const under = document.elementFromPoint(x, y);
  const win = under && under.closest('.win');
  if (!win) return null;

  const id = Number(win.dataset.id);
  if (!id || id === selfId) return null;

  const r = win.getBoundingClientRect();
  if (!r.width || !r.height) return null;

  const fx = (x - r.left) / r.width;
  const fy = (y - r.top) / r.height;
  const edge = 0.35;

  let side;
  if (fx < edge) side = 'left';
  else if (fx > 1 - edge) side = 'right';
  else if (fy < edge) side = 'top';
  else if (fy > 1 - edge) side = 'bottom';
  else side = 'swap';

  return { targetId: id, side, rect: r };
}

function startWindowDrag(ev, st) {
  if (ev.button !== 0) return;
  if (ev.target.closest('button, select, input, a')) return;

  const el = wm.els.get(st.id);
  if (!el) return;

  const startX = ev.clientX;
  const startY = ev.clientY;
  let moved = false;
  let hit = null;

  const onMove = (e) => {
    if (!moved) {
      if (Math.abs(e.clientX - startX) < 5 && Math.abs(e.clientY - startY) < 5) return;
      moved = true;
      el.classList.add('dragging');
      document.body.classList.add('win-dragging');
      focusWindow(st.id);
    }
    hit = hitTest(e.clientX, e.clientY, st.id);
    showZone(hit);
  };

  const onUp = () => {
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', onUp);
    el.classList.remove('dragging');
    document.body.classList.remove('win-dragging');
    hideZone();

    if (!moved || !hit) return;
    if (moveWindow(st.id, hit.targetId, hit.side)) {
      focusWindow(st.id);
      render();
      save();
      setStatus(hit.side === 'swap' ? 'windows swapped' : `window moved ${hit.side}`);
    }
  };

  window.addEventListener('pointermove', onMove);
  window.addEventListener('pointerup', onUp);
}

async function refreshWindow(st) {
  const el = wm.els.get(st.id);
  if (!el) return;

  const sel = el.querySelector('.dev-sel');
  sel.innerHTML = wm.nodes.map((n) =>
    `<option value="${escapeHtml(n.name)}" ${n.name === st.node ? 'selected' : ''}>${escapeHtml(n.name)}</option>`
  ).join('');

  const body = el.querySelector('.win-body');
  try {
    const data = await api(`/api/list?node=${enc(st.node)}&path=${enc(st.path)}`);

    // A device's home is only known after connecting, so a window opened at /
    // hops to the real home once, then stays wherever the user navigates.
    if (!st.homeResolved && st.path === '/' && data.home && data.home !== '/') {
      st.homeResolved = true;
      st.path = data.home;
      save();
      return refreshWindow(st);
    }
    st.homeResolved = true;

    st.entries = data.entries || [];
    st.parent = data.parent || '';
    st.selected.clear();
    renderCrumb(st, el);
    renderRows(st, body);
  } catch (e) {
    st.entries = [];
    st.parent = '';
    renderCrumb(st, el);
    body.innerHTML = `<div class="empty">${escapeHtml(e.message)}</div>`;
    if (needsAuth(e.message)) askPassword(st);
  }
}

function renderCrumb(st, el) {
  const parts = st.path.split('/').filter(Boolean);
  let acc = '';
  const bits = [`<a data-path="/" class="${parts.length ? '' : 'cur'}">${escapeHtml(st.node)}</a>`];
  parts.forEach((p, i) => {
    acc += '/' + p;
    const last = i === parts.length - 1;
    bits.push('<span class="sep">/</span>');
    bits.push(`<a data-path="${escapeHtml(acc)}" class="${last ? 'cur' : ''}">${escapeHtml(p)}</a>`);
  });

  const crumb = el.querySelector('.win-crumb');
  crumb.innerHTML = bits.join('');
  crumb.querySelectorAll('a').forEach((a) => {
    a.onclick = () => { st.path = a.dataset.path; st.selected.clear(); refreshWindow(st); save(); };
  });

  el.querySelector('[data-act="up"]').disabled = !st.parent;
  el.querySelector('[data-act="delete"]').disabled = st.selected.size === 0;
}

function renderRows(st, body) {
  if (!st.entries.length) {
    body.innerHTML = '<div class="empty">empty directory</div>';
    return;
  }

  const rows = st.entries.map((e) => {
    const sel = st.selected.has(e.name);
    const href = e.isDir ? 'javascript:void(0)'
      : `/api/download?node=${enc(st.node)}&path=${enc(join(st.path, e.name))}`;
    return `<tr class="${sel ? 'sel' : ''}" draggable="true" data-name="${escapeHtml(e.name)}">
      <td class="pick"><input type="checkbox" data-name="${escapeHtml(e.name)}" ${sel ? 'checked' : ''}></td>
      <td class="name">
        <a class="${e.isDir ? 'dir' : ''}" draggable="false" href="${href}" data-dir="${e.isDir}" data-name="${escapeHtml(e.name)}">
          <span class="ico">${iconFor(e)}</span><span class="txt">${escapeHtml(e.name)}${e.isDir ? '/' : ''}</span>
        </a>
      </td>
      <td class="num col-size">${e.isDir ? '—' : human(e.size)}</td>
      <td class="num col-mod">${age(e.modTime)}</td>
    </tr>`;
  }).join('');

  body.innerHTML = `<table>
    <thead><tr>
      <th class="pick"></th>
      <th>name</th>
      <th class="num col-size">size</th>
      <th class="num col-mod">modified</th>
    </tr></thead>
    <tbody>${rows}</tbody></table>`;

  body.querySelectorAll('a[data-name]').forEach((a) => {
    a.onclick = (ev) => {
      ev.preventDefault();
      if (a.dataset.dir === 'true') {
        st.path = join(st.path, a.dataset.name);
        st.selected.clear();
        refreshWindow(st);
        save();
      }
    };
  });

  body.querySelectorAll('input[data-name]').forEach((cb) => {
    cb.onchange = () => {
      cb.checked ? st.selected.add(cb.dataset.name) : st.selected.delete(cb.dataset.name);
      cb.closest('tr').classList.toggle('sel', cb.checked);
      el_deleteDisabled(st);
    };
  });

  body.querySelectorAll('tr[draggable]').forEach((tr) => {
    tr.addEventListener('dragstart', (ev) => {
      const name = tr.dataset.name;
      // Dragging a row that is not selected drags just that row.
      const names = st.selected.has(name) ? [...st.selected] : [name];
      ev.dataTransfer.setData('application/x-knot', JSON.stringify({
        winId: st.id, node: st.node, path: st.path, names,
      }));
      ev.dataTransfer.effectAllowed = 'copyMove';
      tr.classList.add('dragging');
      setStatus(`dragging ${names.length} item(s) — drop on another window`);
    });
    tr.addEventListener('dragend', () => {
      tr.classList.remove('dragging');
      setStatus('ready');
    });
  });
}

function el_deleteDisabled(st) {
  const el = wm.els.get(st.id);
  if (!el) return;
  const btn = el.querySelector('[data-act="delete"]');
  if (btn) btn.disabled = st.selected.size === 0;
}

// ============================================================ drop -> transfer

async function onDrop(ev, dst) {
  const raw = ev.dataTransfer.getData('application/x-knot');
  if (!raw) return;

  let payload;
  try { payload = JSON.parse(raw); } catch { return; }

  if (payload.winId === dst.id) {
    setStatus('same window — drop on another one', true);
    return;
  }

  const move = ev.shiftKey;
  await runTransfer(payload, dst, move);
}

async function runTransfer(payload, dst, move) {
  const verb = move ? 'move' : 'copy';
  setStatus(`${verb}ing ${payload.names.length} item(s) ${payload.node} → ${dst.node}…`);

  try {
    const { id } = await api('/api/transfer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        srcNode: payload.node,
        srcPath: payload.path,
        dstNode: dst.node,
        dstPath: dst.path,
        names: payload.names,
        move,
      }),
    });
    await pollJob(id, verb);
  } catch (e) {
    setStatus(e.message, true);
  }
}

async function pollJob(id, verb) {
  for (;;) {
    let j;
    try {
      j = await api('/api/jobs/' + id);
    } catch (e) {
      setStatus(e.message, true);
      $('bar').style.display = 'none';
      return;
    }

    if (j.total > 0) {
      const pct = Math.round((j.copied / j.total) * 100);
      $('bar').style.display = 'block';
      $('bar').firstElementChild.style.width = pct + '%';
      setStatus(`${verb}ing — ${pct}% (${human(j.copied)} / ${human(j.total)})`);
    }

    if (j.done) {
      $('bar').style.display = 'none';
      if (j.err) {
        setStatus(j.err, true);
      } else {
        setStatus(`${verb} complete`);
        for (const w of wm.windows.values()) refreshWindow(w);
      }
      return;
    }
    await new Promise((r) => setTimeout(r, 350));
  }
}

// ============================================================ dialogs

let dlgOk = null;
let uploadTarget = null;

function showDialog(title, bodyHtml, onOk) {
  $('dlgTitle').textContent = title;
  $('dlgBody').innerHTML = bodyHtml;
  dlgOk = onOk;
  $('dlg').showModal();
  const first = $('dlgBody').querySelector('input');
  if (first) { first.focus(); first.select(); }
}

function askName(title, value, onOk) {
  showDialog(title,
    `<div class="field"><span>name</span><input id="nm" value="${escapeHtml(value)}"></div>`,
    async () => {
      const v = $('nm').value.trim();
      if (!v) throw new Error('name required');
      await onOk(v);
    });
}

$('dlgCancel').onclick = () => $('dlg').close();
$('dlgOk').onclick = async () => {
  if (!dlgOk) return $('dlg').close();
  try {
    await dlgOk();
    $('dlg').close();
  } catch (e) {
    setStatus(e.message, true);
  }
};
$('dlgBody').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') { ev.preventDefault(); $('dlgOk').click(); }
});

function askPassword(st) {
  showDialog(`password for ${st.node}`,
    `<div class="field"><span>password</span>
       <input id="pw" type="password" autocomplete="current-password"></div>
     <div style="color:var(--mute);font-size:11.5px">
       held in memory only · ssh-copy-id -p 8022 ${escapeHtml(st.node)} removes this step
     </div>`,
    async () => {
      const password = $('pw').value;
      if (!password) throw new Error('password required');
      const res = await api('/api/auth', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ node: st.node, password }),
      });
      if (res.home && st.path === '/') st.path = res.home;
      await refreshWindow(st);
    });
}

// ============================================================ upload

$('file').onchange = async () => {
  const files = $('file').files;
  const st = uploadTarget;
  if (!files.length || !st) return;
  const fd = new FormData();
  fd.append('node', st.node);
  fd.append('path', st.path);
  for (const f of files) fd.append('file', f);
  setStatus(`uploading ${files.length} file(s)…`);
  try {
    await api('/api/upload', { method: 'POST', body: fd });
    setStatus('uploaded');
    await refreshWindow(st);
  } catch (e) {
    setStatus(e.message, true);
  }
  $('file').value = '';
};

// ============================================================ persistence

const STORE_KEY = 'knot.layout.v1';

function save() {
  try {
    const windows = [...wm.windows.values()].map((w) => ({ id: w.id, node: w.node, path: w.path }));
    localStorage.setItem(STORE_KEY, JSON.stringify({
      root: wm.root, windows, focused: wm.focused, nextId: wm.nextId,
    }));
  } catch { /* private mode, ignore */ }
}

function restore() {
  let saved;
  try { saved = JSON.parse(localStorage.getItem(STORE_KEY) || 'null'); } catch { return false; }
  if (!saved || !saved.root || !Array.isArray(saved.windows) || !saved.windows.length) return false;

  wm.root = saved.root;
  wm.nextId = saved.nextId || saved.windows.length + 1;
  wm.focused = saved.focused;

  for (const w of saved.windows) {
    if (!wm.nodes.some((n) => n.name === w.node)) continue;
    const st = { id: w.id, node: w.node, path: w.path, parent: '', entries: [], selected: new Set(), homeResolved: true };
    wm.windows.set(w.id, st);

    const el = document.createElement('div');
    el.className = 'win';
    el.dataset.id = w.id;
    el.innerHTML = windowMarkup();
    workspace.appendChild(el);
    wm.els.set(w.id, el);
    bindWindow(st, el);
    refreshWindow(st);
  }

  if (!wm.windows.size) return false;
  wm.root = pruneTree(wm.root, new Set(wm.windows.keys()));
  if (!wm.root) return false;
  if (!wm.windows.has(wm.focused)) wm.focused = [...wm.windows.keys()][0];
  render();
  return true;
}

function windowMarkup() {
  return `
    <div class="win-head">
      <select class="dev-sel" title="device"></select>
      <div class="win-crumb"></div>
      <div class="win-tools">
        <button data-act="up" title="parent directory">&#xf062;</button>
        <button data-act="mkdir" title="new folder">&#xf07b;</button>
        <button data-act="upload" title="upload">&#xf093;</button>
        <button data-act="delete" title="delete selected">&#xf1f8;</button>
        <button data-act="split" title="split with a new window">&#xf067;</button>
        <button data-act="close" class="close" title="close window">&#xf00d;</button>
      </div>
    </div>
    <div class="win-body" data-drop-hint=""></div>`;
}

// ============================================================ topbar

$('newWin').onclick = () => {
  const target = wm.focused ?? [...wm.windows.keys()][0] ?? null;
  const name = nextNodeName();
  createWindow(name, homeOf(name), target);
};

$('resetLayout').onclick = () => {
  showDialog('reset layout?', '<div style="color:var(--dim)">Closes every window and starts over.</div>', async () => {
    for (const id of [...wm.windows.keys()]) closeWindow(id);
    localStorage.removeItem(STORE_KEY);
    createWindow(wm.nodes[0].name, wm.nodes[0].home || '/', null);
  });
};

document.addEventListener('keydown', (ev) => {
  const tag = ev.target.tagName;
  if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') return;
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return;

  const st = wm.windows.get(wm.focused);
  if (ev.key === 'n') { ev.preventDefault(); $('newWin').click(); }
  if (ev.key === 'r' && st) { ev.preventDefault(); refreshWindow(st); }
  if (ev.key === 'Backspace' && st && st.parent) {
    ev.preventDefault();
    st.path = st.parent;
    st.selected.clear();
    refreshWindow(st);
    save();
  }
});

// ============================================================ boot

async function loadNodes() {
  const data = await api('/api/nodes');
  wm.nodes = data.nodes || [];
  const online = wm.nodes.filter((n) => n.online).length;
  $('tailnetLabel').textContent = `${online}/${wm.nodes.length} online`;
}

async function ensureAuth() {
  const url = new URL(location.href);
  const t = url.searchParams.get('t');
  if (t) {
    try {
      await api('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: t }),
      });
      url.searchParams.delete('t');
      history.replaceState({}, '', url.pathname + url.search);
      return true;
    } catch { /* fall through */ }
  }
  try { await api('/api/nodes'); return true; } catch { return false; }
}

function askToken() {
  setStatus('token required', true);
  showDialog('access token',
    '<div class="field"><span>token</span><input id="tok" placeholder="printed by knot serve"></div>',
    async () => {
      await api('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: $('tok').value.trim() }),
      });
      await bootWindows();
    });
}

async function bootWindows() {
  await loadNodes();
  if (!wm.nodes.length) {
    setStatus('no nodes configured — edit ~/.config/knot/config.json', true);
    return;
  }
  if (!restore()) {
    createWindow(wm.nodes[0].name, wm.nodes[0].home || '/', null);
  }
  setStatus('ready');
}

(async function boot() {
  try {
    if (!(await ensureAuth())) { askToken(); return; }
    await bootWindows();
  } catch (e) {
    setStatus(e.message, true);
  }
})();
