// Skald dashboard: hash router, data loading, and the live event stream.
import { h, STATUS, refreshTimes } from './util.js';
import { projectsView, projectView, taskView, docView, activityView, errorView } from './views.js';

const main = document.getElementById('main');
const liveEl = document.getElementById('live');
const liveText = document.getElementById('live-text');
const toastEl = document.getElementById('toast');
const tipEl = document.getElementById('tip');
const token = document.querySelector('meta[name="skald-token"]')?.content || '';

// UI state that survives re-renders (filters, pending actions) and the data of
// the current view.
const st = {
  filters: { actor: '', feature: '', showCancelled: false, showAllDone: false },
  confirmCancel: false,
  busy: false,
};
let route = { name: 'projects' };
let data = null;
let loadSeq = 0;

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
    return { name: 'project', slug: parts[1] };
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
    case 'task': return api(`/api/projects/${enc(r.slug)}/tasks/${enc(r.id)}`);
    case 'doc': return api(`/api/projects/${enc(r.slug)}/document?kind=${enc(r.kind)}&title=${enc(r.title)}`);
    default: return null;
  }
}

function sameView(a, b) {
  return a.name === b.name && a.slug === b.slug && a.id === b.id && a.kind === b.kind && a.title === b.title;
}

async function navigate() {
  const next = parseRoute();
  if (!sameView(next, route)) {
    st.confirmCancel = false;
    if (next.name !== route.name || next.slug !== route.slug) st.filters.feature = '';
  }
  route = next;
  data = null;
  markNav();
  await refresh({ hard: true });
}

function markNav() {
  for (const a of document.querySelectorAll('[data-nav]')) {
    if (a.dataset.nav === (route.name === 'activity' ? 'activity' : 'projects')) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
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
    case 'task': return `${data.task.id} ${data.task.title} - Skald`;
    case 'doc': return `${data.document.title} - Skald`;
    case 'activity': return 'Activity - Skald';
    default: return 'Skald';
  }
}

// render rebuilds the view, then restores scroll position and keyboard focus
// so a live update does not disturb someone who is reading or tabbing.
function render() {
  if (!data) return;
  const y = window.scrollY;
  const key = document.activeElement?.dataset?.key;
  let view;
  switch (route.name) {
    case 'projects': view = projectsView(data); break;
    case 'project': view = projectView(feedView(), st, render); break;
    case 'task': view = taskView(data, st, taskAction); break;
    case 'doc': view = docView(data, route.slug); break;
    case 'activity': view = activityView(data, st, render); break;
    default: view = errorView('Not found', 'There is no such page.');
  }
  main.replaceChildren(view);
  document.title = titleFor();
  window.scrollTo(0, y);
  if (key) document.querySelector(`[data-key="${CSS.escape(key)}"]`)?.focus({ preventScroll: true });
}

const feedView = () => data;

// ---- actions ---------------------------------------------------------------

function toast(msg, isErr = false) {
  toastEl.textContent = msg;
  toastEl.classList.toggle('err', isErr);
  toastEl.classList.add('show');
  clearTimeout(toast.t);
  toast.t = setTimeout(() => toastEl.classList.remove('show'), isErr ? 7000 : 3200);
}

const DONE_WORD = { unblock: 'unblocked', cancel: 'cancelled', reopen: 'reopened' };

async function taskAction(action, t) {
  if (action === 'cancel-ask') { st.confirmCancel = true; render(); return; }
  if (action === 'cancel-no') { st.confirmCancel = false; render(); return; }
  st.busy = true;
  render();
  try {
    await api(`/api/projects/${enc(route.slug)}/tasks/${enc(t.id)}/${action}`, {
      method: 'POST', body: JSON.stringify({ expected_version: t.version }),
    });
    toast(`${t.id} ${DONE_WORD[action]}.`);
  } catch (err) {
    toast(err.message, true);
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
  refreshTimer = setTimeout(() => refresh(), 250);
}

function onEvent(ev) {
  ev._fresh = true;
  setTimeout(() => { ev._fresh = false; }, 2500);
  if (data?.feed && (route.name === 'activity' || (route.name === 'project' && ev.project === route.slug))) {
    if (!data.feed.some((e) => e.id === ev.id)) data.feed.unshift(ev);
  }
  const relevant = route.name === 'projects' || route.name === 'activity'
    || ev.project === route.slug;
  if (relevant) scheduleRefresh();
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
  tipEl.replaceChildren(h('span', { class: 'id' }, id), ` ${title}`, h('br'), status);
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
window.addEventListener('scroll', hideTip, { passive: true });

// ---- start -------------------------------------------------------------------

window.addEventListener('hashchange', () => { navigate(); main.focus({ preventScroll: true }); });
setInterval(() => refreshTimes(main), 20000);
setLive('connecting', 'Connecting');
connect();
navigate();
export { STATUS };
