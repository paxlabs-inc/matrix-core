'use client'

/**
 * Design Mode — root provider.
 *
 * Wraps the entire app in a `[data-mx-canvas]` element (the anchor for all
 * selectors and the device frame), keeps the live override stylesheet injected
 * at all times (so saved edits are part of the real app, not just while
 * editing), and mounts the editor chrome when design mode is on.
 *
 * Toggle: Cmd/Ctrl+Shift+D, or the launcher button. Esc deselects / exits.
 */
import { type CSSProperties, type ReactNode, useEffect } from 'react'
import { PenTool } from 'lucide-react'
import { Inspector } from './inspector'
import { SelectionLayer } from './selection-layer'
import { Toolbar } from './toolbar'
import { useMounted } from './hooks'
import { deviceDef } from '@/lib/design/breakpoints'
import { buildCSS, injectCSS } from '@/lib/design/css-engine'
import { useDesignStore } from '@/lib/design/store'

export function DesignModeProvider({ children }: { children: ReactNode }) {
  const mounted = useMounted()
  const enabled = useDesignStore((s) => s.enabled)
  const device = useDesignStore((s) => s.device)
  const overrides = useDesignStore((s) => s.overrides)

  // Keep the override stylesheet live (real media queries; identical to export).
  useEffect(() => {
    if (!mounted) return
    injectCSS(buildCSS(overrides, { mode: 'responsive', important: true }))
  }, [mounted, overrides])

  // Keyboard: toggle + escape. Design Mode is a development-only authoring
  // tool — never wire the toggle (or the launcher below) in production builds.
  useEffect(() => {
    if (process.env.NODE_ENV === 'production') return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.shiftKey && (e.key === 'd' || e.key === 'D')) {
        e.preventDefault()
        useDesignStore.getState().toggleEnabled()
        return
      }
      if (e.key === 'Escape' && useDesignStore.getState().enabled) {
        const s = useDesignStore.getState()
        if (s.selected) s.select(null)
        else s.setEnabled(false)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const dev = deviceDef(device)
  const framed = mounted && enabled && device !== 'responsive' && dev.width != null
  const canvasStyle: CSSProperties | undefined = framed
    ? {
        width: dev.width ?? undefined,
        maxWidth: '100%',
        marginInline: 'auto',
        minHeight: '100vh',
        outline: '1px solid #1f1f22',
        transition: 'width 0.18s ease',
      }
    : undefined

  return (
    <>
      <div data-mx-canvas="true" style={canvasStyle}>
        {children}
      </div>

      {mounted && enabled ? (
        <>
          <Toolbar />
          <SelectionLayer />
          <Inspector />
        </>
      ) : null}

      {mounted && !enabled && process.env.NODE_ENV !== 'production' ? (
        <button
          type="button"
          data-mx-ui="true"
          title="Open Design Mode (Cmd/Ctrl+Shift+D)"
          onClick={() => useDesignStore.getState().setEnabled(true)}
          className="fixed bottom-3 left-3 z-[2147483602] flex h-9 w-9 items-center justify-center rounded-full text-white shadow-2xl transition-transform hover:scale-105"
          style={{ backgroundColor: '#004CED' }}
        >
          <PenTool size={16} />
        </button>
      ) : null}
    </>
  )
}
