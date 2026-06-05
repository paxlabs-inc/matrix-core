#!/usr/bin/env node
// paxeer-net — MCP stdio bridge exposing the Paxeer network to Matrix agents.
//
// READS (no auth): direct EVM JSON-RPC, PaxScan/Blockscout explorer, the Argus
// portfolio + PaxSpot market indexers, price feeds, and the agent-economy
// precompile views (oracle 0x0903, OROB 0x0901, clearing 0x0902, PoFQ 0x0904,
// streams 0x0906, scheduler 0x0905, staking 0x0800).
//
// WRITES (embedded-wallet custody, network-side enforcement): transfers,
// payment streams, scheduled jobs, staking, and generic contract calls
// (DEX swaps). Signing happens on connect.paxportwallet.com; this bridge never
// sees key material. See lib/wallet.mjs for headless auth.
//
// Wire protocol mirrors tools/gideon/rpc-bridge.mjs (newline-delimited JSON-RPC
// over stdio). Run `node tools/paxeer/paxeer-net.mjs --selftest` to smoke it.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { tools, dispatch, TOOL_NAMES } from './lib/tools.mjs'

const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: 'paxeer-net', version: '0.1.0' },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    try {
      return await dispatch(name, args)
    } catch (err) {
      // Surface tool errors as a structured result so the planner can react
      // instead of treating it as a transport failure.
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

// `--selftest` lists the registry, then verifies it against every agent
// manifest that ships paxeer-net. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time so it never reaches the
// fleet. Offline: reads only the local registry + agents/*.json (no network).
// PAXEER_AGENTS_DIR overrides the manifest dir (used by the drift test fixture).
function runSelftest() {
  console.log(`paxeer-net: ${tools.length} tools`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.PAXEER_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`paxeer-net SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`paxeer-net FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'paxeer-net')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`paxeer-net FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`paxeer-net: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`paxeer-net SELFTEST FAILED: no manifest under ${agentsDir} declares a paxeer-net server`)
    process.exit(1)
  }
  if (drift) {
    console.error('paxeer-net SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`paxeer-net OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
