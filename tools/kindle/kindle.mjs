#!/usr/bin/env node
// kindle — MCP stdio bridge exposing KindleLaunch (the Sidiora launchpad on
// Paxeer Network, chain 125) to Matrix agents.
//
// READS (no auth): pools, token/market detail, swap quotes, pending fees, and
// optical info — direct chain-125 eth_call against the canonical 2026-06-20
// deployment manifest (see lib/config.mjs).
//
// WRITES (escalate-class): launch, buy, sell, swap, collect fees, set fee
// strategy, and optical creation. These move value and are reachable ONLY via
// MCL's core_execute; on Neo's conversational surface (KINDLE_READS_ONLY=1) they
// are withheld and refused. Signing happens server-side on the Paxeer embedded
// wallet (connect.paxportwallet.com /v1/agent/*); this bridge never sees key
// material — it reuses tools/paxeer/lib/wallet.mjs so there is one signing path.
//
// Wire protocol mirrors tools/paxeer/paxeer-net.mjs (newline-delimited JSON-RPC
// over stdio). Run `node tools/kindle/kindle.mjs --selftest` to smoke it.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { tools, dispatch, TOOL_NAMES } from './lib/tools.mjs'

const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: 'kindle', version: '0.1.0' },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    try {
      return await dispatch(name, args)
    } catch (err) {
      return { content: [{ type: 'text', text: JSON.stringify({ ok: false, tool: name, error: err?.message ?? String(err) }) }], isError: true }
    }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n')
}
const rpcOk = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

function startStdioServer() {
  const rl = createInterface({ input: process.stdin })
  rl.on('line', async (line) => {
    if (!line.trim()) return
    let req
    try {
      req = JSON.parse(line)
    } catch (err) {
      send(rpcErr(null, -32700, 'parse error: ' + err.message))
      return
    }
    const fn = handlers[req.method]
    if (!fn) {
      if (req.id !== undefined) send(rpcErr(req.id, -32601, `method not found: ${req.method}`))
      return
    }
    try {
      const result = await fn(req.params)
      if (req.id !== undefined && result !== null) send(rpcOk(req.id, result))
    } catch (err) {
      if (req.id !== undefined) send(rpcErr(req.id, -32000, err?.message ?? String(err)))
    }
  })
  process.stdin.on('end', () => process.exit(0))
  process.on('SIGINT', () => process.exit(0))
  process.on('SIGTERM', () => process.exit(0))
}

// `--selftest` lists the registry, then verifies it against every agent manifest
// that ships a `kindle` server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time so it never reaches the fleet.
// Offline: reads only the local registry + agents/*.json (no network).
// KINDLE_AGENTS_DIR overrides the manifest dir (used by the drift test fixture).
// NOTE: run with KINDLE_READS_ONLY unset so the FULL registry is verified — the
// manifest declares the complete read+write set (the daemon's MCL surface).
function runSelftest() {
  console.log(`kindle: ${tools.length} tools`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.KINDLE_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`kindle SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`kindle FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'kindle')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`kindle FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`kindle: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`kindle SELFTEST FAILED: no manifest under ${agentsDir} declares a kindle server`)
    process.exit(1)
  }
  if (drift) {
    console.error('kindle SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`kindle OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
