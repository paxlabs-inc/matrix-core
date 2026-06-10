import PriceFlow from "../../components/ui/smoothui/price-flow";
import { cn } from "@/lib/utils";
import { formatCount, formatPax } from "@/lib/format";

/**
 * Animated numeric displays composed from the smoothui `price-flow` digit
 * ticker. PriceFlow animates exactly one zero-padded digit pair, so arbitrary
 * formatted strings ("0.0008", "1.2K") are rendered as runs of digit pairs
 * (animated) plus literal separators. Odd-length digit runs render their first
 * digit as a literal so PriceFlow's padStart(2) never corrupts the value.
 */
function FlowDigits({ text }: { text: string }) {
  const parts: React.ReactNode[] = [];
  let i = 0;
  let key = 0;
  while (i < text.length) {
    if (/\d/.test(text[i])) {
      let run = "";
      while (i < text.length && /\d/.test(text[i])) {
        run += text[i];
        i++;
      }
      let offset = 0;
      if (run.length % 2 === 1) {
        parts.push(<span key={key++}>{run[0]}</span>);
        offset = 1;
      }
      for (let j = offset; j < run.length; j += 2) {
        parts.push(<PriceFlow key={key++} value={Number(run.slice(j, j + 2))} />);
      }
    } else {
      parts.push(<span key={key++}>{text[i]}</span>);
      i++;
    }
  }
  return <>{parts}</>;
}

/** Animated PAX amount from a wei string, e.g. 0.0008 PAX. */
export function PaxFlow({
  wei,
  withSymbol = true,
  className,
}: {
  wei: string | number | undefined | null;
  withSymbol?: boolean;
  className?: string;
}) {
  const text = formatPax(wei, { withSymbol: false });
  return (
    <span className={cn("inline-flex items-baseline tabular-nums", className)}>
      <FlowDigits text={text} />
      {withSymbol ? <span className="ml-1">PAX</span> : null}
    </span>
  );
}

/** Animated compact count (1.2K, 3.4M). */
export function CountFlow({
  value,
  className,
}: {
  value: number | undefined | null;
  className?: string;
}) {
  return (
    <span className={cn("inline-flex items-baseline tabular-nums", className)}>
      <FlowDigits text={formatCount(value)} />
    </span>
  );
}
