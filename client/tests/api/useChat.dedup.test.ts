/**
 * UI-layer replay dedup (the 2026-07-12 brand-kit transcript bug): a verbatim
 * re-emission of an assistant bubble — whatever server path replayed it
 * (supervisor respawn re-narration, reconnect replay, double-persisted turn) —
 * must never render twice, while legitimate repeats (short confirmations,
 * far-apart re-answers, media-carrying turns) still do.
 */
import { describe, it, expect } from 'vitest'

import {
  ASSISTANT_DEDUP_MIN_CHARS,
  ASSISTANT_DEDUP_WINDOW,
  dedupeReasoning,
  dedupeThread,
  isDuplicateAssistantText,
  type ChatMessage,
} from '@/hooks/api/useChat'

let nextId = 0
function msg(role: ChatMessage['role'], text: string, reasoning?: string): ChatMessage {
  return { id: `m${nextId++}`, role, text, reasoning, ts: 1_700_000_000_000 + nextId }
}

const LONG = 'The brand-kit site looks complete already. Which part should I pick back up?'
const OTHER = 'Deployed matrix-brand-kit to Paxeer Cloud — the preview URL is live and verified.'

describe('isDuplicateAssistantText — verbatim replay guard', () => {
  it('flags a verbatim repeat of a recent assistant bubble', () => {
    const thread = [msg('user', 'pick back up on the brandkit'), msg('assistant', LONG)]
    expect(isDuplicateAssistantText(thread, LONG)).toBe(true)
    expect(isDuplicateAssistantText(thread, ` ${LONG}\n`)).toBe(true)
  })

  it('never flags short confirmations — they legitimately repeat', () => {
    const short = 'Done.'
    expect(short.length).toBeLessThan(ASSISTANT_DEDUP_MIN_CHARS)
    const thread = [msg('assistant', short)]
    expect(isDuplicateAssistantText(thread, short)).toBe(false)
  })

  it('does not flag new answers or user-echoed text', () => {
    const thread = [msg('user', LONG), msg('assistant', OTHER)]
    // Matching a USER turn is not a replay — only assistant bubbles count.
    expect(isDuplicateAssistantText(thread, LONG)).toBe(false)
  })

  it('forgets beyond the bounded window — a far-apart re-answer renders', () => {
    const thread: ChatMessage[] = [msg('assistant', LONG)]
    for (let i = 0; i < ASSISTANT_DEDUP_WINDOW; i++) {
      thread.push(msg('user', `q${i}`), msg('assistant', `${OTHER} (${i})`))
    }
    expect(isDuplicateAssistantText(thread, LONG)).toBe(false)
  })
})

describe('dedupeReasoning — identical thought never stacks twice', () => {
  const THOUGHT =
    'Andrew wants to pick back up on the brandkit step. Let me check what is in the workspace.'

  it('strips reasoning that verbatim-repeats the previous assistant bubble', () => {
    const thread = [msg('assistant', LONG, THOUGHT)]
    expect(dedupeReasoning(thread, THOUGHT)).toBeUndefined()
  })

  it('keeps fresh reasoning and short reasoning untouched', () => {
    const thread = [msg('assistant', LONG, THOUGHT)]
    expect(dedupeReasoning(thread, 'Now the deploy: build first, then paxc. ' + OTHER)).toContain(
      'paxc',
    )
    expect(dedupeReasoning(thread, 'ok')).toBe('ok')
    expect(dedupeReasoning([], THOUGHT)).toBe(THOUGHT)
  })

  it('compares against the nearest assistant bubble only', () => {
    const thread = [
      msg('assistant', LONG, THOUGHT),
      msg('assistant', OTHER, 'different thought entirely, long enough to count'),
    ]
    expect(dedupeReasoning(thread, THOUGHT)).toBe(THOUGHT)
  })
})

describe('dedupeThread — double-persisted turns collapse on reopen', () => {
  it('drops the replayed copy, keeps order and user turns', () => {
    const loaded = [
      msg('user', 'pick back up on the brandkit'),
      msg('assistant', LONG),
      msg('assistant', LONG),
      msg('user', 'can you deploy to paxeer cloud'),
      msg('assistant', LONG),
      msg('assistant', OTHER),
    ]
    const out = dedupeThread(loaded)
    expect(out.map((m) => m.text)).toEqual([
      'pick back up on the brandkit',
      LONG,
      'can you deploy to paxeer cloud',
      OTHER,
    ])
  })
})
