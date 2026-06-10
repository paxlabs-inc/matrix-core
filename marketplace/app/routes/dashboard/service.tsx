import { useEffect, useRef, useState } from "react";
import { Link, useFetcher, useRevalidator } from "react-router";
import {
  ArrowLeft,
  Ban,
  BarChart3,
  Check,
  CloudUpload,
  Pause,
  Play,
  RefreshCw,
  Rocket,
} from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedFileUpload from "../../../components/ui/smoothui/animated-file-upload";
import AnimatedProgressBar from "../../../components/ui/smoothui/animated-progress-bar";
import AnimatedToggle from "../../../components/ui/smoothui/animated-toggle";
import type { Route } from "./+types/service";
import { getEnv } from "@/lib/env";
import {
  callerIdentityFor,
  getDeveloperIdentity,
  getWallet,
  requireUser,
} from "@/lib/auth.server";
import { createDeusClient, DeusApiError } from "@/lib/deus.server";
import type { ManifestOperation } from "@/lib/deus.types";
import { shortAddress } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Badge, CopyChip, KindBadge, Spinner, StatusBadge, Surface } from "@/components/ui";
import { ActionToast } from "@/components/feedback";
import { PaxFlow } from "@/components/pax";

type OpView = ManifestOperation & { description?: string };

export async function loader({ request, params, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, {
    developer: await getDeveloperIdentity(request, env),
    caller: callerIdentityFor(user, wallet),
  });
  const id = params.id;
  const deploymentId = new URL(request.url).searchParams.get("deployment");

  const service = await deus.getService(id);

  let deployment = null;
  if (deploymentId) {
    try {
      deployment = await deus.getDeployment(id, deploymentId);
    } catch {
      deployment = null;
    }
  }

  let logs = [] as Awaited<ReturnType<typeof deus.logs>>;
  if (service.mode === "hosted") {
    try {
      logs = await deus.logs(id);
    } catch {
      logs = [];
    }
  }

  return { service, deployment, logs, deploymentId };
}

export async function action({ request, params, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, {
    developer: await getDeveloperIdentity(request, env),
    caller: callerIdentityFor(user, wallet),
  });
  const id = params.id;
  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");

  try {
    if (intent === "publish" || intent === "pause" || intent === "delist") {
      const res = await deus.setServiceStatus(id, intent);
      return { ok: true as const, status: res.status };
    }
    if (intent === "deploy") {
      const file = form.get("artifact");
      const alwaysWarm = form.get("always_warm") === "on";
      if (!(file instanceof File) || file.size === 0) {
        return { error: "Choose a code bundle to deploy." };
      }
      const up = await deus.uploadArtifact(id, file, file.name || "bundle.tar.gz");
      const dep = await deus.deployService(id, {
        artifact_key: up.artifact_key,
        always_warm: alwaysWarm,
      });
      return { deploymentId: dep.deployment_id, runtime: dep.runtime };
    }
    if (intent === "redeploy") {
      const dep = await deus.redeploy(id, {
        artifact_key: String(form.get("artifact_key") ?? ""),
      });
      return { deploymentId: dep.deployment_id, runtime: dep.runtime };
    }
    return { error: "Unknown action." };
  } catch (err) {
    if (err instanceof DeusApiError) return { error: err.message };
    return { error: "Action failed. Please try again." };
  }
}

const logLevelClass: Record<string, string> = {
  info: "text-[color:var(--accent-fore)]",
  warn: "text-[color:var(--warning)]",
  error: "text-[color:var(--danger)]",
  debug: "text-muted-foreground",
};

export default function ManageService({ loaderData }: Route.ComponentProps) {
  const { service, logs } = loaderData;
  const manifest = service.manifest;
  const operations = (manifest?.operations ?? []) as OpView[];
  const pricing = manifest?.pricing ?? [];
  const priceFor = (name: string) => pricing.find((p) => p.operation === name);

  const lifecycle = useFetcher<typeof action>();
  const deploy = useFetcher<typeof action>();
  const poll = useFetcher<typeof loader>();
  const revalidator = useRevalidator();

  const [deploymentId, setDeploymentId] = useState<string | null>(loaderData.deploymentId);
  const [showUploader, setShowUploader] = useState(false);
  const wentLive = useRef(false);

  // Capture the deployment id returned by a deploy / redeploy submission.
  useEffect(() => {
    const d = deploy.data;
    if (d && "deploymentId" in d && d.deploymentId) {
      setDeploymentId(d.deploymentId);
      setShowUploader(false);
      wentLive.current = false;
    }
  }, [deploy.data]);

  const deployment = poll.data?.deployment ?? null;
  const status = deployment?.status;
  const runtime =
    deployment?.runtime ??
    (deploy.data && "runtime" in deploy.data ? deploy.data.runtime : "node20");

  // Poll the deployment until it goes live.
  useEffect(() => {
    if (!deploymentId || status === "active") return;
    const url = `/dashboard/services/${service.id}?deployment=${deploymentId}`;
    poll.load(url);
    const t = setInterval(() => poll.load(url), 1500);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deploymentId, status, service.id]);

  // When it goes live, refresh service + logs once.
  useEffect(() => {
    if (status === "active" && !wentLive.current) {
      wentLive.current = true;
      revalidator.revalidate();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const deployState: "none" | "deploying" | "active" =
    status === "active"
      ? "active"
      : deploymentId || deploy.state !== "idle"
        ? "deploying"
        : "none";

  const lifecycleError = lifecycle.data && "error" in lifecycle.data ? lifecycle.data.error : null;
  const deployError = deploy.data && "error" in deploy.data ? deploy.data.error : null;
  const isHosted = service.mode === "hosted";

  return (
    <div className="flex flex-col gap-8">
      <Link
        to="/dashboard"
        className="inline-flex w-fit items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
      >
        <ArrowLeft className="size-4" />
        Back to services
      </Link>

      {/* Header */}
      <Surface className="flex flex-col gap-5 p-6 shadow-03">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center gap-2">
              <KindBadge kind={service.kind} />
              <StatusBadge status={service.status} />
              <Badge tone="muted">{isHosted ? "Hosted" : "Proxy"}</Badge>
            </div>
            <h1 className="text-h1 text-foreground">{service.display_name}</h1>
            <p className="body-sm max-w-2xl text-muted-foreground">{service.summary}</p>
            <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
              <span className="eyebrow">Manifest</span>
              <code className="mono">{shortAddress(service.manifest_hash, 6)}</code>
              <CopyChip value={service.manifest_hash} />
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <SmoothButton asChild variant="ghost" size="sm">
              <Link to={`/dashboard/services/${service.id}/analytics`}>
                <BarChart3 className="size-4" />
                Analytics
              </Link>
            </SmoothButton>
            {service.status === "active" ? (
              <>
                <lifecycle.Form method="post">
                  <input type="hidden" name="intent" value="pause" />
                  <SmoothButton type="submit" variant="secondary" size="sm" disabled={lifecycle.state !== "idle"}>
                    <Pause className="size-4" />
                    Pause
                  </SmoothButton>
                </lifecycle.Form>
                <lifecycle.Form method="post">
                  <input type="hidden" name="intent" value="delist" />
                  <SmoothButton type="submit" variant="ghost" size="sm" disabled={lifecycle.state !== "idle"}>
                    <Ban className="size-4" />
                    Delist
                  </SmoothButton>
                </lifecycle.Form>
              </>
            ) : (
              <>
                <lifecycle.Form method="post">
                  <input type="hidden" name="intent" value="publish" />
                  <SmoothButton type="submit" size="sm" disabled={lifecycle.state !== "idle"}>
                    <Play className="size-4" />
                    {service.status === "paused" ? "Resume" : "Publish"}
                  </SmoothButton>
                </lifecycle.Form>
                {service.status !== "draft" && service.status !== "delisted" ? (
                  <lifecycle.Form method="post">
                    <input type="hidden" name="intent" value="delist" />
                    <SmoothButton type="submit" variant="ghost" size="sm" disabled={lifecycle.state !== "idle"}>
                      <Ban className="size-4" />
                      Delist
                    </SmoothButton>
                  </lifecycle.Form>
                ) : null}
              </>
            )}
          </div>
        </div>
        <ActionToast message={lifecycleError} type="error" />
      </Surface>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[1.4fr_1fr]">
        <div className="flex flex-col gap-6">
          {/* Hosted deployment OR proxy endpoint */}
          {isHosted ? (
            <Surface className="flex flex-col gap-5 p-6">
              <div className="flex items-center justify-between">
                <h2 className="text-h3 text-foreground">Cloud deployment</h2>
                {deployState === "active" ? (
                  <span className="inline-flex items-center gap-2 rounded-full bg-[color:color-mix(in_oklab,var(--success)_16%,transparent)] px-3 py-1 text-xs font-medium text-[color:var(--success)]">
                    <span className="size-1.5 rounded-full bg-current" />
                    Live · {runtime}
                  </span>
                ) : deployState === "deploying" ? (
                  <span className="inline-flex items-center gap-2 text-xs font-medium text-muted-foreground">
                    <Spinner className="size-3.5" />
                    Deploying…
                  </span>
                ) : (
                  <Badge tone="muted">Not deployed</Badge>
                )}
              </div>

              {deployState === "deploying" ? (
                <div className="flex flex-col gap-4 rounded-xl bg-secondary/50 p-5 shadow-01">
                  <AnimatedProgressBar
                    key={deploy.state === "idle" ? "building" : "uploading"}
                    value={deploy.state !== "idle" ? 35 : 80}
                    label={deploy.state !== "idle" ? "Uploading your bundle…" : "Deploying to the cloud…"}
                    color="var(--color-brand)"
                  />
                  <p className="text-xs text-muted-foreground">
                    Building your bundle and warming an isolate. This usually takes a few seconds.
                  </p>
                </div>
              ) : deployState === "active" ? (
                <div className="flex flex-col gap-4">
                  <div className="flex items-center gap-3 rounded-xl bg-secondary/50 p-5 shadow-01">
                    <span className="flex size-9 items-center justify-center rounded-full bg-[color:color-mix(in_oklab,var(--success)_18%,transparent)] text-[color:var(--success)]">
                      <Check className="size-5" />
                    </span>
                    <div>
                      <p className="text-sm font-medium text-foreground">Your code is live in the cloud</p>
                      <p className="text-xs text-muted-foreground">
                        Running on {runtime}
                        {deployment?.always_warm ? " · always warm" : ""}.
                      </p>
                    </div>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <deploy.Form method="post">
                      <input type="hidden" name="intent" value="redeploy" />
                      <SmoothButton type="submit" variant="secondary" size="sm" disabled={deploy.state !== "idle"}>
                        <RefreshCw className="size-4" />
                        Redeploy
                      </SmoothButton>
                    </deploy.Form>
                    <SmoothButton
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() => setShowUploader((v) => !v)}
                    >
                      <CloudUpload className="size-4" />
                      Deploy new version
                    </SmoothButton>
                  </div>
                  {showUploader ? <DeployForm fetcher={deploy} /> : null}
                </div>
              ) : (
                <DeployForm fetcher={deploy} />
              )}

              <ActionToast message={deployError} type="error" />
            </Surface>
          ) : manifest?.endpoint?.proxy_url ? (
            <Surface className="flex flex-col gap-3 p-6">
              <h2 className="text-h3 text-foreground">Upstream endpoint</h2>
              <p className="body-sm text-muted-foreground">Calls to this service are forwarded to your own server.</p>
              <div className="flex items-center justify-between gap-3 rounded-lg bg-secondary/60 px-4 py-3 shadow-01">
                <code className="mono truncate text-sm text-foreground">{manifest.endpoint.proxy_url}</code>
                <CopyChip value={manifest.endpoint.proxy_url} />
              </div>
            </Surface>
          ) : null}

          {/* Operations & pricing */}
          <Surface className="flex flex-col gap-4 p-6">
            <h2 className="text-h3 text-foreground">Operations & pricing</h2>
            <div className="flex flex-col gap-2">
              {operations.length === 0 ? (
                <p className="body-sm text-muted-foreground">No operations defined.</p>
              ) : (
                operations.map((op) => {
                  const p = priceFor(op.name);
                  return (
                    <div
                      key={op.name}
                      className="flex flex-col gap-1 rounded-xl bg-secondary/40 p-4 shadow-01 sm:flex-row sm:items-center sm:justify-between"
                    >
                      <div className="flex flex-col gap-1">
                        <div className="flex items-center gap-2">
                          <code className="mono text-sm text-foreground">{op.name}</code>
                          <Badge tone="muted">{op.method ?? "POST"}</Badge>
                        </div>
                        {op.description ? (
                          <p className="text-xs text-muted-foreground">{op.description}</p>
                        ) : null}
                      </div>
                      <div className="text-right">
                        <p className="text-sm font-medium text-foreground tabular-nums">
                          {p ? <PaxFlow wei={p.price_wei} /> : "—"}
                        </p>
                        <p className="text-xs text-muted-foreground">per {p?.unit ?? "unit"}</p>
                      </div>
                    </div>
                  );
                })
              )}
            </div>
          </Surface>
        </div>

        {/* Logs */}
        <div className="flex flex-col gap-6">
          {isHosted ? (
            <Surface className="flex flex-col gap-4 p-6">
              <div className="flex items-center justify-between">
                <h2 className="text-h3 text-foreground">Logs</h2>
                <button
                  type="button"
                  onClick={() => revalidator.revalidate()}
                  className="flex items-center gap-2 text-xs text-muted-foreground transition-colors hover:text-foreground"
                >
                  <RefreshCw className={cn("size-3.5", revalidator.state !== "idle" && "animate-spin")} />
                  Refresh
                </button>
              </div>
              <div className="max-h-80 overflow-auto rounded-xl bg-secondary/70 p-4 shadow-01">
                {logs.length === 0 ? (
                  <p className="body-sm text-muted-foreground">No logs yet. Deploy your code to see activity.</p>
                ) : (
                  <div className="flex flex-col gap-2">
                    {logs.map((line, i) => (
                      <div key={i} className="flex gap-3 text-xs leading-relaxed">
                        <span className="mono shrink-0 text-muted-foreground">
                          {new Date(line.ts).toLocaleTimeString("en-US", { hour12: false })}
                        </span>
                        <span className={cn("mono shrink-0 uppercase", logLevelClass[line.level] ?? "text-muted-foreground")}>
                          {line.level}
                        </span>
                        <span className="mono text-foreground/90">{line.message}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </Surface>
          ) : (
            <Surface className="flex flex-col gap-3 p-6">
              <h2 className="text-h3 text-foreground">Health</h2>
              <p className="body-sm text-muted-foreground">
                This proxy service runs on your own infrastructure. Track usage and reliability in analytics.
              </p>
              <SmoothButton asChild variant="secondary" size="sm" className="w-fit">
                <Link to={`/dashboard/services/${service.id}/analytics`}>
                  <BarChart3 className="size-4" />
                  View analytics
                </Link>
              </SmoothButton>
            </Surface>
          )}
        </div>
      </div>
    </div>
  );
}

function DeployForm({ fetcher }: { fetcher: ReturnType<typeof useFetcher<typeof action>> }) {
  const busy = fetcher.state !== "idle";
  const [file, setFile] = useState<File | null>(null);
  const [alwaysWarm, setAlwaysWarm] = useState(false);

  function submit() {
    if (!file) return;
    const form = new FormData();
    form.set("intent", "deploy");
    form.set("artifact", file, file.name);
    if (alwaysWarm) form.set("always_warm", "on");
    fetcher.submit(form, { method: "post", encType: "multipart/form-data" });
  }

  return (
    <div className="flex flex-col gap-4 rounded-xl bg-secondary/40 p-5 shadow-01">
      <div className="flex flex-col gap-2">
        <span className="body-sm font-medium text-foreground">Code bundle</span>
        <AnimatedFileUpload
          accept=".tar.gz,.gz,.zip,.js"
          onFilesSelected={(files) => setFile(files[0] ?? null)}
        />
        <span className="text-xs text-muted-foreground">.tar.gz, .zip, or a single .js entrypoint</span>
      </div>
      <div className="flex items-center gap-3 text-sm text-foreground">
        <AnimatedToggle
          checked={alwaysWarm}
          onChange={setAlwaysWarm}
          size="sm"
          label="Keep always warm"
        />
        Keep always warm
        <span className="text-xs text-muted-foreground">(no cold startshigher cost)</span>
      </div>
      <SmoothButton type="button" onClick={submit} disabled={busy || !file} className="w-fit">
        {busy ? <Spinner /> : <Rocket className="size-4" />}
        {busy ? "Uploading…" : "Deploy to the cloud"}
      </SmoothButton>
    </div>
  );
}
