import type {
  ApiErrorBody,
  CatalogResponse,
  CreateServiceResponse,
  DeployServiceRequest,
  DeployServiceResponse,
  DeploymentLogLine,
  DeploymentResponse,
  DiscoverRequest,
  DiscoverResponse,
  EarningsResponse,
  InvokeRequest,
  InvokeResponse,
  MeResponse,
  MyService,
  PublishServiceResponse,
  QuoteRequest,
  QuoteResponse,
  ServiceAnalytics,
  ServiceResponse,
  SpendResponse,
  UploadArtifactResponse,
} from "./deus.types";

const DEFAULT_BASE = "https://deus.paxeer.app";
const DEFAULT_TIMEOUT_MS = 15_000;

export interface DeusEnv {
  DEUS_API_URL?: string;
}

export interface CallerIdentity {
  bearer?: string;
  did?: string;
  wallet?: string;
}

export interface DeveloperIdentity {
  wallet?: string;
}

export interface DeusClientOptions {
  baseUrl?: string;
  caller?: CallerIdentity;
  developer?: DeveloperIdentity;
  timeoutMs?: number;
}

/** Typed error carrying the Deus API status + parsed envelope. */
export class DeusApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly body?: ApiErrorBody;

  constructor(status: number, body?: ApiErrorBody, fallback?: string) {
    super(body?.message || fallback || `Deus API error ${status}`);
    this.name = "DeusApiError";
    this.status = status;
    this.code = body?.error || "request_failed";
    this.body = body;
  }
}

export function resolveBaseUrl(env?: DeusEnv): string {
  const raw = env?.DEUS_API_URL?.trim();
  return (raw && raw.length > 0 ? raw : DEFAULT_BASE).replace(/\/+$/, "");
}

type RequestInitLike = {
  method?: string;
  body?: BodyInit | null;
  headers?: Record<string, string>;
  signal?: AbortSignal;
};

export class DeusClient {
  readonly baseUrl: string;
  private readonly caller?: CallerIdentity;
  private readonly developer?: DeveloperIdentity;
  private readonly timeoutMs: number;

  constructor(opts: DeusClientOptions = {}) {
    this.baseUrl = (opts.baseUrl ?? DEFAULT_BASE).replace(/\/+$/, "");
    this.caller = opts.caller;
    this.developer = opts.developer;
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  private callerHeaders(): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.caller?.bearer) h["Authorization"] = `Bearer ${this.caller.bearer}`;
    if (this.caller?.did) h["X-Caller-DID"] = this.caller.did;
    if (this.caller?.wallet) h["X-Caller-Wallet"] = this.caller.wallet;
    return h;
  }

  private developerHeaders(): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.developer?.wallet) {
      h["X-Developer-Wallet"] = this.developer.wallet;
      h["X-Developer-Address"] = this.developer.wallet;
    }
    return h;
  }

  private async request<T>(path: string, init: RequestInitLike = {}): Promise<T> {
    const url = `${this.baseUrl}${path}`;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(url, {
        method: init.method ?? "GET",
        headers: init.headers,
        body: init.body,
        signal: controller.signal,
      });
      const text = await res.text();
      const data = text ? safeJson(text) : undefined;
      if (!res.ok) {
        throw new DeusApiError(
          res.status,
          isApiErrorBody(data) ? data : undefined,
          text.slice(0, 200)
        );
      }
      return data as T;
    } catch (err) {
      if (err instanceof DeusApiError) throw err;
      if (err instanceof Error && err.name === "AbortError") {
        throw new DeusApiError(504, undefined, "Deus API timed out");
      }
      throw new DeusApiError(
        502,
        undefined,
        err instanceof Error ? err.message : "network error"
      );
    } finally {
      clearTimeout(timer);
    }
  }

  private jsonInit(
    method: string,
    body: unknown,
    extraHeaders: Record<string, string> = {}
  ): RequestInitLike {
    return {
      method,
      headers: { "Content-Type": "application/json", ...extraHeaders },
      body: body === undefined ? undefined : JSON.stringify(body),
    };
  }

  // ─── Public marketplace ─────────────────────────────────────────────────
  discover(req: DiscoverRequest): Promise<DiscoverResponse> {
    return this.request<DiscoverResponse>(
      "/v1/discover",
      this.jsonInit("POST", {
        query: req.query ?? "",
        filters: req.filters ?? {},
        limit: req.limit ?? 24,
      })
    );
  }

  getService(id: string): Promise<ServiceResponse> {
    return this.request<ServiceResponse>(`/v1/services/${encodeURIComponent(id)}`);
  }

  /**
   * Catalog browse against the REAL Go endpoint: `GET /v1/catalog?limit&offset`
   * returning `{services,total,limit,offset}` (deus/pkg/types/api.go). The Go
   * handler has no kind/query filtering, so filtered browsing routes through
   * `POST /v1/discover` (which does) and is adapted to the catalog shape.
   */
  async catalog(params: {
    limit?: number;
    offset?: number;
    kind?: string;
    query?: string;
  } = {}): Promise<CatalogResponse> {
    const limit = params.limit ?? 24;
    const offset = params.offset ?? 0;

    if (params.kind || params.query) {
      const filters: Record<string, string> = {};
      if (params.kind) filters.kind = params.kind;
      const disc = await this.discover({
        query: params.query ?? "",
        filters,
        limit: limit + offset,
      });
      const services = disc.results.slice(offset, offset + limit).map((r) => ({
        id: r.id,
        slug: r.slug,
        display_name: r.display_name,
        summary: r.summary,
        kind: r.kind,
        status: "active",
        quality_score: r.quality_score,
        uptime_bps: r.uptime_bps,
        price_wei: r.operations[0]?.price_wei,
        unit: r.operations[0]?.unit,
      }));
      return { services, total: disc.results.length, limit, offset };
    }

    const qs = new URLSearchParams();
    qs.set("limit", String(limit));
    qs.set("offset", String(offset));
    return this.request<CatalogResponse>(`/v1/catalog?${qs.toString()}`);
  }

  quote(serviceId: string, req: QuoteRequest): Promise<QuoteResponse> {
    return this.request<QuoteResponse>(
      `/v1/quote/${encodeURIComponent(serviceId)}`,
      this.jsonInit("POST", req, this.callerHeaders())
    );
  }

  invoke(serviceId: string, req: InvokeRequest): Promise<InvokeResponse> {
    const headers = { ...this.callerHeaders() };
    if (req.idempotency_key) headers["Idempotency-Key"] = req.idempotency_key;
    return this.request<InvokeResponse>(
      `/v1/invoke/${encodeURIComponent(serviceId)}`,
      this.jsonInit("POST", req, headers)
    );
  }

  // ─── Developer / dashboard ──────────────────────────────────────────────
  createService(manifest: Record<string, unknown>): Promise<CreateServiceResponse> {
    return this.request<CreateServiceResponse>(
      "/v1/services",
      this.jsonInit("POST", { manifest }, this.developerHeaders())
    );
  }

  publishService(serviceId: string): Promise<PublishServiceResponse> {
    return this.request<PublishServiceResponse>(
      `/v1/services/${encodeURIComponent(serviceId)}/publish`,
      this.jsonInit("POST", {}, this.developerHeaders())
    );
  }

  setServiceStatus(
    serviceId: string,
    action: "publish" | "pause" | "delist"
  ): Promise<{ id: string; status: string }> {
    return this.request<{ id: string; status: string }>(
      `/v1/services/${encodeURIComponent(serviceId)}/${action}`,
      this.jsonInit("POST", {}, this.developerHeaders())
    );
  }

  uploadArtifact(
    serviceId: string,
    file: Blob,
    filename: string
  ): Promise<UploadArtifactResponse> {
    const form = new FormData();
    form.append("artifact", file, filename);
    form.append("filename", filename);
    return this.request<UploadArtifactResponse>(
      `/v1/services/${encodeURIComponent(serviceId)}/artifacts`,
      { method: "POST", body: form, headers: this.developerHeaders() }
    );
  }

  deployService(
    serviceId: string,
    req: DeployServiceRequest
  ): Promise<DeployServiceResponse> {
    return this.request<DeployServiceResponse>(
      `/v1/services/${encodeURIComponent(serviceId)}/deploy`,
      this.jsonInit("POST", { runtime: "node20", ...req }, this.developerHeaders())
    );
  }

  getDeployment(serviceId: string, deploymentId: string): Promise<DeploymentResponse> {
    return this.request<DeploymentResponse>(
      `/v1/services/${encodeURIComponent(serviceId)}/deployments/${encodeURIComponent(
        deploymentId
      )}`,
      { headers: this.developerHeaders() }
    );
  }

  redeploy(serviceId: string, req: DeployServiceRequest): Promise<DeployServiceResponse> {
    return this.request<DeployServiceResponse>(
      `/v1/services/${encodeURIComponent(serviceId)}/redeploy`,
      this.jsonInit("POST", { runtime: "node20", ...req }, this.developerHeaders())
    );
  }

  async logs(serviceId: string): Promise<DeploymentLogLine[]> {
    try {
      const res = await this.request<{ logs: DeploymentLogLine[] }>(
        `/v1/services/${encodeURIComponent(serviceId)}/logs`,
        { headers: this.developerHeaders() }
      );
      return res.logs ?? [];
    } catch (err) {
      if (err instanceof DeusApiError && (err.status === 404 || err.status === 501)) {
        return [];
      }
      throw err;
    }
  }

  async myServices(): Promise<MyService[]> {
    const res = await this.request<{ services: MyService[] }>(`/v1/me/services`, {
      headers: this.developerHeaders(),
    });
    return res.services ?? [];
  }

  analytics(serviceId: string): Promise<ServiceAnalytics> {
    return this.request<ServiceAnalytics>(
      `/v1/services/${encodeURIComponent(serviceId)}/analytics`,
      { headers: this.developerHeaders() }
    );
  }

  earnings(): Promise<EarningsResponse> {
    return this.request<EarningsResponse>(`/v1/me/earnings`, {
      headers: this.developerHeaders(),
    });
  }

  payout(serviceId: string, payoutAddress: string): Promise<{ settlement_id: string }> {
    return this.request<{ settlement_id: string }>(
      `/v1/services/${encodeURIComponent(serviceId)}/payout`,
      this.jsonInit("POST", { payout_address: payoutAddress }, this.developerHeaders())
    );
  }

  me(): Promise<MeResponse> {
    return this.request<MeResponse>(`/v1/me`, {
      headers: { ...this.callerHeaders(), ...this.developerHeaders() },
    });
  }

  spend(): Promise<SpendResponse> {
    return this.request<SpendResponse>(`/v1/me/spend`, {
      headers: this.callerHeaders(),
    });
  }
}

/** Build a client bound to the worker env + (optional) request identity. */
export function createDeusClient(
  env?: DeusEnv,
  opts: Omit<DeusClientOptions, "baseUrl"> = {}
): DeusClient {
  return new DeusClient({ baseUrl: resolveBaseUrl(env), ...opts });
}

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

function isApiErrorBody(v: unknown): v is ApiErrorBody {
  return (
    typeof v === "object" &&
    v !== null &&
    "error" in v &&
    typeof (v as { error: unknown }).error === "string"
  );
}
