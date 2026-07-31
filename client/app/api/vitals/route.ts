/**
 * /api/vitals — beacon endpoint for client web-vitals samples.
 *
 * The route runs on the Edge runtime so it's cheap to keep warm; it
 * accepts a small JSON body and forwards a structured log line that
 * the gateway-side log scraper can pick up. We deliberately do NOT
 * write to a database from this route — high-cardinality vitals data
 * belongs in a metrics pipeline, not Postgres.
 */
import { NextResponse } from 'next/server'

export const runtime = 'edge'
export const dynamic = 'force-dynamic'

interface VitalsPayload {
  name: string
  value: number
  rating?: string
  id?: string
  delta?: number
  navigationType?: string
  release?: string
}

export async function POST(req: Request) {
  let body: VitalsPayload | null = null
  try {
    body = (await req.json()) as VitalsPayload
  } catch {
    return new NextResponse('bad json', { status: 400 })
  }
  if (!body || typeof body.name !== 'string' || typeof body.value !== 'number') {
    return new NextResponse('missing fields', { status: 400 })
  }
  // Structured log; the gateway's filebeat/loki shipper picks it up by
  // name. Routing onward to a real metrics pipeline (StatsD, Prom
  // remote-write) is a deploy-side concern.
  const line = JSON.stringify({
    kind: 'web-vital',
    ts: new Date().toISOString(),
    ...body,
  })
  // eslint-disable-next-line no-console
  console.log(line)
  return new NextResponse(null, { status: 204 })
}
