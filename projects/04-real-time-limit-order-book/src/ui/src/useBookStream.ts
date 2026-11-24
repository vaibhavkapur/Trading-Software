import { useEffect, useRef, useState } from "react";
import type { BookMetric, BookView, FeedHealth, Hello, ServerMessage, StatusEvent } from "./types";

export interface StreamState {
  connected: boolean;
  hello: Hello | null;
  book: BookView | null;
  health: FeedHealth | null;
  metric: BookMetric | null;
  events: StatusEvent[];
  /** Milliseconds since the last server message (local monotonic clock). */
  msSinceMessage: number | null;
}

const EMPTY: Omit<StreamState, "msSinceMessage"> = {
  connected: false,
  hello: null,
  book: null,
  health: null,
  metric: null,
  events: [],
};
const MAX_EVENTS = 200;
// Noisy on a healthy feed and not actionable in a timeline.
const HIDDEN_KINDS = new Set(["resync_requested"]);

export function useBookStream(symbol: string | null, depth: number): StreamState {
  const [state, setState] = useState(EMPTY);
  const lastMessageAt = useRef<number | null>(null);
  const [now, setNow] = useState(() => performance.now());

  // Watchdog tick: staleness must surface even when no message arrives.
  useEffect(() => {
    const id = window.setInterval(() => setNow(performance.now()), 250);
    return () => window.clearInterval(id);
  }, []);

  useEffect(() => {
    if (!symbol) return;
    let closed = false;
    let ws: WebSocket | null = null;
    let retry: number | undefined;
    lastMessageAt.current = null;
    setState(EMPTY);

    const open = () => {
      const scheme = window.location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(`${scheme}://${window.location.host}/v1/books/${encodeURIComponent(symbol)}/stream?depth=${depth}`);
      ws.onopen = () => setState((s) => ({ ...s, connected: true }));
      ws.onmessage = (ev) => {
        lastMessageAt.current = performance.now();
        const msg = JSON.parse(ev.data as string) as ServerMessage;
        setState((s) => {
          switch (msg.type) {
            case "hello":
              return { ...s, hello: msg };
            case "snapshot":
            case "book":
              return { ...s, book: msg };
            case "health":
              return { ...s, health: msg };
            case "metric":
              return { ...s, metric: msg };
            case "status":
              return HIDDEN_KINDS.has(msg.kind) ? s : { ...s, events: [msg, ...s.events].slice(0, MAX_EVENTS) };
            default:
              return s;
          }
        });
      };
      ws.onclose = () => {
        if (closed) return;
        // Keep the last book on screen (dimmed by the freshness logic) and retry.
        setState((s) => ({ ...s, connected: false }));
        retry = window.setTimeout(open, 1000);
      };
    };
    open();
    return () => {
      closed = true;
      window.clearTimeout(retry);
      ws?.close();
    };
  }, [symbol, depth]);

  const msSinceMessage = lastMessageAt.current === null ? null : Math.max(0, now - lastMessageAt.current);
  return { ...state, msSinceMessage };
}
