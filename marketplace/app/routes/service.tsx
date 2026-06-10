import { Cloud, Plug, Hash } from "lucide-react";
import type { Route } from "./+types/service";
import { getEnv } from "@/lib/env";
import { createDeusClient, DeusApiError } from "@/lib/deus.server";
import { callerIdentityFor, getUser, getWallet, isDevAuth } from "@/lib/auth.server";
import { allowRequest, clientKey } from "@/lib/limits.server";
import { verifyTurnstile } from "@/lib/turnstile.server";
import { parseForm, tryItSchema } from "@/lib/validate.server";
import { Badge, KindBadge, SectionHeading, StatusBadge, Stat } from "@/components/ui";
import { PaxFlow } from "@/components/pax";
import { TryItPanel } from "@/components/try-it";
import Breadcrumb from "../../components/ui/smoothui/breadcrumb";
import BasicAccordion from "../../components/ui/smoothui/basic-accordion";
import { formatUptime, shortAddress, weiToPax } from "@/lib/format";
import type { ManifestOperation } from "@/lib/deus.types";

export function meta({ data }: Route.MetaArgs) {
  if (!data?.service) return [{ title: "ServiceDeus" }];
  const s = data.service;
  const title = `${s.display_name}Deus`;
  const desc = s.summary;
  const canonical = `${data.origin}/services/${s.slug}`;
  const priceWei = s.manifest?.pricing?.[0]?.price_wei;
  return [
    { title },
    { name: "description", content: desc },
    { property: "og:type", content: "website" },
    { property: "og:site_name", content: "Deus" },
    { property: "og:title", content: title },
    { property: "og:description", content: desc },
    { property: "og:url", content: canonical },
    { property: "og:image", content: `${data.origin}/icon.svg` },
    { name: "twitter:card", content: "summary" },
    { name: "twitter:title", content: title },
    { name: "twitter:description", content: desc },
    { tagName: "link", rel: "canonical", href: canonical },
    {
      "script:ld+json": {
        "@context": "https://schema.org",
        "@type": "Product",
        name: s.display_name,
        description: desc,
        url: canonical,
        brand: { "@type": "Brand", name: "Deus" },
        ...(priceWei
          ? {
              offers: {
                "@type": "Offer",
                priceCurrency: "PAX",
                price: weiToPax(priceWei),
                availability:
                  s.status === "active"
                    ? "https://schema.org/InStock"
                    : "https://schema.org/Discontinued",
                url: canonical,
              },
            }
          : {}),
      },
    },
  ];
}

export async function loader({ request, params, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const id = params.id;
  const deus = createDeusClient(env);
  let service;
  try {
    service = await deus.getService(id);
  } catch (err) {
    if (err instanceof DeusApiError && err.status === 404) {
      throw new Response("Service not found", { status: 404 });
    }
    // Backend blip: surface a retryable 503 (branded error page), not a 500.
    throw new Response("Service temporarily unavailable", { status: 503 });
  }
  const wallet = await getWallet(request, env);
  return {
    service,
    wallet,
    allowDev: isDevAuth(env),
    turnstileSiteKey: env.TURNSTILE_SITE_KEY || null,
    origin: new URL(request.url).origin,
  };
}

export async function action({ request, params, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const id = params.id;
  const [user, wallet] = await Promise.all([getUser(request, env), getWallet(request, env)]);
  const deus = createDeusClient(env, { caller: callerIdentityFor(user, wallet) });
  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");

  const parsed = parseForm(tryItSchema, form, ["operation", "units", "args"]);
  if (!parsed.ok) return { intent, error: parsed.error };
  const { operation, units: unitCount, args: rawArgs } = parsed.data;
  const units = String(unitCount);

  if (intent === "quote") {
    try {
      const quote = await deus.quote(id, { operation, estimated_units: units });
      return { intent, quote };
    } catch (err) {
      return { intent, error: errMsg(err) };
    }
  }

  if (intent === "run") {
    if (!wallet) return { intent, error: "Connect a wallet to run this call." };

    // Invoke spends real money — rate-limit per wallet (falling back to IP)
    // and require the bot challenge when Turnstile is provisioned.
    const limitKey = wallet ?? clientKey(request);
    if (!(await allowRequest(env.RL_INVOKE, limitKey))) {
      return { intent, error: "Too many calls. Slow down and try again shortly." };
    }
    const captcha = String(form.get("cf-turnstile-response") ?? "") || null;
    if (!(await verifyTurnstile(env, captcha, clientKey(request)))) {
      return { intent, error: "Verification failed. Complete the challenge and retry." };
    }

    let parsedArgs: Record<string, unknown> = {};
    try {
      parsedArgs = JSON.parse(rawArgs || "{}");
    } catch {
      return { intent, error: "Arguments must be valid JSON." };
    }
    try {
      const quote = await deus.quote(id, { operation, estimated_units: units });
      const result = await deus.invoke(id, {
        operation,
        args: parsedArgs,
        quote_id: quote.quote_id,
        payment: { rail: "direct" },
        idempotency_key: crypto.randomUUID(),
      });
      return { intent, quote, result };
    } catch (err) {
      return { intent, error: errMsg(err) };
    }
  }

  return { intent };
}

function errMsg(err: unknown): string {
  if (err instanceof DeusApiError) return err.message;
  return err instanceof Error ? err.message : "Something went wrong.";
}

export default function ServiceDetail({ loaderData }: Route.ComponentProps) {
  const { service, wallet, allowDev, turnstileSiteKey } = loaderData;
  const manifest = service.manifest;
  const operations: ManifestOperation[] = manifest?.operations ?? [];
  const pricing = manifest?.pricing ?? [];
  const tags = manifest?.tags ?? [];

  return (
    <div className="mx-auto max-w-7xl px-6 py-10 sm:px-8">
      <Breadcrumb
        items={[
          { label: "Catalog", href: "/catalog" },
          { label: service.display_name },
        ]}
      />

      {/* Header */}
      <header className="mt-6 flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2">
          <KindBadge kind={service.kind} />
          <StatusBadge status={service.status} />
          <Badge tone="muted">
            {service.mode === "hosted" ? (
              <><Cloud className="size-3.5" /> Hosted</>
            ) : (
              <><Plug className="size-3.5" /> Proxy</>
            )}
          </Badge>
        </div>
        <h1 className="display-3 text-foreground">{service.display_name}</h1>
        <p className="body-lg max-w-3xl text-muted-foreground">{service.summary}</p>
        {tags.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {tags.map((t) => (
              <span key={t} className="rounded-full bg-secondary px-3 py-1 text-xs text-muted-foreground">
                {t}
              </span>
            ))}
          </div>
        ) : null}
      </header>

      <div className="mt-10 grid grid-cols-1 gap-10 lg:grid-cols-3">
        {/* Details */}
        <div className="flex flex-col gap-10 lg:col-span-2">
          <div className="grid grid-cols-3 gap-6 rounded-xl bg-card p-6 shadow-02">
            <Stat label="Uptime target" value={formatUptime(manifest?.sla?.target_uptime_bps)} />
            <Stat label="Operations" value={operations.length} />
            <Stat label="Settlement" value="PAX" hint={`chain ${service.chain_id ?? 125}`} />
          </div>

          {manifest?.description ? (
            <section className="flex flex-col gap-3">
              <SectionHeading eyebrow="Overview" title="About this service" />
              <p className="body text-muted-foreground">{manifest.description}</p>
            </section>
          ) : null}

          <section className="flex flex-col gap-4">
            <SectionHeading eyebrow="Operations" title="What you can call" />
            <div className="flex flex-col gap-3">
              {operations.map((op) => {
                const price = pricing.find((p) => p.operation === op.name);
                return (
                  <div key={op.name} className="flex flex-col gap-3 rounded-xl bg-card p-5 shadow-01">
                    <div className="flex flex-wrap items-center justify-between gap-3">
                      <div className="flex items-center gap-3">
                        <span className="mono rounded-md bg-secondary px-2 py-1 text-xs text-muted-foreground">
                          {op.method ?? "POST"}
                        </span>
                        <span className="text-h3 text-foreground">{op.name}</span>
                      </div>
                      {price ? (
                        <Badge tone="accent">
                          <PaxFlow wei={price.price_wei} /> / {price.unit}
                        </Badge>
                      ) : null}
                    </div>
                    {(op as { description?: string }).description ? (
                      <p className="body-sm text-muted-foreground">
                        {(op as { description?: string }).description}
                      </p>
                    ) : null}
                    <SchemaAccordion
                      input={op.input_schema}
                      output={op.output_schema}
                    />
                  </div>
                );
              })}
            </div>
          </section>

          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Hash className="size-3.5" />
            <span className="mono">{shortAddress(service.manifest_hash, 8)}</span>
            <span>· integrity-checked manifest</span>
          </div>

          <a
            href={`mailto:abuse@paxeer.app?subject=${encodeURIComponent(
              `Report service: ${service.slug}`
            )}`}
            className="text-xs text-muted-foreground underline-offset-4 transition-colors hover:text-foreground hover:underline"
          >
            Report this service
          </a>
        </div>

        {/* Try it (sticky) */}
        <div className="lg:col-span-1">
          <div className="lg:sticky lg:top-24">
            <TryItPanel
              operations={operations}
              pricing={pricing}
              wallet={wallet}
              allowDev={allowDev}
              turnstileSiteKey={turnstileSiteKey}
            />
          </div>
        </div>
      </div>
    </div>
  );
}

/** Input/output JSON schemas via the smoothui basic-accordion. */
function SchemaAccordion({
  input,
  output,
}: {
  input?: Record<string, unknown>;
  output?: Record<string, unknown>;
}) {
  const items = [
    { id: "input", title: "Input schema", schema: input },
    { id: "output", title: "Output schema", schema: output },
  ]
    .filter((s) => s.schema && Object.keys(s.schema).length > 0)
    .map((s) => ({
      id: s.id,
      title: s.title,
      content: (
        <pre className="mono max-h-48 overflow-auto text-xs text-foreground">
          {JSON.stringify(s.schema, null, 2)}
        </pre>
      ),
    }));
  if (items.length === 0) return null;
  return <BasicAccordion items={items} allowMultiple />;
}
