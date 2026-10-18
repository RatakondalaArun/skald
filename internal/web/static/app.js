// Skald dashboard: hash router, data loading, and the live event stream.
import { h, STATUS, refreshTimes } from './util.js';
import { projectsView, projectView, docView, activityView, errorView, railProjects } from './views.js';

const main = document.getElementById('main');
const liveEl = document.getElementById('live');
const liveText = document.getElementById('live-text');
const toastEl = document.getElementById('toast');
const tipEl = document.getElementById('tip');
const token = document.querySelector('meta[name="skald-token"]')?.content || '';

// UI state that survives re-renders (filters, pending actions) and the data of
// the current view.
const st = {
  filters: { actor: '', feature: '', query: '', ready: false, showCancelled: false, showAllDone: false },
  confirmCancel: false,
  form: null, // which inline task form is open: block, done, note or evidence
  draft: {}, // text typed into those forms
  busy: false,
};
let route = { name: 'projects' };
let data = null;
let loadSeq = 0;
let projectList = [];
const railEl = document.getElementById('rail-projects');

// ---- api -------------------------------------------------------------------

class ApiError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `Request failed with status ${status}.`);
    this.status = status;
    this.kind = body?.error?.kind || 'unknown';
    this.current = body?.error?.current_task;
  }
}

async function api(path, opts = {}) {
  const init = { headers: { Accept: 'application/json' }, ...opts };
  if (opts.method === 'POST') init.headers = { ...init.headers, 'Content-Type': 'application/json', 'X-Skald-Token': token };
  let res;
  try {
    res = await fetch(path, init);
  } catch {
    throw new ApiError(0, { error: { kind: 'network', message: 'Cannot reach the skald server. Is skald serve still running?' } });
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) throw new ApiError(res.status, body);
  return body;
}

const enc = encodeURIComponent;

// ---- routing -----------------------------------------------------------------

function parseRoute() {
  const parts = location.hash.replace(/^#\/?/, '').split('/').filter(Boolean).map(decodeURIComponent);
  if (parts[0] === 'activity') return { name: 'activity' };
  if (parts[0] === 'p' && parts[1]) {
    if (parts[2] === 't' && parts[3]) return { name: 'task', slug: parts[1], id: parts[3] };
    if (parts[2] === 'doc' && parts[3] && parts[4]) return { name: 'doc', slug: parts[1], kind: parts[3], title: parts[4] };
    const view = ['features', 'activity', 'docs'].includes(parts[2]) ? parts[2] : 'board';
    return { name: 'project', slug: parts[1], view };
  }
  return { name: 'projects' };
}

async function loadData(r) {
  switch (r.name) {
    case 'projects': return api('/api/projects');
    case 'activity': return { feed: (await api('/api/events?limit=100')).events.reverse() };
    case 'project': {
      const base = `/api/projects/${enc(r.slug)}`;
      const [summary, board, ev] = await Promise.all([api(base), api(`${base}/board`), api(`${base}/events?limit=80`)]);
      return { summary, board, feed: ev.events.reverse() };
    }
    case 'task': {
      const base = `/api/projects/${enc(r.slug)}`;
      const [summary, board, ev, taskData] = await Promise.all([api(base), api(`${base}/board`), api(`${base}/events?limit=80`), api(`${base}/tasks/${enc(r.id)}`)]);
      return { summary, board, feed: ev.events.reverse(), taskData };
    }
    case 'doc': return api(`/api/projects/${enc(r.slug)}/document?kind=${enc(r.kind)}&title=${enc(r.title)}`);
    default: return null;
  }
}

function sameView(a, b) {
  return a.name === b.name && a.slug === b.slug && a.id === b.id && a.kind === b.kind && a.title === b.title && a.view === b.view;
}

async function navigate() {
  const next = parseRoute();
  if (!sameView(next, route)) {
    st.confirmCancel = false;
    st.form = null;
    st.draft = {};
    if (next.slug !== route.slug) Object.assign(st.filters, { feature: '', query: '', ready: false });
  }
  const prev = route;
  route = next;
  // Opening or closing a task keeps the board on screen behind the drawer.
  const sameProject = prev.slug === next.slug && ['project', 'task'].includes(prev.name) && ['project', 'task'].includes(next.name);
  if (!sameProject) data = null;
  markNav();
  await refresh({ hard: true });
  if (route === next) (document.querySelector('.drawer') || main).focus({ preventScroll: true });
}

function markNav() {
  for (const a of document.querySelectorAll('[data-nav]')) {
    if (a.dataset.nav === (route.name === 'activity' ? 'activity' : 'projects') && !route.slug) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  paintRail();
}

// refresh loads the current route's data and renders it. A soft refresh (after
// a live event) keeps the old view on failure instead of replacing it.
async function refresh({ hard = false } = {}) {
  const seq = ++loadSeq;
  const r = route;
  let fresh;
  try {
    fresh = await loadData(r);
  } catch (err) {
    if (seq !== loadSeq) return;
    if (hard || !data) {
      const back = r.slug ? [`#/p/${enc(r.slug)}`, 'Back to the project'] : ['#/', 'Back to all projects'];
      const missing = err.status === 404;
      main.replaceChildren(r.name === 'task' || r.name === 'doc'
        ? errorView(missing ? 'Not found' : 'Could not load this page', err.message, ...back)
        : errorView(missing ? 'No such project' : 'Could not load this page', err.message));
      document.title = 'Skald';
    }
    return;
  }
  if (seq !== loadSeq) return; // a newer navigation won
  // Keep live events that arrived after the feed was fetched.
  if (data?.feed && fresh.feed && !hard) {
    const have = new Set(fresh.feed.map((e) => e.id));
    fresh.feed = [...data.feed.filter((e) => !have.has(e.id) && e.id > (fresh.feed.at(-1)?.id || 0)), ...fresh.feed];
  }
  data = fresh;
  render();
}

function titleFor() {
  if (!data) return 'Skald';
  switch (route.name) {
    case 'project': return `${data.summary.project.name} - Skald`;
    case 'task': return `${data.taskData.task.id} ${data.taskData.task.title} - Skald`;
    case 'doc': return `${data.document.title} - Skald`;
    case 'activity': return 'Activity - Skald';
    default: return 'Skald';
  }
}

// render rebuilds the view, then restores scroll position and keyboard focus
// so a live update does not disturb someone who is reading or tabbing.
function render() {
  if (!data) return;
  const key = document.activeElement?.dataset?.key;
  const scroll = new Map([...document.querySelectorAll('[data-scroll]')].map((el) => [el.dataset.scroll, el.scrollTop]));
  const mainY = main.scrollTop;
  const boardEl = document.querySelector('.board');
  const boardX = boardEl ? boardEl.scrollLeft : 0;
  const hadDrawer = !!document.querySelector('.drawer');
  const ae = document.activeElement;
  const caret = ae && ('selectionStart' in ae) && ae.dataset?.key ? [ae.selectionStart, ae.selectionEnd] : null;
  let view;
  switch (route.name) {
    case 'projects': view = projectsView(data); break;
    case 'project':
    case 'task': view = projectView(data, st, render, route, taskAction); break;
    case 'doc': view = docView(data, route.slug); break;
    case 'activity': view = activityView(data, st, render); break;
    default: view = errorView('Not found', 'There is no such page.');
  }
  main.replaceChildren(view);
  document.title = titleFor();
  main.scrollTop = mainY;
  for (const el of document.querySelectorAll('[data-scroll]')) el.scrollTop = scroll.get(el.dataset.scroll) || 0;
  const nb = document.querySelector('.board');
  if (nb) nb.scrollLeft = boardX;
  // A live refresh rebuilds the drawer; keep focus inside it.
  if (hadDrawer && !key) document.querySelector('.drawer')?.focus({ preventScroll: true });
  if (key) {
    const el = document.querySelector(`[data-key="${CSS.escape(key)}"]`);
    el?.focus({ preventScroll: true });
    if (el && caret) { try { el.setSelectionRange(caret[0], caret[1]); } catch { /* not a text field */ } }
  }
}

// ---- rail ------------------------------------------------------------------

function paintRail() {
  railEl.replaceChildren(...railProjects(projectList, route.slug));
}
async function refreshRail() {
  try {
    projectList = (await api('/api/projects')).projects;
    paintRail();
  } catch { /* the main view reports connection problems */ }
}

// ---- actions ---------------------------------------------------------------

function toast(msg, isErr = false) {
  toastEl.textContent = msg;
  toastEl.classList.toggle('err', isErr);
  toastEl.classList.add('show');
  clearTimeout(toast.t);
  toast.t = setTimeout(() => toastEl.classList.remove('show'), isErr ? 7000 : 3200);
}

const DONE_WORD = { unblock: 'unblocked', cancel: 'cancelled', reopen: 'reopened', start: 'started', edit: 'updated', 'dep-add': 'dependency added', 'dep-remove': 'dependency removed', done: 'marked done', block: 'blocked', note: 'note added', evidence: 'evidence added' };

async function githubSync() {
  st.busy = true;
  render();
  try {
    const r = await api(`/api/projects/${enc(route.slug)}/github/sync`, { method: 'POST', body: JSON.stringify({}) });
    const parts = [`Synced ${r.repo}: ${r.tasks_created} new, ${r.tasks_updated} updated, ${r.status_changes} status changes`];
    if (r.skipped.length) parts.push(`${r.skipped.length} skipped`);
    if (r.diverged.length) parts.push(`${r.diverged.length} finished here but open on GitHub`);
    toast(parts.join(', ') + '.');
  } catch (err) {
    toast(err.message, true);
  } finally {
    st.busy = false;
  }
  await refresh({ hard: true });
}

async function taskAction(action, t, extra) {
  if (action === 'github-sync') { await githubSync(); return; }
  if (action === 'cancel-ask') { st.confirmCancel = true; render(); return; }
  if (action === 'cancel-no') { st.confirmCancel = false; render(); return; }
  if (action === 'form') {
    st.draft = extra === 'edit' ? { title: t.title, description: t.description || '', criteria: t.acceptance_criteria || '', priority: String(t.priority), weight: String(t.weight) } : {};
    st.form = extra; st.confirmCancel = false; render(); const box = document.querySelector('.formbox');
    box?.scrollIntoView({ block: 'nearest' });
    box?.querySelector('[data-key^="f-"]')?.focus({ preventScroll: true });
    return;
  }
  st.busy = true;
  render();
  // Notes and evidence are not version-checked; status changes are.
  const unversioned = ['note', 'evidence', 'dep-add', 'dep-remove'].includes(action);
  const body = unversioned ? extra : { expected_version: t.version, ...extra };
  const path = action.startsWith('dep-') ? `dependencies/${action.slice(4)}` : action;
  try {
    await api(`/api/projects/${enc(route.slug)}/tasks/${enc(t.id)}/${path}`, { method: 'POST', body: JSON.stringify(body) });
    toast(`${t.id} ${DONE_WORD[action]}.`);
    st.form = null;
    st.draft = {};
  } catch (err) {
    toast(err.message, true);
    // Completing without evidence: take them straight to the evidence form.
    if (err.kind === 'evidence_required') st.form = 'evidence';
  } finally {
    st.busy = false;
    st.confirmCancel = false;
  }
  await refresh({ hard: true });
}

// ---- live stream -------------------------------------------------------------

function setLive(state, text) {
  liveEl.dataset.state = state;
  liveText.textContent = text;
}

let refreshTimer = 0;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => { refresh(); refreshRail(); }, 250);
}

function onEvent(ev) {
  ev._fresh = true;
  setTimeout(() => { ev._fresh = false; }, 2500);
  if (data?.feed && (route.name === 'activity' || ((route.name === 'project' || route.name === 'task') && ev.project === route.slug))) {
    if (!data.feed.some((e) => e.id === ev.id)) data.feed.unshift(ev);
  }
  scheduleRefresh(); // any project can change the rail's numbers
}

let es;
function connect() {
  es?.close();
  es = new EventSource('/events');
  es.onopen = () => {
    setLive('live', 'Live');
    refresh(); // covers anything that happened while disconnected
  };
  es.onmessage = (m) => {
    try { onEvent(JSON.parse(m.data)); } catch { /* ignore a malformed event */ }
  };
  es.onerror = () => {
    setLive('retry', 'Reconnecting');
    // The browser retries by itself unless the connection was closed for good.
    if (es.readyState === EventSource.CLOSED) setTimeout(connect, 3000);
  };
}

// ---- tooltip -----------------------------------------------------------------

function showTip(el) {
  const [id, title, status] = el.dataset.tip.split('\t');
  tipEl.replaceChildren(h('span', { class: 'id' }, id), ` ${title}`, status ? [h('br'), status] : null);
  tipEl.hidden = false;
  const r = el.getBoundingClientRect();
  const w = tipEl.offsetWidth;
  const left = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), window.innerWidth - w - 8);
  const top = r.top - tipEl.offsetHeight - 8;
  tipEl.style.left = `${left}px`;
  tipEl.style.top = `${top < 8 ? r.bottom + 8 : top}px`;
}
const hideTip = () => { tipEl.hidden = true; };
document.addEventListener('pointerover', (e) => { const el = e.target.closest?.('[data-tip]'); if (el) showTip(el); });
document.addEventListener('pointerout', (e) => { if (e.target.closest?.('[data-tip]')) hideTip(); });
document.addEventListener('focusin', (e) => { const el = e.target.closest?.('[data-tip]'); if (el) showTip(el); });
document.addEventListener('focusout', hideTip);
document.addEventListener('scroll', hideTip, { capture: true, passive: true });

// ---- theme, header, shortcuts -------------------------------------------------

const themeBtn = document.getElementById('theme');
const THEMES = ['auto', 'light', 'dark'];
const THEME_ICON = { auto: '◐', light: '☀', dark: '☾' };
function currentTheme() { const t = document.documentElement.dataset.theme; return THEMES.includes(t) ? t : 'auto'; }
function paintTheme() {
  const t = currentTheme();
  themeBtn.textContent = THEME_ICON[t];
  themeBtn.title = `Theme: ${t} (click to change)`;
  themeBtn.setAttribute('aria-label', `Color theme: ${t}. Activate to change.`);
}
themeBtn.addEventListener('click', () => {
  const next = THEMES[(THEMES.indexOf(currentTheme()) + 1) % THEMES.length];
  if (next === 'auto') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = next;
  try { if (next === 'auto') localStorage.removeItem('skald-theme'); else localStorage.setItem('skald-theme', next); } catch { /* ignore */ }
  paintTheme();
});
paintTheme();

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && ['INPUT', 'SELECT', 'TEXTAREA'].includes(document.activeElement?.tagName)) { document.activeElement.blur(); return; }
  if (e.key === 'Escape' && route.name === 'task') { location.hash = `#/p/${enc(route.slug)}`; return; }
  if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
  const tag = document.activeElement?.tagName;
  if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') return;
  const box = document.getElementById('board-search');
  if (box) { e.preventDefault(); box.focus(); box.select(); }
});

// ---- start -------------------------------------------------------------------

window.addEventListener('hashchange', navigate);
setInterval(() => refreshTimes(main), 20000);
setLive('connecting', 'Connecting');
connect();
navigate();
refreshRail();
export { STATUS };
