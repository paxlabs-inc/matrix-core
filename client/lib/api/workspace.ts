/**
 * Workspace resource — the Neo daemon's coding-workbench backend
 * (NEO-WORKBENCH req 4, 8.1). Project-scoped views of the REAL VM workspace:
 * file tree, file read, atomic file WRITE with version hashes (the editable
 * editor), git diff, bounded exec (the terminal), and the minimal project
 * registry. All routes are Neo-owned (no /cody prefix — codyd is retired) and
 * ride the shared apiFetch (router origin + bearer + retry).
 */
import { apiFetch, apiSend } from '@/lib/api/client'

// --- projects ---------------------------------------------------------------

export interface NeoProject {
  id: string
  name: string
  root: string
  archived?: boolean
  created_at?: string
}

/** GET /projects — default project first, then registry entries. */
export async function listProjects(): Promise<NeoProject[]> {
  const res = await apiFetch<{ projects: NeoProject[] }>('/projects')
  return res.projects ?? []
}

/** POST /projects — create a project (a real workspace subdirectory). */
export async function createProject(input: { name: string; dir?: string }): Promise<NeoProject> {
  return apiSend<NeoProject>('/projects', input)
}

/** PATCH /projects/{id} — rename and/or (un)archive. */
export async function updateProject(
  id: string,
  patch: { name?: string; archived?: boolean },
): Promise<NeoProject> {
  return apiSend<NeoProject>(`/projects/${encodeURIComponent(id)}`, patch, { method: 'PATCH' })
}

/** DELETE /projects/{id}?purge=true — drop the record (and subtree). */
export async function deleteProject(
  id: string,
  opts: { purge?: boolean } = {},
): Promise<{ deleted: boolean; id: string; purged: boolean }> {
  const qs = opts.purge ? '?purge=true' : ''
  return apiSend(`/projects/${encodeURIComponent(id)}${qs}`, undefined, { method: 'DELETE' })
}

// --- workspace --------------------------------------------------------------

function projQS(project?: string): string {
  return project && project !== 'default' ? `project=${encodeURIComponent(project)}` : 'project='
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

/** GET /workspace/tree — the real project listing, depth-first. */
export async function getTree(project?: string, signal?: AbortSignal): Promise<WorkspaceTree> {
  const data = await apiFetch<WorkspaceTree>(`/workspace/tree?${projQS(project)}`, { signal })
  return { entries: data.entries ?? [], truncated: !!data.truncated }
}

export interface WorkspaceFile {
  path: string
  content: string
  size: number
  /** sha256 of the FULL file — the version the editor saves against. */
  hash: string
  truncated: boolean
}

/** GET /workspace/file — one file's content + version hash. */
export async function getFile(
  project: string | undefined,
  path: string,
  signal?: AbortSignal,
): Promise<WorkspaceFile> {
  return apiFetch<WorkspaceFile>(
    `/workspace/file?${projQS(project)}&path=${encodeURIComponent(path)}`,
    { signal },
  )
}

/** A successful save: the new authoritative version. */
export interface WorkspaceWriteOK {
  stale?: false
  path: string
  hash: string
  size: number
}

/** The save was REFUSED because the file moved under the buffer (someone —
 *  usually Neo — wrote it since base_hash). hash is the CURRENT on-disk
 *  version; the caller surfaces the conflict, never silently retries. */
export interface WorkspaceWriteStale {
  stale: true
  hash: string
}

export type WorkspaceWriteResult = WorkspaceWriteOK | WorkspaceWriteStale

/** PUT /workspace/file — atomic save; base_hash makes staleness detectable. */
export async function writeFile(
  project: string | undefined,
  path: string,
  content: string,
  baseHash?: string,
): Promise<WorkspaceWriteResult> {
  try {
    return await apiSend<WorkspaceWriteOK>(
      `/workspace/file?${projQS(project)}`,
      { path, content, base_hash: baseHash },
      { method: 'PUT' },
    )
  } catch (e) {
    const stale = parseStaleConflict(e)
    if (stale) return stale
    throw e
  }
}

/** apiFetch surfaces a 409 as an ApiError carrying the JSON body; fold the
 *  daemon's {stale:true, hash} conflict shape back into a typed result. */
function parseStaleConflict(e: unknown): WorkspaceWriteStale | null {
  if (!e || typeof e !== 'object') return null
  const err = e as { status?: number; body?: unknown }
  if (err.status !== 409) return null
  const body = (err.body ?? {}) as { stale?: boolean; hash?: string }
  if (body.stale !== true) return null
  return { stale: true, hash: typeof body.hash === 'string' ? body.hash : '' }
}

export interface WorkspaceDiff {
  git: boolean
  diff: string
  untracked: string[]
}

/** GET /workspace/diff — the real `git diff` (empty for non-git projects). */
export async function getDiff(project?: string, signal?: AbortSignal): Promise<WorkspaceDiff> {
  const data = await apiFetch<WorkspaceDiff>(`/workspace/diff?${projQS(project)}`, { signal })
  return { git: !!data.git, diff: data.diff ?? '', untracked: data.untracked ?? [] }
}

export interface ExecResult {
  cmd: string
  exit: number
  output: string
  timed_out: boolean
}

/** POST /workspace/exec — one bounded command in the project root. */
export async function execCommand(
  project: string | undefined,
  cmd: string,
  timeoutSecs?: number,
): Promise<ExecResult> {
  return apiSend<ExecResult>(`/workspace/exec?${projQS(project)}`, {
    cmd,
    timeout_secs: timeoutSecs,
  })
}

// --- preview ----------------------------------------------------------------

/** POST /workspace/preview — (re)provision the project's sandbox preview.
 *  Returns 202; the lifecycle unfolds as durable preview.* events on the
 *  conversation's live topic. */
export async function requestPreview(
  project: string | undefined,
  conversationId: string,
): Promise<{ preview: string }> {
  return apiSend<{ preview: string }>(`/workspace/preview?${projQS(project)}`, {
    conversation_id: conversationId,
  })
}
