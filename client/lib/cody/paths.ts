/**
 * Workspace path helpers (NEO-WORKBENCH). Neo's tool steps carry the RAW
 * paths its fs tools were called with (usually absolute VM paths like
 * /workspace/<project>/src/app.ts), while the workspace API and the file
 * tree speak project-relative paths. These folds bridge the two without the
 * client having to know the VM's workspace root.
 */

/** Normalize a step/tool path to a project-relative path (best-effort). */
export function relWorkspacePath(path: string, project?: string): string {
  let p = path.replace(/\\/g, '/').replace(/^\.\//, '')
  if (!p.startsWith('/')) return p
  // Absolute: strip up to and including the project segment when present.
  if (project && project !== 'default') {
    const marker = `/${project}/`
    const i = p.indexOf(marker)
    if (i >= 0) return p.slice(i + marker.length)
  }
  // Known workspace roots.
  for (const root of ['/data/workspace/', '/workspace/']) {
    if (p.startsWith(root)) {
      p = p.slice(root.length)
      // Without a project we can only strip the root itself.
      return p
    }
  }
  // Unknown absolute root: fall back to the trailing segments.
  return p.replace(/^\/+/, '')
}

/** True when a raw step path addresses the same file as a relative path. */
export function pathsMatch(rawPath: string, relPath: string, project?: string): boolean {
  if (!rawPath || !relPath) return false
  if (rawPath === relPath) return true
  const rel = relWorkspacePath(rawPath, project)
  return rel === relPath || rawPath.endsWith('/' + relPath)
}

/** Find the live-typing buffer for a (relative) path in the typing map,
 *  whose keys are the RAW paths the daemon streamed. */
export function typingFor<T>(
  typing: Record<string, T> | undefined,
  relPath: string,
  project?: string,
): T | undefined {
  if (!typing) return undefined
  if (typing[relPath]) return typing[relPath]
  for (const key of Object.keys(typing)) {
    if (pathsMatch(key, relPath, project)) return typing[key]
  }
  return undefined
}
