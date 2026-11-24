package gateway

import "net/http"

func (g *Gateway) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(auditUIHTML))
}

const auditUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Agentic Trading Guardrails — Audit & Replay Console</title>
<style>
  :root { --bg: #0f172a; --card: #1e293b; --border: #334155; --text: #e2e8f0; --muted: #94a3b8; --green: #22c55e; --red: #ef4444; --amber: #f59e0b; --blue: #38bdf8; }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: var(--bg); color: var(--text); padding: 24px; }
  header { display: flex; justify-content: space-between; align-items: center; border-bottom: 1px solid var(--border); padding-bottom: 16px; margin-bottom: 24px; }
  .badge { background: rgba(245, 158, 11, 0.18); color: var(--amber); border: 1px solid var(--amber); padding: 4px 10px; border-radius: 999px; font-size: 12px; font-weight: 700; letter-spacing: 0.05em; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 14px; margin-bottom: 24px; }
  .card { background: var(--card); border: 1px solid var(--border); border-radius: 10px; padding: 16px; }
  .metric-label { font-size: 12px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; }
  .metric-val { font-size: 26px; font-weight: 700; margin-top: 6px; }
  .ok { color: var(--green); }
  .bad { color: var(--red); }
  .info { color: var(--blue); }
  h2 { font-size: 16px; margin: 0 0 12px; color: var(--blue); }
  .controls { display: flex; gap: 10px; margin-bottom: 14px; flex-wrap: wrap; }
  input, button { background: #0b1220; border: 1px solid var(--border); color: var(--text); padding: 8px 12px; border-radius: 6px; font-size: 13px; }
  button { background: #2563eb; border-color: #3b82f6; cursor: pointer; font-weight: 600; }
  button:hover { background: #1d4ed8; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td { text-align: left; padding: 10px 8px; border-bottom: 1px solid var(--border); vertical-align: top; }
  th { color: var(--muted); font-weight: 600; }
  code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
  pre { background: #0b1220; padding: 10px; border-radius: 6px; overflow-x: auto; margin: 6px 0 0; }
  .pill { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 11px; font-weight: 700; }
  .pill-allow { background: rgba(34,197,94,0.18); color: var(--green); }
  .pill-deny { background: rgba(239,68,68,0.2); color: var(--red); }
  .pill-neutral { background: rgba(56,189,248,0.16); color: var(--blue); }
  .split { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 24px; }
  @media (max-width: 900px) { .split { grid-template-columns: 1fr; } }
</style>
</head>
<body>
<header>
  <div>
    <h1 style="margin:0;font-size:22px;">Agentic Trading Guardrails — Audit &amp; Replay Console</h1>
    <div style="color:var(--muted);font-size:13px;margin-top:4px;">Fail-closed policy enforcement, signed one-use approval capabilities &amp; hash-chained audit trail</div>
  </div>
  <div class="badge">PAPER / SIMULATED ONLY</div>
</header>

<div class="grid" id="metrics-grid"></div>

<div class="split">
  <div class="card">
    <h2>Active Policy Bundle</h2>
    <div id="policy-box" style="color:var(--muted);">Loading...</div>
  </div>
  <div class="card">
    <h2>Proposal Replay Inspector</h2>
    <div class="controls">
      <input id="proposal-input" placeholder="Proposal ID (e.g. p-1)" />
      <button onclick="loadReplay()">Replay Proposal</button>
      <button onclick="refreshAll()">Refresh All</button>
    </div>
    <div id="replay-box" style="color:var(--muted);">Enter a proposal ID to replay actor, principal, policies, approval, and paper broker response.</div>
  </div>
</div>

<div class="card">
  <h2>Immutable Hash-Chained Audit Log</h2>
  <div class="controls">
    <input id="filter-actor" placeholder="Filter Actor ID" />
    <input id="filter-proposal" placeholder="Filter Proposal ID" />
    <button onclick="loadAudit()">Apply Filter</button>
  </div>
  <table>
    <thead>
      <tr>
        <th>Seq</th>
        <th>UTC Receive Time</th>
        <th>Category</th>
        <th>Actor / Principal</th>
        <th>Proposal</th>
        <th>Action &amp; Outcome</th>
        <th>Policy Version</th>
        <th>Details (Redacted)</th>
      </tr>
    </thead>
    <tbody id="audit-body"></tbody>
  </table>
</div>

<script>
async function refreshAll() {
  await Promise.all([loadMetrics(), loadPolicy(), loadAudit()]);
}
async function loadMetrics() {
  const [m, h] = await Promise.all([
    fetch('/v1/metrics').then(r => r.json()),
    fetch('/healthz').then(r => r.json())
  ]);
  const cards = [
    ['Decisions (Allow / Deny)', m.authorization_allow + ' / ' + m.authorization_deny, 'info'],
    ['Auth Latency p50 / p99', m.auth_latency_p50_micros + 'µs / ' + m.auth_latency_p99_micros + 'µs', 'info'],
    ['Broker Write PEP Coverage', m.broker_write_pep_coverage_pct.toFixed(1) + '%', 'ok'],
    ['Audit Completeness', m.audit_completeness_pct.toFixed(1) + '%', 'ok'],
    ['Audit Hash Chain', h.chain_verified ? 'VERIFIED' : 'TAMPERED', h.chain_verified ? 'ok' : 'bad'],
    ['Total Audit Events', m.audit_events_total, 'info']
  ];
  document.getElementById('metrics-grid').innerHTML = cards.map(([label, val, cls]) =>
    '<div class="card"><div class="metric-label">' + label + '</div><div class="metric-val ' + cls + '">' + val + '</div></div>'
  ).join('');
}
async function loadPolicy() {
  const data = await fetch('/v1/policies').then(r => r.json());
  const a = data.active;
  if (!a) return;
  document.getElementById('policy-box').innerHTML =
    '<div><strong>' + a.id + '</strong> <code>' + a.version + '</code> — Kill Switch: <strong class="' + (a.kill_switch_active ? 'bad' : 'ok') + '">' + (a.kill_switch_active ? 'ACTIVE' : 'OFF') + '</strong></div>' +
    '<div style="margin-top:6px;font-size:12px;color:var(--muted);">Hash: <code>' + a.hash + '</code></div>' +
    '<pre>' + JSON.stringify({ org: a.org, rules: a.rules }, null, 2) + '</pre>';
}
async function loadAudit() {
  const actor = document.getElementById('filter-actor').value.trim();
  const prop = document.getElementById('filter-proposal').value.trim();
  const qs = new URLSearchParams();
  if (actor) qs.set('actor_id', actor);
  if (prop) qs.set('proposal_id', prop);
  const data = await fetch('/v1/audit?' + qs.toString()).then(r => r.json());
  const rows = (data.events || []).slice().reverse().map(ev => {
    const cls = (ev.outcome === 'ALLOW' || ev.outcome === 'APPROVED' || ev.outcome === 'FILLED_PAPER' || ev.outcome === 'PROPOSED') ? 'pill-allow'
      : (ev.outcome === 'DENY' || ev.outcome === 'BROKER_ERROR') ? 'pill-deny' : 'pill-neutral';
    return '<tr>' +
      '<td>#' + ev.seq + '</td>' +
      '<td><code>' + ev.received_at_utc + '</code></td>' +
      '<td><code>' + ev.category + '</code></td>' +
      '<td>' + (ev.actor_id || '-') + '<br><span style="color:var(--muted);font-size:11px;">' + (ev.principal_id || '') + '</span></td>' +
      '<td><code>' + (ev.proposal_id || '-') + '</code></td>' +
      '<td>' + ev.action + ' <span class="pill ' + cls + '">' + ev.outcome + '</span></td>' +
      '<td><code>' + (ev.policy_version || '-') + '</code></td>' +
      '<td><code>' + JSON.stringify(ev.details || {}) + '</code></td>' +
      '</tr>';
  });
  document.getElementById('audit-body').innerHTML = rows.join('') || '<tr><td colspan="8" style="color:var(--muted);">No audit events recorded yet.</td></tr>';
}
async function loadReplay() {
  const pid = document.getElementById('proposal-input').value.trim();
  if (!pid) return;
  const data = await fetch('/v1/audit/replay?proposal_id=' + encodeURIComponent(pid)).then(r => r.json());
  document.getElementById('replay-box').innerHTML =
    '<div>Actor: <code>' + (data.actor_id || '-') + '</code> | Principal: <code>' + (data.principal_id || '-') + '</code> | Account: <code>' + (data.account_id || '-') + '</code> | Chain: <strong class="' + (data.chain_verified ? 'ok' : 'bad') + '">' + (data.chain_verified ? 'VERIFIED' : 'BROKEN') + '</strong></div>' +
    '<pre>' + JSON.stringify(data.events, null, 2) + '</pre>';
}
refreshAll();
setInterval(refreshAll, 5000);
</script>
</body>
</html>
`
