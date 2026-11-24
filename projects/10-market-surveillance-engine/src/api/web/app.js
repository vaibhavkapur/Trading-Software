'use strict';

// The analyst token lives in this variable only. It is never written to
// storage, a cookie or the URL, so closing the tab forgets it.
let token = '';
let me = '';
let view = 'alerts';
let pickedAlert = '';
let pickedCase = '';
let timer = null;

const $ = (id) => document.getElementById(id);

// el builds DOM nodes with text content only; nothing from the server is
// ever interpreted as markup.
function el(tag, props, ...kids) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'class') node.className = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) node.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    node.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return node;
}

function showError(msg) {
  const box = $('error');
  box.textContent = msg || '';
  box.hidden = !msg;
}

async function api(method, path, body) {
  const headers = { Authorization: 'Bearer ' + token };
  if (body) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  let data = null;
  try { data = await res.json(); } catch (_) { /* no body */ }
  if (!res.ok) {
    throw new Error(data && data.error ? data.error.message : res.status + ' ' + res.statusText);
  }
  return data;
}

async function guard(fn) {
  try {
    showError('');
    return await fn();
  } catch (err) {
    showError(err.message);
    return undefined;
  }
}

const time = (iso) => (iso ? new Date(iso).toISOString().slice(11, 23) : '');
const stamp = (iso) => (iso ? new Date(iso).toISOString().replace('T', ' ').slice(0, 19) : '');
const badge = (text, cls) => el('span', { class: 'badge ' + (cls || '') }, text);
const num = (v) => (typeof v === 'number' ? v.toLocaleString('en-US', { maximumFractionDigits: 2 }) : v);

// ---- connect ----

$('login').addEventListener('submit', async (e) => {
  e.preventDefault();
  token = $('token').value.trim();
  const res = await guard(() => api('GET', '/v1/whoami'));
  if (!res) {
    token = '';
    return;
  }
  me = res.analyst;
  $('token').value = '';
  $('who').textContent = 'Signed in as ' + me;
  $('app').hidden = false;
  await refresh();
  clearInterval(timer);
  timer = setInterval(refresh, 5000);
});

// refresh reloads the summary and the visible list. The detail pane is
// left alone so a half-written note is not wiped by the timer.
async function refresh() {
  await guard(async () => {
    renderStats(await api('GET', '/v1/stats'));
    if (view === 'alerts') await loadAlerts();
    else await loadCases();
  });
}

function renderStats(s) {
  const stat = (k, v, sub) => el('div', { class: 'stat' }, el('div', { class: 'k' }, k), el('div', { class: 'v' }, v), el('div', { class: 's' }, sub || '\u00a0'));
  const open = s.alerts.by_status.OPEN || 0;
  const unresolved = s.cases.total - (s.cases.by_status.RESOLVED || 0);
  $('stats').replaceChildren(
    stat('Events', num(s.events.total), num(s.events.late) + ' late, kept out of detection'),
    stat('Alerts', num(s.alerts.total), open + ' open'),
    stat('Alerts per million events', num(s.alerts.per_million_events)),
    stat('Duplicate-alert rate', (100 * s.alerts.duplicate_rate).toFixed(1) + '%', num(s.alerts.suppressed) + ' repeats folded in'),
    stat('Cases', num(s.cases.total), unresolved + ' unresolved'),
    stat('Watermark (UTC)', s.stream.watermark ? time(s.stream.watermark) : 'none yet', s.stream.buffered_events + ' events waiting'),
    stat('Ruleset', s.ruleset.version, s.ruleset.hash.slice(0, 12)),
  );
}

// ---- tabs ----

function show(which) {
  view = which;
  $('view-alerts').hidden = which !== 'alerts';
  $('view-cases').hidden = which !== 'cases';
  $('tab-alerts').setAttribute('aria-selected', String(which === 'alerts'));
  $('tab-cases').setAttribute('aria-selected', String(which === 'cases'));
  refresh();
}
$('tab-alerts').addEventListener('click', () => show('alerts'));
$('tab-cases').addEventListener('click', () => show('cases'));
for (const id of ['f-status', 'f-rule', 'f-symbol', 'c-status']) $(id).addEventListener('change', refresh);

// ---- alerts ----

async function loadAlerts() {
  const q = new URLSearchParams();
  if ($('f-status').value) q.set('status', $('f-status').value);
  if ($('f-rule').value) q.set('rule_id', $('f-rule').value);
  if ($('f-symbol').value.trim()) q.set('symbol', $('f-symbol').value.trim());
  const { alerts } = await api('GET', '/v1/alerts?' + q);
  $('alerts-empty').hidden = alerts.length > 0;
  $('alerts').querySelector('tbody').replaceChildren(...alerts.map((a) =>
    el('tr', { class: 'pick' + (a.id === pickedAlert ? ' sel' : ''), tabindex: 0, onclick: () => showAlert(a.id),
      onkeydown: (e) => { if (e.key === 'Enter') showAlert(a.id); } },
      el('td', {}, badge(a.severity, a.severity)),
      el('td', {}, a.rule_id),
      el('td', { class: 'wrap' }, a.accounts.join(', ')),
      el('td', {}, a.symbol),
      el('td', { class: 'mono' }, time(a.window_start) + ' \u2013 ' + time(a.window_end)),
      el('td', { class: 'num' }, a.occurrences + '\u00d7'),
      el('td', {}, badge(a.status)))));
}

function kvTable(observed, thresholds) {
  const rows = (obj) => Object.keys(obj).sort().map((k) => el('tr', {}, el('th', {}, k), el('td', { class: 'num' }, num(obj[k]))));
  return el('div', { class: 'row' },
    el('div', {}, el('h3', {}, 'Observed'), el('table', { class: 'kv' }, el('tbody', {}, rows(observed)))),
    el('div', {}, el('h3', {}, 'Thresholds in force'), el('table', { class: 'kv' }, el('tbody', {}, rows(thresholds)))));
}

function eventsTable(events) {
  const who = (e) => (e.kind === 'trade' ? e.buy_account + ' buys from ' + e.sell_account : e.account_id);
  const ref = (e) => (e.kind === 'trade' ? e.trade_id : e.order_id);
  return el('div', { class: 'scroll' }, el('table', {},
    el('thead', {}, el('tr', {}, ['Event time', 'Received', 'What', 'Side', 'Qty', 'Price', 'Account', 'Order / trade'].map((h) => el('th', {}, h)))),
    el('tbody', {}, events.map((e) => el('tr', {},
      el('td', { class: 'mono' }, time(e.event_time)),
      el('td', { class: 'mono' }, time(e.received_at)),
      el('td', {}, e.kind === 'trade' ? 'TRADE' : e.action),
      el('td', {}, e.side || ''),
      el('td', { class: 'num' }, e.qty ? num(e.qty) : ''),
      el('td', { class: 'num' }, e.price || ''),
      el('td', { class: 'wrap' }, who(e)),
      el('td', { class: 'mono wrap' }, ref(e)))))));
}

async function showAlert(id) {
  pickedAlert = id;
  const d = await guard(() => api('GET', '/v1/alerts/' + encodeURIComponent(id)));
  if (!d) return;
  const a = d.alert;
  const title = el('input', { type: 'text', placeholder: 'Case title (optional)', 'aria-label': 'Case title' });
  const note = el('input', { type: 'text', placeholder: 'First note (optional)', 'aria-label': 'First note' });
  const open = el('form', { class: 'act', onsubmit: async (e) => {
    e.preventDefault();
    const c = await guard(() => api('POST', '/v1/alerts/' + encodeURIComponent(id) + '/cases', { title: title.value, note: note.value }));
    if (c) { pickedCase = c.id; show('cases'); showCase(c.id); }
  } }, el('h3', {}, 'Open a case'), el('div', { class: 'row' }, title, note, el('button', { type: 'submit' }, 'Open case')));

  $('alert-detail').replaceChildren(
    el('h2', {}, a.rule_id + ' v' + a.rule_version + ' \u00b7 ' + a.symbol, ' ', badge(a.severity, a.severity), ' ', badge(a.status)),
    el('p', { class: 'explain' }, a.explanation),
    el('p', { class: 'meta' }, a.disclaimer),
    el('p', { class: 'meta mono' }, a.id + ' \u00b7 ruleset ' + a.ruleset_version + ' (' + a.ruleset_hash.slice(0, 12) + ') \u00b7 detected '
      + a.occurrences + '\u00d7, first ' + stamp(a.first_detected_at) + ' UTC'),
    kvTable(a.observed, a.thresholds),
    el('h3', {}, 'Evidence: ' + d.events.length + ' events' + (a.evidence_truncated ? ' (list truncated)' : '')
      + (d.missing_event_refs ? ', ' + d.missing_event_refs.length + ' not found' : '')),
    eventsTable(d.events),
    a.status === 'IN_CASE'
      ? el('p', { class: 'meta' }, 'Under review in ' + (a.case_ids || []).join(', ') + '.')
      : open);
  if (view === 'alerts') loadAlerts();
}

// ---- cases ----

async function loadCases() {
  const q = $('c-status').value ? '?status=' + $('c-status').value : '';
  const { cases } = await api('GET', '/v1/cases' + q);
  $('cases-empty').hidden = cases.length > 0;
  $('cases').querySelector('tbody').replaceChildren(...cases.map((c) =>
    el('tr', { class: 'pick' + (c.id === pickedCase ? ' sel' : ''), tabindex: 0, onclick: () => showCase(c.id),
      onkeydown: (e) => { if (e.key === 'Enter') showCase(c.id); } },
      el('td', { class: 'mono' }, c.id),
      el('td', { class: 'wrap' }, c.title),
      el('td', {}, badge(c.status)),
      el('td', {}, c.priority || ''),
      el('td', {}, c.assignee || ''),
      el('td', { class: 'num' }, c.alert_ids.length),
      el('td', { class: 'mono' }, stamp(c.updated_at)))));
}

function action(title, fields, label, run) {
  return el('form', { class: 'act', onsubmit: async (e) => { e.preventDefault(); await run(); } },
    el('h3', {}, title), el('div', { class: 'row' }, fields, el('button', { type: 'submit' }, label)));
}

function select(options, label) {
  return el('select', { 'aria-label': label }, options.map((o) => el('option', { value: o[0] }, o[1])));
}

async function act(id, verb, body) {
  const c = await guard(() => api('POST', '/v1/cases/' + encodeURIComponent(id) + '/' + verb, body));
  if (c) { await showCase(id); refresh(); }
}

const describe = (ev) => {
  const d = ev.details || {};
  const parts = [];
  if (d.alert_id) parts.push('alert ' + d.alert_id + ' (' + d.event_count + ' events frozen)');
  if (d.priority) parts.push(d.priority + ', assigned to ' + d.assignee);
  if (d.disposition) parts.push(d.disposition);
  if (d.from_status && d.to_status && d.from_status !== d.to_status) parts.push(d.from_status + ' \u2192 ' + d.to_status);
  if (d.reason) parts.push(d.reason);
  if (d.summary) parts.push(d.summary);
  if (d.bundle_hash) parts.push('bundle ' + d.bundle_hash.slice(0, 12));
  return parts.join(' \u00b7 ');
};

async function showCase(id) {
  pickedCase = id;
  const c = await guard(() => api('GET', '/v1/cases/' + encodeURIComponent(id)));
  if (!c) return;
  const forms = [];
  const resolved = c.status === 'RESOLVED';

  if (!resolved) {
    const pri = select([['P1', 'P1 urgent'], ['P2', 'P2 normal'], ['P3', 'P3 low']], 'Priority');
    pri.value = c.priority || 'P2';
    const who = el('input', { type: 'text', value: c.assignee || me, 'aria-label': 'Assignee' });
    forms.push(action(c.status === 'OPEN' ? 'Triage' : 'Re-prioritise or reassign', [pri, who], c.status === 'OPEN' ? 'Start review' : 'Update',
      () => act(id, 'triage', { priority: pri.value, assignee: who.value })));
  }
  const text = el('textarea', { placeholder: 'What did you check, and what did you find?', 'aria-label': 'Note' });
  forms.push(action('Add a note', [text], 'Add note', () => act(id, 'notes', { text: text.value })));
  if (c.status === 'IN_REVIEW') {
    const why = el('input', { type: 'text', placeholder: 'Why does this need escalating?', 'aria-label': 'Escalation reason' });
    forms.push(action('Escalate', [why], 'Escalate', () => act(id, 'escalate', { reason: why.value })));
  }
  if (c.status === 'IN_REVIEW' || c.status === 'ESCALATED') {
    const disp = select([
      ['FALSE_POSITIVE', 'False positive'], ['BENIGN_EXPLAINED', 'Benign, explained'],
      ['INCONCLUSIVE', 'Inconclusive'], ['REFERRED_FOR_REVIEW', 'Referred for further review']], 'Disposition');
    const sum = el('input', { type: 'text', placeholder: 'Summary of the outcome (required)', 'aria-label': 'Resolution summary' });
    forms.push(action('Resolve', [disp, sum], 'Resolve', () => act(id, 'resolve', { disposition: disp.value, summary: sum.value })));
  }
  if (resolved) {
    const why = el('input', { type: 'text', placeholder: 'Why is this being reopened?', 'aria-label': 'Reopen reason' });
    forms.push(action('Reopen', [why], 'Reopen', () => act(id, 'reopen', { reason: why.value })));
  }

  const exportBtn = el('button', { type: 'button', class: 'quiet', onclick: () => guard(async () => {
    const bundle = await api('GET', '/v1/cases/' + encodeURIComponent(id) + '/evidence');
    const url = URL.createObjectURL(new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' }));
    const a = el('a', { href: url, download: id + '-evidence.json' });
    document.body.append(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    await showCase(id);
  }) }, 'Export evidence (recorded in the audit trail)');

  $('case-detail').replaceChildren(
    el('h2', {}, c.id + ' \u00b7 ', c.title, ' ', badge(c.status)),
    el('p', { class: 'meta' }, 'Opened by ' + c.created_by + ' at ' + stamp(c.created_at) + ' UTC'
      + (c.assignee ? ' \u00b7 ' + c.priority + ', assigned to ' + c.assignee : '')
      + (c.disposition ? ' \u00b7 ' + c.disposition + ': ' + c.summary : '')),
    el('p', { class: 'meta mono' }, 'Alerts: ' + c.alert_ids.join(', ')),
    el('div', { class: 'row' }, exportBtn),
    el('h3', {}, 'Notes'),
    c.analyst_notes.length
      ? c.analyst_notes.map((n) => el('div', { class: 'note' }, el('div', {}, n.text), el('div', { class: 'by' }, n.author + ' \u00b7 ' + stamp(n.at) + ' UTC')))
      : el('p', { class: 'meta' }, 'No notes yet.'),
    forms,
    el('h3', {}, 'Audit trail'),
    el('ol', { class: 'trail' }, (c.audit_events || []).map((ev) => el('li', {},
      el('div', {}, el('strong', {}, ev.action), ' by ' + ev.actor + ' \u00b7 ' + stamp(ev.time) + ' UTC'),
      el('div', { class: 'meta' }, describe(ev)),
      el('div', { class: 'meta mono' }, '#' + ev.seq + ' ' + ev.hash.slice(0, 16))))));
  if (view === 'cases') loadCases();
}

$('verify').addEventListener('click', () => guard(async () => {
  const st = await api('GET', '/v1/audit/verify');
  $('verify-result').textContent = st.verified
    ? 'Chain intact: ' + st.events + ' events, head ' + st.head.slice(0, 12)
    : 'CHAIN BROKEN: ' + st.error;
}));
