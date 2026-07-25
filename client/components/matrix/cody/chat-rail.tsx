'use client'

/**
 * The workbench chat rail (NEO-WORKBENCH req 1) — a REAL Neo conversation:
 * the same useChat reducer, message components, and composer the dashboard
 * uses, plus the inline artifact/action cards folding this run's workbench
 * steps (req 3). The rail is the primary column; the workbench slides in
 * beside it.
 */
import { useEffect, useRef, useState } from 'react'

import { Link } from '@/i18n/navigation'
import type { UseChatResult } from '@/hooks/api/useChat'
import {
  NeoAssistantMessage,
  NeoLiveTurn,
  NeoUserMessage,
} from '@/components/matrix/neo/neo-message'
import { NeoComposer, type NeoMode } from '@/components/matrix/neo/neo-composer'
import { ArtifactCards } from '@/components/matrix/cody/artifact-card'
import { isNearBottom } from '@/lib/cody/scroll'

const STARTERS = [
  {
    label: 'Build a landing page',
    prompt:
      'Build a striking landing page for this project — bold typography, real content, no template look.',
  },
  {
    label: 'Start a React app',
    prompt:
      'Scaffold a React + Vite app with routing and a clean component structure, then run it.',
  },
  {
    label: 'Explain this codebase',
    prompt:
      'Walk me through this project: what it does, how it is laid out, and where to start reading.',
  },
] as const

/** The first-run hero: what this surface IS, plus three starters that fill
 *  the composer (the user still owns the send). */
function EmptyHero({
  projectName,
  onPick,
}: {
  projectName?: string
  onPick: (draft: string) => void
}) {
  return (
    <div className="flex flex-col items-center gap-4 pt-10 text-center sm:gap-5 sm:pt-16">
      <div>
        <h2 className="text-2xl font-medium">What should Neo build?</h2>
        <p className="text-muted-foreground mx-auto mt-2 max-w-md text-sm">
          {projectName
            ? `Describe it and Neo writes real files in ${projectName}, runs the commands, and shows you the result.`
            : 'Describe it and Neo writes real files, runs the commands, and shows you the result.'}
        </p>
      </div>
      <div className="flex flex-wrap justify-center gap-2">
        {STARTERS.map((s) => (
          <button
            key={s.label}
            onClick={() => onPick(s.prompt)}
            className="bg-surface-secondary hover:bg-surface-hover min-h-11 rounded-full px-3.5 py-2 text-xs"
          >
            {s.label}
          </button>
        ))}
      </div>
      <Link
        href="/code"
        className="text-muted-foreground hover:text-foreground inline-flex min-h-11 items-center text-xs underline underline-offset-2"
      >
        Browse 140+ design templates
      </Link>
    </div>
  )
}

export function ChatRail({
  chat,
  projectName,
  onOpenFile,
  onRevealTerminal,
  onStop,
}: {
  chat: UseChatResult
  projectName?: string
  onOpenFile: (path: string) => void
  onRevealTerminal: () => void
  onStop: () => void
}) {
  const [draft, setDraft] = useState('')
  const [mode, setMode] = useState<NeoMode>('auto')
  const scrollRef = useRef<HTMLDivElement>(null)
  // Whether new content should auto-scroll into view. Starts true; flips off
  // when the user scrolls up to read, back on when they return near the bottom.
  const followRef = useRef(true)

  const { messages, task, phase, resuming, connectionRetrying } = chat
  const working = phase === 'working' || phase === 'thinking'

  // Track intentional reader position: only follow while near the bottom.
  const onScroll = () => {
    const el = scrollRef.current
    if (el) followRef.current = isNearBottom(el)
  }

  // Follow the conversation, but never yank the view while the user is reading
  // older turns — resume following only once they return near the bottom.
  useEffect(() => {
    const el = scrollRef.current
    if (el && followRef.current) el.scrollTop = el.scrollHeight
  }, [messages.length, task?.steps.length, task?.streamingAnswer])

  // Switching conversations jumps to the newest turn and re-arms following.
  useEffect(() => {
    followRef.current = true
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [chat.conversationId])

  const submit = () => {
    const text = draft.trim()
    if (!text) return
    // Sending is an intentional return to the live tail: re-arm following.
    followRef.current = true
    chat.send(text)
    setDraft('')
  }

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <div
        ref={scrollRef}
        onScroll={onScroll}
        className="min-h-0 flex-1 overflow-y-auto px-3 py-3 sm:px-4"
      >
        <div className="mx-auto flex w-full max-w-2xl flex-col gap-3">
          {messages.length === 0 && !working && !chat.error ? (
            <EmptyHero projectName={projectName} onPick={setDraft} />
          ) : null}

          {chat.error ? (
            <div
              role="alert"
              className="bg-destructive/10 text-destructive rounded-lg px-3 py-2 text-sm"
            >
              {chat.error}
            </div>
          ) : null}

          {messages.map((m, index) =>
            m.role === 'user' ? (
              <NeoUserMessage
                key={m.id}
                message={m}
                onFork={
                  chat.conversationId && !working
                    ? () => chat.forkConversation(chat.conversationId!, index + 1)
                    : undefined
                }
              />
            ) : (
              <NeoAssistantMessage
                key={m.id}
                message={m}
                onFork={
                  chat.conversationId && !working
                    ? () => chat.forkConversation(chat.conversationId!, index + 1)
                    : undefined
                }
              />
            ),
          )}

          {/* This run's work, folded into artifact cards (live + reopen). */}
          {task && task.steps.length > 0 ? (
            <ArtifactCards
              steps={task.steps}
              onOpenFile={onOpenFile}
              onRevealTerminal={onRevealTerminal}
            />
          ) : null}

          {resuming ? (
            <div className="text-muted-foreground text-sm">Reconnecting to your task…</div>
          ) : connectionRetrying ? (
            <div className="text-muted-foreground text-sm">Connection lost — retrying…</div>
          ) : null}

          {working && task && !task.done ? (
            <NeoLiveTurn
              thinking={task.thinking}
              streamingAnswer={task.streamingAnswer}
              label="Working"
              reduce={false}
            />
          ) : null}
        </div>
      </div>

      <div className="shrink-0 px-3 pb-3 sm:px-4 sm:pb-4">
        <div className="mx-auto w-full max-w-2xl">
          <NeoComposer
            value={draft}
            onChange={setDraft}
            onSubmit={submit}
            mode={mode}
            onModeChange={setMode}
            variant="bar"
            placeholder={projectName ? `Ask Neo about ${projectName}…` : 'Ask Neo anything…'}
            isRunning={working}
            onStop={onStop}
          />
        </div>
      </div>
    </div>
  )
}
