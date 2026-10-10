// Views: each takes loaded data plus the UI state and returns a DOM node.
import { h, mk, STATUS, PRIORITY, plural, pct, fmtDate, timeEl, taskHref, eventLine, actorName } from './util.js';

const FEATURE_MARK = { planned: 'todo', active: 'in_progress', done: 'done', cancelled: 'cancelled' };
const FEATURE_LABEL = { planned: 'Not started', active: 'In progress', done: 'Done', cancelled: 'Cancelled' };

function progressBar(fraction, label) {
  const v = pct(fraction);
  return h('div', {
    class: 'bar-track', role: 'progressbar', 'aria-label': label,
    'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(v),
  }, h('div', { class: 'bar-fill', style: { width: `${v}%` } }));
}

const tipFor = (t) => `${t.id}\t${t.title}\t${STATUS[t.status] || t.status}`;

export function errorView(title, message, backHref = '#/', backText = 'Back to all projects') {
  return h('div', { class: 'error-box' }, h('h1', null, title), h('p', null, message),
    h('p', { style: { marginTop: '14px' } }, h('a', { href: backHref }, backText)));
}

// ---- projects ----------------------------------------------------------------

export function projectsView({ projects }) {
  const root = h('div', null, h('div', { class: 'head', style: { paddingTop: '14px' } }, h('h1', null, 'Projects')));
  if (!projects.length) {
    root.append(h('p', { class: 'empty', style: { marginTop: '20px' } },
      'No projects yet. Run skald project create "Name" in a repository, or ask Claude Code to plan one with create_project.'));
    return root;
  }
  const list = h('ul', { class: 'rows', style: { marginTop: '20px' } });
  for (const o of projects) {
    const c = o.counts;
    const countable = c.total - c.cancelled;
    const flags = [];
    if (c.blocked) flags.push(h('span', { class: 'chip' }, mk('blocked'), `${c.blocked} blocked`));
    if (c.in_progress) flags.push(h('span', { class: 'chip' }, mk('in_progress'), `${c.in_progress} in progress`));
    list.append(h('li', { class: 'row row-project' }, h('a', { href: `#/p/${encodeURIComponent(o.project.slug)}`, 'data-key': `p-${o.project.slug}` },
      h('div', null, h('div', { class: 'name' }, o.project.name),
        h('div', { class: 'meta' }, countable ? `${c.done} of ${plural(countable, 'task')} done` : 'No tasks yet')),
      progressBar(o.progress.fraction, `${o.project.name} progress`),
      h('div', { class: 'pct' }, `${pct(o.progress.fraction)}%`),
      h('div', { class: 'flags', style: { gridColumn: '1 / -1', textAlign: 'left', display: 'flex', gap: '16px' } }, flags))));
  }
  root.append(list);
  return root;
}

// ---- project overview ------------------------------------------------------

function taskStrip(slug, features, tasks) {
  const byFeature = new Map(features.map((f) => [f.id, []]));
  for (const t of tasks) (byFeature.get(t.feature) || byFeature.set(t.feature, []).get(t.feature)).push(t);
  const groups = [];
  for (const [fid, ts] of byFeature) {
    if (!ts.length) continue;
    ts.sort((a, b) => a.n - b.n);
    const f = features.find((x) => x.id === fid);
    groups.push(h('div', { class: 'strip-group', role: 'group', 'aria-label': f ? f.title : fid },
      ts.map((t) => h('a', {
        class: `tick ${t.status}`, href: taskHref(slug, t.id), tabindex: '-1',
        'aria-label': `${t.id} ${t.title}, ${STATUS[t.status]}`, 'data-tip': tipFor(t),
      }))));
  }
  return h('div', { class: 'strip', role: 'group', 'aria-label': 'Every task by status, grouped by feature. The board below lists the same tasks.' }, groups);
}

function legend(c) {
  const item = (status, n) => h('span', null, h('i', { class: `tick ${status}` }), `${STATUS[status]} `, h('b', null, String(n)));
  const out = [item('done', c.done), item('in_progress', c.in_progress), item('blocked', c.blocked), item('todo', c.todo)];
  if (c.cancelled) out.push(item('cancelled', c.cancelled));
  return h('div', { class: 'legend' }, out);
}

function featureRows(features, st, rerender) {
  if (!features.length) return h('p', { class: 'empty' }, 'No features yet. A plan from Claude Code (create_plan) or skald plan import adds them.');
  return h('ul', { class: 'rows' }, features.map((f) => {
    const countable = f.counts.total - f.counts.cancelled;
    const on = st.filters.feature === f.id;
    return h('li', { class: 'row row-feature', 'aria-current': on ? 'true' : null },
      h('button', {
        type: 'button', 'aria-pressed': on ? 'true' : 'false', 'data-key': `f-${f.id}`,
        title: on ? 'Show tasks from every feature' : 'Show only this feature on the board',
        onclick: () => { st.filters.feature = on ? '' : f.id; rerender(); },
      },
      h('div', null, h('div', { class: 'name' }, f.title),
        h('div', { class: 'meta' }, h('span', { class: 'chip' }, mk(FEATURE_MARK[f.status] || 'todo'), FEATURE_LABEL[f.status] || f.status),
          `, ${f.counts.done} of ${plural(countable, 'task')} done`)),
      progressBar(f.progress.fraction, `${f.title} progress`),
      h('div', { class: 'pct' }, f.counted || f.counts.total ? `${pct(f.progress.fraction)}%` : '0%')));
  }));
}

function taskLink(slug, t, { showFeature, features }) {
  const notes = [];
  if (t.status === 'blocked' && t.blocked_reason) notes.push(h('span', { class: 'note warn' }, `Blocked: ${t.blocked_reason}`));
  if (t.status === 'in_progress') {
    notes.push(h('span', { class: 'note' }, t.assignee ? `Started by ${actorName(t.assignee)}` : 'In progress'));
    if (t.stale) notes.push(h('span', { class: 'note warn' }, 'Stale: no update for 4 hours or more'));
  }
  const tags = [];
  if (t.priority === 0) tags.push(h('span', { class: 'tag urgent' }, 'urgent'));
  else if (t.priority === 1) tags.push(h('span', { class: 'tag' }, 'high'));
  if (t.ready) tags.push(h('span', { class: 'tag' }, 'ready'));
  const f = showFeature && features.find((x) => x.id === t.feature);
  return h('a', { class: `t is-${t.status}`, href: taskHref(slug, t.id), 'data-key': `t-${t.id}` },
    h('span', { class: 'id' }, t.id), ' ', tags,
    h('span', { class: 'title' }, t.title), notes,
    f ? h('span', { class: 'note' }, f.title) : null);
}

function board(slug, features, tasks, st, rerender) {
  const filter = st.filters.feature;
  const visible = tasks.filter((t) => (!filter || t.feature === filter));
  const cancelled = visible.filter((t) => t.status === 'cancelled');
  const cols = [
    ['todo', visible.filter((t) => t.status === 'todo')],
    ['in_progress', visible.filter((t) => t.status === 'in_progress')],
    ['blocked', visible.filter((t) => t.status === 'blocked')],
    ['done', visible.filter((t) => t.status === 'done').sort((a, b) => (b.completed_at || '').localeCompare(a.completed_at || ''))],
  ];
  if (st.filters.showCancelled) cols.push(['cancelled', cancelled]);
  const fobj = features.find((f) => f.id === filter);
  const tools = h('div', { class: 'board-tools' },
    fobj ? h('span', null, 'Feature: ', h('b', null, fobj.title), ' ',
      h('button', { type: 'button', class: 'linkbtn', 'data-key': 'clear-feature', onclick: () => { st.filters.feature = ''; rerender(); } }, 'Show all features')) : null,
    cancelled.length || st.filters.showCancelled ? h('label', null,
      h('input', { type: 'checkbox', checked: st.filters.showCancelled ? true : null, 'data-key': 'show-cancelled',
        onchange: (e) => { st.filters.showCancelled = e.target.checked; rerender(); } }),
      ` Show cancelled (${cancelled.length})`) : null);
  const colEls = cols.map(([status, ts]) => {
    let shown = ts;
    let more = null;
    if (status === 'done' && !st.filters.showAllDone && ts.length > 8) {
      shown = ts.slice(0, 8);
      more = h('li', null, h('button', { type: 'button', class: 'linkbtn empty', 'data-key': 'show-all-done', onclick: () => { st.filters.showAllDone = true; rerender(); } }, `Show ${ts.length - 8} older`));
    }
    return h('section', { class: `col c-${status}`, 'aria-label': STATUS[status] },
      h('h3', null, mk(status), STATUS[status], h('span', { class: 'count' }, String(ts.length))),
      ts.length ? h('ul', null, shown.map((t) => h('li', null, taskLink(slug, t, { showFeature: !filter, features }))), more)
        : h('p', { class: 'empty' }, { todo: 'Nothing waiting.', in_progress: 'Nothing in progress.', blocked: 'Nothing blocked.', done: 'Nothing done yet.', cancelled: 'None.' }[status]));
  });
  return h('div', null, tools, h('div', { class: 'board' }, colEls));
}

function readyNext(slug, tasks) {
  const ready = tasks.filter((t) => t.ready);
  if (!ready.length) return h('p', { class: 'empty' }, 'Nothing is ready. Tasks become ready when everything they depend on is done.');
  const shown = ready.slice(0, 8);
  return h('div', null, h('ul', { class: 'next' }, shown.map((t) => h('li', null, taskLink(slug, t, { showFeature: false, features: [] })))),
    ready.length > shown.length ? h('p', { class: 'empty' }, `and ${ready.length - shown.length} more ready`) : null);
}

export function sagaList(events, st, rerender, { showProject = false, limit = 25 } = {}) {
  const actors = [...new Set(events.map((e) => e.actor))].sort();
  if (st.filters.actor && !actors.includes(st.filters.actor)) actors.push(st.filters.actor);
  const select = h('select', {
    id: 'actor-filter', 'data-key': 'actor-filter',
    onchange: (e) => { st.filters.actor = e.target.value; rerender(); },
  }, h('option', { value: '' }, 'Everyone'), actors.map((a) => h('option', { value: a, selected: a === st.filters.actor ? true : null }, actorName(a))));
  // Dependency edits are noise in a feed (a plan import adds dozens); they
  // still show in the task's own history.
  const shown = events
    .filter((e) => !e.type.startsWith('task.dependency_') && (!st.filters.actor || e.actor === st.filters.actor))
    .slice(0, limit);
  return h('div', null,
    h('div', { class: 'saga-tools' }, h('label', { for: 'actor-filter' }, 'Show activity by'), select),
    shown.length
      ? h('ol', { class: 'saga', style: { listStyle: 'none' } }, shown.map((ev) => h('li', { class: ev._fresh ? 'fresh' : null, 'data-id': String(ev.id) }, eventLine(ev, { showProject }))))
      : h('p', { class: 'empty' }, events.length ? 'No activity from that actor.' : 'Nothing has happened yet. Activity appears here as Claude plans and works.'));
}

export function projectView(d, st, rerender) {
  const { summary, board: b, feed } = d;
  const p = summary.project;
  const c = summary.counts;
  const countable = c.total - c.cancelled;
  const parts = [];
  parts.push(countable ? `${c.done} of ${plural(countable, 'task')} done` : 'No tasks yet');
  if (c.in_progress) parts.push(`${c.in_progress} in progress`);
  if (c.blocked) parts.push(`${c.blocked} blocked`);
  if (summary.ready_count) parts.push(`${summary.ready_count} ready to start`);
  const docs = summary.documents || [];
  return h('div', null,
    h('p', { class: 'crumb' }, h('a', { href: '#/' }, 'All projects')),
    h('div', { class: 'head' },
      h('div', null, h('h1', null, p.name), h('p', { class: 'sub' }, p.description ? `${p.description}. ` : '', parts.join(', '), '.')),
      h('div', { class: 'figure', title: 'Weighted by feature and task weights, so it can differ from the plain task count.' },
        h('span', { class: 'n' }, String(pct(summary.progress.fraction))), h('span', { class: 'u' }, '%'))),
    h('div', { class: 'strip-wrap' }, taskStrip(p.slug, summary.features, b.tasks), legend(c),
      b.capped ? h('p', { class: 'empty' }, 'This project has more than 2000 tasks; the strip and board show the first 2000.') : null),
    h('div', { class: 'layout' },
      h('div', null,
        h('section', { class: 'section' }, h('h2', null, 'Features', h('span', { class: 'count' }, 'Select one to filter the board')), featureRows(summary.features, st, rerender)),
        h('section', { class: 'section' }, h('h2', null, 'Board'), board(p.slug, summary.features, b.tasks, st, rerender))),
      h('aside', { 'aria-label': 'Ready tasks and activity' },
        h('section', { class: 'section' }, h('h2', null, 'Ready next'), readyNext(p.slug, b.tasks)),
        h('section', { class: 'section' }, h('h2', null, 'Activity'), sagaList(feed, st, rerender)),
        docs.length ? h('section', { class: 'section' }, h('h2', null, 'Documents'),
          h('ul', { class: 'links' }, docs.map((x) => h('li', null, h('a', { href: `#/p/${encodeURIComponent(p.slug)}/doc/${encodeURIComponent(x.kind)}/${encodeURIComponent(x.title)}` },
            h('span', null, x.title), h('span', { class: 'id' }, `${x.kind}, v${x.version}`)))))) : null)));
}

// ---- task detail -----------------------------------------------------------

const linkList = (slug, items, empty) => items.length
  ? h('ul', { class: 'links' }, items.map((t) => h('li', null, h('a', { href: taskHref(slug, t.id) }, mk(t.status), h('span', { class: 'id' }, t.id), h('span', null, t.title)))))
  : h('p', { class: 'empty' }, empty);

export function taskView(d, st, act) {
  const t = d.task;
  const slug = d.project;
  const fact = (k, v) => (v ? [h('dt', null, k), h('dd', null, v)] : []);
  const buttons = [];
  if (t.status === 'blocked') buttons.push(h('button', { type: 'button', class: 'btn primary', 'data-key': 'act-unblock', disabled: st.busy ? true : null, onclick: () => act('unblock', t) }, 'Unblock'));
  if (t.status === 'done' || t.status === 'cancelled') buttons.push(h('button', { type: 'button', class: 'btn', 'data-key': 'act-reopen', disabled: st.busy ? true : null, onclick: () => act('reopen', t) }, 'Reopen'));
  if (t.status !== 'done' && t.status !== 'cancelled') {
    buttons.push(st.confirmCancel
      ? h('span', { class: 'actions', style: { margin: 0 } },
        h('button', { type: 'button', class: 'btn danger', 'data-key': 'act-cancel-yes', disabled: st.busy ? true : null, onclick: () => act('cancel', t) }, 'Cancel this task'),
        h('button', { type: 'button', class: 'btn', 'data-key': 'act-cancel-no', onclick: () => act('cancel-no', t) }, 'Keep it'))
      : h('button', { type: 'button', class: 'btn', 'data-key': 'act-cancel', onclick: () => act('cancel-ask', t) }, 'Cancel task'));
  }
  const unmet = d.unmet.length ? h('div', { class: 'callout info' }, `Waiting for ${d.unmet.join(', ')} to finish before it can start.`) : null;
  return h('div', null,
    h('p', { class: 'crumb' }, h('a', { href: `#/p/${encodeURIComponent(slug)}` }, slug), ' / ', t.id),
    h('div', { class: 'detail' },
      h('div', null,
        h('span', { class: 'chip' }, mk(t.status), STATUS[t.status], t.stale ? ', stale' : '', t.ready ? ', ready to start' : ''),
        h('h1', null, t.title),
        t.status === 'blocked' && t.blocked_reason ? h('div', { class: 'callout' }, h('b', null, 'Blocked. '), t.blocked_reason) : null,
        unmet,
        t.description ? h('div', { class: 'section' }, h('h2', null, 'Description'), h('p', { class: 'prose' }, t.description)) : null,
        t.acceptance_criteria ? h('div', { class: 'section' }, h('h2', null, 'Acceptance criteria'), h('p', { class: 'prose' }, t.acceptance_criteria)) : null,
        h('div', { class: 'section' }, h('h2', null, 'Evidence'),
          d.artifacts.length ? h('ul', { class: 'links evidence' }, d.artifacts.map((a) => h('li', null,
            h('span', { class: 'kind' }, a.kind), h('span', { class: 'mono' }, a.ref), a.summary ? h('div', { class: 'muted' }, a.summary) : null,
            h('div', { class: 'muted', style: { fontSize: '13px' } }, `${actorName(a.actor)}, `, timeEl(a.created_at)))))
            : h('p', { class: 'empty' }, 'No evidence recorded yet. A task needs a commit, PR, test report, link or manual note before it can be completed.')),
        h('div', { class: 'section' }, h('h2', null, 'History'),
          d.events.length ? h('ol', { class: 'saga', style: { listStyle: 'none' } }, d.events.map((ev) => h('li', { class: ev._fresh ? 'fresh' : null }, eventLine({ ...ev, task: '' })))) : h('p', { class: 'empty' }, 'No history yet.'))),
      h('aside', { 'aria-label': 'Task details' },
        h('dl', { class: 'facts' },
          h('dt', null, 'Feature'), h('dd', null, d.feature.title),
          h('dt', null, 'Priority'), h('dd', null, PRIORITY[t.priority] || String(t.priority)),
          h('dt', null, 'Weight'), h('dd', null, String(t.weight)),
          fact('Started by', t.assignee ? actorName(t.assignee) : ''), fact('Started', fmtDate(t.started_at)),
          fact('Completed', fmtDate(t.completed_at)), fact('Updated', fmtDate(t.updated_at)),
          h('dt', null, 'Version'), h('dd', null, String(t.version))),
        buttons.length ? h('div', { class: 'actions' }, buttons) : null,
        h('div', { class: 'section' }, h('h2', null, 'Depends on'), linkList(slug, d.dependencies, 'Nothing. It can start any time.')),
        h('div', { class: 'section' }, h('h2', null, 'Unlocks'), linkList(slug, d.dependents, 'No task is waiting for this one.')))));
}

export function docView(d, slug) {
  return h('div', null,
    h('p', { class: 'crumb' }, h('a', { href: `#/p/${encodeURIComponent(slug)}` }, slug), ' / documents'),
    h('h1', null, d.document.title),
    h('p', { class: 'muted', style: { margin: '6px 0 18px' } }, `${d.document.kind}, version ${d.version}, saved ${fmtDate(d.created_at)} by ${actorName(d.actor)}`),
    h('p', { class: 'prose' }, d.body));
}

// ---- activity --------------------------------------------------------------

export function activityView({ feed }, st, rerender) {
  return h('div', null,
    h('div', { class: 'head', style: { paddingTop: '14px' } }, h('h1', null, 'Activity')),
    h('div', { class: 'section', style: { maxWidth: '760px', marginTop: '20px' } }, sagaList(feed, st, rerender, { showProject: true, limit: 100 })));
}
