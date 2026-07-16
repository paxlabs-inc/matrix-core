'use client'

/**
 * NeoAuditTrail — transparency surface for Cassandra 2.0's silent-voice
 * controller edits and identity-leak detections.
 *
 * Renders as a screen in Neo's Computer panel: each entry shows what
 * Cassandra changed (original → mod) or where an identity leak was caught.
 * Read-only; no edit controls.
 *
 * Design system: separation by background TONE only (bg-card / bg-muted),
 * no border strokes for depth, single accent via text-primary, no emojis/glow.
 */
import { ShieldAlert, ShieldCheck, BrainIcon } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { CassandraAuditEntry } from '@/hooks/api/useChat'

function SideBadge({ side }: { side?: string }) {
  if (!side) return null
  const isDoubt = side === 'doubt'
  return (
    <span
      className={cn(
        'rounded px-1.5 py-0.5 text-[0.6rem] font-medium tracking-wide uppercase',
        isDoubt ? 'bg-yellow-500/15 text-yellow-400' : 'bg-green-500/15 text-green-400',
      )}
    >
      {side}
    </span>
  )
}

function TriggerLabel({ trigger }: { trigger?: string }) {
  if (!trigger) return null
  const labels: Record<string, string> = {
    loop: 'Loop detected',
    cyclic: 'Cyclic reasoning',
    semantic_repeat: 'Semantic repeat',
    premature_close: 'Premature close',
    unverified_close: 'Unverified close',
    thrash: 'Thrashing',
    over_verify: 'Over-verifying',
    oscillation: 'Oscillating',
    refuted_premise: 'Refuted premise',
    ungrounded_close: 'Ungrounded close',
  }
  return <span className="text-muted-foreground text-[0.68rem]">{labels[trigger] ?? trigger}</span>
}

function ModEntry({ entry }: { entry: CassandraAuditEntry }) {
  return (
    <div className="bg-card flex flex-col gap-2.5 rounded-xl p-3.5">
      <div className="flex items-center gap-2">
        <span className="bg-primary/15 text-primary grid size-7 shrink-0 place-items-center rounded-lg">
          <BrainIcon className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground text-sm font-bold">Cassandra intervened</p>
          <div className="mt-0.5 flex items-center gap-2">
            <SideBadge side={entry.side} />
            <TriggerLabel trigger={entry.trigger} />
            {entry.step != null && (
              <span className="text-muted-foreground/60 text-[0.6rem]">step {entry.step}</span>
            )}
          </div>
        </div>
      </div>

      {entry.cassandraMod && (
        <div className="bg-muted/50 rounded-lg px-3 py-2.5">
          <p className="text-muted-foreground mb-1 text-[0.65rem] font-medium tracking-wide uppercase">
            What Cassandra added
          </p>
          <p className="text-foreground/90 text-[0.82rem] leading-relaxed whitespace-pre-line">
            {entry.cassandraMod}
          </p>
        </div>
      )}

      {entry.originalContent && (
        <details className="group">
          <summary className="text-muted-foreground hover:text-foreground cursor-pointer text-[0.72rem] font-medium transition-colors">
            Show original message
          </summary>
          <p className="text-muted-foreground/70 mt-1.5 text-[0.78rem] leading-relaxed whitespace-pre-line">
            {entry.originalContent}
          </p>
        </details>
      )}
    </div>
  )
}

function LeakEntry({ entry }: { entry: CassandraAuditEntry }) {
  return (
    <div className="bg-card flex items-start gap-3 rounded-xl p-3.5">
      <span className="grid size-7 shrink-0 place-items-center rounded-lg bg-yellow-500/15 text-yellow-400">
        <ShieldAlert className="size-4" />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-bold">Identity leak caught</p>
        <p className="text-muted-foreground mt-0.5 text-[0.78rem]">
          The model attempted to self-identify as a different AI during{' '}
          {entry.where === 'answer' ? 'answer generation' : 'delivery'}. Re-anchored to Neo
          identity.
        </p>
      </div>
    </div>
  )
}

export function NeoAuditTrail({ audit }: { audit?: CassandraAuditEntry[] }) {
  if (!audit || audit.length === 0) {
    return (
      <div className="text-muted-foreground/70 flex flex-col items-center gap-2 py-12 text-center">
        <ShieldCheck className="size-6" />
        <p className="text-xs">No interventions this run</p>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2.5">
      {audit.map((entry, i) =>
        entry.kind === 'mod' ? (
          <ModEntry key={`mod-${i}`} entry={entry} />
        ) : (
          <LeakEntry key={`leak-${i}`} entry={entry} />
        ),
      )}
    </div>
  )
}
