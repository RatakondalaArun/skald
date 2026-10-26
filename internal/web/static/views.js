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
  return h('div', { class: 'page' }, h('div', { class: 'error-box' }, h('h1', null, title), h('p', null, message),
    h('p', { style: { marginTop: '14px' } }, h('a', { href: backHref }, backText))));
}

// ---- projects ----------------------------------------------------------------

export function projectsView({ projects }) {
  const root = h('div', { class: 'page' }, h('div', { class: 'page-head' }, h('h1', null, 'Projects')));
  if (!projects.length) {
    root.append(h('p', { class: 'empty' },
      'No projects yet. Run skald project create "Name" in a repository, or ask Claude Code to plan one with create_project.'));
    return root;
  }
  const list = h('ul', { class: 'cards projects-grid' });
  for (const o of projects) {
    const c = o.counts;
    const countable = c.total - c.cancelled;
    const flags = [];
    if (c.blocked) flags.push(h('span', { class: 'chip' }, mk('blocked'), `${c.blocked} blocked`));
    if (c.in_progress) flags.push(h('span', { class: 'chip' }, mk('in_progress'), `${c.in_progress} in progress`));
    list.append(h('li', null, h('a', { class: 'card', href: `#/p/${encodeURIComponent(o.project.slug)}`, 'data-key': `p-${o.project.slug}` },
      h('div', { class: 'name' }, o.project.name),
      h('div', { class: 'meta' }, countable ? `${c.done} of ${plural(countable, 'task')} done` : 'No tasks yet'),
      progressBar(o.progress.fraction, `${o.project.name} progress`),
      h('div', { class: 'foot' }, h('div', { class: 'flags' }, flags.length ? flags : h('span', { class: 'muted' }, 'Nothing in flight')),
        h('span', { class: 'pct' }, `${pct(o.progress.fraction)}%`)))));
  }
  root.append(list);
  return root;
}

// ---- rail ------------------------------------------------------------------

export function railProjects(projects, activeSlug) {
  if (!projects.length) return [];
  return [h('div', { class: 'rail-head' }, 'Projects'), ...projects.map((o) => {
    const on = o.project.slug === activeSlug;
    return h('a', { href: `#/p/${encodeURIComponent(o.project.slug)}`, 'aria-current': on ? 'page' : null },
      h('div', { class: 'rp-name' }, h('b', null, o.project.name),
        h('span', { class: 'rp-pct' }, o.counts.blocked ? mk('blocked') : null, `${pct(o.progress.fraction)}%`)),
      h('div', { class: 'rp-bar' }, h('i', { style: { width: `${pct(o.progress.fraction)}%` } })));
  })];
}

// ---- project page ----------------------------------------------------------

// Past this many tasks one tick each stops being legible (and would wrap into
// dozens of rows), so show one proportional bar per status instead.
const STRIP_MAX = 150;

function statusBar(tasks) {
  const n = {};
  for (const t of tasks) n[t.status] = (n[t.status] || 0) + 1;
  const segs = ['done', 'in_progress', 'blocked', 'todo', 'cancelled'].filter((s) => n[s]).map((s) => h('span', {
    class: `seg ${s}`, style: { flexGrow: String(n[s]) }, 'data-tip': `${STATUS[s]}\t${n[s]} of ${tasks.length} tasks\t`,
  }));
  return h('div', { class: 'statusbar', role: 'img', 'aria-label': ['done', 'in_progress', 'blocked', 'todo', 'cancelled'].filter((s) => n[s]).map((s) => `${n[s]} ${STATUS[s]}`).join(', ') }, segs);
}

function taskStrip(slug, features, tasks) {
  if (tasks.length > STRIP_MAX) return statusBar(tasks);
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
  return h('div', { class: 'strip', role: 'group', 'aria-label': 'Every task by status, grouped by feature. The board lists the same tasks.' }, groups);
}

function legend(c) {
  const item = (status, n) => h('span', null, h('i', { class: `tick ${status}` }), `${STATUS[status]} `, h('b', null, String(n)));
  const out = [item('done', c.done), item('in_progress', c.in_progress), item('blocked', c.blocked), item('todo', c.todo)];
  if (c.cancelled) out.push(item('cancelled', c.cancelled));
  return h('div', { class: 'legend' }, out);
}

function featureList(slug, features, st, rerender) {
  if (!features.length) return h('p', { class: 'empty' }, 'No features yet. A plan from Claude Code (create_plan) or skald plan import adds them.');
  return h('ul', { class: 'cards feature-list' }, features.map((f) => {
    const countable = f.counts.total - f.counts.cancelled;
    const on = st.filters.feature === f.id;
    return h('li', null, h('button', {
      type: 'button', class: 'card', 'aria-pressed': on ? 'true' : 'false', 'data-key': `f-${f.id}`,
      title: 'Show this feature on the board',
      onclick: () => { st.filters.feature = f.id; location.hash = `#/p/${encodeURIComponent(slug)}`; },
    }, h('div', { class: 'card-row' },
      h('div', null, h('div', { class: 'name' }, f.title),
        h('div', { class: 'meta' }, h('span', { class: 'chip' }, mk(FEATURE_MARK[f.status] || 'todo'), FEATURE_LABEL[f.status] || f.status),
          `, ${f.counts.done} of ${plural(countable, 'task')} done`)),
      progressBar(f.progress.fraction, `${f.title} progress`),
      h('div', { class: 'pct' }, `${pct(f.progress.fraction)}%`))),
    f.github_url ? h('a', { class: 'gh-link', href: f.github_url, target: '_blank', rel: 'noopener noreferrer' }, `GitHub milestone #${f.github_milestone}`) : null);
  }));
}

function taskCard(slug, t, { showFeature, features, open }) {
  const notes = [];
  if (t.status === 'blocked' && t.blocked_reason) notes.push(h('span', { class: 'note warn' }, `Blocked: ${t.blocked_reason}`));
  if (t.status === 'in_progress') {
    notes.push(h('span', { class: 'note' }, t.assignee ? `Started by ${actorName(t.assignee)}` : 'In progress'));
    if (t.stale) notes.push(h('span', { class: 'note warn' }, 'Stale: no update for 4 hours or more'));
  }
  const tags = [];
  if (t.priority === 0) tags.push(h('span', { class: 'tag urgent' }, 'urgent'));
  else if (t.priority === 1) tags.push(h('span', { class: 'tag' }, 'high'));
  if (t.ready) tags.push(h('span', { class: 'tag ready' }, 'ready'));
  if (t.github_issue) tags.push(h('span', { class: 'tag gh', title: `GitHub issue #${t.github_issue}` }, `#${t.github_issue}`));
  const labels = (t.tags || []).length ? h('span', { class: 'labels' }, t.tags.map((x) => h('span', { class: 'label' }, x))) : null;
  const f = showFeature && features.find((x) => x.id === t.feature);
  return h('a', { class: `t is-${t.status}${open ? ' is-open' : ''}`, href: taskHref(slug, t.id), 'data-key': `t-${t.id}`, 'aria-current': open ? 'true' : null },
    h('span', { class: 'id' }, t.id), ' ', tags,
    h('span', { class: 'title' }, t.title), labels, notes,
    f ? h('span', { class: 'note' }, f.title) : null);
}

function board(slug, features, tasks, st, rerender, openId) {
  const f = st.filters;
  const q = f.query.trim().toLowerCase();
  const visible = tasks.filter((t) => (!f.feature || t.feature === f.feature)
    && (!f.ready || t.ready)
    && (!f.tag || (t.tags || []).includes(f.tag))
    && (!q || `${t.id} ${t.title} ${(t.tags || []).join(' ')}`.toLowerCase().includes(q)));
  const cancelled = visible.filter((t) => t.status === 'cancelled');
  const cols = [
    ['todo', visible.filter((t) => t.status === 'todo')],
    ['in_progress', visible.filter((t) => t.status === 'in_progress')],
    ['blocked', visible.filter((t) => t.status === 'blocked')],
    ['done', visible.filter((t) => t.status === 'done').sort((a, b) => (b.completed_at || '').localeCompare(a.completed_at || ''))],
  ];
  if (f.showCancelled) cols.push(['cancelled', cancelled]);

  const search = h('div', { class: 'search' },
    h('input', {
      type: 'search', id: 'board-search', placeholder: 'Filter tasks', 'aria-label': 'Filter tasks by id, title or tag',
      'data-key': 'board-search', value: f.query, autocomplete: 'off',
      oninput: (e) => {
        f.query = e.target.value;
        const pos = e.target.selectionStart;
        rerender();
        const i = document.getElementById('board-search');
        i?.focus(); i?.setSelectionRange(pos, pos);
      },
    }), h('kbd', null, '/'));
  const featureSel = features.length > 1 ? h('select', {
    'aria-label': 'Feature', 'data-key': 'feature-filter',
    onchange: (e) => { f.feature = e.target.value; rerender(); },
  }, h('option', { value: '' }, 'All features'), features.map((x) => h('option', { value: x.id, selected: x.id === f.feature ? true : null }, x.title))) : null;
  const allTags = [...new Set(tasks.flatMap((t) => t.tags || []))].sort();
  if (f.tag && !allTags.includes(f.tag)) allTags.push(f.tag);
  const tagSel = allTags.length ? h('select', {
    'aria-label': 'Tag', 'data-key': 'tag-filter',
    onchange: (e) => { f.tag = e.target.value; rerender(); },
  }, h('option', { value: '' }, 'All tags'), allTags.map((x) => h('option', { value: x, selected: x === f.tag ? true : null }, x))) : null;
  const readyPill = h('button', { type: 'button', class: 'pill', 'aria-pressed': f.ready ? 'true' : 'false', 'data-key': 'ready-only', title: 'Only tasks whose dependencies are all done',
    onclick: () => { f.ready = !f.ready; rerender(); } }, 'Ready to start');
  const cancelledBox = cancelled.length || f.showCancelled ? h('label', null,
    h('input', { type: 'checkbox', checked: f.showCancelled ? true : null, 'data-key': 'show-cancelled',
      onchange: (e) => { f.showCancelled = e.target.checked; rerender(); } }),
    `Cancelled (${cancelled.length})`) : null;
  const filtered = f.feature || f.tag || f.ready || q;
  const clear = filtered ? h('button', { type: 'button', class: 'linkbtn', 'data-key': 'clear-filters',
    onclick: () => { Object.assign(f, { feature: '', tag: '', ready: false, query: '' }); rerender(); } }, 'Clear filters') : null;

  const empty = { todo: 'Nothing waiting.', in_progress: 'Nothing in progress.', blocked: 'Nothing blocked.', done: 'Nothing done yet.', cancelled: 'None.' };
  const colEls = cols.map(([status, ts]) => {
    let shown = ts;
    let more = null;
    if (status === 'done' && !f.showAllDone && ts.length > 30) {
      shown = ts.slice(0, 30);
      more = h('li', null, h('button', { type: 'button', class: 'linkbtn empty', 'data-key': 'show-all-done', onclick: () => { f.showAllDone = true; rerender(); } }, `Show ${ts.length - 30} older`));
    }
    return h('section', { class: `col c-${status}`, 'aria-label': STATUS[status] },
      h('h3', null, mk(status), STATUS[status], h('span', { class: 'count' }, String(ts.length))),
      ts.length
        ? h('ul', { class: 'col-list', 'data-scroll': `col-${status}`, tabindex: '0', 'aria-label': `${STATUS[status]} tasks` },
          shown.map((t) => h('li', null, taskCard(slug, t, { showFeature: !f.feature, features, open: t.id === openId }))), more)
        : h('p', { class: 'empty' }, filtered ? 'No matching tasks.' : empty[status]));
  });
  return [h('div', { class: 'toolbar' }, search, featureSel, tagSel, readyPill, cancelledBox, clear), h('div', { class: 'board' }, colEls)];
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

const TABS = [['board', 'Board'], ['features', 'Features'], ['activity', 'Activity'], ['docs', 'Documents']];

// githubAuthLine shows who a sync signs in as, the code of a sign-in waiting
// for approval, or a sign-in button.
function githubAuthLine(gh, st, act) {
  if (!gh) return null;
  const btn = (key, label, title) => h('button', { type: 'button', class: 'linkbtn', 'data-key': key, disabled: st.busy ? true : null, title, onclick: () => act(key) }, label);
  if (gh.pending) {
    return h('span', { class: 'gh-auth' }, 'Enter ', h('code', { class: 'gh-code' }, gh.pending.user_code), ' at ',
      h('a', { href: gh.pending.verification_uri, target: '_blank', rel: 'noopener noreferrer' }, gh.pending.verification_uri.replace(/^https?:\/\//, '')),
      ' to sign in. ', btn('github-logout', 'Cancel', 'Stop this sign-in'));
  }
  if (gh.signed_in) {
    const who = gh.login ? `Signed in as ${gh.login}` : `Using the token from ${gh.source}`;
    return h('span', { class: 'gh-auth' }, `${who}. `, gh.source === 'skald github login' ? btn('github-logout', 'Sign out', 'Forget the saved GitHub token') : null);
  }
  if (!gh.configured) return h('span', { class: 'gh-auth' }, 'No GitHub token: run skald github login or set GITHUB_TOKEN.');
  return h('span', { class: 'gh-auth' }, btn('github-login', 'Sign in to GitHub', 'Sign in with a one-time code on github.com'));
}

export function projectView(d, st, rerender, route, act) {
  const { summary, board: b, feed } = d;
  const p = summary.project;
  const c = summary.counts;
  const countable = c.total - c.cancelled;
  const docs = summary.documents || [];
  const view = route.view || 'board';
  const parts = [];
  parts.push(countable ? `${c.done} of ${plural(countable, 'task')} done` : 'No tasks yet');
  if (summary.ready_count) parts.push(`${summary.ready_count} ready to start`);
  const counts = { board: countable, features: summary.features.length, activity: null, docs: docs.length || null };
  const slugEnc = encodeURIComponent(p.slug);

  let body;
  let fill = false;
  if (view === 'features') body = h('div', null, featureList(p.slug, summary.features, st, rerender));
  else if (view === 'activity') body = h('div', { class: 'narrow' }, sagaList(feed, st, rerender, { limit: 100 }));
  else if (view === 'docs') {
    body = docs.length
      ? h('div', { class: 'narrow' }, h('ul', { class: 'links' }, docs.map((x) => h('li', null, h('a', { href: `#/p/${slugEnc}/doc/${encodeURIComponent(x.kind)}/${encodeURIComponent(x.title)}` },
        h('span', null, x.title), h('span', { class: 'id' }, `${x.kind}, v${x.version}`))))))
      : h('p', { class: 'empty' }, 'No documents yet. Claude saves plans and notes here with save_document.');
  } else {
    fill = true;
    body = board(p.slug, summary.features, b.tasks, st, rerender, route.name === 'task' ? d.taskData?.task.id : null);
  }

  return h('div', { class: 'proj' },
    h('header', { class: 'proj-head' },
      h('div', { class: 'proj-title' },
        h('div', null, h('h1', null, p.name), h('p', { class: 'sub' }, p.description ? `${p.description}. ` : '', parts.join(', '), '.'),
          p.github ? h('p', { class: 'sub' }, 'Linked to ', h('a', { href: `https://github.com/${p.github}`, target: '_blank', rel: 'noopener noreferrer' }, p.github), ' ',
            h('button', { type: 'button', class: 'btn', 'data-key': 'github-sync', disabled: st.busy ? true : null, title: 'Pull milestones, issues, pull requests and discussions', onclick: () => act('github-sync') }, 'Sync from GitHub'),
            githubAuthLine(st.gh, st, act)) : null),
        h('div', { class: 'figure', title: 'Weighted by feature and task weights, so it can differ from the plain task count.' },
          h('span', { class: 'n' }, String(pct(summary.progress.fraction))), h('span', { class: 'u' }, '%'))),
      taskStrip(p.slug, summary.features, b.tasks), legend(c),
      b.capped ? h('p', { class: 'muted' }, 'This project has more than 2000 tasks; the strip and board show the first 2000.') : null),
    h('nav', { class: 'tabs', 'aria-label': 'Project views' }, TABS.filter(([k]) => k !== 'docs' || docs.length || view === 'docs').map(([k, label]) =>
      h('a', { href: k === 'board' ? `#/p/${slugEnc}` : `#/p/${slugEnc}/${k}`, 'aria-current': view === k ? 'page' : null },
        label, counts[k] != null ? h('span', { class: 'count' }, String(counts[k])) : null))),
    h('div', { class: `tab-body${fill ? ' fill' : ''}`, 'data-scroll': 'tab' }, body),
    route.name === 'task' && d.taskData ? taskDrawer(d.taskData, st, act, b.tasks) : null);
}

// ---- task drawer -----------------------------------------------------------

const linkList = (slug, items, empty, onRemove) => items.length
  ? h('ul', { class: 'links' }, items.map((t) => h('li', { class: onRemove ? 'removable' : null },
    h('a', { href: taskHref(slug, t.id) }, mk(t.status), h('span', { class: 'id' }, t.id), h('span', null, t.title)),
    onRemove ? h('button', { type: 'button', class: 'linkbtn rm', 'data-key': `rm-${t.id}`, title: `Stop waiting for ${t.id}`, 'aria-label': `Remove dependency on ${t.id}`, onclick: () => onRemove(t) }, 'Remove') : null)))
  : h('p', { class: 'empty' }, empty);

function copyText(text, btn) {
  const done = () => { const old = btn.textContent; btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = old; }, 1200); };
  navigator.clipboard?.writeText(text).then(done, () => {});
}

const KINDS = [['commit', 'Commit'], ['pr', 'Pull request'], ['test_report', 'Test report'], ['link', 'Link'], ['manual', 'Manual check']];

function taskDrawer(d, st, act, allTasks = []) {
  const t = d.task;
  const slug = d.project;
  const close = `#/p/${encodeURIComponent(slug)}`;
  const fact = (k, v) => (v ? [h('dt', null, k), h('dd', null, v)] : []);
  const dis = st.busy ? true : null;
  const btn = (key, label, cls, onclick, extra = {}) => h('button', { type: 'button', class: `btn ${cls}`.trim(), 'data-key': `act-${key}`, disabled: dis, onclick, ...extra }, label);
  const open = (name) => () => act('form', t, name);
  const waiting = d.unmet.length > 0;
  const buttons = [];
  if (t.status === 'todo') buttons.push(btn('start', 'Start task', 'primary', () => act('start', t),
    waiting ? { disabled: true, title: `Waiting for ${d.unmet.join(', ')}` } : {}));
  if (t.status === 'in_progress') buttons.push(btn('done', 'Mark done', 'primary', open('done')));
  if (t.status === 'todo' || t.status === 'in_progress') buttons.push(btn('block', 'Block', '', open('block')));
  if (t.status === 'blocked') buttons.push(btn('unblock', 'Unblock', 'primary', () => act('unblock', t)));
  if (t.status === 'done' || t.status === 'cancelled') buttons.push(btn('reopen', 'Reopen', '', () => act('reopen', t)));
  if (t.status !== 'done' && t.status !== 'cancelled') {
    buttons.push(st.confirmCancel
      ? [btn('cancel-yes', 'Cancel this task', 'danger', () => act('cancel', t)), btn('cancel-no', 'Keep it', '', () => act('cancel-no', t))]
      : btn('cancel', 'Cancel task', '', () => act('cancel-ask', t)));
  }
  const more = [btn('edit', 'Edit', '', open('edit')), btn('note', 'Add note', '', open('note'))];
  if (t.status !== 'cancelled') more.push(btn('evidence', 'Add evidence', '', open('evidence')));

  const unremovable = new Set([t.id, ...d.dependencies.map((x) => x.id)]);
  const depChoices = allTasks.filter((x) => !unremovable.has(x.id) && x.status !== 'cancelled');
  // Inline forms. Drafts live in st.draft so a live refresh does not erase typing.
  const dr = st.draft;
  const text = (key, label, props = {}) => h('label', { class: 'field' }, label,
    h('textarea', { rows: '3', 'data-key': `f-${key}`, oninput: (e) => { dr[key] = e.target.value; }, ...props }, dr[key] || ''));
  const line = (key, label, props = {}) => h('label', { class: 'field' }, label,
    h('input', { type: 'text', value: dr[key] || '', 'data-key': `f-${key}`, autocomplete: 'off', oninput: (e) => { dr[key] = e.target.value; }, ...props }));
  const form = (title, fields, submitLabel, submit, hint) => h('form', { class: 'formbox', onsubmit: (e) => { e.preventDefault(); submit(); } },
    h('h2', null, title), hint ? h('p', { class: 'hint' }, hint) : null, fields,
    h('div', { class: 'actions' },
      h('button', { type: 'submit', class: 'btn primary', 'data-key': 'f-submit', disabled: dis }, submitLabel),
      h('button', { type: 'button', class: 'btn', 'data-key': 'f-cancel', onclick: open(null) }, 'Back')));
  let formEl = null;
  if (st.form === 'block') formEl = form('Block this task', text('reason', 'What is it waiting for?', { required: true }), 'Block task', () => act('block', t, { reason: dr.reason || '' }));
  else if (st.form === 'done') {
    formEl = form('Mark this task done', [text('note', 'Note (optional)'),
      d.artifacts.length ? null : h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: dr.force ? true : null, 'data-key': 'f-force', onchange: (e) => { dr.force = e.target.checked; } }), 'Skip the evidence check (recorded as forced)')],
    'Mark done', () => act('done', t, { note: dr.note || '', force: !!dr.force }),
      d.artifacts.length ? `${plural(d.artifacts.length, 'piece')} of evidence recorded.` : 'No evidence yet. If this project requires it, add some first.');
  } else if (st.form === 'note') formEl = form('Add a note', text('text', 'Note', { required: true }), 'Add note', () => act('note', t, { text: dr.text || '' }));
  else if (st.form === 'edit') {
    formEl = form('Edit task', [
      line('title', 'Title', { required: true }),
      text('description', 'Description'),
      text('criteria', 'Acceptance criteria'),
      h('div', { class: 'pair' },
        h('label', { class: 'field' }, 'Priority', h('select', { 'data-key': 'f-priority', onchange: (e) => { dr.priority = e.target.value; } },
          PRIORITY.map((name, i) => h('option', { value: String(i), selected: String(dr.priority) === String(i) ? true : null }, `${i} ${name}`)))),
        line('weight', 'Weight (1 to 10)', { type: 'number', min: '1', max: '10' })),
      line('tags', 'Tags (comma separated)', { placeholder: 'backend, ui' })],
    'Save changes', () => act('edit', t, {
      title: dr.title || '', description: dr.description || '', acceptance_criteria: dr.criteria || '',
      priority: Number(dr.priority), weight: Number(dr.weight),
      tags: (dr.tags || '').split(',').map((x) => x.trim()).filter(Boolean),
    }));
  } else if (st.form === 'dep') {
    formEl = form('Make this task wait for another', [
      line('dep', 'Task id or number', { required: true, list: 'dep-options', placeholder: 'T-12' }),
      h('datalist', { id: 'dep-options' }, depChoices.map((x) => h('option', { value: x.id }, x.title)))],
    'Add dependency', () => act('dep-add', t, { depends_on: dr.dep || '' }),
    'It cannot start until that task is done. Skald refuses a link that would make a loop.');
  } else if (st.form === 'evidence') {
    formEl = form('Add evidence', [
      h('label', { class: 'field' }, 'Kind', h('select', { 'data-key': 'f-kind', onchange: (e) => { dr.kind = e.target.value; } },
        KINDS.map(([val, label]) => h('option', { value: val, selected: (dr.kind || 'commit') === val ? true : null }, label)))),
      line('ref', 'Reference (SHA, URL, report path)', { required: true }),
      line('summary', 'What it shows (optional)')], 'Add evidence', () => act('evidence', t, { kind: dr.kind || 'commit', ref: dr.ref || '', summary: dr.summary || '' }));
  }
  const unmet = d.unmet.length ? h('div', { class: 'callout info' }, `Waiting for ${d.unmet.join(', ')} to finish before it can start.`) : null;
  return h('div', { class: 'drawer-wrap' },
    h('a', { class: 'scrim', href: close, tabindex: '-1', 'aria-label': 'Close task' }),
    h('aside', { class: 'drawer', role: 'dialog', 'aria-label': `${t.id} ${t.title}`, tabindex: '-1' },
      h('div', { class: 'drawer-top' },
        h('span', null, h('span', { class: 'id' }, `${slug} / ${t.id}`),
          h('button', { type: 'button', class: 'btn copy', 'data-key': 'copy-id', title: 'Copy the task id', onclick: (e) => copyText(t.id, e.currentTarget) }, 'Copy id')),
        h('a', { class: 'btn', href: close, 'data-key': 'drawer-close', title: 'Close (Esc)' }, 'Close')),
      h('div', { class: 'drawer-body', 'data-scroll': 'drawer' },
        h('span', { class: 'chip' }, mk(t.status), STATUS[t.status], t.stale ? ', stale' : '', t.ready ? ', ready to start' : ''),
        h('h1', null, t.title),
        h('div', { class: 'actions' }, buttons),
        h('div', { class: 'actions more' }, more),
        formEl,
        t.status === 'blocked' && t.blocked_reason ? h('div', { class: 'callout' }, h('b', null, 'Blocked. '), t.blocked_reason) : null,
        unmet,
        t.description ? h('div', { class: 'section' }, h('h2', null, 'Description'), h('p', { class: 'prose' }, t.description)) : null,
        t.acceptance_criteria ? h('div', { class: 'section' }, h('h2', null, 'Acceptance criteria'), h('p', { class: 'prose' }, t.acceptance_criteria)) : null,
        h('div', { class: 'section' }, h('h2', null, 'Details'),
          h('dl', { class: 'facts' },
            h('dt', null, 'Feature'), h('dd', null, d.feature.title),
            h('dt', null, 'Priority'), h('dd', null, PRIORITY[t.priority] || String(t.priority)),
            h('dt', null, 'Weight'), h('dd', null, String(t.weight)),
            fact('Tags', (t.tags || []).length ? h('span', { class: 'labels' }, t.tags.map((x) => h('span', { class: 'label' }, x))) : ''),
            fact('Started by', t.assignee ? actorName(t.assignee) : ''), fact('Started', fmtDate(t.started_at)),
            fact('Completed', fmtDate(t.completed_at)), fact('Updated', fmtDate(t.updated_at)),
            h('dt', null, 'Version'), h('dd', null, String(t.version)),
            t.github ? [h('dt', null, 'GitHub'), h('dd', null, h('a', { href: t.github, target: '_blank', rel: 'noopener noreferrer' }, `Issue #${t.github_issue}`))] : [])),
        h('div', { class: 'section' }, h('h2', null, 'Evidence'),
          d.artifacts.length ? h('ul', { class: 'links evidence' }, d.artifacts.map((a) => h('li', null,
            h('span', { class: 'kind' }, a.kind), h('span', { class: 'mono' }, a.ref), a.summary ? h('div', { class: 'muted' }, a.summary) : null,
            h('div', { class: 'muted', style: { fontSize: '13px' } }, `${actorName(a.actor)}, `, timeEl(a.created_at)))))
            : h('p', { class: 'empty' }, 'No evidence recorded yet. A task needs a commit, PR, test report, link or manual note before it can be completed.')),
        h('div', { class: 'section' }, h('h2', null, 'Depends on'),
          linkList(slug, d.dependencies, 'Nothing. It can start any time.', (x) => act('dep-remove', t, { depends_on: x.id })),
          h('div', { class: 'actions more' }, btn('dep', 'Add dependency', '', open('dep')))),
        h('div', { class: 'section' }, h('h2', null, 'Unlocks'), linkList(slug, d.dependents, 'No task is waiting for this one.')),
        h('div', { class: 'section' }, h('h2', null, 'History'),
          d.events.length ? h('ol', { class: 'saga', style: { listStyle: 'none' } }, d.events.map((ev) => h('li', { class: ev._fresh ? 'fresh' : null }, eventLine({ ...ev, task: '' })))) : h('p', { class: 'empty' }, 'No history yet.')))));
}

export function docView(d, slug) {
  return h('div', { class: 'page' },
    h('p', { class: 'muted' }, h('a', { href: `#/p/${encodeURIComponent(slug)}/docs` }, slug), ' / documents'),
    h('h1', { style: { marginTop: '8px' } }, d.document.title),
    h('p', { class: 'muted', style: { margin: '6px 0 18px' } }, `${d.document.kind}, version ${d.version}, saved ${fmtDate(d.created_at)} by ${actorName(d.actor)}`),
    h('p', { class: 'prose doc-body' }, d.body));
}

// ---- activity --------------------------------------------------------------

export function activityView({ feed }, st, rerender) {
  return h('div', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', null, 'Activity')),
    h('div', { style: { maxWidth: '760px' } }, sagaList(feed, st, rerender, { showProject: true, limit: 100 })));
}
