import { useState } from "react";
import { formatQty, formatTicks } from "./format";
import type { Freshness } from "./freshness";
import type { BookView, Level } from "./types";

interface Row extends Level {
  cumulative: number;
}

function withCumulative(levels: Level[]): Row[] {
  let total = 0;
  return levels.map((l) => ({ ...l, cumulative: (total += l.qty) }));
}

interface Props {
  book: BookView | null;
  tickSize: string;
  freshness: Freshness;
  rows: number;
}

export function Ladder({ book, tickSize, freshness, rows }: Props) {
  const [hover, setHover] = useState<string | null>(null);
  const bids = withCumulative(book?.bids.slice(0, rows) ?? []);
  const asks = withCumulative(book?.asks.slice(0, rows) ?? []);
  const maxCumulative = Math.max(1, bids.at(-1)?.cumulative ?? 0, asks.at(-1)?.cumulative ?? 0);
  const bestBid = bids[0]?.price_ticks;
  const bestAsk = asks[0]?.price_ticks;
  const spread = bestBid !== undefined && bestAsk !== undefined ? bestAsk - bestBid : null;

  const renderRow = (side: "ask" | "bid", r: Row, best: boolean) => {
    const key = `${side}-${r.price_ticks}`;
    return (
      <tr
        key={key}
        className={`${side}${best ? " best" : ""}${hover === key ? " hover" : ""}`}
        onMouseEnter={() => setHover(key)}
        onMouseLeave={() => setHover(null)}
        title={`${side === "bid" ? "Bid" : "Ask"} ${formatTicks(r.price_ticks, tickSize)} · size ${formatQty(r.qty)} · cumulative ${formatQty(r.cumulative)}`}
      >
        <td className="side-label">{best ? (side === "bid" ? "Best bid" : "Best ask") : ""}</td>
        <td className="num price">{formatTicks(r.price_ticks, tickSize)}</td>
        <td className="num">{formatQty(r.qty)}</td>
        <td className="num muted">{formatQty(r.cumulative)}</td>
        <td className="bar-cell" aria-hidden="true">
          <span className="bar" style={{ width: `${(r.cumulative / maxCumulative) * 100}%` }} />
        </td>
      </tr>
    );
  };

  return (
    <section className={`card ladder${freshness.trustworthy ? "" : " untrusted"}`} aria-label="Depth ladder">
      <header className="card-head">
        <h2>Depth ladder</h2>
        <span className="legend">
          <span className="swatch ask" /> Asks <span className="swatch bid" /> Bids
          <span className="muted"> · bar = cumulative size</span>
        </span>
      </header>
      {!freshness.trustworthy && (
        <div className="overlay" role="alert">
          <strong>{freshness.headline.toUpperCase()} — prices are not current</strong>
          <span>{freshness.reason}</span>
        </div>
      )}
      {book && book.integrity !== "ok" && (
        <div className="integrity" role="alert">
          <strong>⚠ {book.integrity === "crossed" ? "Crossed book" : "Locked book"}</strong> best bid{" "}
          {book.integrity === "crossed" ? "is above" : "equals"} best ask, as published by the feed.
        </div>
      )}
      <table>
        <thead>
          <tr>
            <th />
            <th className="num">Price</th>
            <th className="num">Size</th>
            <th className="num">Cumulative</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {[...asks].reverse().map((r) => renderRow("ask", r, r.price_ticks === bestAsk))}
          <tr className="spread-row">
            <td className="side-label">Spread</td>
            <td className="num" colSpan={4}>
              {spread === null ? "–" : `${formatTicks(spread, tickSize)} (${spread} tick${Math.abs(spread) === 1 ? "" : "s"})`}
            </td>
          </tr>
          {bids.map((r) => renderRow("bid", r, r.price_ticks === bestBid))}
          {!book && (
            <tr>
              <td colSpan={5} className="muted empty">
                No book image received yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}
