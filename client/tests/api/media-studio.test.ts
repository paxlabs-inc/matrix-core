import { describe, expect, it } from 'vitest'
import { normalizeStudioJob } from '@/lib/api/media-studio'

describe('normalizeStudioJob', () => {
  it('normalizes legacy null assets before the studio renders a job', () => {
    const job = normalizeStudioJob({
      id: 'job-legacy',
      kind: 'text-to-image',
      status: 'succeeded',
      provider: 'matrix',
      progress: 100,
      request: {
        kind: 'text-to-image',
        prompt: 'A finished generation',
        seed: -1,
      },
      assets: null,
      created_at: '2026-07-27T12:00:00Z',
      updated_at: '2026-07-27T12:01:00Z',
    })

    expect(job.assets).toEqual([])
  })
})
