import { useEffect, useMemo, useState } from "react";
import { useFetcher } from "react-router";
import { Check, Play, ShieldCheck, Timer, Coins } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedTabs from "../../components/ui/smoothui/animated-tabs";
import NumberFlow from "../../components/ui/smoothui/number-flow";
import { WalletButton } from "@/components/wallet";
import { TurnstileWidget } from "@/components/turnstile";
import { Badge, Spinner } from "@/components/ui";
import { ActionToast } from "@/components/feedback";
import { PaxFlow } from "@/components/pax";
import type {
  InvokeResponse,
  ManifestOperation,
  ManifestPricing,
  QuoteResponse,
} from "@/lib/deus.types";

interface TryItData {
  intent?: string;
  quote?: QuoteResponse;
  result?: InvokeResponse;
  error?: string;
}

function templateFor(op?: ManifestOperation): string {
  const props = op?.input_schema?.properties as Record<string, unknown> | undefined;
  if (props && typeof props === "object") {
    const obj: Record<string, string> = {};
    for (const key of Object.keys(props)) obj[key] = "";
    if (Object.keys(obj).length > 0) return JSON.stringify(obj, null, 2);
  }
  return JSON.stringify({ input: "" }, null, 2);
}

export function TryItPanel({
  operations,
  pricing,
  wallet,
  allowDev,
  turnstileSiteKey = null,
}: {
  operations: ManifestOperation[];
  pricing: ManifestPricing[];
  wallet: string | null;
  allowDev: boolean;
  turnstileSiteKey?: string | null;
}) {
  const [op, setOp] = useState(operations[0]?.name ?? "");
  const [units, setUnits] = useState(1);
  const [args, setArgs] = useState(() => templateFor(operations[0]));
  const quoteFetcher = useFetcher<TryItData>();
  const runFetcher = useFetcher<TryItData>();

  // Pull a fresh, signed quote whenever the operation or unit count changes.
  useEffect(() => {
    if (!op) return;
    quoteFetcher.submit(
      { intent: "quote", operation: op, units: String(units) },
      { method: "post" }
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [op, units]);

  useEffect(() => {
    setArgs(templateFor(operations.find((o) => o.name === op)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [op]);

  const manifestPrice = useMemo(
    () => pricing.find((p) => p.operation === op)?.price_wei,
    [pricing, op]
  );
  const quotedPrice = quoteFetcher.data?.quote?.unit_price_wei ?? manifestPrice;
  const result = runFetcher.data?.result;
  const runError = runFetcher.data?.error;
  const running = runFetcher.state !== "idle";

  return (
    <div className="flex flex-col gap-4 rounded-2xl bg-card p-6 shadow-04">
      <div className="flex items-center justify-between">
        <h3 className="text-h3 text-foreground">Try it</h3>
        <Badge tone="accent">
          {quotedPrice ? <PaxFlow wei={quotedPrice} /> : "—"}
          <span className="opacity-70">/ call</span>
        </Badge>
      </div>

      {/* Operation selectorsmoothui animated-tabs */}
      {operations.length > 1 ? (
        <AnimatedTabs
          tabs={operations.map((o) => ({ id: o.name, label: o.name }))}
          activeTab={op}
          variant="pill"
          layoutId="tryit-op"
          onChange={setOp}
          className="flex-wrap"
        />
      ) : null}

      {/* Unitssmoothui number-flow stepper drives the quote. */}
      <div className="flex items-center justify-between gap-3">
        <span className="eyebrow">Units</span>
        <NumberFlow
          value={units}
          onChange={setUnits}
          min={1}
          max={999}
          className="min-h-0 flex-row justify-end gap-0"
        />
      </div>

      <label className="flex flex-col gap-2">
        <span className="eyebrow">Arguments</span>
        <textarea
          value={args}
          onChange={(e) => setArgs(e.target.value)}
          spellCheck={false}
          rows={6}
          className="mono w-full resize-y rounded-lg bg-secondary p-3 text-sm text-foreground outline-none focus:ring-2 focus:ring-ring"
        />
      </label>

      <runFetcher.Form method="post" className="flex flex-col gap-3">
        <input type="hidden" name="intent" value="run" />
        <input type="hidden" name="operation" value={op} />
        <input type="hidden" name="units" value={units} />
        <input type="hidden" name="args" value={args} />
        {wallet ? <TurnstileWidget siteKey={turnstileSiteKey} /> : null}
        {wallet ? (
          <SmoothButton type="submit" variant="default" size="lg" disabled={running}>
            {running ? <Spinner /> : <Play className="size-4" />}
            {running ? "Running…" : "Run call"}
          </SmoothButton>
        ) : (
          <div className="flex flex-col items-center gap-2 rounded-xl bg-secondary/60 p-4 text-center">
            <p className="body-sm text-muted-foreground">
              Connect a wallet to run a live, paid call. Quotes are free.
            </p>
            <WalletButton wallet={wallet} allowDev={allowDev} />
          </div>
        )}
      </runFetcher.Form>

      <ActionToast message={runError} type="error" />

      {result ? <ResultView result={result} /> : null}
    </div>
  );
}

function ResultView({ result }: { result: InvokeResponse }) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-2 text-[color:var(--success)]">
        <span className="flex size-5 items-center justify-center rounded-full bg-[color:color-mix(in_oklab,var(--success)_22%,transparent)]">
          <Check className="size-3.5" />
        </span>
        <span className="text-sm font-medium">Completed</span>
      </div>

      <div>
        <span className="eyebrow">Result</span>
        <pre className="mono mt-2 max-h-72 overflow-auto rounded-lg bg-secondary p-3 text-xs text-foreground">
          {JSON.stringify(result.result, null, 2)}
        </pre>
      </div>

      {/* Clean receipt summaryno protocol jargon. */}
      <div className="grid grid-cols-3 gap-3 rounded-xl bg-secondary/60 p-4">
        <Metric
          icon={<Coins className="size-3.5" />}
          label="Charged"
          value={<PaxFlow wei={result.charged_wei} />}
        />
        <Metric icon={<Timer className="size-3.5" />} label="Latency" value={`${result.latency_ms} ms`} />
        <Metric
          icon={<ShieldCheck className="size-3.5" />}
          label="Receipt"
          value={result.receipt?.gateway_sig ? "Signed" : "Recorded"}
        />
      </div>
    </div>
  );
}

function Metric({
  icon,
  label,
  value,
}: {
  icon: React.ReactNode;
  label: string;
  value: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
        {icon}
        {label}
      </span>
      <span className="text-sm text-foreground tabular-nums">{value}</span>
    </div>
  );
}
