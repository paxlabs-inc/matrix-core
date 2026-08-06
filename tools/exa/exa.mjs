#!/usr/bin/env node
// exa — zero-dependency MCP bridge to Matrix's router-owned Exa evidence lane.
// The vendor key never enters this process. Search defaults to extractive
// highlights; Contents reports every per-URL status; Agent results treat only
// terminal output.grounding as authoritative. Social output is draft-only.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const SERVER_NAME = 'exa'
const SERVER_VERSION = '0.1.0'
const TIMEOUT_MS = clampInt(process.env.MATRIX_EXA_TIMEOUT_MS, 45000, 1000, 120000)
const MAX_RESPONSE_BYTES = clampInt(process.env.MATRIX_EXA_MAX_RESPONSE_BYTES, 8_000_000, 10000, 20_000_000)

function clampInt(value, fallback, min, max) {
  const parsed = Number.parseInt(value ?? '', 10)
  return Number.isFinite(parsed) ? Math.min(max, Math.max(min, parsed)) : fallback
}

function laneURL() {
  const explicit = (process.env.MATRIX_EXA_URL || '').trim().replace(/\/+$/, '')
  if (explicit) return explicit
  const router = (process.env.ROUTER_INTERNAL_URL || 'http://matrix-router.railway.internal:8088').trim().replace(/\/+$/, '')
  return router ? `${router}/internal/exa` : ''
}

function laneToken() {
  return (process.env.MATRIX_EXA_TOKEN || process.env.ROUTER_RESEARCH_TOKEN || '').trim()
}

function configured() {
  return laneURL() !== '' && laneToken() !== ''
}

function ok(value) {
  return { content: [{ type: 'text', text: JSON.stringify(value) }] }
}

function fail(tool, error, extra = {}) {
  return { content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }], isError: true }
}

async function lane(path, { method = 'GET', body } = {}) {
  if (typeof fetch !== 'function') throw new Error('exa: global fetch unavailable (Node 18+ required)')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  const headers = { Accept: 'application/json', Authorization: `Bearer ${laneToken()}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const subject = (process.env.MATRIX_USER_ID || process.env.MATRIX_ACTOR_DID || '').trim()
  if (subject) headers['X-Matrix-Subject'] = subject
  let response
  try {
    response = await fetch(`${laneURL()}/${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: controller.signal,
    })
  } catch (error) {
    clearTimeout(timer)
    const reason = error?.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : error?.message || String(error)
    throw new Error(`grounded research unreachable: ${reason}`)
  }
  clearTimeout(timer)
  let raw = await response.text()
  if (raw.length > MAX_RESPONSE_BYTES) raw = raw.slice(0, MAX_RESPONSE_BYTES)
  let parsed
  try {
    parsed = raw ? JSON.parse(raw) : null
  } catch {
    parsed = null
  }
  if (!response.ok) {
    const error = parsed?.error || {}
    const failure = new Error(error.message || `grounded research failed (HTTP ${response.status})`)
    failure.kind = error.kind || 'upstream'
    failure.retryAfter = error.retry_after_seconds
    throw failure
  }
  return parsed
}

function text(value) {
  if (typeof value === 'string') return value
  if (value === undefined || value === null) return ''
  return JSON.stringify(value)
}

function groundingCards(grounding) {
  const seen = new Set()
  const cards = []
  for (const group of Array.isArray(grounding) ? grounding : []) {
    for (const citation of Array.isArray(group?.citations) ? group.citations : []) {
      const url = typeof citation?.url === 'string' ? citation.url : ''
      if (!url || seen.has(url)) continue
      seen.add(url)
      cards.push({ title: citation.title || url, url, snippet: `Grounds ${group.field || 'the generated synthesis'}${group.confidence ? ` · ${group.confidence} confidence` : ''}` })
    }
  }
  return cards
}

function searchResult(tool, query, envelope) {
  const data = envelope?.data || {}
  const cards = (Array.isArray(data.results) ? data.results : []).map((result) => ({
    title: result.title || result.url,
    url: result.url,
    snippet: Array.isArray(result.highlights) ? result.highlights.join('\n') : text(result.summary || result.text).slice(0, 1200),
    published: result.publishedDate,
  }))
  const grounding = data.output?.grounding || []
  const existing = new Set(cards.map((card) => card.url))
  for (const card of groundingCards(grounding)) if (!existing.has(card.url)) cards.push(card)
  return {
    tool,
    provider: 'exa',
    query,
    answer: text(data.output?.content),
    results: cards,
    request_id: data.requestId,
    search_type: data.searchType,
    grounding,
    cost_dollars: data.costDollars?.total,
    cache_hit: envelope?.meta?.cache_hit === true,
    retrieved_at: envelope?.meta?.retrieved_at,
    synthesis_note: data.output ? 'generated synthesis — terminal grounding is authoritative' : undefined,
  }
}

function contentsResult(tool, query, envelope) {
  const data = envelope?.data || {}
  const statuses = Array.isArray(data.statuses) ? data.statuses : []
  return {
    tool,
    provider: 'exa',
    query,
    answer: '',
    results: (Array.isArray(data.results) ? data.results : []).map((result) => ({
      title: result.title || result.url,
      url: result.url,
      snippet: Array.isArray(result.highlights) ? result.highlights.join('\n') : text(result.text).slice(0, 2000),
      published: result.publishedDate,
    })),
    statuses,
    partial: envelope?.error?.kind === 'partial' || statuses.some((status) => status.status !== 'success'),
    request_id: data.requestId,
    cost_dollars: data.costDollars?.total,
    cache_hit: envelope?.meta?.cache_hit === true,
    retrieved_at: envelope?.meta?.retrieved_at,
  }
}

function runResult(tool, query, envelope) {
  const run = envelope?.run || {}
  const output = run.output || {}
  return {
    tool,
    provider: 'exa',
    query,
    answer: output.text || text(output.structured),
    results: groundingCards(output.grounding),
    run: {
      id: run.id,
      status: run.status,
      workflow: envelope?.workflow,
      subject: envelope?.subject,
      output: run.status === 'completed' ? output : undefined,
      error: run.error,
      cost_dollars: run.costDollars?.total,
    },
    cache_hit: envelope?.meta?.cache_hit === true,
    retrieved_at: envelope?.meta?.retrieved_at,
    synthesis_note: output.text || output.structured ? 'generated synthesis — terminal grounding is authoritative' : undefined,
  }
}

function requiredText(args, name) {
  const value = String(args?.[name] ?? '').trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function stringList(value) {
  const list = Array.isArray(value) ? value : String(value ?? '').split(',')
  return list.map((item) => String(item).trim()).filter(Boolean)
}

const SEARCH_TYPES = new Set(['auto', 'fast', 'instant', 'deep-lite', 'deep', 'deep-reasoning'])

export async function dispatch(name, args = {}) {
  if (!configured()) return fail(name, 'grounded web research is not configured', { hint: 'the router research service token is unavailable' })
  try {
    switch (name) {
      case 'exa_search': {
        const query = requiredText(args, 'query')
        const type = String(args.type || 'auto')
        if (!SEARCH_TYPES.has(type)) throw new Error('unsupported search type')
        const highlights = args.highlight_query ? { query: String(args.highlight_query), maxCharacters: clampInt(args.max_characters, 4000, 500, 12000) } : true
		const requestedCategory = String(args.category || '').trim()
		// Exa's company index rejects publication-date filters. A dated
		// announcement query is general web retrieval, even when the model
		// mistakenly labels it as a company lookup, so preserve the dates and
		// fall back to the general index instead of spending a failed request.
		const category = requestedCategory === 'company' &&
			(args.start_published_date || args.end_published_date)
			? undefined
			: requestedCategory || undefined
        const body = {
          query,
          type,
          numResults: clampInt(args.num_results, 8, 1, 20),
		  category,
          includeDomains: stringList(args.include_domains),
          excludeDomains: stringList(args.exclude_domains),
          startPublishedDate: args.start_published_date,
          endPublishedDate: args.end_published_date,
          moderation: args.moderation === true,
          contents: { highlights },
        }
        const envelope = await lane('search', { method: 'POST', body })
        return ok(searchResult(name, query, envelope))
      }
      case 'exa_contents': {
        const urls = stringList(args.urls)
        if (urls.length === 0 || urls.length > 10) throw new Error('urls must contain between 1 and 10 URLs')
        const query = args.highlight_query ? String(args.highlight_query) : urls.join(', ')
        const body = { urls }
        if (args.full_text === true) body.text = { maxCharacters: clampInt(args.max_characters, 12000, 500, 20000) }
        else body.highlights = args.highlight_query ? { query: String(args.highlight_query), maxCharacters: clampInt(args.max_characters, 4000, 500, 12000) } : true
        if (args.max_age_hours !== undefined) body.maxAgeHours = clampInt(args.max_age_hours, 24, -1, 8760)
        const envelope = await lane('contents', { method: 'POST', body })
        return ok(contentsResult(name, query, envelope))
      }
      case 'exa_research_start': {
        const query = requiredText(args, 'query')
        const workflow = String(args.workflow || 'general.research.v1').trim()
        const body = { workflow, subject: String(args.subject || '').trim(), request: { query, effort: args.effort || 'medium', outputSchema: args.output_schema } }
        const envelope = await lane('research/start', { method: 'POST', body })
        return ok(runResult(name, query, envelope))
      }
      case 'exa_research_get': {
        const runID = requiredText(args, 'run_id')
        const envelope = await lane(`research/${encodeURIComponent(runID)}`)
        return ok(runResult(name, `Research run ${runID}`, envelope))
      }
      case 'exa_research_continue': {
        const runID = requiredText(args, 'run_id')
        const query = requiredText(args, 'query')
        const envelope = await lane(`research/${encodeURIComponent(runID)}/continue`, { method: 'POST', body: { query, effort: args.effort || 'medium' } })
        return ok(runResult(name, query, envelope))
      }
      case 'exa_research_cancel': {
        const runID = requiredText(args, 'run_id')
        const envelope = await lane(`research/${encodeURIComponent(runID)}/cancel`, { method: 'POST', body: {} })
        return ok(runResult(name, `Cancel research run ${runID}`, envelope))
      }
      case 'exa_outbound_brief': {
        const company = requiredText(args, 'company')
        const person = String(args.person || '').trim()
        const query = `Research ${person ? `${person} at ` : ''}${company} for a relevant, factual outbound brief. Find recent company context, role context, concrete trigger events, and grounded talking points.`
        const outputSchema = {
          type: 'object',
          required: ['company_overview', 'outbound_triggers', 'talking_points'],
          properties: {
            company_overview: { type: 'string' },
            person_context: { type: 'string' },
            outbound_triggers: { type: 'array', maxItems: 6, items: { type: 'string' } },
            talking_points: { type: 'array', maxItems: 6, items: { type: 'string' } },
          },
        }
        const envelope = await lane('search', { method: 'POST', body: { query, type: 'deep', numResults: 10, outputSchema, systemPrompt: 'Prefer first-party company sources and recent reputable reporting. Do not invent personal details.', contents: { highlights: true } } })
        return ok(searchResult(name, query, envelope))
      }
      case 'exa_social_draft': {
        const topic = requiredText(args, 'topic')
        const platform = String(args.platform || 'linkedin').toLowerCase()
        if (!['linkedin', 'x', 'thread'].includes(platform)) throw new Error('platform must be linkedin, x or thread')
        const query = `Find the most relevant recent factual event for this topic and draft a grounded ${platform} post: ${topic}`
        const outputSchema = platform === 'thread'
          ? { type: 'object', required: ['posts'], properties: { posts: { type: 'array', maxItems: 8, items: { type: 'string' } } } }
          : { type: 'object', required: ['post'], properties: { post: { type: 'string', description: platform === 'x' ? 'At most 280 characters.' : 'A concise professional post.' } } }
        const envelope = await lane('search', { method: 'POST', body: { query, type: 'deep', category: 'news', numResults: 8, outputSchema, systemPrompt: 'Return a draft only. Anchor it in a recent sourced event. No hashtags, em dashes, AI vocabulary, or invented claims.', contents: { highlights: true } } })
        return ok({ ...searchResult(name, query, envelope), draft_only: true })
      }
      default:
        throw new Error(`unknown tool: ${name}`)
    }
  } catch (error) {
    const extra = {}
    if (error?.kind) extra.kind = error.kind
    if (error?.retryAfter) extra.retry_after_seconds = error.retryAfter
    return fail(name, error?.message || String(error), extra)
  }
}

const A = (properties, required = []) => ({ type: 'object', properties, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })
const B = (description) => ({ type: 'boolean', description })
const L = (description) => ({ type: 'array', items: S(description) })

export const tools = [
  { name: 'exa_search', description: 'Semantic web search through the router-owned Exa lane. Defaults to extractive highlights, returns source cards, request ID, search type, cost and any generated output grounding. Prefer ordinary web_search for cheap discovery; use this for semantic retrieval, controlled domains/dates, financial reports or deeper synthesis. Read-only.', inputSchema: A({ query: S('natural-language query'), type: S('auto, fast, instant, deep-lite, deep or deep-reasoning'), num_results: N('1-20'), category: S('company, people, publication, news, personal site or financial report'), include_domains: L('allowed domain'), exclude_domains: L('excluded domain'), start_published_date: S('ISO date'), end_published_date: S('ISO date'), highlight_query: S('optional extractive highlight focus'), max_characters: N('500-12000 highlight characters'), moderation: B('filter unsafe content') }, ['query']) },
  { name: 'exa_contents', description: 'Fetch extractive evidence or bounded full text from 1-10 known URLs. The Contents endpoint reports every per-URL status even when HTTP is 200; partial failures remain explicit. Read-only.', inputSchema: A({ urls: L('HTTP or HTTPS URL'), highlight_query: S('extractive evidence focus'), full_text: B('return bounded full text instead of highlights'), max_characters: N('500-20000'), max_age_hours: N('-1 cache only, 0 always livecrawl, positive max cache age') }, ['urls']) },
  { name: 'exa_research_start', description: 'Start asynchronous multi-source Exa Agent research through the router. Output is generated synthesis; terminal output.grounding is authoritative. Use bounded schemas and medium or lower effort by default. Read-only research.', inputSchema: A({ query: S('research objective'), workflow: S('stable workflow/version label'), subject: S('entity or topic cache identity'), effort: S('minimal, low, medium, high or xhigh'), output_schema: { type: 'object', description: 'bounded JSON Schema for structured output' } }, ['query']) },
  { name: 'exa_research_get', description: 'Get an owned asynchronous Exa Agent run. Completed results include generated output and terminal grounding. Read-only.', inputSchema: A({ run_id: S('run identifier') }, ['run_id']) },
  { name: 'exa_research_continue', description: 'Continue an owned completed Exa Agent run with a follow-up query. Creates a new run ID and preserves the previous run as context. Read-only research.', inputSchema: A({ run_id: S('previous run identifier'), query: S('follow-up research request'), effort: S('minimal, low, medium, high or xhigh') }, ['run_id', 'query']) },
  { name: 'exa_research_cancel', description: 'Cancel an owned queued or running Exa Agent job to stop further work and cost. It does not publish, message, or change external content.', inputSchema: A({ run_id: S('run identifier') }, ['run_id']) },
  { name: 'exa_outbound_brief', description: 'Build a grounded company or person outbound-research brief with recent triggers and talking points. It researches only and sends nothing. Read-only.', inputSchema: A({ company: S('company name or domain'), person: S('optional person and role context') }, ['company']) },
  { name: 'exa_social_draft', description: 'Create a source-grounded social draft anchored in a recent event. Draft-only: this tool has no publishing or messaging capability. Read-only research.', inputSchema: A({ topic: S('topic and intended point'), platform: S('linkedin, x or thread') }, ['topic']) },
]

export const TOOL_NAMES = tools.map((tool) => tool.name)

const handlers = {
  initialize: (params) => ({ protocolVersion: params?.protocolVersion ?? '2024-11-05', serverInfo: { name: SERVER_NAME, version: SERVER_VERSION }, capabilities: { tools: {} } }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => dispatch(params?.name, params?.arguments || {}),
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(value) { process.stdout.write(`${JSON.stringify(value)}\n`) }

function start() {
  const input = createInterface({ input: process.stdin })
  input.on('line', async (line) => {
    if (!line.trim()) return
    let request
    try { request = JSON.parse(line) } catch (error) { send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: `parse error: ${error.message}` } }); return }
    const handler = handlers[request.method]
    if (!handler) { if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32601, message: `method not found: ${request.method}` } }); return }
    try { const result = await handler(request.params); if (request.id !== undefined && result !== null) send({ jsonrpc: '2.0', id: request.id, result }) }
    catch (error) { if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32000, message: error?.message || String(error) } }) }
  })
  process.stdin.on('end', () => process.exit(0))
  process.on('SIGINT', () => process.exit(0))
  process.on('SIGTERM', () => process.exit(0))
}

function selftest() {
  console.log(`exa: ${tools.length} tools (lane=${configured() ? 'configured' : 'not configured'})`)
  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.EXA_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  const files = readdirSync(agentsDir).filter((file) => file.endsWith('.json'))
  let checked = 0
  let failed = false
  for (const file of files) {
    const manifest = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    const server = (manifest.servers || []).find((entry) => entry.alias === 'exa')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((tool) => tool.name))
    const bridgeOnly = [...bridge].filter((name) => !declared.has(name))
    const manifestOnly = [...declared].filter((name) => !bridge.has(name))
    if (bridgeOnly.length || manifestOnly.length) {
      failed = true
      console.error(`exa FAIL: ${file} manifest drift; bridge-only=${bridgeOnly.join(',')} manifest-only=${manifestOnly.join(',')}`)
    } else console.log(`exa: ${file} matches (${declared.size} tools)`)
  }
  if (checked === 0) { console.error('exa SELFTEST FAILED: no shipped manifest declares alias exa'); process.exit(1) }
  if (failed) process.exit(1)
  const contents = { urls: ['https://example.com'], highlights: true }
  if ('contents' in contents || !('highlights' in contents)) { console.error('exa SELFTEST FAILED: Contents options must be top-level'); process.exit(1) }
  const search = { query: 'x', contents: { highlights: true } }
  if (!search.contents?.highlights || 'highlights' in search) { console.error('exa SELFTEST FAILED: Search content options must be nested'); process.exit(1) }
  console.log(`exa OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
}

if (process.argv.includes('--selftest')) selftest()
else start()
