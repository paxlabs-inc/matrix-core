import { describe, it, expect } from 'vitest'
import {
  formatUsd,
  shortAddress,
  getAgent,
  getTool,
  getModel,
  runs,
  receipts,
  agents,
  tools,
  models,
} from '@/lib/matrix-data'

describe('formatUsd', () => {
  it('formats positive amounts', () => {
    expect(formatUsd(1.5)).toMatch(/\$1\.50/)
  })

  it('formats zero', () => {
    expect(formatUsd(0)).toMatch(/\$0\.00/)
  })

  it('formats large amounts with thousands separator', () => {
    const result = formatUsd(10000)
    expect(result).toMatch(/10,000/)
  })
})

describe('shortAddress', () => {
  it('truncates a long hex address', () => {
    const addr = '0x1234567890abcdef1234567890abcdef12345678'
    const result = shortAddress(addr)
    expect(result.length).toBeLessThan(addr.length)
    expect(result).toContain('…')
  })

  it('returns short addresses unchanged', () => {
    const short = '0x1234'
    expect(shortAddress(short)).toBe(short)
  })
})

describe('fixture data integrity', () => {
  it('all runs have required fields', () => {
    for (const run of runs) {
      expect(run).toHaveProperty('id')
      expect(run).toHaveProperty('title')
      expect(run).toHaveProperty('status')
      expect(run).toHaveProperty('agentId')
    }
  })

  it('all receipts reference a valid run id', () => {
    const runIds = new Set(runs.map((r) => r.id))
    for (const receipt of receipts) {
      expect(runIds.has(receipt.runId)).toBe(true)
    }
  })

  it('getAgent returns undefined for unknown id', () => {
    expect(getAgent('nonexistent-id')).toBeUndefined()
  })

  it('getAgent returns the correct agent', () => {
    const first = agents[0]
    if (first) expect(getAgent(first.id)).toEqual(first)
  })

  it('getTool returns undefined for unknown id', () => {
    expect(getTool('nonexistent-id')).toBeUndefined()
  })

  it('getModel returns the correct model', () => {
    const first = models[0]
    if (first) expect(getModel(first.id)).toEqual(first)
  })

  it('tools array is non-empty', () => {
    expect(tools.length).toBeGreaterThan(0)
  })
})
