/**
 * Presentation helpers. Client-safe (no server-only imports). Deus prices are
 * wei strings of PAX (18 decimals); we surface human PAX, never raw wei.
 */

const WEI_PER_PAX = 1_000_000_000_000_000_000n;

/** Convert a PAX amount (string/number) to a wei string (18 decimals). */
export function paxToWei(pax: string | number | undefined | null): string {
  if (pax === undefined || pax === null || pax === "") return "0";
  const n = typeof pax === "number" ? pax : Number.parseFloat(pax);
  if (Number.isNaN(n) || n <= 0) return "0";
  // Scale via micro-PAX to keep precision without floating point drift.
  const micro = BigInt(Math.round(n * 1e6));
  return (micro * 1_000_000_000_000n).toString();
}

/** Convert a wei string to a PAX number (lossy for display only). */
export function weiToPax(wei: string | number | undefined | null): number {
  if (wei === undefined || wei === null || wei === "") return 0;
  try {
    const w = typeof wei === "number" ? BigInt(Math.round(wei)) : BigInt(wei);
    const whole = w / WEI_PER_PAX;
    const frac = w % WEI_PER_PAX;
    return Number(whole) + Number(frac) / 1e18;
  } catch {
    return 0;
  }
}

/** Format a wei string as a PAX amount, e.g. "0.0042 PAX". */
export function formatPax(
  wei: string | number | undefined | null,
  opts: { maxFractionDigits?: number; withSymbol?: boolean } = {}
): string {
  const { maxFractionDigits = 4, withSymbol = true } = opts;
  const pax = weiToPax(wei);
  let str: string;
  if (pax === 0) {
    str = "0";
  } else if (pax < 0.0001) {
    str = pax.toExponential(2);
  } else {
    str = pax.toLocaleString("en-US", {
      minimumFractionDigits: 0,
      maximumFractionDigits: maxFractionDigits,
    });
  }
  return withSymbol ? `${str} PAX` : str;
}

/** Compact large counts: 1.2K, 3.4M. */
export function formatCount(n: number | undefined | null): string {
  if (!n) return "0";
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n % 1000 === 0 ? 0 : 1)}K`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** 0x1234…cdef */
export function shortAddress(addr: string | undefined | null, size = 4): string {
  if (!addr) return "";
  if (addr.length <= size * 2 + 2) return addr;
  return `${addr.slice(0, size + 2)}…${addr.slice(-size)}`;
}

/** Basis points (9990) → "99.9%". Truncates (never rounds uptime up). */
export function formatUptime(bps: number | undefined | null): string {
  if (bps === undefined || bps === null) return "—";
  if (bps % 100 === 0) return `${bps / 100}%`;
  return `${(Math.floor(bps / 10) / 10).toFixed(1)}%`;
}

/** quality_score is a 0..1 decimal string → "94". */
export function formatQuality(score: string | number | undefined | null): string {
  if (score === undefined || score === null || score === "") return "—";
  const n = typeof score === "number" ? score : Number.parseFloat(score);
  if (Number.isNaN(n)) return "—";
  return Math.round((n <= 1 ? n * 100 : n)).toString();
}

export function formatRelativeTime(iso: string | undefined | null): string {
  if (!iso) return "";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const diff = Date.now() - then;
  const mins = Math.round(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(iso).toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

export function formatDate(iso: string | undefined | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString("en-US", {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export type StatusTone = "active" | "draft" | "paused" | "delisted" | "neutral";

/** Map a service/deployment status to a semantic tone class set. */
export function statusTone(status: string | undefined | null): StatusTone {
  switch ((status ?? "").toLowerCase()) {
    case "active":
    case "live":
    case "published":
      return "active";
    case "draft":
    case "pending":
    case "deploying":
      return "draft";
    case "paused":
      return "paused";
    case "delisted":
    case "superseded":
    case "failed":
      return "delisted";
    default:
      return "neutral";
  }
}

export function titleCase(s: string | undefined | null): string {
  if (!s) return "";
  return s.replace(/[_-]+/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}
