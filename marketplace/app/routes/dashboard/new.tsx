import { useMemo, useState } from "react";
import { Form, Link, redirect, useNavigation } from "react-router";
import { ArrowLeft, Plus, Sparkles, Trash2 } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedInput from "../../../components/ui/smoothui/animated-input";
import AnimatedTabs from "../../../components/ui/smoothui/animated-tabs";
import BasicDropdown from "../../../components/ui/smoothui/basic-dropdown";
import type { Route } from "./+types/new";
import { getEnv } from "@/lib/env";
import { developerIdentityFor, getWallet, requireUser } from "@/lib/auth.server";
import { createDeusClient, DeusApiError } from "@/lib/deus.server";
import { paxToWei } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Badge, KindBadge } from "@/components/ui";
import { ActionToast } from "@/components/feedback";
import { PaxFlow } from "@/components/pax";

interface OpRow {
  name: string;
  method: string;
  description: string;
  price: string;
  unit: string;
  inputSchema: string;
  outputSchema: string;
}

function blankOp(): OpRow {
  return { name: "", method: "POST", description: "", price: "", unit: "request", inputSchema: "", outputSchema: "" };
}

function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
}

function parseSchema(raw: string): Record<string, unknown> {
  const s = (raw || "").trim();
  if (!s) return { type: "object" };
  try {
    const v = JSON.parse(s);
    return v && typeof v === "object" ? (v as Record<string, unknown>) : { type: "object" };
  } catch {
    return { type: "object" };
  }
}

export function meta() {
  return [{ title: "New listing · Deus" }];
}

export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const form = await request.formData();
  const get = (k: string) => String(form.get(k) ?? "").trim();

  const display_name = get("display_name");
  const slug = get("slug") || slugify(display_name);
  const kind = get("kind") || "data";
  const mode = get("mode") || "proxy";
  const summary = get("summary");
  const description = get("description");
  const proxy_url = get("proxy_url");
  const tags = get("tags").split(",").map((t) => t.trim()).filter(Boolean);

  let rows: OpRow[] = [];
  try {
    const parsed = JSON.parse(get("operations_json") || "[]");
    if (Array.isArray(parsed)) rows = parsed as OpRow[];
  } catch {
    // fall through to validation below
  }
  rows = rows.filter((r) => r && r.name && r.name.trim());

  if (!display_name) return { error: "Give your service a name." };
  if (rows.length === 0) return { error: "Add at least one operation with a name." };

  for (const r of rows) {
    for (const [field, raw] of [["input", r.inputSchema], ["output", r.outputSchema]] as const) {
      if (raw && raw.trim()) {
        try {
          JSON.parse(raw);
        } catch {
          return { error: `Operation “${r.name}” has invalid ${field} JSON schema.` };
        }
      }
    }
  }

  const manifest: Record<string, unknown> = {
    schema_version: "2026-01",
    slug,
    kind,
    mode,
    display_name,
    summary,
    ...(description ? { description } : {}),
    tags,
    operations: rows.map((r) => ({
      name: r.name.trim(),
      method: (r.method || "POST").toUpperCase(),
      description: r.description || undefined,
      input_schema: parseSchema(r.inputSchema),
      output_schema: parseSchema(r.outputSchema),
    })),
    pricing: rows.map((r) => {
      const wei = paxToWei(r.price);
      return {
        operation: r.name.trim(),
        model: "per_unit",
        unit: r.unit || "request",
        price_wei: wei,
        min_charge_wei: wei,
      };
    }),
    ...(mode === "proxy" && proxy_url ? { endpoint: { proxy_url } } : {}),
  };

  const deus = createDeusClient(env, { developer: developerIdentityFor(wallet) });
  try {
    const res = await deus.createService(manifest);
    if (res.validation && !res.validation.ok) {
      return { warnings: res.validation.warnings };
    }
    return redirect(`/dashboard/services/${res.id}`);
  } catch (err) {
    if (err instanceof DeusApiError) return { error: err.message };
    return { error: "Something went wrong creating your listing. Please try again." };
  }
}

const textareaCls =
  "rounded-lg bg-secondary px-4 py-3 text-sm text-foreground shadow-01 outline-none transition-shadow placeholder:text-muted-foreground focus:ring-2 focus:ring-ring";

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-2">
      <span className="body-sm font-medium text-foreground">
        {label}
        {hint ? <span className="ml-2 text-xs font-normal text-muted-foreground">{hint}</span> : null}
      </span>
      {children}
    </label>
  );
}

const METHODS = ["POST", "GET", "PUT", "DELETE"];

export default function NewListing({ actionData }: Route.ComponentProps) {
  const navigation = useNavigation();
  const submitting = navigation.state !== "idle" && navigation.formMethod === "POST";

  const [displayName, setDisplayName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [kind, setKind] = useState<"data" | "agent">("data");
  const [mode, setMode] = useState<"proxy" | "hosted">("proxy");
  const [summary, setSummary] = useState("");
  const [description, setDescription] = useState("");
  const [tags, setTags] = useState("");
  const [proxyUrl, setProxyUrl] = useState("");
  const [ops, setOps] = useState<OpRow[]>([blankOp()]);
  const [advanced, setAdvanced] = useState<Record<number, boolean>>({});

  const effectiveSlug = slugEdited ? slug : slugify(displayName);

  function onName(v: string) {
    setDisplayName(v);
    if (!slugEdited) setSlug(slugify(v));
  }
  function updateOp(i: number, patch: Partial<OpRow>) {
    setOps((prev) => prev.map((o, idx) => (idx === i ? { ...o, ...patch } : o)));
  }
  function removeOp(i: number) {
    setOps((prev) => prev.filter((_, idx) => idx !== i));
  }

  const tagList = useMemo(
    () => tags.split(",").map((t) => t.trim()).filter(Boolean),
    [tags]
  );

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-3">
        <Link to="/dashboard" className="inline-flex w-fit items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground">
          <ArrowLeft className="size-4" />
          Back to services
        </Link>
        <h1 className="text-h1 text-foreground">New listing</h1>
        <p className="body-sm text-muted-foreground">
          Describe your service and its priced operations. You can publish it once it’s created.
        </p>
      </header>

      <ActionToast message={actionData?.error} type="error" />

      <Form method="post" className="grid grid-cols-1 gap-6 lg:grid-cols-[1.6fr_1fr]">
        {/* hidden mirrors of the smoothui-controlled fields */}
        <input type="hidden" name="display_name" value={displayName} />
        <input type="hidden" name="slug" value={effectiveSlug} />
        <input type="hidden" name="summary" value={summary} />
        <input type="hidden" name="description" value={description} />
        <input type="hidden" name="tags" value={tags} />
        <input type="hidden" name="proxy_url" value={proxyUrl} />
        <input type="hidden" name="kind" value={kind} />
        <input type="hidden" name="mode" value={mode} />
        <input type="hidden" name="operations_json" value={JSON.stringify(ops)} />

        <div className="flex flex-col gap-6">
          {actionData?.warnings && actionData.warnings.length > 0 ? (
            <div className="flex flex-col gap-1 rounded-xl bg-[color:color-mix(in_oklab,var(--warning)_16%,transparent)] px-4 py-3 text-sm text-[color:var(--warning)] shadow-01">
              <span className="font-medium">Please review:</span>
              <ul className="list-disc pl-5">
                {actionData.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          ) : null}

          <section className="flex flex-col gap-6 rounded-2xl bg-card p-6 pt-7 shadow-03">
            <h2 className="text-h3 text-foreground">Basics</h2>
            <AnimatedInput label="Display name" value={displayName} onChange={onName} placeholder="Aether Weather Oracle" />
            <AnimatedInput
              label="Slug"
              value={effectiveSlug}
              onChange={(v) => {
                setSlug(slugify(v));
                setSlugEdited(true);
              }}
              placeholder="aether-weather"
              inputClassName="mono"
            />
            <AnimatedInput label="Summary" value={summary} onChange={setSummary} placeholder="Hyper-local forecasts for any coordinate on Earth." />
            <Field label="Description" hint="optional">
              <textarea
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                rows={3}
                placeholder="What it does, inputs, and what makes it reliable."
                className={cn(textareaCls, "resize-y")}
              />
            </Field>
            <AnimatedInput label="Tags (comma-separated)" value={tags} onChange={setTags} placeholder="weather, geospatial, forecast" />
          </section>

          <section className="flex flex-col gap-5 rounded-2xl bg-card p-6 shadow-03">
            <h2 className="text-h3 text-foreground">Type & delivery</h2>
            <Field label="Kind">
              <AnimatedTabs
                tabs={[
                  { id: "data", label: "Data" },
                  { id: "agent", label: "Agents" },
                ]}
                activeTab={kind}
                variant="segment"
                layoutId="new-kind"
                onChange={(id) => setKind(id as "data" | "agent")}
                className="w-full"
              />
            </Field>
            <Field label="Delivery">
              <AnimatedTabs
                tabs={[
                  { id: "proxy", label: "Proxyforward to your URL" },
                  { id: "hosted", label: "Hostedrun in the cloud" },
                ]}
                activeTab={mode}
                variant="segment"
                layoutId="new-mode"
                onChange={(id) => setMode(id as "proxy" | "hosted")}
                className="w-full"
              />
            </Field>
            {mode === "proxy" ? (
              <AnimatedInput
                label="Proxy URL"
                value={proxyUrl}
                onChange={setProxyUrl}
                placeholder="https://api.your-service.com"
                inputClassName="mono"
              />
            ) : (
              <p className="rounded-lg bg-secondary/50 px-4 py-3 text-sm text-muted-foreground shadow-01">
                You’ll upload your code and we’ll run it in the cloud after the listing is created.
              </p>
            )}
          </section>

          <section className="flex flex-col gap-4 rounded-2xl bg-card p-6 shadow-03">
            <div className="flex items-center justify-between">
              <h2 className="text-h3 text-foreground">Operations</h2>
              <span className="eyebrow">{ops.length} total</span>
            </div>

            {ops.map((op, i) => (
              <div key={i} className="flex flex-col gap-5 rounded-xl bg-secondary/40 p-4 pt-5 shadow-01">
                <div className="flex items-center justify-between">
                  <span className="body-sm font-medium text-foreground">Operation {i + 1}</span>
                  {ops.length > 1 ? (
                    <SmoothButton type="button" variant="ghost" size="sm" onClick={() => removeOp(i)}>
                      <Trash2 className="size-3.5" />
                      Remove
                    </SmoothButton>
                  ) : null}
                </div>

                <div className="grid grid-cols-1 gap-4 sm:grid-cols-[2fr_1fr]">
                  <AnimatedInput
                    label="Name"
                    value={op.name}
                    onChange={(v) => updateOp(i, { name: v })}
                    placeholder="forecast"
                    inputClassName="mono"
                  />
                  <BasicDropdown
                    label={op.method || "POST"}
                    items={METHODS.map((m) => ({ id: m, label: m }))}
                    onChange={(item) => updateOp(i, { method: String(item.id) })}
                  />
                </div>

                <AnimatedInput
                  label="Description"
                  value={op.description}
                  onChange={(v) => updateOp(i, { description: v })}
                  placeholder="Point forecast up to 14 days"
                />

                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                  <AnimatedInput
                    label="Price (PAX per unit)"
                    value={op.price}
                    onChange={(v) => updateOp(i, { price: v })}
                    placeholder="0.0008"
                    inputClassName="mono"
                  />
                  <AnimatedInput
                    label="Unit"
                    value={op.unit}
                    onChange={(v) => updateOp(i, { unit: v })}
                    placeholder="request"
                  />
                </div>

                <button
                  type="button"
                  onClick={() => setAdvanced((p) => ({ ...p, [i]: !p[i] }))}
                  className="w-fit text-xs font-medium text-primary underline-offset-4 hover:underline"
                >
                  {advanced[i] ? "Hide" : "Add"} input / output schema
                </button>
                {advanced[i] ? (
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    <Field label="Input schema" hint="JSON">
                      <textarea
                        value={op.inputSchema}
                        onChange={(e) => updateOp(i, { inputSchema: e.target.value })}
                        rows={4}
                        placeholder={'{ "type": "object" }'}
                        className={cn(textareaCls, "mono resize-y")}
                      />
                    </Field>
                    <Field label="Output schema" hint="JSON">
                      <textarea
                        value={op.outputSchema}
                        onChange={(e) => updateOp(i, { outputSchema: e.target.value })}
                        rows={4}
                        placeholder={'{ "type": "object" }'}
                        className={cn(textareaCls, "mono resize-y")}
                      />
                    </Field>
                  </div>
                ) : null}
              </div>
            ))}

            <SmoothButton type="button" variant="secondary" onClick={() => setOps((p) => [...p, blankOp()])}>
              <Plus className="size-4" />
              Add operation
            </SmoothButton>
          </section>

          <div className="flex items-center justify-end gap-3">
            <SmoothButton asChild variant="ghost">
              <Link to="/dashboard">Cancel</Link>
            </SmoothButton>
            <SmoothButton type="submit" size="lg" disabled={submitting}>
              <Sparkles className="size-4" />
              {submitting ? "Creating…" : "Create listing"}
            </SmoothButton>
          </div>
        </div>

        {/* Live preview */}
        <aside className="lg:sticky lg:top-12 lg:self-start">
          <div className="flex flex-col gap-5 rounded-2xl bg-card p-6 shadow-04">
            <p className="eyebrow">Live preview</p>
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <KindBadge kind={kind} />
                <Badge tone="muted">{mode === "hosted" ? "Hosted" : "Proxy"}</Badge>
              </div>
              <h3 className="text-h3 text-foreground">{displayName || "Untitled service"}</h3>
              <code className="mono text-xs text-muted-foreground">{effectiveSlug || "your-slug"}</code>
            </div>
            {summary ? <p className="body-sm text-muted-foreground">{summary}</p> : null}
            {tagList.length > 0 ? (
              <div className="flex flex-wrap gap-2">
                {tagList.map((t) => (
                  <Badge key={t} tone="neutral">
                    {t}
                  </Badge>
                ))}
              </div>
            ) : null}
            <div className="flex flex-col gap-2 rounded-xl bg-secondary/50 p-4">
              <p className="eyebrow">Operations</p>
              {ops.filter((o) => o.name.trim()).length === 0 ? (
                <p className="body-sm text-muted-foreground">Add an operation to see pricing.</p>
              ) : (
                ops
                  .filter((o) => o.name.trim())
                  .map((o, i) => (
                    <div key={i} className="flex items-center justify-between gap-3 text-sm">
                      <span className="mono text-foreground">{o.name}</span>
                      <span className="text-muted-foreground">
                        {o.price ? (
                          <>
                            <PaxFlow wei={paxToWei(o.price)} /> / {o.unit || "unit"}
                          </>
                        ) : (
                          "—"
                        )}
                      </span>
                    </div>
                  ))
              )}
            </div>
          </div>
        </aside>
      </Form>
    </div>
  );
}
