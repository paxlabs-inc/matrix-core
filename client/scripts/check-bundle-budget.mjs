#!/usr/bin/env node
/**
 * Fail CI when any single JS chunk exceeds budget (regression guard).
 * Turbopack emits many hashed chunks; we track max chunk size, not sum.
 * Run after `pnpm build`.
 */
import fs from 'node:fs'
import path from 'node:path'

const CHUNKS_DIR = '.next/static/chunks'
const MAX_CHUNK_KB = Number(process.env.BUNDLE_MAX_CHUNK_KB ?? 800)
const MAX_LARGE_CHUNKS = Number(process.env.BUNDLE_MAX_LARGE_CHUNKS ?? 8)

function kb(bytes) {
  return bytes / 1024
}

if (!fs.existsSync(CHUNKS_DIR)) {
  console.error('check-bundle-budget: run `pnpm build` first — .next/static/chunks missing')
  process.exit(1)
}

const files = fs.readdirSync(CHUNKS_DIR).filter((f) => f.endsWith('.js'))
const sizes = files
  .map((file) => ({ file, size: fs.statSync(path.join(CHUNKS_DIR, file)).size }))
  .sort((a, b) => b.size - a.size)

let failed = false
const largest = sizes[0]
if (largest && kb(largest.size) > MAX_CHUNK_KB) {
  console.error(
    `✗ largest chunk ${largest.file}: ${kb(largest.size).toFixed(1)} KB > ${MAX_CHUNK_KB} KB`,
  )
  failed = true
}

const large = sizes.filter((s) => kb(s.size) > MAX_CHUNK_KB * 0.75)
if (large.length > MAX_LARGE_CHUNKS) {
  console.error(
    `✗ ${large.length} chunks > ${(MAX_CHUNK_KB * 0.75).toFixed(0)} KB (max ${MAX_LARGE_CHUNKS})`,
  )
  failed = true
}

if (failed) process.exit(1)
console.log(
  `✓ bundle budget OK (${files.length} chunks, largest ${kb(largest?.size ?? 0).toFixed(1)} KB)`,
)
