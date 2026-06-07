#!/usr/bin/env node
// Hermetic integration test for tachyon.mjs (the stdio MCP proxy).
//
// Stands up two in-process HTTP servers — a fake tachyond /rpc and a fake
// embedded-wallet auth endpoint — then spawns the real proxy over stdio and
// drives JSON-RPC frames through it. Asserts:
//   1. tools/list advertises exactly the 9 tachyon tools.
//   2. tools/call forwards as JSON-RPC {method: <tool>, params: <args>} and the
//      engine envelope is shaped into an MCP CallToolResult (isError mirrors ok).
//   3. WRITE-token policy: tachyon_call simulate_only=true injects NO
//      wallet_token; simulate_only=false and tachyon_deploy inject the minted
//      agent bearer (via the ed25519 challenge/verify handshake).
//
// No network, no go daemon, no fixed ports. Run: node tools/tachyon/proxy.selftest.mjs

import { createServer } from 'node:http'
import { spawn } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { fileURLToPath } from 'node:url'

const PROXY = fileURLToPath(new URL('./tachyon.mjs', import.meta.url))
const WALLET_TOKEN = 'WALLET_TOK_TEST_123'

const failures = []
function check(name, cond, detail) {
  if (cond) console.log(`  ok  - ${name}`)
  else {
    failures.push(name)
    console.error(`  FAIL- ${name}${detail ? ` (${detail})` : ''}`)
  }
}

function listen(server) {
  return new Promise((res) => server.listen(0, '127.0.0.1', () => res(server.address().port)))
}

function readBody(req) {
  return new Promise((res) => {
    let d = ''
    req.on('data', (c) => (d += c))
    req.on('end', () => res(d))
  })
}

async function main() {
  // ── fake tachyond /rpc: echoes method + params back inside an envelope ──
  const rpcCalls = []
  const rpcServer = createServer(async (req, res) => {
    const body = JSON.parse((await readBody(req)) || '{}')
    rpcCalls.push({ method: body.method, params: body.params })
    res.setHeader('content-type', 'application/json')
    res.end(
      JSON.stringify({
        jsonrpc: '2.0',
        id: body.id,
        result: { ok: true, data: { echoed_method: body.method, wallet_token: body.params?.wallet_token ?? null, project_id: 'deadbeef' } },
      }),
    )
  })

  // ── fake embedded wallet: challenge/verify -> token ──
  const walletServer = createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json')
    if (req.url === '/v1/agent/auth/challenge') {
      res.end(JSON.stringify({ message: 'sign-this', nonce: 'nonce-1' }))
    } else if (req.url === '/v1/agent/auth/verify') {
      res.end(JSON.stringify({ token: WALLET_TOKEN }))
    } else {
      res.statusCode = 404
      res.end('{}')
    }
  })

  const rpcPort = await listen(rpcServer)
  const walletPort = await listen(walletServer)

  // ── temp ed25519 seed (64-hex) so the proxy can mint a wallet token ──
  const dir = mkdtempSync(join(tmpdir(), 'tach-proxy-'))
  const keyfile = join(dir, 'executor.key')
  writeFileSync(keyfile, randomBytes(32).toString('hex'))

  const child = spawn('node', [PROXY], {
    env: {
      ...process.env,
      MATRIX_TACHYON_URL: `http://127.0.0.1:${rpcPort}/rpc`,
      PAXEER_AGENT_KEYFILE: keyfile,
      PAXEER_AGENT_LABEL: 'test',
      PAXEER_WALLET_API: `http://127.0.0.1:${walletPort}`,
    },
    stdio: ['pipe', 'pipe', 'inherit'],
  })

  const responses = new Map()
  let buf = ''
  child.stdout.on('data', (c) => {
    buf += c
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (!line) continue
      const msg = JSON.parse(line)
      responses.set(msg.id, msg)
    }
  })

  const reqs = [
    { jsonrpc: '2.0', id: 1, method: 'tools/list', params: {} },
    { jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'tachyon_compile', arguments: { sources: { 'src/A.sol': 'x' } } } },
    { jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'tachyon_call', arguments: { to: '0xabc', method: 'foo', simulate_only: true } } },
    { jsonrpc: '2.0', id: 4, method: 'tools/call', params: { name: 'tachyon_call', arguments: { to: '0xabc', method: 'bar', simulate_only: false } } },
    { jsonrpc: '2.0', id: 5, method: 'tools/call', params: { name: 'tachyon_deploy', arguments: { idempotency_key: 'k1', chain_id: 'c', contract: 'A' } } },
  ]
  for (const r of reqs) child.stdin.write(JSON.stringify(r) + '\n')

  // Wait for all responses (the proxy is long-lived; ending stdin would make it
  // exit and cut off in-flight async tools/call fetches). Then shut it down.
  const deadline = Date.now() + 15000
  while (responses.size < reqs.length && Date.now() < deadline) {
    await new Promise((res) => setTimeout(res, 50))
  }
  child.stdin.end()
  child.kill()

  rpcServer.close()
  walletServer.close()

  // ── assertions ──
  const env = (id) => JSON.parse(responses.get(id).result.content[0].text)
  const isErr = (id) => responses.get(id).result.isError === true

  check('tools/list returns 9 tools', responses.get(1)?.result?.tools?.length === 9, `got ${responses.get(1)?.result?.tools?.length}`)
  check('compile forwarded as tachyon_compile', rpcCalls.some((c) => c.method === 'tachyon_compile' && c.params?.sources), JSON.stringify(rpcCalls.map((c) => c.method)))
  check('compile envelope shaped (ok=true, isError=false)', env(2).ok === true && isErr(2) === false)

  const callSim = rpcCalls.find((c) => c.method === 'tachyon_call' && c.params?.method === 'foo')
  check('simulate_only call carries NO wallet_token', callSim && callSim.params.wallet_token == null)

  const callBcast = rpcCalls.find((c) => c.method === 'tachyon_call' && c.params?.method === 'bar')
  check('broadcast call injects minted wallet_token', callBcast && callBcast.params.wallet_token === WALLET_TOKEN, callBcast && JSON.stringify(callBcast.params.wallet_token))

  const deploy = rpcCalls.find((c) => c.method === 'tachyon_deploy')
  check('deploy injects minted wallet_token', deploy && deploy.params.wallet_token === WALLET_TOKEN)

  if (failures.length) {
    console.error(`\ntachyon proxy SELFTEST FAILED: ${failures.length} check(s)`) 
    process.exit(1)
  }
  console.log('\ntachyon proxy OK (5 checks passed)')
  process.exit(0)
}

main().catch((e) => {
  console.error('proxy selftest crashed:', e)
  process.exit(1)
})
