'use client'

import { AnimatePresence, motion, MotionConfig } from 'motion/react'
import { Cpu, ShieldCheck, Activity, ChevronRight, type LucideIcon } from '@/lib/matrix-icons'
import { useEffect, useMemo, useRef, useState } from 'react'
import useMeasure from 'react-use-measure'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import { Switch } from '@/components/ui/switch'
import { Slider } from '@/components/ui/slider'
import type { PolicyToggles } from '@/lib/matrix-data'

function useClickOutside(ref: React.RefObject<HTMLElement | null>, handler: () => void) {
  useEffect(() => {
    function onDown(e: MouseEvent | TouchEvent) {
      const el = ref.current
      if (!el || el.contains(e.target as Node)) return
      handler()
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('touchstart', onDown)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('touchstart', onDown)
    }
  }, [ref, handler])
}

interface Tab {
  title: string
  icon: LucideIcon
}

const transition = { delay: 0.08, type: 'spring' as const, bounce: 0, duration: 0.5 }

const variants = {
  initial: (d: number) => ({ x: `${110 * d}%`, opacity: 0 }),
  active: { x: '0%', opacity: 1 },
  exit: (d: number) => ({ x: `${-110 * d}%`, opacity: 0 }),
}

export function ControlDock({
  modelName,
  onPickModel,
  toggles,
  onToggle,
  perTaskUsd,
  onPerTaskChange,
  className,
}: {
  modelName: string
  onPickModel: () => void
  toggles: PolicyToggles
  onToggle: (key: keyof PolicyToggles, value: boolean) => void
  perTaskUsd: number
  onPerTaskChange: (usd: number) => void
  className?: string
}) {
  const t = useTranslations('controlDock')

  const tabs: Tab[] = [
    { title: t('modelTab'), icon: Cpu },
    { title: t('policiesTab'), icon: ShieldCheck },
    { title: t('statusTab'), icon: Activity },
  ]

  const [selected, setSelected] = useState<number | null>(null)
  const [direction, setDirection] = useState(1)
  const [measureRef, bounds] = useMeasure()

  const containerRef = useRef<HTMLDivElement>(null)
  useClickOutside(containerRef, () => setSelected(null))

  function selectTab(index: number) {
    if (selected === null) return setSelected(index)
    if (selected === index) return setSelected(null)
    setDirection(index > selected ? 1 : -1)
    setSelected(index)
  }

  const content = useMemo(() => {
    switch (selected) {
      case 0:
        return (
          <div className="flex flex-col gap-1 pt-1">
            <p className="text-muted-foreground px-2 text-xs">{t('activeModel')}</p>
            <button
              type="button"
              onClick={onPickModel}
              className="hover:bg-accent flex items-center justify-between gap-2 rounded-xl px-2 py-2.5 text-left text-sm font-medium transition-colors"
            >
              <span className="flex items-center gap-2">
                <Cpu className="text-primary size-4" />
                {modelName}
              </span>
              <ChevronRight className="text-muted-foreground size-4" />
            </button>
            <p className="text-muted-foreground px-2 pt-1 text-xs">{t('modelHelp')}</p>
          </div>
        )
      case 1: {
        const rows: { key: keyof PolicyToggles; label: string }[] = [
          { key: 'approveOnchain', label: t('approveSpends') },
          { key: 'approveExternalSend', label: t('approveMessages') },
          { key: 'freezeAll', label: t('freezeAll') },
        ]
        return (
          <div className="flex flex-col gap-2 pt-1">
            <div className="px-2">
              <div className="flex items-center justify-between text-sm">
                <span>{t('perTaskLimit')}</span>
                <span className="text-muted-foreground font-mono text-xs">${perTaskUsd}</span>
              </div>
              <Slider
                value={[perTaskUsd]}
                onValueChange={(v) => onPerTaskChange(v[0])}
                min={1}
                max={50}
                step={1}
                className="mt-2"
              />
            </div>
            <div className="flex flex-col">
              {rows.map((r) => (
                <label
                  key={r.key}
                  className="hover:bg-accent flex cursor-pointer items-center justify-between gap-2 rounded-lg px-2 py-2 text-sm transition-colors"
                >
                  <span
                    className={cn(r.key === 'freezeAll' && toggles.freezeAll && 'text-destructive')}
                  >
                    {r.label}
                  </span>
                  <Switch checked={toggles[r.key]} onCheckedChange={(v) => onToggle(r.key, v)} />
                </label>
              ))}
            </div>
          </div>
        )
      }
      case 2:
        return (
          <div className="flex flex-col gap-3 pt-2">
            {[
              { label: t('runtime'), value: 27, ok: true, note: t('operational') },
              { label: t('rpcStatus'), value: 24, ok: true, note: t('operational') },
            ].map((row) => (
              <div key={row.label} className="space-y-1.5 px-2">
                <div className="flex justify-between text-xs">
                  <span className="text-muted-foreground">{row.label}</span>
                  <span className="text-primary/70">{row.note}</span>
                </div>
                <div className="flex h-8 w-full justify-between gap-0.5">
                  {Array.from({ length: 29 }).map((_, i) => (
                    <span
                      key={i}
                      className={cn(
                        'h-8 w-0.5 rounded-full',
                        i < row.value ? 'bg-primary' : 'bg-muted',
                      )}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>
        )
      default:
        return null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected, modelName, onPickModel, toggles, onToggle, perTaskUsd, onPerTaskChange, t])

  return (
    <div
      className={cn(
        'pointer-events-none fixed inset-x-0 bottom-6 z-20 hidden justify-center md:flex',
        className,
      )}
    >
      <div ref={containerRef} className="pointer-events-auto">
        <MotionConfig transition={{ duration: 0.45, type: 'spring', bounce: 0 }}>
          <motion.div
            initial={false}
            animate={{
              height: selected === null ? 52 : bounds.height + 56,
              width: selected === null ? 248 : 300,
            }}
            className="border-border bg-card/95 relative mx-auto overflow-hidden rounded-2xl border shadow-lg backdrop-blur"
          >
            <div>
              <AnimatePresence mode="popLayout" initial={false} custom={direction}>
                <motion.div
                  key={selected}
                  variants={variants}
                  initial="initial"
                  animate="active"
                  exit="exit"
                  custom={direction}
                  transition={transition}
                  className="px-2 pb-[52px]"
                >
                  <div ref={measureRef}>{content}</div>
                </motion.div>
              </AnimatePresence>

              <div className="border-border bg-card/95 absolute inset-x-0 bottom-0 border-t p-1.5 backdrop-blur">
                <div className="flex items-center justify-center gap-1">
                  {tabs.map((tab, index) => {
                    const isActive = selected === index
                    return (
                      <motion.button
                        key={tab.title}
                        initial={false}
                        animate={{
                          gap: isActive ? '0.4rem' : 0,
                          paddingLeft: isActive ? '0.85rem' : '0.6rem',
                          paddingRight: isActive ? '0.85rem' : '0.6rem',
                        }}
                        onClick={() => selectTab(index)}
                        className={cn(
                          'relative flex h-9 items-center justify-center rounded-xl text-sm font-medium transition-colors',
                          isActive
                            ? 'bg-primary/10 text-primary'
                            : 'text-muted-foreground hover:bg-accent hover:text-foreground',
                        )}
                      >
                        <tab.icon className="size-4" />
                        <AnimatePresence initial={false}>
                          {isActive && (
                            <motion.span
                              initial={{ width: 0, opacity: 0 }}
                              animate={{ width: 'auto', opacity: 1 }}
                              exit={{ width: 0, opacity: 0 }}
                              transition={transition}
                              className="overflow-hidden whitespace-nowrap"
                            >
                              {tab.title}
                            </motion.span>
                          )}
                        </AnimatePresence>
                      </motion.button>
                    )
                  })}
                </div>
              </div>
            </div>
          </motion.div>
        </MotionConfig>
      </div>
    </div>
  )
}
