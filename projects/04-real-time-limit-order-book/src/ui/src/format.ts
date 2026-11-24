/** Formats integer ticks as a decimal price using the instrument tick size, without floats. */
export function formatTicks(ticks: number, tickSize: string): string {
  const [int = "0", frac = ""] = tickSize.split(".");
  const unit = BigInt(int + frac);
  const value = BigInt(Math.trunc(ticks)) * unit;
  const negative = value < 0n;
  const digits = (negative ? -value : value).toString().padStart(frac.length + 1, "0");
  const cut = digits.length - frac.length;
  const body = frac.length === 0 ? digits : `${digits.slice(0, cut)}.${digits.slice(cut)}`;
  return negative ? `-${body}` : body;
}

/** Formats a fractional tick value (mid, microprice) with extra decimals. */
export function formatFractionalTicks(ticks: number, tickSize: string, extra = 2): string {
  const decimals = (tickSize.split(".")[1] ?? "").length;
  return (ticks * Number(tickSize)).toFixed(decimals + extra);
}

export const formatQty = (qty: number) => qty.toLocaleString("en-US");

/** `HH:MM:SS.mmm` in UTC from epoch microseconds. */
export function formatTime(us: number | null): string {
  if (us === null) return "–";
  return new Date(us / 1000).toISOString().slice(11, 23);
}

export function formatDateTime(us: number | null): string {
  if (us === null) return "–";
  return new Date(us / 1000).toISOString().replace("T", " ").replace("Z", " UTC");
}

export function formatDuration(ms: number | null): string {
  if (ms === null) return "–";
  const abs = Math.abs(ms);
  if (abs < 1000) return `${ms} ms`;
  if (abs < 120_000) return `${(ms / 1000).toFixed(1)} s`;
  if (abs < 7_200_000) return `${Math.round(ms / 60_000)} min`;
  if (abs < 172_800_000) return `${Math.round(ms / 3_600_000)} h`;
  return `${Math.round(ms / 86_400_000)} d`;
}
