'use client'

/**
 * Design Mode — top toolbar.
 *
 * Device frame switcher, breakpoint bucket selector, select/move mode, undo/
 * redo, export, reset, and close. Edits always target the active breakpoint.
 */
import { useState } from 'react'
import {
  Download,
  Monitor,
  MousePointer2,
  Move,
  RotateCcw,
  Smartphone,
  Tablet,
  Undo2,
  Redo2,
  X,
} from 'lucide-react'
import { ExportPanel } from './export-panel'
import { IconButton } from './controls'
import { BREAKPOINTS, DEVICES } from '@/lib/design/breakpoints'
import { useDesignStore } from '@/lib/design/store'
import type { PreviewDevice } from '@/lib/design/types'

const DEVICE_ICON: Record<PreviewDevice, typeof Monitor> = {
  responsive: Monitor,
  desktop: Monitor,
  tablet: Tablet,
  mobile: Smartphone,
}

export function Toolbar() {
  const enabled = useDesignStore((s) => s.enabled)
  const device = useDesignStore((s) => s.device)
  const bp = useDesignStore((s) => s.activeBreakpoint)
  const mode = useDesignStore((s) => s.mode)
  const overrides = useDesignStore((s) => s.overrides)
  const canUndo = useDesignStore((s) => s.past.length > 0)
  const canRedo = useDesignStore((s) => s.futureStack.length > 0)
  const [exportOpen, setExportOpen] = useState(false)

  if (!enabled) return null

  const store = useDesignStore.getState

  return (
    <>
      <div
        data-mx-ui="true"
        className="fixed top-3 left-1/2 z-[2147483602] flex -translate-x-1/2 items-center gap-2 rounded-lg bg-[#0a0a0b] px-2 py-1.5 text-white shadow-2xl"
      >
        <span className="px-1 text-[11px] font-semibold tracking-wide text-[#E4E4E7]">Design</span>

        {/* device frame */}
        <div className="flex items-center gap-0.5 rounded bg-[#161618] p-0.5">
          {DEVICES.map((d) => {
            const Icon = DEVICE_ICON[d.key]
            const active = device === d.key
            return (
              <button
                key={d.key}
                type="button"
                title={d.label + (d.width ? ` · ${d.width}px` : '')}
                onClick={() => store().setDevice(d.key)}
                className="flex h-6 items-center gap-1 rounded-[3px] px-1.5 text-[11px] font-medium transition-colors"
                style={
                  active ? { backgroundColor: '#004CED', color: '#fff' } : { color: '#A1A1AA' }
                }
              >
                <Icon size={13} />
              </button>
            )
          })}
        </div>

        {/* breakpoint bucket */}
        <div className="flex items-center gap-0.5 rounded bg-[#161618] p-0.5">
          {BREAKPOINTS.map((b) => {
            const active = bp === b.key
            return (
              <button
                key={b.key}
                type="button"
                title={`Edit ${b.label}${b.min ? ` (≥${b.min}px)` : ''}`}
                onClick={() => store().setBreakpoint(b.key)}
                className="h-6 rounded-[3px] px-1.5 text-[10px] font-semibold transition-colors"
                style={
                  active ? { backgroundColor: '#004CED', color: '#fff' } : { color: '#A1A1AA' }
                }
              >
                {b.label}
              </button>
            )
          })}
        </div>

        {/* mode */}
        <div className="flex items-center gap-0.5 rounded bg-[#161618] p-0.5">
          <IconButton
            active={mode === 'select'}
            title="Select"
            onClick={() => store().setMode('select')}
          >
            <MousePointer2 size={14} />
          </IconButton>
          <IconButton
            active={mode === 'move'}
            title="Move (drag to reposition)"
            onClick={() => store().setMode('move')}
          >
            <Move size={14} />
          </IconButton>
        </div>

        {/* history */}
        <div className="flex items-center gap-0.5">
          <IconButton disabled={!canUndo} title="Undo" onClick={() => store().undo()}>
            <Undo2 size={14} />
          </IconButton>
          <IconButton disabled={!canRedo} title="Redo" onClick={() => store().redo()}>
            <Redo2 size={14} />
          </IconButton>
        </div>

        {/* actions */}
        <div className="flex items-center gap-0.5">
          <IconButton title="Export CSS / Tailwind / JSON" onClick={() => setExportOpen(true)}>
            <Download size={14} />
          </IconButton>
          <IconButton
            title="Reset all overrides"
            onClick={() => {
              if (Object.keys(overrides).length && window.confirm('Discard all design overrides?'))
                store().resetAll()
            }}
          >
            <RotateCcw size={14} />
          </IconButton>
          <IconButton
            title="Exit design mode (Cmd/Ctrl+Shift+D)"
            onClick={() => store().setEnabled(false)}
          >
            <X size={14} />
          </IconButton>
        </div>
      </div>

      {exportOpen ? <ExportPanel onClose={() => setExportOpen(false)} /> : null}
    </>
  )
}
