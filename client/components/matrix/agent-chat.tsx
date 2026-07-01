'use client'

/**
 * AgentChat — the chat-first home surface.
 *
 * The user talks to the Liaison in plain language; behind the scenes the
 * compiler/planner/executor do the work and the Liaison narrates it back
 * as assistant turns. Technical detail lives in the "under the hood"
 * transcript (RunTranscriptSheet), not here.
 *
 * Built on the AI Elements primitives:
 *   - Conversation  → sticky-to-bottom auto-scroll + jump-to-latest button.
 *   - PromptInput   → auto-growing multiline composer pinned to the bottom,
 *                     with a wired attach menu, file chips, and submit state.
 * The mic button is wired to the Web Speech API for live dictation and
 * hides itself where the browser has no support.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslations } from 'next-intl'
import { toast } from 'sonner'
import { MicIcon, MicOffIcon, Sparkles } from '@/lib/matrix-icons'
import {
  uploadFromObjectURL,
  mediaKindForMime,
  loadMediaObjectURL,
  type MediaKind,
} from '@/lib/api/media'
import { Button } from '@/components/ui/button'
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from '@/components/ai-elements/conversation'
import { Message, MessageContent, MessageResponse } from '@/components/ai-elements/message'
import { Reasoning, ReasoningContent, ReasoningTrigger } from '@/components/ai-elements/reasoning'
import {
  Attachment,
  AttachmentInfo,
  AttachmentPreview,
  AttachmentRemove,
  Attachments,
} from '@/components/ai-elements/attachments'
import {
  PromptInput,
  PromptInputActionAddAttachments,
  PromptInputActionMenu,
  PromptInputActionMenuContent,
  PromptInputActionMenuTrigger,
  PromptInputBody,
  PromptInputButton,
  PromptInputFooter,
  PromptInputProvider,
  PromptInputSubmit,
  PromptInputTextarea,
  PromptInputTools,
  usePromptInputAttachments,
  usePromptInputController,
  type PromptInputMessage,
} from '@/components/ai-elements/prompt-input'
import Loader from '@/components/ui/box-loader'
import { cn } from '@/lib/utils'
import type { ChatMessage, ChatMedia, ChatPhase } from '@/hooks/api/useChat'
import type { Agent } from '@/lib/matrix-data'

/** First name (or handle) for a warm, personal greeting. Accepts a full
 *  name or an email; rejects machine identifiers (DIDs, URIs, UUIDs, long
 *  hex) so we never greet someone as "matrix://agent/did:matrix:…". */
function firstName(name?: string): string {
  const trimmed = (name ?? '').trim()
  if (!trimmed) return ''
  // Machine identifiers — bail to a generic greeting instead.
  if (/[:/]/.test(trimmed)) return ''
  if (/^[0-9a-f-]{16,}$/i.test(trimmed)) return ''
  const local = trimmed.includes('@') ? (trimmed.split('@')[0] ?? trimmed) : trimmed
  const first = local.split(/\s+/)[0] ?? ''
  if (!first || first.length > 24) return ''
  return first.charAt(0).toUpperCase() + first.slice(1)
}

/** Map a live agent's role to a starter-prompt translation key, so the
 *  starter chips reflect what THIS user's roster can actually do. Returns
 *  null when no role matches, signalling a generic per-agent fallback. */
function agentPromptKey(role: string): string | null {
  const r = role.toLowerCase()
  if (r.includes('research')) return 'suggestionResearch'
  if (r.includes('inbox') || r.includes('schedul')) return 'suggestionInbox'
  if (r.includes('sheet') || r.includes('ops') || r.includes('record')) return 'suggestionReconcile'
  if (r.includes('doc') || r.includes('deliverable') || r.includes('build'))
    return 'suggestionDocument'
  return null
}

function buildSuggestions(agents: Agent[], t: ReturnType<typeof useTranslations>): string[] {
  const seen = new Set<string>()
  const fromRoster: string[] = []
  for (const agent of agents) {
    const key = agentPromptKey(agent.role)
    const prompt = key
      ? t(key)
      : t('suggestionAgentFallback', { name: agent.name, role: agent.role.toLowerCase() })
    if (!seen.has(prompt)) {
      seen.add(prompt)
      fromRoster.push(prompt)
    }
    if (fromRoster.length === 3) break
  }
  if (fromRoster.length >= 3) return fromRoster
  return [t('suggestionResearch'), t('suggestionInbox'), t('suggestionDocument')]
}

export function AgentChat({
  messages,
  phase,
  send,
  userName,
  agents = [],
  pendingGate,
  answerGate,
}: {
  messages: ChatMessage[]
  phase: ChatPhase
  send: (text: string) => void
  /** Display label for the signed-in user, used in the welcome greeting. */
  userName?: string
  /** Live agent roster — drives the capability-aware starter prompts. */
  agents?: Agent[]
  pendingGate?: { question: string; options: string[]; nodeId: string; intentId: string } | null
  answerGate?: (approved: boolean, answer?: string) => void
}) {
  const t = useTranslations('agentChat')
  const busy = phase !== 'idle'
  const name = firstName(userName)
  const greeting = name ? t('welcomeBack', { name }) : t('welcome')
  const suggestions = useMemo(() => buildSuggestions(agents, t), [agents, t])
  const capabilityLine = useMemo(
    () =>
      agents.length
        ? agents
            .slice(0, 4)
            .map((a) => a.role)
            .join(' · ')
        : t('capabilityFallback'),
    [agents, t],
  )

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Conversation className="min-h-0 flex-1 overflow-y-auto">
        <ConversationContent className="mx-auto w-full max-w-3xl">
          {messages.length === 0 ? (
            <ConversationEmptyState
              className="min-h-[40vh]"
              icon={<Sparkles className="size-6" />}
              title={greeting}
              description={t('emptyDescription')}
            />
          ) : (
            messages.map((m) => <ChatBubble key={m.id} message={m} />)
          )}

          {pendingGate && (
            <Message from="assistant">
              <MessageContent>
                <div
                  role="group"
                  aria-label={t('gateRegionLabel')}
                  aria-live="polite"
                  className="flex flex-col gap-3"
                >
                  <p className="text-sm">{pendingGate.question || t('gateFallback')}</p>
                  <div className="flex flex-wrap gap-2">
                    {pendingGate.options && pendingGate.options.length > 0 ? (
                      pendingGate.options.map((opt) => (
                        <Button key={opt} size="sm" onClick={() => answerGate?.(true, opt)}>
                          {opt}
                        </Button>
                      ))
                    ) : (
                      <>
                        <Button size="sm" onClick={() => answerGate?.(true)}>
                          {t('approve')}
                        </Button>
                        <Button size="sm" variant="secondary" onClick={() => answerGate?.(false)}>
                          {t('deny')}
                        </Button>
                      </>
                    )}
                  </div>
                </div>
              </MessageContent>
            </Message>
          )}

          {busy && !pendingGate && (
            <Message from="assistant">
              <MessageContent>
                <div className="flex items-center gap-3 overflow-visible">
                  <Loader />
                  <span className="text-muted-foreground text-sm">
                    {phase === 'working' ? t('working') : t('thinking')}
                  </span>
                </div>
              </MessageContent>
            </Message>
          )}
        </ConversationContent>
        <ConversationScrollButton />
      </Conversation>

      <div className="mx-auto w-full max-w-3xl px-4 pt-2 pb-[calc(env(safe-area-inset-bottom)+4.5rem)] md:pb-4">
        {messages.length === 0 && (
          <div className="mb-3 flex flex-col items-center gap-2">
            <p className="text-muted-foreground/70 font-mono text-[10px] tracking-wider uppercase">
              {capabilityLine}
            </p>
            <div className="flex flex-wrap justify-center gap-2">
              {suggestions.map((s) => (
                <button
                  key={s}
                  type="button"
                  disabled={busy}
                  onClick={() => send(s)}
                  className="text-muted-foreground hover:bg-accent hover:text-foreground bg-card rounded-full px-3 py-1.5 text-xs transition disabled:opacity-50"
                >
                  {s}
                </button>
              ))}
            </div>
          </div>
        )}
        <PromptInputProvider>
          <ChatComposer onSend={send} busy={busy} />
        </PromptInputProvider>
      </div>
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/*  Message bubble + media rendering                                          */
/* -------------------------------------------------------------------------- */

interface RenderMedia {
  url: string
  kind: MediaKind
  prompt?: string
}

// Matches the attachment markers the composer embeds for uploaded files, e.g.
// "[attached image: /media/2026..ab.png]". Used to render the user's upload as
// a thumbnail and strip the marker from the visible bubble text.
const USER_MEDIA_RE = /\[attached (image|video|audio):\s*(\/media\/[^\]\s]+)\]/g

function parseUserMedia(text: string): { clean: string; items: RenderMedia[] } {
  const items: RenderMedia[] = []
  USER_MEDIA_RE.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = USER_MEDIA_RE.exec(text)) !== null) {
    items.push({ url: m[2], kind: m[1] as MediaKind })
  }
  const clean = text
    .replace(USER_MEDIA_RE, '')
    .replace(/\n{3,}/g, '\n\n')
    .trim()
  return { clean, items }
}

function ChatBubble({ message: m }: { message: ChatMessage }) {
  if (m.role === 'assistant') {
    return (
      <Message from="assistant">
        <MessageContent>
          {m.reasoning ? (
            <Reasoning className="mb-2" defaultOpen={false}>
              <ReasoningTrigger />
              <ReasoningContent>{m.reasoning}</ReasoningContent>
            </Reasoning>
          ) : null}
          {m.text ? <MessageResponse>{m.text}</MessageResponse> : null}
          <MediaList media={m.media} />
        </MessageContent>
      </Message>
    )
  }
  const { clean, items } = parseUserMedia(m.text)
  return (
    <Message from="user">
      <MessageContent>
        {items.length > 0 && (
          <div className="mb-2 flex flex-wrap gap-2">
            {items.map((it, i) => (
              <MediaItem key={`${it.url}-${i}`} url={it.url} kind={it.kind} />
            ))}
          </div>
        )}
        {clean && <span className="whitespace-pre-wrap">{clean}</span>}
      </MessageContent>
    </Message>
  )
}

function MediaList({ media }: { media?: ChatMedia[] }) {
  if (!media || media.length === 0) return null
  return (
    <div className="mt-2 flex flex-col gap-2">
      {media.map((mi, i) => (
        <MediaItem key={`${mi.url}-${i}`} url={mi.url} kind={mi.kind} prompt={mi.prompt} />
      ))}
    </div>
  )
}

/** Loads a /media reference as an authed blob object URL and renders it. The
 *  reference lives on the per-user machine behind the bearer, so a raw <img>
 *  src cannot reach it — we fetch the bytes and hand the element a blob URL. */
function MediaItem({ url, kind, prompt }: RenderMedia) {
  const t = useTranslations('agentChat')
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    let obj: string | null = null
    setSrc(null)
    setFailed(false)
    loadMediaObjectURL(url).then((o) => {
      if (!active) {
        if (o) URL.revokeObjectURL(o)
        return
      }
      if (o) {
        obj = o
        setSrc(o)
      } else {
        setFailed(true)
      }
    })
    return () => {
      active = false
      if (obj) URL.revokeObjectURL(obj)
    }
  }, [url])

  if (failed) {
    return <span className="text-muted-foreground text-xs">{t('mediaUnavailable')}</span>
  }
  if (!src) {
    return <div className="bg-muted h-48 w-full max-w-sm animate-pulse rounded-lg" aria-hidden />
  }
  if (kind === 'video') {
    return (
      <video
        src={src}
        controls
        className="max-h-[28rem] w-full max-w-md rounded-lg"
        preload="metadata"
      />
    )
  }
  if (kind === 'audio') {
    return <audio src={src} controls className="w-full max-w-md" preload="metadata" />
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt={prompt || t('mediaImageAlt')}
      className="max-h-[28rem] w-full max-w-md rounded-lg object-contain"
    />
  )
}

/* -------------------------------------------------------------------------- */
/*  Composer                                                                  */
/* -------------------------------------------------------------------------- */

function ChatComposer({ onSend, busy }: { onSend: (text: string) => void; busy: boolean }) {
  const t = useTranslations('agentChat')
  const { textInput } = usePromptInputController()

  // Keep the latest input value in a ref so async dictation results append
  // onto current text rather than a stale closure.
  const valueRef = useRef(textInput.value)
  useEffect(() => {
    valueRef.current = textInput.value
  }, [textInput.value])

  const dictation = useDictation((chunk) => {
    const prev = valueRef.current.trim()
    textInput.setInput(prev ? `${prev} ${chunk}` : chunk)
  })

  const [uploading, setUploading] = useState(false)

  const handleSubmit = useCallback(
    async (message: PromptInputMessage) => {
      const text = message.text.trim()
      const files = message.files ?? []
      if ((!text && files.length === 0) || busy || uploading) return

      // No attachments — send the text straight through.
      if (files.length === 0) {
        onSend(text)
        return
      }

      // Upload each attachment to the agent's machine volume, then embed its
      // /media reference so Neo can edit/animate/transcribe it. The composer
      // represents files as blob object URLs, so we re-read the bytes.
      setUploading(true)
      try {
        const markers: string[] = []
        for (const f of files) {
          if (!f.url) continue
          const filename = f.filename || 'upload'
          const up = await uploadFromObjectURL(f.url, filename)
          const kind = up.kind !== 'file' ? up.kind : mediaKindForMime(f.mediaType ?? up.mime)
          markers.push(`[attached ${kind}: ${up.url}]`)
        }
        const composed = [text, markers.join('\n')].filter(Boolean).join('\n\n')
        onSend(composed)
      } catch {
        toast.error(t('uploadError'))
      } finally {
        setUploading(false)
      }
    },
    [onSend, busy, uploading, t],
  )

  return (
    <PromptInput
      onSubmit={handleSubmit}
      multiple
      accept="image/*,video/*,audio/*"
      className="bg-card rounded-2xl shadow-sm"
    >
      <PromptInputBody>
        <ComposerAttachments />
        <PromptInputTextarea
          placeholder={uploading ? t('uploading') : t('placeholder')}
          disabled={busy || uploading}
          className="bg-transparent"
        />
      </PromptInputBody>
      <PromptInputFooter className="px-2 pb-2">
        <PromptInputTools>
          <PromptInputActionMenu>
            <PromptInputActionMenuTrigger />
            <PromptInputActionMenuContent>
              <PromptInputActionAddAttachments />
            </PromptInputActionMenuContent>
          </PromptInputActionMenu>
          {dictation.supported && (
            <PromptInputButton
              onClick={dictation.toggle}
              tooltip={dictation.listening ? t('stopDictation') : t('dictate')}
              aria-label={dictation.listening ? t('stopDictation') : t('dictate')}
              className={cn(dictation.listening && 'text-primary')}
              aria-pressed={dictation.listening}
            >
              {dictation.listening ? (
                <MicOffIcon className="size-4" />
              ) : (
                <MicIcon className="size-4" />
              )}
            </PromptInputButton>
          )}
        </PromptInputTools>
        <PromptInputSubmit
          status={busy || uploading ? 'submitted' : undefined}
          disabled={busy || uploading}
        />
      </PromptInputFooter>
    </PromptInput>
  )
}

function ComposerAttachments() {
  const { files, remove } = usePromptInputAttachments()
  if (files.length === 0) return null
  return (
    <Attachments variant="inline" className="px-3 pt-3">
      {files.map((file) => (
        <Attachment key={file.id} data={file} onRemove={() => remove(file.id)}>
          <AttachmentPreview />
          <AttachmentInfo />
          <AttachmentRemove />
        </Attachment>
      ))}
    </Attachments>
  )
}

/* -------------------------------------------------------------------------- */
/*  Dictation (Web Speech API)                                                */
/* -------------------------------------------------------------------------- */

interface SpeechRecognitionLike {
  lang: string
  continuous: boolean
  interimResults: boolean
  start: () => void
  stop: () => void
  onresult: ((event: SpeechRecognitionResultLike) => void) | null
  onend: (() => void) | null
  onerror: (() => void) | null
}

interface SpeechRecognitionResultLike {
  results: ArrayLike<ArrayLike<{ transcript: string }>>
}

type SpeechRecognitionCtor = new () => SpeechRecognitionLike

function getSpeechRecognitionCtor(): SpeechRecognitionCtor | null {
  if (typeof window === 'undefined') return null
  const w = window as unknown as {
    SpeechRecognition?: SpeechRecognitionCtor
    webkitSpeechRecognition?: SpeechRecognitionCtor
  }
  return w.SpeechRecognition ?? w.webkitSpeechRecognition ?? null
}

function useDictation(onResult: (transcript: string) => void) {
  const [supported, setSupported] = useState(false)
  const [listening, setListening] = useState(false)
  const recognitionRef = useRef<SpeechRecognitionLike | null>(null)
  const onResultRef = useRef(onResult)

  useEffect(() => {
    onResultRef.current = onResult
  }, [onResult])

  useEffect(() => {
    setSupported(getSpeechRecognitionCtor() !== null)
    return () => {
      recognitionRef.current?.stop()
      recognitionRef.current = null
    }
  }, [])

  const toggle = useCallback(() => {
    if (listening) {
      recognitionRef.current?.stop()
      return
    }
    const Ctor = getSpeechRecognitionCtor()
    if (!Ctor) return
    const recognition = new Ctor()
    recognition.lang = 'en-US'
    recognition.continuous = false
    recognition.interimResults = false
    recognition.onresult = (event) => {
      const last = event.results[event.results.length - 1]
      const transcript = last?.[0]?.transcript?.trim()
      if (transcript) onResultRef.current(transcript)
    }
    recognition.onend = () => setListening(false)
    recognition.onerror = () => setListening(false)
    recognitionRef.current = recognition
    recognition.start()
    setListening(true)
  }, [listening])

  return { supported, listening, toggle }
}
