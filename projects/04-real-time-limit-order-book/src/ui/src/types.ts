// Mirrors the server's JSON. Timestamps are microseconds since the Unix epoch (UTC).

export type SyncState = "awaiting_snapshot" | "synced" | "resyncing";
export type Integrity = "ok" | "locked" | "crossed";

export interface Level {
  price_ticks: number;
  qty: number;
}

export interface BookView {
  symbol: string;
  seq: number;
  state: SyncState;
  integrity: Integrity;
  stale: boolean;
  bids: Level[];
  asks: Level[];
  event_ts: number | null;
  recv_ts: number | null;
  asof: number;
  checksum: string;
}

export interface FeedHealth {
  symbol: string;
  state: SyncState;
  last_seq: number;
  gap_count: number;
  duplicate_count: number;
  rejected_count: number;
  recovery_count: number;
  drift_count: number;
  crossed_events: number;
  integrity: Integrity;
  stale_ms: number;
  stale: boolean;
  last_event_ts: number | null;
  last_recv_ts: number | null;
  venue_lag_ms: number | null;
  clock_skew_suspected: boolean;
  last_recovery_ms: number | null;
  unsynced_for_ms: number | null;
  buffered: number;
  asof: number;
}

export interface BookMetric {
  symbol: string;
  ts: number;
  seq: number;
  spread_ticks: number | null;
  mid_ticks_x2: number | null;
  mid_px: number | null;
  microprice: number | null;
  imbalance: number | null;
  depth_imbalance: number | null;
  bid_depth: number;
  ask_depth: number;
  ofi: number;
}

export interface StatusEvent {
  symbol: string;
  ts: number;
  kind: string;
  seq: number | null;
  value_ms: number | null;
  detail: string;
}

export interface SourceInfo {
  kind: "sim" | "replay";
  simulated: boolean;
  detail: string;
  admin: boolean;
  replay_speed: number | null;
  replay_paused: boolean | null;
}

export interface Hello {
  symbol: string;
  instrument: { symbol: string; tick_size: string; lot_size: string };
  depth: number;
  stale_after_ms: number;
  metric_depth: number;
  source: SourceInfo;
}

export type ServerMessage =
  | ({ type: "hello" } & Hello)
  | ({ type: "snapshot" | "book" } & BookView)
  | ({ type: "health" } & FeedHealth)
  | ({ type: "metric" } & BookMetric)
  | ({ type: "status" } & StatusEvent);
