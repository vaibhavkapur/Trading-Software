import type { BookView } from "./types";

export type FreshnessLevel = "live" | "stale" | "resyncing" | "awaiting" | "disconnected";

export interface Freshness {
  level: FreshnessLevel;
  /** False whenever the displayed prices must not be read as current. */
  trustworthy: boolean;
  headline: string;
  reason: string;
}

export interface FreshnessInput {
  connected: boolean;
  /** Milliseconds since the last message from the server, on this browser's monotonic clock. */
  msSinceMessage: number | null;
  staleAfterMs: number;
  book: Pick<BookView, "state" | "stale" | "asof" | "recv_ts"> | null;
}

const secs = (ms: number) => `${(ms / 1000).toFixed(1)}s`;

/**
 * Decides whether the ladder may be presented as current. The worst condition
 * wins. Nothing here compares the browser clock with the server's or the
 * venue's: link silence uses a local monotonic timer, and feed staleness is
 * decided by the server from its own receive times.
 */
export function assess(input: FreshnessInput): Freshness {
  const { connected, msSinceMessage, staleAfterMs, book } = input;
  if (!connected) {
    return {
      level: "disconnected",
      trustworthy: false,
      headline: "Disconnected",
      reason: "No connection to the stream API. Prices shown are the last ones received.",
    };
  }
  if (msSinceMessage === null || book === null) {
    return { level: "awaiting", trustworthy: false, headline: "Connecting", reason: "Waiting for the first book image." };
  }
  if (msSinceMessage > staleAfterMs) {
    return {
      level: "stale",
      trustworthy: false,
      headline: "Stale",
      reason: `Nothing received from the stream API for ${secs(msSinceMessage)}.`,
    };
  }
  if (book.state === "awaiting_snapshot") {
    return { level: "awaiting", trustworthy: false, headline: "Awaiting snapshot", reason: "The book has not been built yet." };
  }
  if (book.stale) {
    const idle = book.recv_ts === null ? null : (book.asof - book.recv_ts) / 1000;
    return {
      level: "stale",
      trustworthy: false,
      headline: "Stale feed",
      reason:
        idle === null
          ? "The server has not received any market data."
          : `The server has not received market data for ${secs(idle)}.`,
    };
  }
  if (book.state === "resyncing") {
    return {
      level: "resyncing",
      trustworthy: false,
      headline: "Resyncing",
      reason: "A sequence gap was detected. The book is frozen until a snapshot arrives.",
    };
  }
  return { level: "live", trustworthy: true, headline: "Live", reason: "Book is synced and receiving data." };
}
