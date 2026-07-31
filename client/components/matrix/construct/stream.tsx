'use client'

/**
 * Stream — an append-only temporal byte/line/event sequence: terminal output,
 * logs, a raw reasoning trace. Rendered as a tone-only window (three quiet
 * traffic dots, no stroke), mono body, with per-channel tinting (a command vs
 * stdout vs stderr) and a live caret while open.
 */
import { TerminalIcon } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { Stream as StreamPayload, StreamChunk } from '@/lib/construct/types.gen'

function chunkTone(channel?: string): string {
  if (channel === 'command') return 'text-foreground'
  if (channel === 'stderr') return 'text-destructive'
  return 'text-muted-foreground'
}

function ChunkLine({ chunk }: { chunk: StreamChunk }) {
  if (chunk.channel === 'command') {
    return (
      <div className="flex gap-2">
        <span className="text-primary shrink-0 select-none">$</span>
        <span className="text-foreground break-all whitespace-pre-wrap">
          {chunk.text.replace(/^\$\s*/, '')}
        </span>
      </div>
    )
  }
  return (
    <pre className={cn('break-all whitespace-pre-wrap', chunkTone(chunk.channel))}>
      {chunk.text}
    </pre>
  )
}

export function StreamView({ stream }: { stream: StreamPayload }) {
  const chunks = stream.chunks ?? []
  return (
    <div className="bg-background overflow-hidden rounded-xl">
      <div className="bg-foreground/[0.045] flex items-center gap-2.5 px-3 py-2">
        <span aria-hidden className="flex shrink-0 items-center gap-1.5">
          <span className="bg-foreground/15 size-2.5 rounded-full" />
          <span className="bg-foreground/15 size-2.5 rounded-full" />
          <span className="bg-foreground/15 size-2.5 rounded-full" />
        </span>
        <TerminalIcon className="text-muted-foreground size-3.5" />
        <span className="text-foreground/80 text-xs font-medium">{stream.title || 'Stream'}</span>
        {stream.source && (
          <span className="text-muted-foreground/70 ml-auto truncate font-mono text-[0.68rem]">
            {stream.source}
          </span>
        )}
      </div>
      <div className="max-h-72 overflow-auto px-3.5 py-3 font-mono text-[0.78rem] leading-relaxed">
        {chunks.map((c, i) => (
          <ChunkLine key={`${c.seq}-${i}`} chunk={c} />
        ))}
        {!stream.closed && (
          <span className="bg-primary mt-1.5 inline-block h-3.5 w-1.5 animate-pulse align-middle" />
        )}
      </div>
    </div>
  )
}
