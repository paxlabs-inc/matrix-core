/**
 * Recent runs — a small localStorage record of runs this client dispatched,
 * per project. codyd exposes a run's trace by conversation id but no
 * "list runs" endpoint, so the History page reflects what this browser has
 * started (honest about its scope rather than implying a server-side list).
 * Each entry's live outcome is resolved from its trace on open.
 */
export interface RecentRun {
  convID: string
  projectID: string
  title: string
  /** Epoch ms; passed in by the caller (module stays pure/testable). */
  startedAt: number
}

const KEY = 'cody:recent-runs:v1'
const MAX = 50

function read(): RecentRun[] {
  if (typeof window === 'undefined') return []
  try {
    const raw = window.localStorage.getItem(KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? (parsed as RecentRun[]) : []
  } catch {
    return []
  }
}

function write(runs: RecentRun[]): void {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(KEY, JSON.stringify(runs.slice(0, MAX)))
  } catch {
    /* quota / disabled storage — recent runs are best-effort */
  }
}

/** Record (or move-to-front) a dispatched run. */
export function rememberRun(run: RecentRun): RecentRun[] {
  const rest = read().filter((r) => r.convID !== run.convID)
  const next = [run, ...rest]
  write(next)
  return next
}

/** All remembered runs, newest first, optionally scoped to a project. */
export function listRecentRuns(projectID?: string): RecentRun[] {
  const all = read().sort((a, b) => b.startedAt - a.startedAt)
  return projectID ? all.filter((r) => r.projectID === projectID) : all
}

export function forgetRun(convID: string): RecentRun[] {
  const next = read().filter((r) => r.convID !== convID)
  write(next)
  return next
}
