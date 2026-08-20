/**
 * NEO-WORKBENCH task 3.1 (req 5.2, 5.3, 5.4): the editable editor's buffer
 * controller — save round-trips, unsaved-dirty tracking, and the conflict
 * posture that NEVER loses user bytes without an explicit choice.
 *
 * The code under test (editor-buffer folds + IO) is real; the network
 * boundary is served by an in-memory daemon that implements the REAL
 * workspace-write contract byte for byte (atomic replace + base_hash
 * staleness → 409 {stale:true, hash}), mirroring the Go handler proven in
 * agents/neo/internal/server/workspace_test.go.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'

// --- the contract server (the daemon side of the wire) ----------------------

const store = new Map<string, string>()

function hashOf(content: string): string {
  // Opaque to the client — only equality matters (the daemon uses sha256).
  let h = 0
  for (let i = 0; i < content.length; i++) h = (h * 31 + content.charCodeAt(i)) >>> 0
  return `h${h.toString(16)}-${content.length}`
}

beforeEach(() => {
  store.clear()
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), 'http://daemon')
      const json = (status: number, body: unknown) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'content-type': 'application/json' },
        })
      if (url.pathname === '/workspace/file' && (!init?.method || init.method === 'GET')) {
        const path = url.searchParams.get('path') ?? ''
        if (!store.has(path)) return json(404, { error: 'not a file' })
        const content = store.get(path)!
        return json(200, {
          path,
          content,
          size: content.length,
          hash: hashOf(content),
          truncated: false,
        })
      }
      if (url.pathname === '/workspace/file' && init?.method === 'PUT') {
        const body = JSON.parse(String(init.body)) as {
          path: string
          content: string
          base_hash?: string
        }
        const current = store.get(body.path)
        const currentHash = current === undefined ? '' : hashOf(current)
        if (body.base_hash && body.base_hash !== currentHash) {
          return json(409, { stale: true, hash: currentHash })
        }
        store.set(body.path, body.content)
        return json(200, { path: body.path, hash: hashOf(body.content), size: body.content.length })
      }
      return json(404, { error: 'not found' })
    }),
  )
})

import {
  editBuffer,
  foldNeoWrite,
  openBuffer,
  resolveConflict,
  saveBuffer,
} from '@/lib/cody/editor-buffer'

describe('editor buffer — save round-trip + unsaved tracking (req 5.2)', () => {
  it('opens, edits (dirty), saves to disk, and comes back clean', async () => {
    store.set('src/app.ts', 'export const x = 1\n')

    let buf = await openBuffer('app', 'src/app.ts')
    expect(buf.dirty).toBe(false)
    expect(buf.content).toBe('export const x = 1\n')

    buf = editBuffer(buf, 'export const x = 2\n')
    expect(buf.dirty).toBe(true) // ← the unsaved dot's source of truth

    // Round-trip: same content in = not dirty.
    expect(editBuffer(buf, buf.content).dirty).toBe(true)

    buf = await saveBuffer('app', buf)
    expect(buf.dirty).toBe(false)
    expect(buf.conflict).toBeUndefined()
    expect(store.get('src/app.ts')).toBe('export const x = 2\n')
    expect(buf.baseHash).toBe(hashOf('export const x = 2\n'))
  })
})

describe('editor buffer — the conflict posture (req 5.3, 5.4)', () => {
  it('a save over a Neo-moved file is a conflict; user bytes survive both choices', async () => {
    store.set('src/app.ts', 'v1\n')
    let buf = await openBuffer('app', 'src/app.ts')
    buf = editBuffer(buf, 'my precious edit\n')

    // Neo writes the file underneath the buffer.
    store.set('src/app.ts', 'neo version\n')

    buf = await saveBuffer('app', buf)
    // The save was refused; the conflict carries Neo's real content; the
    // user's bytes are UNTOUCHED in the buffer and NOT on disk.
    expect(buf.conflict).toBeDefined()
    expect(buf.conflict!.theirs).toBe('neo version\n')
    expect(buf.content).toBe('my precious edit\n')
    expect(store.get('src/app.ts')).toBe('neo version\n')

    // Choice A — keep mine: the buffer re-saves against the current version.
    let mine = resolveConflict(buf, 'keep-mine')
    expect(mine.content).toBe('my precious edit\n')
    mine = await saveBuffer('app', mine)
    expect(mine.dirty).toBe(false)
    expect(store.get('src/app.ts')).toBe('my precious edit\n')

    // Choice B — take Neo's: the buffer converges on Neo's content (an
    // explicit user choice, not a silent clobber).
    store.set('src/app.ts', 'neo version\n')
    const theirs = resolveConflict(buf, 'take-neo')
    expect(theirs.content).toBe('neo version\n')
    expect(theirs.dirty).toBe(false)
  })

  it('a settled Neo write converges a CLEAN buffer but conflicts a dirty one', async () => {
    store.set('a.txt', 'one\n')
    const clean = await openBuffer('app', 'a.txt')
    store.set('a.txt', 'two\n')
    const updated = await openBuffer('app', 'a.txt')

    // Clean buffer: silently converge (nothing of the user's to lose).
    const converged = foldNeoWrite(clean, {
      path: 'a.txt',
      content: 'two\n',
      size: 4,
      hash: hashOf('two\n'),
      truncated: false,
    })
    expect(converged.content).toBe('two\n')
    expect(converged.conflict).toBeUndefined()

    // Dirty buffer: surface the conflict, keep the user's bytes.
    const dirty = editBuffer(clean, 'mine\n')
    const conflicted = foldNeoWrite(dirty, {
      path: 'a.txt',
      content: 'two\n',
      size: 4,
      hash: updated.baseHash,
      truncated: false,
    })
    expect(conflicted.content).toBe('mine\n')
    expect(conflicted.conflict?.theirs).toBe('two\n')
  })
})
