#!/usr/bin/env node
// cortex-mcp — exposes ~/.cursor/cortex-mem.sh verbs as an MCP HTTP server.
//
// Wire protocol: JSON-RPC 2.0 over HTTP POST (MCP Streamable HTTP transport).
// Auth: Authorization: Bearer <CORTEX_MCP_TOKEN>
//
// Tools exposed:
//   cortex_recall              load all memories (call first each session)
//   cortex_guard               print hard+firm rules before risky action
//   cortex_verify              Merkle tamper-check
//   cortex_brief               salience-ranked context bundle
//   cortex_remember_fact       store a fact
//   cortex_remember_preference store a preference
//   cortex_remember_constraint store a constraint/rule
//   cortex_remember_decision   store a decision
//   cortex_note_outcome        record a task outcome

import { createServer } from 'node:http'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)

const PORT   = parseInt(process.env.CORTEX_MCP_PORT  ?? '4242', 10)
const TOKEN  = process.env.CORTEX_MCP_TOKEN  ?? ''
const SCRIPT = process.env.CORTEX_MEM_SCRIPT ?? `${process.env.HOME}/.cursor/cortex-mem.sh`

async function cs(...args) {
  try {
    const { stdout } = await execFileAsync('bash', [SCRIPT, ...args], {
      timeout: 30_000,
      env: { ...process.env },
    })
    return { ok: true, text: stdout.trim() }
  } catch (err) {
    const msg = err?.stderr?.trim() || err?.message || String(err)
    return { ok: false, text: msg }
  }
}

const TOOLS = [
  {
    name: 'cortex_recall',
    description: 'Load all persistent memories (integrity-checked). Call this at the start of every session.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'cortex_guard',
    description: 'Print hard+firm rules fail-closed. Call this before any destructive, irreversible, or security-sensitive action.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'cortex_verify',
    description: 'Tamper-check the memory store (Merkle journal replay). Returns VERIFIED <root> or FAILED.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'cortex_brief',
    description: 'Return a salience-ranked, budget-bounded context bundle for the current task.',
    inputSchema: {
      type: 'object',
      properties: {
        budget: { type: 'number', description: 'Approximate token budget (default 2000)' },
      },
      required: [],
    },
  },
  {
    name: 'cortex_remember_fact',
    description: 'Store a durable fact about the user, repo, or environment.',
    inputSchema: {
      type: 'object',
      properties: {
        text:      { type: 'string', description: 'The fact statement' },
        subject:   { type: 'string', description: 'URI subject (default matrix://knowledge/repo)' },
        predicate: { type: 'string', description: 'Predicate verb (default note)' },
      },
      required: ['text'],
    },
  },
  {
    name: 'cortex_remember_preference',
    description: 'Store a stated preference (like/dislike about how to work).',
    inputSchema: {
      type: 'object',
      properties: {
        topic:    { type: 'string' },
        polarity: { type: 'string', enum: ['prefer', 'avoid', 'neutral', 'do', 'dont'] },
        strength: { type: 'number', minimum: 0, maximum: 1, description: '0..1 strength' },
        rationale: { type: 'string' },
      },
      required: ['topic', 'polarity', 'strength'],
    },
  },
  {
    name: 'cortex_remember_constraint',
    description: 'Store a standing rule or guardrail.',
    inputSchema: {
      type: 'object',
      properties: {
        text:     { type: 'string' },
        polarity: { type: 'string', enum: ['do', 'dont', 'avoid', 'prefer'] },
        strength: { type: 'string', enum: ['soft', 'firm', 'hard'] },
        source:   { type: 'string', description: 'Origin (default user_declared)' },
      },
      required: ['text', 'polarity', 'strength'],
    },
  },
  {
    name: 'cortex_remember_decision',
    description: 'Store a locked decision made together with the user.',
    inputSchema: {
      type: 'object',
      properties: { text: { type: 'string' } },
      required: ['text'],
    },
  },
  {
    name: 'cortex_note_outcome',
    description: 'Record the result of a completed task.',
    inputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string' },
        result:  { type: 'string', enum: ['success', 'failure', 'partial'] },
      },
      required: ['summary', 'result'],
    },
  },
]

async function callTool(name, args) {
  switch (name) {
    case 'cortex_recall':
      return cs('recall')
    case 'cortex_guard':
      return cs('guard')
    case 'cortex_verify':
      return cs('verify')
    case 'cortex_brief':
      return cs('brief', ...(args.budget != null ? [String(args.budget)] : []))
    case 'cortex_remember_fact':
      return cs(
        'remember-fact', args.text,
        ...(args.subject   ? [args.subject]   : []),
        ...(args.predicate ? [args.predicate] : []),
      )
    case 'cortex_remember_preference':
      return cs(
        'remember-preference', args.topic, args.polarity, String(args.strength),
        ...(args.rationale ? [args.rationale] : []),
      )
    case 'cortex_remember_constraint':
      return cs(
        'remember-constraint', args.text, args.polarity, args.strength,
        ...(args.source ? [args.source] : []),
      )
    case 'cortex_remember_decision':
      return cs('remember-decision', args.text)
    case 'cortex_note_outcome':
      return cs('note-outcome', args.summary, args.result)
    default: {
      const e = new Error(`unknown tool: ${name}`)
      e.code = -32601
      throw e
    }
  }
}

const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: 'cortex-mcp', version: '1.0.0' },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools: TOOLS }),
  'tools/call': async (params) => {
    const name = params?.name ?? ''
    const args = params?.arguments ?? {}
    const res  = await callTool(name, args)
    return { content: [{ type: 'text', text: res.text }], isError: !res.ok }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

const rpcOk  = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

const server = createServer(async (req, res) => {
  if (TOKEN) {
    const auth = req.headers['authorization'] ?? ''
    if (auth !== `Bearer ${TOKEN}`) {
      res.writeHead(401, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ error: 'unauthorized' }))
      return
    }
  }

  if (req.method === 'GET') {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ name: 'cortex-mcp', version: '1.0.0', status: 'ok' }))
    return
  }

  if (req.method !== 'POST') {
    res.writeHead(405, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ error: 'method not allowed' }))
    return
  }

  const chunks = []
  for await (const chunk of req) chunks.push(chunk)
  let rpc
  try {
    rpc = JSON.parse(Buffer.concat(chunks).toString())
  } catch {
    res.writeHead(400, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(rpcErr(null, -32700, 'parse error')))
    return
  }

  const fn = handlers[rpc.method]
  let response
  if (!fn) {
    response = rpc.id !== undefined
      ? rpcErr(rpc.id, -32601, `method not found: ${rpc.method}`)
      : null
  } else {
    try {
      const result = await fn(rpc.params)
      response = (rpc.id !== undefined && result !== null)
        ? rpcOk(rpc.id, result)
        : null
    } catch (err) {
      response = rpc.id !== undefined
        ? rpcErr(rpc.id, err?.code ?? -32000, err?.message ?? String(err))
        : null
    }
  }

  if (response) {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(response))
  } else {
    res.writeHead(204)
    res.end()
  }
})

server.listen(PORT, '127.0.0.1', () => {
  console.log(`cortex-mcp listening on 127.0.0.1:${PORT}`)
})
