#!/usr/bin/env node
/**
 * build-self-model-graph.mjs — compile the codegraph self-model at
 * /root/matrix/graph/self-model into a compact, static JSON consumed by
 * the Neo self-model brain visualization (no daemon API required).
 *
 *   pnpm build:selfmodel        # writes public/self-model/graph.json
 *
 * Source format (.kvx):
 *   NODE id=<id> kind=<kind>
 *     digest=... file=<path> range=<start>:<end> lang=go exported=<bool> ...
 *     sig=<signature with \n escapes>
 *     doc=<doc comment with \n escapes>
 *   EDGES
 *     contains=a,b,c   defines=...   implements=...
 *     ^contains=<parent>   ^defines=<parent>   ^implements=...
 *
 * Output schema (arrays keep the payload compact; indices refer to `nodes`):
 *   {
 *     meta: { summary, merkle, scope, generated, modules: [{id,count}] },
 *     nodes: [{ id, n, k, f, l, s, d, p, m, pk, c }],
 *     impl: [[fromIdx, toIdx]]
 *   }
 */
import { readFileSync, writeFileSync, mkdirSync, readdirSync, statSync } from 'node:fs'
import { join, dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const SOURCE = process.env.SELF_MODEL_DIR ?? '/root/matrix/graph/self-model'
const OUT = resolve(dirname(fileURLToPath(import.meta.url)), '../public/self-model/graph.json')

const SIG_MAX = 360
const DOC_MAX = 520

function walk(dir, acc = []) {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry)
    if (statSync(p).isDirectory()) walk(p, acc)
    else if (entry.endsWith('.kvx')) acc.push(p)
  }
  return acc
}

function unescape(text) {
  return text.replaceAll('\\n', '\n').replaceAll('\\t', '  ')
}

function clip(text, max) {
  if (!text) return undefined
  const t = unescape(text)
  return t.length > max ? `${t.slice(0, max)}…` : t
}

function shortName(id) {
  // centra/agents/neo/internal/agent.Agent#activeGoal -> activeGoal
  // matrix/cody/internal/llm.Client.Chat       -> Chat
  // centra/agents/neo/internal/agent/agent.go         -> agent.go
  // centra/agents/neo/internal/agent                  -> agent
  const hash = id.lastIndexOf('#')
  if (hash !== -1) return id.slice(hash + 1)
  const slash = id.lastIndexOf('/')
  const tail = id.slice(slash + 1)
  if (tail.includes('.go')) return tail
  const dot = tail.lastIndexOf('.')
  return dot !== -1 ? tail.slice(dot + 1) : tail
}

function moduleOf(id) {
  // matrix/<module>/... (also plain "centra/core/cortex" package)
  const m = /^matrix\/([^/.]+)/.exec(id)
  return m ? m[1] : null
}

/* ------------------------------ parse ------------------------------ */

const files = walk(join(SOURCE, 'matrix'))
const rawNodes = []

for (const file of files) {
  const text = readFileSync(file, 'utf8')
  const blocks = text.split(/\n(?=NODE )/)
  for (const block of blocks) {
    const head = /^NODE id=(\S+)(?: kind=(\S*))?/.exec(block)
    if (!head) continue
    const [, id, kind] = head
    const attrs = /^\s+digest=\S+ file=(\S*) range=(\d+):(\d+)/m.exec(block)
    const sig = /^\s+sig=(.*)$/m.exec(block)
    const doc = /^\s+doc=(.*)$/m.exec(block)
    const edges = {}
    const edgeSection = block.split(/^EDGES$/m)[1] ?? ''
    for (const em of edgeSection.matchAll(/^\s+(\^?)([a-z_]+)=(\S+)$/gm)) {
      edges[`${em[1]}${em[2]}`] = em[3].split(',')
    }
    rawNodes.push({
      id,
      kind: kind || 'symbol',
      file: attrs?.[1] ?? '',
      range: attrs ? `${attrs[2]}:${attrs[3]}` : '',
      sig: sig?.[1],
      doc: doc?.[1],
      edges,
    })
  }
}

/* ------------------------- index and link -------------------------- */

// Drop meta nodes (repo:, mod:) from the cloud — modules become regions.
const codeNodes = rawNodes.filter((n) => !n.id.startsWith('repo:') && !n.id.startsWith('mod:'))

const MODULES = [...new Set(codeNodes.map((n) => moduleOf(n.id)).filter(Boolean))].sort()
const modIndex = new Map(MODULES.map((m, i) => [m, i]))

const idToIdx = new Map(codeNodes.map((n, i) => [n.id, i]))

// Package lookup: package node's file attr is its directory.
const pkgByDir = new Map()
codeNodes.forEach((n, i) => {
  if (n.kind === 'package') pkgByDir.set(n.file, i)
})

function packageOf(node, idx) {
  if (node.kind === 'package') return idx
  let dir = node.file.includes('/') ? node.file.slice(0, node.file.lastIndexOf('/')) : node.file
  while (dir) {
    const hit = pkgByDir.get(dir)
    if (hit !== undefined) return hit
    const cut = dir.lastIndexOf('/')
    if (cut === -1) break
    dir = dir.slice(0, cut)
  }
  return -1
}

const impl = []
const nodes = codeNodes.map((n, i) => {
  const parentId = n.edges['^contains']?.[0] ?? n.edges['^defines']?.[0]
  const parent = parentId !== undefined ? (idToIdx.get(parentId) ?? -1) : -1
  const children = [...(n.edges.contains ?? []), ...(n.edges.defines ?? [])]
    .map((id) => idToIdx.get(id))
    .filter((x) => x !== undefined)
  for (const target of n.edges.implements ?? []) {
    const t = idToIdx.get(target)
    if (t !== undefined) impl.push([i, t])
  }
  const entry = {
    id: n.id,
    n: shortName(n.id),
    k: n.kind,
    f: n.file,
    l: n.range,
    m: modIndex.get(moduleOf(n.id)) ?? 0,
    pk: packageOf(n, i),
    p: parent,
  }
  const sig = clip(n.sig, SIG_MAX)
  const doc = clip(n.doc, DOC_MAX)
  if (sig) entry.s = sig
  if (doc) entry.d = doc
  if (children.length > 0) entry.c = children
  return entry
})

/* ------------------------------ meta ------------------------------- */

const selfModel = JSON.parse(readFileSync(join(SOURCE, 'self-model.json'), 'utf8'))
const counts = MODULES.map((m) => nodes.filter((n) => n.m === modIndex.get(m)).length)

const out = {
  meta: {
    summary: selfModel.summary?.split('\n')[0] ?? '',
    merkle: selfModel.merkle ?? '',
    scope: selfModel.scope ?? [],
    generated: new Date().toISOString(),
    modules: MODULES.map((m, i) => ({ id: m, count: counts[i] })),
  },
  nodes,
  impl,
}

mkdirSync(dirname(OUT), { recursive: true })
writeFileSync(OUT, JSON.stringify(out))

const kb = (statSync(OUT).size / 1024).toFixed(0)
console.log(`self-model graph: ${nodes.length} nodes, ${impl.length} implements edges`)
console.log(`modules: ${MODULES.map((m, i) => `${m}=${counts[i]}`).join(' ')}`)
console.log(`wrote ${OUT} (${kb} KB)`)
