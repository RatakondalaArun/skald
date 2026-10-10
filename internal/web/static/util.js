// Small helpers shared by the views. Everything is built with DOM calls and
// textContent, never innerHTML, so event text from the database cannot inject
// markup.

export function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style') Object.assign(el.style, v);
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid);
  }
  return el;
}

export const STATUS = {
  todo: 'To do',
  in_progress: 'In progress',
  blocked: 'Blocked',
  done: 'Done',
  cancelled: 'Cancelled',
};
export const PRIORITY = ['urgent', 'high', 'normal', 'low'];

export const mk = (status) => h('span', { class: `mk ${status}`, 'aria-hidden': 'true' });

export function plural(n, one, many) {
  return `${n} ${n === 1 ? one : many || one + 's'}`;
}

export function pct(fraction) {
  return Math.round((fraction || 0) * 100);
}

export function fmtDate(iso) {
  if (!iso) return '';
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function relTime(iso, now = Date.now()) {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 10) return 'just now';
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 86400 * 14) return `${Math.floor(s / 86400)}d ago`;
  return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

export function timeEl(iso) {
  return h('time', { class: 'rel', datetime: iso, title: fmtDate(iso) }, relTime(iso));
}

export function refreshTimes(root = document) {
  const now = Date.now();
  for (const t of root.querySelectorAll('time.rel')) t.textContent = relTime(t.getAttribute('datetime'), now);
}

// Who did it, in words. Actors look like "human", "human:web" and
// "claude:<client>/<session>".
export function actorName(a) {
  if (a === 'human') return 'You (CLI)';
  if (a === 'human:web') return 'You (dashboard)';
  if (a.startsWith('claude:')) {
    const session = a.slice(7).split('/')[1];
    return session ? `Claude (${session.slice(0, 8)})` : 'Claude';
  }
  return a;
}

export const taskNum = (id) => Number(String(id).replace(/^\D+-/, ''));
export const taskHref = (slug, id) => `#/p/${encodeURIComponent(slug)}/t/${taskNum(id)}`;

const clip = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s);

// One event as a sentence: the actor, what they did, and the task it was about.
export function eventLine(ev, { showProject = false } = {}) {
  const p = ev.payload || {};
  const who = h('span', { class: 'who', title: ev.actor }, actorName(ev.actor));
  const link = ev.task && ev.project ? h('a', { href: taskHref(ev.project, ev.task) }, ev.task) : ev.task;
  const about = ev.task ? [' ', link, ev.title ? ` ${ev.title}` : ''] : [];
  const inTask = !ev.task; // shown in a task's own history: no need to name the task
  let verb = null;
  let quote = '';
  switch (ev.type) {
    case 'task.created': verb = inTask ? 'created the task' : 'added'; break;
    case 'task.updated': verb = inTask ? 'edited the task' : 'edited'; break;
    case 'task.status_changed':
      if (p.from === 'blocked') verb = 'unblocked';
      else if (p.to === 'in_progress') verb = 'started';
      else if (p.to === 'done') { verb = p.forced ? 'finished (forced)' : 'finished'; quote = p.note || ''; }
      else if (p.to === 'blocked') { verb = 'blocked'; quote = p.reason || ''; }
      else if (p.to === 'cancelled') verb = 'cancelled';
      else if (p.to === 'todo') verb = 'reopened';
      else verb = `moved to ${p.to}`;
      break;
    case 'task.note': verb = inTask ? 'added a note' : 'noted on'; quote = p.text || ''; break;
    case 'artifact.added': verb = inTask ? `recorded ${p.kind || 'evidence'} evidence` : `recorded ${p.kind || 'evidence'} on`; quote = p.ref || ''; break;
    default: break;
  }
  let body;
  if (ev.type === 'task.dependency_added' || ev.type === 'task.dependency_removed') {
    const dep = `T-${p.depends_on}`;
    const depEl = ev.project ? h('a', { href: taskHref(ev.project, dep) }, dep) : dep;
    const added = ev.type === 'task.dependency_added';
    body = inTask
      ? [added ? 'made the task wait for ' : 'stopped it waiting for ', depEl]
      : [added ? 'made ' : 'unlinked ', link, ev.title ? ` ${ev.title}` : '', added ? ' wait for ' : ' from ', depEl];
  } else if (verb) {
    body = [verb, ...about];
  } else {
    switch (ev.type) {
      case 'feature.created': body = ['added feature ', p.title || ev.entity]; break;
      case 'feature.updated': body = ['edited feature ', ev.entity]; break;
      case 'feature.cancelled': body = ['cancelled feature ', ev.entity]; break;
      case 'project.created': body = ['created project ', p.name || '']; break;
      case 'project.archived': body = ['archived the project']; break;
      case 'project.updated': body = ['updated the project']; break;
      case 'document.version_added': body = [`saved ${p.kind || 'document'} “${p.title || ''}” as version ${p.version}`]; break;
      default: body = [ev.type]; break;
    }
  }
  return [
    who, ' ', h('span', { class: 'what' }, body),
    showProject && ev.project ? h('span', { class: 'proj' }, ` in ${ev.project}`) : null,
    quote ? h('span', { class: 'quote' }, `“${clip(quote, 160)}”`) : null,
    h('time', { class: 'rel', datetime: ev.at, title: fmtDate(ev.at) }, relTime(ev.at)),
  ];
}
