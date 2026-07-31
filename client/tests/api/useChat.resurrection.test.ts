import { describe, expect, it } from 'vitest'
import { EMPTY_TASK, retractTaskAttempt } from '@/hooks/api/useChat'

describe('resurrection stream retraction', () => {
  it('erases only the rejected live attempt and keeps settled task state', () => {
    const task = {
      ...EMPTY_TASK,
      intentId: 'run-7.2',
      streamTurn: 3,
      thinking: 'This repair is not valid.',
      streamingAnswer: 'I already provided the answer above.',
      answer: 'Earlier committed commentary.',
      steps: [
        {
          id: 'step-1',
          kind: 'narration' as const,
          running: false,
          ok: true,
          title: 'Checked the saved work',
        },
      ],
    }

    const retracted = retractTaskAttempt(task, 3)

    expect(retracted).toMatchObject({
      intentId: 'run-7.2',
      thinking: '',
      streamingAnswer: '',
      answer: 'Earlier committed commentary.',
      steps: task.steps,
    })
    expect(retracted.streamTurn).toBeUndefined()
    expect(retractTaskAttempt(task, 2)).toBe(task)
  })
})
