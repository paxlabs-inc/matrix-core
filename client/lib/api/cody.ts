/**
 * Cody resource — the /cody sibling app's HTTP surface over codyd.
 *
 * codyd sits behind the same router as every other Matrix resource, so this
 * module calls the shared `apiFetch`/`apiSend` wrapper (Bearer auth, retry,
 * request-id) exactly like `chat.ts`/`conversations.ts` — no new transport
 * plumbing. Live events ride the shared SSE hub via `subscribeEvents`; this
 * module only covers the request/response routes.
 *
 * Every route + field here mirrors codyd's internal/server (server.go,
 * workspace.go, projects.go). The SSE event family it produces is folded by
 * hooks/api/useCody.ts, not here.
 */
import { apiFetch, apiSend } from '@/lib/api/client'
import type { TraceEvent } from '@/lib/api/conversations'

// codyd is proxied by the router under the /cody/* prefix (its own :8090
// engine), so every Cody route is addressed with this base to disambiguate
// from Neo's identical /chat, /events, /intents paths on the daemon front.
const CODY = '/cody'

export type CodyMode = 'prototype' | 'engineer' | 'architect'
export type CodyRunStatus = 'running' | 'completed' | 'failed' | 'stopped' | 'needs_input'

/** A project in the codyd registry (GET/POST /projects, GET/PATCH /projects/:id). */
export interface CodyProject {
  id: string
  name: string
  root: string
  mode: CodyMode
  archived?: boolean
  created_at: string
}

interface ProjectsResponse {
  projects: CodyProject[]
}

/** GET /projects — the registry, default project synthesized first. */
export async function listProjects(signal?: AbortSignal): Promise<CodyProject[]> {
  const data = await apiFetch<ProjectsResponse>(`${CODY}/projects`, { signal })
  return data.projects ?? []
}

/** POST /projects — create a project with a mode; slugified into /workspace/<dir>. */
export async function createProject(
  input: { name: string; mode: CodyMode; dir?: string },
  signal?: AbortSignal,
): Promise<CodyProject> {
  return apiSend<CodyProject>(`${CODY}/projects`, input, { signal })
}

/** GET /projects/:id. */
export async function getProject(id: string, signal?: AbortSignal): Promise<CodyProject> {
  return apiFetch<CodyProject>(`${CODY}/projects/${encodeURIComponent(id)}`, { signal })
}

/** PATCH /projects/:id — change the project mode (rejected for the default project). */
export async function setProjectMode(
  id: string,
  mode: CodyMode,
  signal?: AbortSignal,
): Promise<CodyProject> {
  return updateProject(id, { mode }, signal)
}

/** PATCH /projects/:id — any subset of {mode, name, archived} (not the default project). */
export async function updateProject(
  id: string,
  patch: { mode?: CodyMode; name?: string; archived?: boolean },
  signal?: AbortSignal,
): Promise<CodyProject> {
  return apiSend<CodyProject>(`${CODY}/projects/${encodeURIComponent(id)}`, patch, {
    method: 'PATCH',
    signal,
  })
}

/**
 * DELETE /projects/:id — remove a project from the registry. Refused (409)
 * while a run is live in it. With purge=true the /workspace/<dir> subtree is
 * also removed from the machine.
 */
export async function deleteProject(
  id: string,
  opts: { purge?: boolean } = {},
  signal?: AbortSignal,
): Promise<{ deleted: boolean; id: string; purged: boolean }> {
  const q = opts.purge ? '?purge=true' : ''
  return apiSend<{ deleted: boolean; id: string; purged: boolean }>(
    `${CODY}/projects/${encodeURIComponent(id)}${q}`,
    undefined,
    { method: 'DELETE', signal },
  )
}

/** POST /chat — dispatch a run. `mode` is only honored for the default project. */
export interface CodyChatInput {
  message: string
  conversation_id?: string
  mode?: CodyMode
  project_id?: string
  /** Pasted spec document (wins over spec_path when both are set). */
  spec?: string
  /** Workspace-relative path to a spec file to adopt. */
  spec_path?: string
}

export interface CodyDispatch {
  conversation_id: string
  kind: 'dispatch'
  fresh: boolean
  intent_id: string
  events_url: string
  poll_url: string
}

export async function submitChat(
  input: CodyChatInput,
  signal?: AbortSignal,
): Promise<CodyDispatch> {
  return apiSend<CodyDispatch>(`${CODY}/chat`, input, { signal })
}

/**
 * GET /conversations — the server-side, cross-device run history sourced from
 * codyd's durable ledgers (newest first). This is History's source of truth;
 * localStorage remains only a fast-path cache. Mirrors conversationSummary
 * (server.go handleConversationList).
 */
export interface CodyConversationSummary {
  id: string
  title: string
  status: CodyRunStatus
  mode: CodyMode
  project?: string
  updated_at: string
}

export async function getConversations(signal?: AbortSignal): Promise<CodyConversationSummary[]> {
  const data = await apiFetch<{ conversations: CodyConversationSummary[] }>(
    `${CODY}/conversations`,
    { signal },
  )
  return data.conversations ?? []
}

/**
 * GET /conversations/:id/trace — the durable run timeline for reopen.
 * codyd's trace envelope adds `status` + `mode` over the Neo shape; `live_run`
 * is non-empty only while a run is in flight, telling the caller to also
 * subscribe to /events?intent_id=<live_run> after replaying `events`.
 */
export interface CodyTrace {
  intent_id: string
  live_run: string
  status: CodyRunStatus
  mode: CodyMode
  events: TraceEvent[]
}

export async function getCodyTrace(convID: string, signal?: AbortSignal): Promise<CodyTrace> {
  return apiFetch<CodyTrace>(`${CODY}/conversations/${encodeURIComponent(convID)}/trace`, {
    signal,
  })
}

/** POST /intents/:id/stop — stop a live run. */
export async function stopRun(
  intentID: string,
  signal?: AbortSignal,
): Promise<{ stopped: boolean }> {
  return apiSend<{ stopped: boolean }>(
    `${CODY}/intents/${encodeURIComponent(intentID)}/stop`,
    undefined,
    {
      signal,
    },
  )
}

/**
 * POST /intents/:id/answer — resolve a needs_input stall. Both stop-and-ask
 * and SDR/DLR approval ride this channel: free text, or a decision verdict
 * (approve, or override with a payload). Must carry text or a verdict.
 */
export interface AnswerVerdict {
  decision: 'approve' | 'override'
  /** Arbitrary JSON payload for an override (e.g. the chosen stack/design). */
  payload?: unknown
}

export async function answerIntent(
  intentID: string,
  answer: { text?: string; verdict?: AnswerVerdict },
  signal?: AbortSignal,
): Promise<{ answered: boolean }> {
  return apiSend<{ answered: boolean }>(
    `${CODY}/intents/${encodeURIComponent(intentID)}/answer`,
    answer,
    {
      signal,
    },
  )
}

/** POST /intents/:id/steer — fold a mid-run correction in at the next boundary. */
export async function steerIntent(
  intentID: string,
  text: string,
  signal?: AbortSignal,
): Promise<{ steered: boolean }> {
  return apiSend<{ steered: boolean }>(
    `${CODY}/intents/${encodeURIComponent(intentID)}/steer`,
    { text },
    { signal },
  )
}

// ── Workspace routes (project-scoped via ?project=<id>) ──────────────────────

function projectQuery(projectID?: string, extra?: Record<string, string>): string {
  const qs = new URLSearchParams()
  if (projectID) qs.set('project', projectID)
  for (const [k, v] of Object.entries(extra ?? {})) qs.set(k, v)
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export interface TreeEntry {
  path: string
  dir: boolean
  size?: number
}

export interface WorkspaceTree {
  entries: TreeEntry[]
  truncated: boolean
}

/** GET /workspace/tree — the project file tree, truncation surfaced honestly. */
export async function getTree(projectID?: string, signal?: AbortSignal): Promise<WorkspaceTree> {
  const data = await apiFetch<WorkspaceTree>(`${CODY}/workspace/tree${projectQuery(projectID)}`, {
    signal,
  })
  return { entries: data.entries ?? [], truncated: !!data.truncated }
}

export interface WorkspaceFile {
  path: string
  content: string
  size: number
  truncated: boolean
}

/** GET /workspace/file — a single file's content for the read-only viewer. */
export async function getFile(
  path: string,
  projectID?: string,
  signal?: AbortSignal,
): Promise<WorkspaceFile> {
  return apiFetch<WorkspaceFile>(`${CODY}/workspace/file${projectQuery(projectID, { path })}`, {
    signal,
  })
}

export interface WorkspaceDiff {
  git: boolean
  diff: string
  untracked: string[]
}

/** GET /workspace/diff — staged+unstaged unified diff plus untracked paths. */
export async function getDiff(projectID?: string, signal?: AbortSignal): Promise<WorkspaceDiff> {
  const data = await apiFetch<WorkspaceDiff>(`${CODY}/workspace/diff${projectQuery(projectID)}`, {
    signal,
  })
  return { git: !!data.git, diff: data.diff ?? '', untracked: data.untracked ?? [] }
}

export interface ExecResult {
  cmd: string
  exit: number
  output: string
  timed_out: boolean
}

/** POST /workspace/exec — bounded command-run (Architect terminal); v1 not a PTY. */
export async function execCommand(
  cmd: string,
  opts: { projectID?: string; timeoutSecs?: number } = {},
  signal?: AbortSignal,
): Promise<ExecResult> {
  return apiSend<ExecResult>(
    `${CODY}/workspace/exec${projectQuery(opts.projectID)}`,
    { cmd, timeout_secs: opts.timeoutSecs },
    { signal },
  )
}

export interface SnapshotInfo {
  id: string
  label: string
  created_at?: string
}

/** POST /workspace/snapshot — take a named checkpoint. */
export async function createSnapshot(
  input: { label?: string; conversation_id?: string; projectID?: string },
  signal?: AbortSignal,
): Promise<{ id: string; label: string }> {
  return apiSend<{ id: string; label: string }>(
    `${CODY}/workspace/snapshot${projectQuery(input.projectID)}`,
    { label: input.label, conversation_id: input.conversation_id },
    { signal },
  )
}

/** GET /workspace/snapshots — the checkpoint list. */
export async function listSnapshots(
  projectID?: string,
  signal?: AbortSignal,
): Promise<SnapshotInfo[]> {
  const data = await apiFetch<{ snapshots: SnapshotInfo[] }>(
    `${CODY}/workspace/snapshots${projectQuery(projectID)}`,
    { signal },
  )
  return data.snapshots ?? []
}

/** POST /workspace/restore — restore a checkpoint; refused (409) while a run is live. */
export async function restoreSnapshot(
  input: { id: string; conversation_id?: string; projectID?: string },
  signal?: AbortSignal,
): Promise<{ restored: boolean; id: string }> {
  return apiSend<{ restored: boolean; id: string }>(
    `${CODY}/workspace/restore${projectQuery(input.projectID)}`,
    { id: input.id, conversation_id: input.conversation_id },
    { signal },
  )
}

/**
 * POST /workspace/preview — (re)provision the project's on-demand sandbox
 * preview (req 7.3, single client action). Returns 202; the lifecycle unfolds
 * on preview.pending/ready/failed events the reducer folds into run.preview.
 * Returns 503 when previews are not configured in the environment.
 */
export async function rePreview(
  input: { conversation_id: string; projectID?: string },
  signal?: AbortSignal,
): Promise<{ preview: string }> {
  return apiSend<{ preview: string }>(
    `${CODY}/workspace/preview${projectQuery(input.projectID)}`,
    { conversation_id: input.conversation_id },
    { signal },
  )
}

// ── Media plane (POST /upload, GET /media/<name>) ─────────────────────────────

export type CodyMediaKind = 'image' | 'video' | 'audio' | 'file'

/** An upload stored on the user's machine volume (mirrors Neo's media plane). */
export interface CodyUploadedMedia {
  /** codyd-relative reference, e.g. "/media/2026..ab12.png". */
  url: string
  name: string
  kind: CodyMediaKind
  mime: string
  bytes: number
}

/** POST /upload — store one attachment on the machine volume. */
export async function uploadCodyMedia(file: Blob, filename: string): Promise<CodyUploadedMedia> {
  const form = new FormData()
  form.append('file', file, filename)
  return apiFetch<CodyUploadedMedia>(`${CODY}/upload`, {
    method: 'POST',
    body: form,
    // Uploads are not idempotent and can be large; no retries, generous timeout.
    retries: 0,
    timeoutMs: 120_000,
  })
}

/**
 * Fetch a codyd "/media/<name>" reference as an authed blob object URL (an
 * <img> src cannot carry the bearer). Caller owns revoking the URL.
 */
export async function loadCodyMediaObjectURL(ref: string): Promise<string | null> {
  if (!ref) return null
  try {
    const path = ref.startsWith('/media/') ? `${CODY}${ref}` : ref
    const res = await apiFetch<Response>(path, { raw: true, timeoutMs: 60_000 })
    const blob = await res.blob()
    return URL.createObjectURL(blob)
  } catch {
    return null
  }
}

// ── GitHub integration (/github/*) ────────────────────────────────────────────

/** GET /github/status — whether a token is linked and as whom. */
export interface GithubStatus {
  linked: boolean
  login?: string
}

export async function githubStatus(signal?: AbortSignal): Promise<GithubStatus> {
  return apiFetch<GithubStatus>(`${CODY}/github/status`, { signal })
}

/** POST /github/link — validate + store a token on the user's machine volume. */
export async function githubLink(
  token: string,
  signal?: AbortSignal,
): Promise<{ linked: boolean; login: string }> {
  return apiSend<{ linked: boolean; login: string }>(`${CODY}/github/link`, { token }, { signal })
}

/** DELETE /github/link — remove the stored token. */
export async function githubUnlink(signal?: AbortSignal): Promise<{ linked: boolean }> {
  return apiSend<{ linked: boolean }>(`${CODY}/github/link`, undefined, {
    method: 'DELETE',
    signal,
  })
}

/** One repo row from GET /github/repos (most recently pushed first). */
export interface GithubRepo {
  full_name: string
  name: string
  private: boolean
  description: string
  default_branch: string
  clone_url: string
  pushed_at: string
}

export async function githubRepos(signal?: AbortSignal): Promise<GithubRepo[]> {
  const data = await apiFetch<{ repos: GithubRepo[] }>(`${CODY}/github/repos`, { signal })
  return data.repos ?? []
}

/**
 * POST /github/clone — clone a repo onto the machine as a NEW project
 * (/workspace/<dir>, registered with a mode). Returns the created project.
 */
export async function githubClone(
  input: { full_name?: string; url?: string; name?: string; mode?: CodyMode },
  signal?: AbortSignal,
): Promise<CodyProject> {
  return apiSend<CodyProject>(`${CODY}/github/clone`, input, {
    signal,
    // A clone can take a while on big repos; do not retry a non-idempotent op.
    retries: 0,
    timeoutMs: 300_000,
  })
}

/**
 * POST /github/create — create a repo on GitHub and wire it as the addressed
 * project's origin remote (no push; the user drives commits).
 */
export interface GithubCreated {
  full_name: string
  url: string
  clone_url: string
  project: string
  remote: string
}

export async function githubCreate(
  input: { name: string; private?: boolean; project_id?: string },
  signal?: AbortSignal,
): Promise<GithubCreated> {
  return apiSend<GithubCreated>(`${CODY}/github/create`, input, { signal, retries: 0 })
}
