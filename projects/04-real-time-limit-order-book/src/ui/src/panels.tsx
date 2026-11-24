import { useState } from "react";
import { formatDateTime, formatDuration, formatFractionalTicks, formatQty, formatTicks, formatTime } from "./format";
import type { Freshness, FreshnessLevel } from "./freshness";
import type { BookMetric, BookView, FeedHealth, SourceInfo, StatusEvent } from "./types";

const STATUS_ICON: Record<FreshnessLevel, string> = {
  live: "●",
  stale: "▲",
  resyncing: "↻",
  awaiting: "…",
  disconnected: "✕",
};

export function FreshnessBadge({ freshness }: { freshness: Freshness }) {
  return (
    <span className={`badge status-${freshness.level}`} title={freshness.reason}>
      <span aria-hidden="true">{STATUS_ICON[freshness.level]}</span> {freshness.headline}
    </span>
  );
}

function Tile({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value">{value}</div>
      {sub && <div className="tile-sub">{sub}</div>}
    </div>
  );
}

/** Signed ratio in [-1, 1] drawn from a centre line: left = ask-heavy, right = bid-heavy. */
function ImbalanceMeter({ label, value }: { label: string; value: number | null }) {
  const pct = value === null ? 0 : Math.min(1, Math.abs(value)) * 50;
  const text = value === null ? "–" : `${value > 0 ? "+" : ""}${(value * 100).toFixed(0)}%`;
  const lean = value === null || value === 0 ? "balanced" : value > 0 ? "bid-heavy" : "ask-heavy";
  return (
    <div className="tile" title={`${label}: ${text} (${lean})`}>
      <div className="tile-label">{label}</div>
      <div className="tile-value">{text}</div>
      <div className="meter" aria-hidden="true">
        <span
          className={`meter-fill ${value !== null && value < 0 ? "ask" : "bid"}`}
          style={value !== null && value < 0 ? { right: "50%", width: `${pct}%` } : { left: "50%", width: `${pct}%` }}
        />
        <span className="meter-mid" />
      </div>
      <div className="tile-sub">{lean}</div>
    </div>
  );
}

interface MetricsProps {
  book: BookView | null;
  metric: BookMetric | null;
  tickSize: string;
  metricDepth: number;
  trustworthy: boolean;
}

export function Metrics({ book, metric, tickSize, metricDepth, trustworthy }: MetricsProps) {
  const bid = book?.bids[0];
  const ask = book?.asks[0];
  // Metrics are only produced for a synced book; hide a leftover one otherwise.
  const m = trustworthy ? metric : null;
  return (
    <section className={`card${trustworthy ? "" : " untrusted"}`} aria-label="Top of book and metrics">
      <header className="card-head">
        <h2>Top of book</h2>
        {!trustworthy && <span className="muted">last known values</span>}
      </header>
      <div className="tiles">
        <Tile label="Best bid" value={bid ? formatTicks(bid.price_ticks, tickSize) : "–"} sub={bid ? `size ${formatQty(bid.qty)}` : undefined} />
        <Tile label="Best ask" value={ask ? formatTicks(ask.price_ticks, tickSize) : "–"} sub={ask ? `size ${formatQty(ask.qty)}` : undefined} />
        <Tile
          label="Spread"
          value={m?.spread_ticks == null ? "–" : formatTicks(m.spread_ticks, tickSize)}
          sub={m?.spread_ticks == null ? undefined : `${m.spread_ticks} tick${Math.abs(m.spread_ticks) === 1 ? "" : "s"}`}
        />
        <Tile label="Mid" value={m?.mid_px == null ? "–" : formatFractionalTicks(m.mid_px, tickSize, 1)} />
        <Tile label="Microprice" value={m?.microprice == null ? "–" : formatFractionalTicks(m.microprice, tickSize)} sub="size-weighted mid" />
        <ImbalanceMeter label="Imbalance (top)" value={m?.imbalance ?? null} />
        <ImbalanceMeter label={`Imbalance (${metricDepth} levels)`} value={m?.depth_imbalance ?? null} />
        <Tile
          label={`Depth (${metricDepth} levels)`}
          value={m ? `${formatQty(m.bid_depth)} / ${formatQty(m.ask_depth)}` : "–"}
          sub="bid / ask"
        />
      </div>
    </section>
  );
}

export function HealthPanel({ health, book, msSinceMessage }: { health: FeedHealth | null; book: BookView | null; msSinceMessage: number | null }) {
  const rows: [string, string, string?][] = health
    ? [
        ["Book state", health.state.replace("_", " ")],
        ["Last sequence", health.last_seq.toLocaleString("en-US")],
        ["Venue event time", formatDateTime(health.last_event_ts), "Timestamp set by the venue on the last event."],
        ["Server receive time", formatDateTime(health.last_recv_ts), "Server's local clock when that event arrived."],
        ["Feed idle", formatDuration(health.stale_ms), "Time since the server last received data (server clock only)."],
        ["Stream idle", msSinceMessage === null ? "–" : formatDuration(Math.round(msSinceMessage)), "Time since this page last heard from the server (browser timer)."],
        [
          "Receive − venue time",
          health.venue_lag_ms === null ? "–" : `${formatDuration(health.venue_lag_ms)}${health.clock_skew_suspected ? " ⚠ clock skew" : ""}`,
          "Difference between two unsynchronized clocks: indicative only, not a latency measurement.",
        ],
        ["Sequence gaps", String(health.gap_count)],
        ["Recoveries", `${health.recovery_count}${health.last_recovery_ms === null ? "" : ` (last ${formatDuration(health.last_recovery_ms)})`}`],
        ["Duplicates ignored", String(health.duplicate_count)],
        ["Rejected events", String(health.rejected_count)],
        ["Crossed episodes", String(health.crossed_events)],
        ["Snapshot drift", String(health.drift_count)],
        ["Book checksum", book?.checksum ?? "–", "FNV-1a over the displayed levels."],
      ]
    : [];
  return (
    <section className="card" aria-label="Feed health">
      <header className="card-head">
        <h2>Feed health</h2>
      </header>
      {health ? (
        <dl className="kv">
          {rows.map(([k, v, hint]) => (
            <div key={k} title={hint}>
              <dt>{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <p className="muted empty">No health report yet.</p>
      )}
    </section>
  );
}

const SEVERITY: Record<string, "critical" | "warning" | "good" | "info"> = {
  gap_detected: "critical",
  decode_error: "critical",
  rejected: "critical",
  checkpoint_mismatch: "critical",
  drift_detected: "critical",
  crossed: "warning",
  locked: "warning",
  stale: "warning",
  clock_skew: "warning",
  duplicate: "warning",
  stale_snapshot: "warning",
  client_lagged: "warning",
  source_restarted: "warning",
  resynced: "good",
  fresh: "good",
  uncrossed: "good",
  clock_skew_cleared: "good",
  snapshot_applied: "info",
};
const SEVERITY_ICON = { critical: "✕", warning: "▲", good: "✓", info: "•" };

export function Timeline({ events }: { events: StatusEvent[] }) {
  return (
    <section className="card timeline" aria-label="Event timeline">
      <header className="card-head">
        <h2>Event timeline</h2>
        <span className="muted">newest first · server receive time (UTC)</span>
      </header>
      {events.length === 0 ? (
        <p className="muted empty">No feed events. Gaps, resyncs, stale periods and integrity warnings appear here.</p>
      ) : (
        <ol>
          {events.map((e, i) => {
            const sev = SEVERITY[e.kind] ?? "info";
            return (
              <li key={`${e.ts}-${e.kind}-${i}`}>
                <time>{formatTime(e.ts)}</time>
                <span className={`pill sev-${sev}`}>
                  <span aria-hidden="true">{SEVERITY_ICON[sev]}</span> {e.kind.replace(/_/g, " ")}
                </span>
                <span className="detail">
                  {e.detail}
                  {e.value_ms !== null && ` (${formatDuration(e.value_ms)})`}
                </span>
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}

const FAULTS: { label: string; hint: string; body: Record<string, unknown> }[] = [
  { label: "Drop 5 deltas", hint: "Lose 5 messages: sequence gap, then resync", body: { fault: "drop", count: 5 } },
  { label: "Drop 200 deltas", hint: "A longer outage", body: { fault: "drop", count: 200 } },
  { label: "Duplicate", hint: "Deliver the next delta twice", body: { fault: "duplicate" } },
  { label: "Reorder", hint: "Swap the next two deltas", body: { fault: "reorder" } },
  { label: "Corrupt frame", hint: "Truncate the next delta", body: { fault: "garbage" } },
  { label: "Cross book", hint: "Venue publishes a bid above the best ask", body: { fault: "cross" } },
  { label: "Clock skew +5 s", hint: "Venue timestamps run ahead", body: { fault: "clock_skew", offset_ms: 5000, count: 400 } },
  { label: "Stall 5 s", hint: "Venue goes silent: stale alert", body: { fault: "stall", ms: 5000 } },
];

async function post(path: string, body: unknown): Promise<string | null> {
  try {
    const resp = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (resp.ok) return null;
    const json = (await resp.json().catch(() => null)) as { error?: string } | null;
    return json?.error ?? `HTTP ${resp.status}`;
  } catch (e) {
    return String(e);
  }
}

export function Controls({ source }: { source: SourceInfo | null }) {
  const [error, setError] = useState<string | null>(null);
  const [paused, setPaused] = useState(source?.replay_paused ?? false);
  if (!source) return null;
  const run = async (path: string, body: unknown) => setError(await post(path, body));
  return (
    <section className="card" aria-label="Simulator controls">
      <header className="card-head">
        <h2>{source.kind === "sim" ? "Fault injection" : "Replay"}</h2>
        <span className="muted">simulator control, not market activity</span>
      </header>
      {!source.admin ? (
        <p className="muted empty">Controls are disabled. Start the server with <code>--admin</code> to enable them.</p>
      ) : source.kind === "sim" ? (
        <div className="buttons">
          {FAULTS.map((f) => (
            <button key={f.label} title={f.hint} onClick={() => run("/v1/admin/faults", f.body)}>
              {f.label}
            </button>
          ))}
        </div>
      ) : (
        <div className="buttons">
          {[0.25, 1, 5, 25].map((speed) => (
            <button key={speed} onClick={() => run("/v1/admin/replay", { speed })}>
              {speed}×
            </button>
          ))}
          <button
            onClick={() => {
              setPaused(!paused);
              void run("/v1/admin/replay", { paused: !paused });
            }}
          >
            {paused ? "Resume" : "Pause"}
          </button>
        </div>
      )}
      {error && <p className="error" role="alert">{error}</p>}
    </section>
  );
}
