'use client'

/**
 * NeoMemoryCard — Neo's activated working memory for the current run.
 *
 * Surfaced from the `memory.activation` event (once per turn): a plain-language
 * "story so far" recap plus a coarse, oldest-first timeline of what has
 * happened. It rides the durable trace, so it survives conversation reopen +
 * agent respawn.
 *
 * READ-ONLY by design: the agent's memory is never editable by a human. This
 * component renders it as prose + a beaded timeline and offers no edit control.
 *
 * Design system: separation by background TONE only (bg-card / bg-muted), no
 * border strokes for depth, single accent via text-primary, no emojis/glow.
 */
import { BrainIcon } from '@/lib/matrix-icons'
import type { NeoMemory } from '@/hooks/api/useChat'
import { useTranslations } from 'next-intl'

/** Render Neo's activated working memory (from memory.activation events). */
export function NeoMemoryCard({ memory }: { memory?: NeoMemory }) {
  const t = useTranslations('neoMemory')
  if (!memory) return null
  const { storySoFar, timeline, excerpts } = memory
  if (!storySoFar && timeline.length === 0 && excerpts.length === 0) return null

  return (
    <div className="flex flex-col gap-3">
      {storySoFar ? (
        <div className="bg-card flex flex-col gap-2 rounded-xl p-3.5">
          <div className="flex items-center gap-2">
            <span className="bg-primary/15 text-primary grid size-7 shrink-0 place-items-center rounded-lg">
              <BrainIcon className="size-4" />
            </span>
            <p className="text-foreground text-sm font-bold">{t('storyTitle')}</p>
          </div>
          <p className="text-muted-foreground text-[0.85rem] leading-relaxed whitespace-pre-line">
            {storySoFar}
          </p>
        </div>
      ) : null}

      {timeline.length > 0 ? (
        <div className="bg-card flex flex-col gap-2.5 rounded-xl p-3.5">
          <p className="text-foreground px-0.5 text-sm font-bold">{t('timelineTitle')}</p>
          <ol className="flex flex-col">
            {timeline.map((beat, i) => {
              const last = i === timeline.length - 1
              return (
                <li key={`${i}-${beat}`} className="flex gap-3">
                  {/* bead + connector rail — tone-only, no stroke */}
                  <div className="flex flex-col items-center pt-1">
                    <span className="bg-primary size-2 shrink-0 rounded-full" />
                    {!last ? <span className="bg-muted mt-1 w-px flex-1" /> : null}
                  </div>
                  <p className="text-muted-foreground pb-3 text-[0.82rem] leading-snug">{beat}</p>
                </li>
              )
            })}
          </ol>
        </div>
      ) : null}

      {excerpts.length > 0 ? (
        <div className="bg-card flex flex-col gap-3 rounded-xl p-3.5">
          <p className="text-foreground text-sm font-bold">{t('recalledTitle')}</p>
          <ol className="flex flex-col gap-2">
            {excerpts.map((excerpt) => (
              <li
                key={`${excerpt.conversationId}-${excerpt.seqLo}-${excerpt.seqHi}`}
                className="bg-muted flex flex-col gap-1.5 rounded-lg p-3"
              >
                <div className="text-muted-foreground flex flex-wrap gap-x-2 text-[0.72rem] font-medium">
                  <span>{excerpt.exact ? t('exact') : t('approximate')}</span>
                  <span>{t('conversation', { id: excerpt.conversationId })}</span>
                  {excerpt.date ? <span>{excerpt.date}</span> : null}
                </div>
                <p className="text-foreground text-[0.82rem] leading-relaxed whitespace-pre-line">
                  {excerpt.text}
                </p>
              </li>
            ))}
          </ol>
        </div>
      ) : null}
    </div>
  )
}
