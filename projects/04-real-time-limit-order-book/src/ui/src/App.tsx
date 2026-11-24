import { useEffect, useState } from "react";
import { assess } from "./freshness";
import { Ladder } from "./Ladder";
import { Controls, FreshnessBadge, HealthPanel, Metrics, Timeline } from "./panels";
import { useBookStream } from "./useBookStream";

const DEPTH = 15;

export function App() {
  const [symbols, setSymbols] = useState<string[]>([]);
  const [symbol, setSymbol] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      fetch("/v1/symbols")
        .then((r) => r.json())
        .then((body: { symbols: { symbol: string }[] }) => {
          if (cancelled) return;
          const list = body.symbols.map((s) => s.symbol);
          setSymbols(list);
          setSymbol((current) => current ?? list[0] ?? null);
          setLoadError(null);
        })
        .catch(() => {
          if (cancelled) return;
          setLoadError("Cannot reach the stream API. Retrying…");
          window.setTimeout(load, 2000);
        });
    load();
    return () => {
      cancelled = true;
    };
  }, []);

  const stream = useBookStream(symbol, DEPTH);
  const staleAfterMs = stream.hello?.stale_after_ms ?? 2000;
  const freshness = assess({
    connected: stream.connected,
    msSinceMessage: stream.msSinceMessage,
    staleAfterMs,
    book: stream.book,
  });
  const tickSize = stream.hello?.instrument.tick_size ?? "0.01";
  const source = stream.hello?.source ?? null;

  return (
    <div className="app">
      <header className="top">
        <div>
          <h1>Limit order book</h1>
          <p className="muted">{source?.detail ?? "connecting…"}</p>
        </div>
        <div className="top-right">
          <span className="badge simulated" title="No real market data or orders are involved">
            SIMULATED DATA
          </span>
          <FreshnessBadge freshness={freshness} />
        </div>
      </header>

      <nav className="symbols" aria-label="Symbols">
        {symbols.map((s) => (
          <button key={s} className={s === symbol ? "active" : ""} onClick={() => setSymbol(s)}>
            {s}
          </button>
        ))}
        {stream.book && (
          <span className="muted asof">
            seq {stream.book.seq.toLocaleString("en-US")} · image as of{" "}
            {new Date(stream.book.asof / 1000).toISOString().slice(11, 23)} UTC
          </span>
        )}
      </nav>

      {loadError && <p className="error" role="alert">{loadError}</p>}
      {!freshness.trustworthy && symbol && (
        <div className={`banner status-${freshness.level}`} role="alert">
          <strong>{freshness.headline}.</strong> {freshness.reason}
        </div>
      )}

      <main className="grid">
        <Ladder book={stream.book} tickSize={tickSize} freshness={freshness} rows={DEPTH} />
        <div className="column">
          <Metrics
            book={stream.book}
            metric={stream.metric}
            tickSize={tickSize}
            metricDepth={stream.hello?.metric_depth ?? 5}
            trustworthy={freshness.trustworthy}
          />
          <HealthPanel health={stream.health} book={stream.book} msSinceMessage={stream.msSinceMessage} />
          <Controls source={source} />
        </div>
        <Timeline events={stream.events} />
      </main>
    </div>
  );
}
