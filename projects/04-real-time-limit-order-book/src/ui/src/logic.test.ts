import { describe, expect, it } from "vitest";
import { assess } from "./freshness";
import { formatDuration, formatTicks, formatTime } from "./format";

const book = { state: "synced" as const, stale: false, asof: 2_000_000, recv_ts: 1_990_000 };
const base = { connected: true, msSinceMessage: 50, staleAfterMs: 2000, book };

describe("assess", () => {
  it("is live only when connected, recent, synced and not stale", () => {
    expect(assess(base)).toMatchObject({ level: "live", trustworthy: true });
  });
  it("never trusts prices after a disconnect, even if the last image looked fine", () => {
    expect(assess({ ...base, connected: false })).toMatchObject({ level: "disconnected", trustworthy: false });
  });
  it("flags a silent link using only the local timer", () => {
    const f = assess({ ...base, msSinceMessage: 2001 });
    expect(f).toMatchObject({ level: "stale", trustworthy: false });
    expect(f.reason).toContain("2.0s");
    expect(assess({ ...base, msSinceMessage: 2000 }).level).toBe("live");
  });
  it("flags a feed the server reports as stale", () => {
    const f = assess({ ...base, book: { ...book, stale: true, asof: 9_000_000, recv_ts: 4_000_000 } });
    expect(f).toMatchObject({ level: "stale", trustworthy: false });
    expect(f.reason).toContain("5.0s");
  });
  it("flags resync and missing snapshot", () => {
    expect(assess({ ...base, book: { ...book, state: "resyncing" } })).toMatchObject({ level: "resyncing", trustworthy: false });
    expect(assess({ ...base, book: { ...book, state: "awaiting_snapshot" } })).toMatchObject({ level: "awaiting", trustworthy: false });
    expect(assess({ ...base, book: null }).trustworthy).toBe(false);
    expect(assess({ ...base, msSinceMessage: null }).trustworthy).toBe(false);
  });
  it("lets staleness outrank resyncing", () => {
    expect(assess({ ...base, book: { ...book, state: "resyncing", stale: true } }).level).toBe("stale");
  });
});

describe("format", () => {
  it("formats ticks exactly", () => {
    expect(formatTicks(10025, "0.01")).toBe("100.25");
    expect(formatTicks(7, "0.01")).toBe("0.07");
    expect(formatTicks(22, "0.05")).toBe("1.10");
    expect(formatTicks(10, "25")).toBe("250");
    expect(formatTicks(12345678, "0.0001")).toBe("1234.5678");
  });
  it("formats times in UTC and durations compactly", () => {
    expect(formatTime(1_767_225_600_123_000)).toBe("00:00:00.123");
    expect(formatTime(null)).toBe("–");
    expect(formatDuration(250)).toBe("250 ms");
    expect(formatDuration(-5000)).toBe("-5.0 s");
    expect(formatDuration(3 * 86_400_000)).toBe("3 d");
  });
});
