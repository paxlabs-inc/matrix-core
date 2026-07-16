/**
 * Editor buffer controller (NEO-WORKBENCH req 5) — the pure state machine
 * behind the editable editor's honest conflict posture. The CodeMirror pane
 * is a thin binding over this: open loads a file + its version hash, edits
 * mark the buffer dirty, saves go through the workspace write API with
 * base_hash, and a STALE save (Neo wrote the file underneath the buffer)
 * surfaces a conflict with an explicit take-Neo's / keep-mine choice — user
 * bytes are NEVER lost without that choice.
 */
import {
  getFile,
  writeFile,
  type WorkspaceFile,
  type WorkspaceWriteResult,
} from '@/lib/api/workspace'

export interface EditorConflict {
  /** Neo's current on-disk content (the "theirs" side of the diff). */
  theirs: string
  /** The on-disk version hash to save against after a choice. */
  theirsHash: string
}

export interface EditorBuffer {
  path: string
  /** The buffer's current text (the user's, once dirty). */
  content: string
  /** The version hash the buffer was loaded from / last saved as. */
  baseHash: string
  /** True when the buffer diverges from its loaded/saved version. */
  dirty: boolean
  /** The file was larger than the read cap — editing is disabled. */
  truncated: boolean
  /** A pending Neo-vs-user conflict awaiting an explicit choice. */
  conflict?: EditorConflict
  saving?: boolean
  error?: string
}

export function bufferFromFile(file: WorkspaceFile): EditorBuffer {
  return {
    path: file.path,
    content: file.content,
    baseHash: file.hash,
    dirty: false,
    truncated: file.truncated,
  }
}

/** A user keystroke: dirty tracks real divergence, not edit events. */
export function editBuffer(buf: EditorBuffer, content: string): EditorBuffer {
  if (content === buf.content) return buf
  return { ...buf, content, dirty: true }
}

/** Fold a save's result into the buffer. */
export function foldSaveResult(buf: EditorBuffer, result: WorkspaceWriteResult): EditorBuffer {
  if (result.stale) {
    // The file moved under the buffer — keep the user's bytes untouched and
    // surface the conflict (the caller fetches "theirs" for the diff).
    return { ...buf, saving: false, conflict: { theirs: '', theirsHash: result.hash } }
  }
  return { ...buf, saving: false, dirty: false, baseHash: result.hash, conflict: undefined }
}

/** Resolve a surfaced conflict. 'keep-mine' re-saves the user's buffer
 *  against the CURRENT on-disk version; 'take-neo' replaces the buffer with
 *  Neo's content (the user chose to discard their edit — explicitly). */
export function resolveConflict(buf: EditorBuffer, choice: 'take-neo' | 'keep-mine'): EditorBuffer {
  const c = buf.conflict
  if (!c) return buf
  if (choice === 'take-neo') {
    return {
      ...buf,
      content: c.theirs,
      baseHash: c.theirsHash,
      dirty: false,
      conflict: undefined,
    }
  }
  // keep-mine: buffer stays; the save retries against theirsHash.
  return { ...buf, baseHash: c.theirsHash, conflict: undefined, dirty: true }
}

/** When Neo settles a write to the open file and the buffer is CLEAN, the
 *  editor just converges on the new content. A dirty buffer instead surfaces
 *  the conflict — never a silent clobber. */
export function foldNeoWrite(buf: EditorBuffer, file: WorkspaceFile): EditorBuffer {
  if (file.path !== buf.path) return buf
  if (!buf.dirty) return bufferFromFile(file)
  if (file.hash === buf.baseHash) return buf
  return { ...buf, conflict: { theirs: file.content, theirsHash: file.hash } }
}

// --- async helpers (thin IO around the pure folds) --------------------------

export async function openBuffer(project: string | undefined, path: string): Promise<EditorBuffer> {
  return bufferFromFile(await getFile(project, path))
}

/** Save the buffer. On a stale conflict the current on-disk content is
 *  fetched so the conflict card can show a real diff. */
export async function saveBuffer(
  project: string | undefined,
  buf: EditorBuffer,
): Promise<EditorBuffer> {
  const result = await writeFile(project, buf.path, buf.content, buf.baseHash || undefined)
  const next = foldSaveResult(buf, result)
  if (next.conflict) {
    try {
      const theirs = await getFile(project, buf.path)
      return {
        ...next,
        conflict: { theirs: theirs.content, theirsHash: theirs.hash },
      }
    } catch {
      return next
    }
  }
  return next
}
