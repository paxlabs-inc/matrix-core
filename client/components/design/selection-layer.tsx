'use client'

/**
 * Design Mode — selection + manipulation overlay.
 *
 * Sits above the app at a high z-index. While enabled it:
 *  - intercepts hover/click on real components to select them (capture phase,
 *    skipping editor chrome and blocking the app's own handlers);
 *  - draws a hover outline and a selection box glued to the live element rect
 *    (rAF-tracked so it follows scroll/resize/edits);
 *  - lets you drag the grip to move (writes `transform: translate`) and drag any
 *    of 8 handles to resize (writes width/height, compensating top/left edges).
 *
 * All writes target the active breakpoint bucket, so moving/resizing on a given
 * device only affects that device.
 */
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent as ReactPointerEvent,
} from 'react'
import { ACCENT } from './controls'
import {
  computeSelector,
  isEditorNode,
  resolveSelector,
  selectorLabel,
} from '@/lib/design/selector'
import { useDesignStore } from '@/lib/design/store'
import { formatTranslate, parseTranslate } from '@/lib/design/values'

interface Rect {
  top: number
  left: number
  width: number
  height: number
}

type HandleDir = 'n' | 's' | 'e' | 'w' | 'ne' | 'nw' | 'se' | 'sw'

const HANDLES: HandleDir[] = ['nw', 'n', 'ne', 'e', 'se', 's', 'sw', 'w']

const CURSOR: Record<HandleDir, string> = {
  n: 'ns-resize',
  s: 'ns-resize',
  e: 'ew-resize',
  w: 'ew-resize',
  ne: 'nesw-resize',
  sw: 'nesw-resize',
  nw: 'nwse-resize',
  se: 'nwse-resize',
}

function rectOf(el: HTMLElement | null): Rect | null {
  if (!el) return null
  const r = el.getBoundingClientRect()
  return { top: r.top, left: r.left, width: r.width, height: r.height }
}

export function SelectionLayer() {
  const enabled = useDesignStore((s) => s.enabled)
  const selected = useDesignStore((s) => s.selected)
  const mode = useDesignStore((s) => s.mode)

  const [hoverRect, setHoverRect] = useState<Rect | null>(null)
  const [hoverLabel, setHoverLabel] = useState('')
  const [selRect, setSelRect] = useState<Rect | null>(null)

  // --- hover + click selection (capture phase) ------------------------------
  useEffect(() => {
    if (!enabled) return

    const targetAt = (x: number, y: number): HTMLElement | null => {
      const el = document.elementFromPoint(x, y) as HTMLElement | null
      if (!el || isEditorNode(el)) return null
      return el
    }

    const onMove = (e: MouseEvent) => {
      if (useDesignStore.getState().dragging) return
      const el = targetAt(e.clientX, e.clientY)
      if (!el) {
        setHoverRect(null)
        return
      }
      setHoverRect(rectOf(el))
      setHoverLabel(selectorLabel(computeSelector(el)))
    }

    const onClick = (e: MouseEvent) => {
      const el = targetAt(e.clientX, e.clientY)
      if (!el) return
      e.preventDefault()
      e.stopPropagation()
      useDesignStore.getState().select(computeSelector(el))
    }

    // Block the app's own mousedown/contextmenu so editing never triggers
    // navigation, text selection, or button presses on the real components.
    const block = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (isEditorNode(el)) return
      e.preventDefault()
      e.stopPropagation()
    }

    document.addEventListener('mousemove', onMove, true)
    document.addEventListener('click', onClick, true)
    document.addEventListener('mousedown', block, true)
    document.addEventListener('contextmenu', block, true)
    return () => {
      document.removeEventListener('mousemove', onMove, true)
      document.removeEventListener('click', onClick, true)
      document.removeEventListener('mousedown', block, true)
      document.removeEventListener('contextmenu', block, true)
      setHoverRect(null)
    }
  }, [enabled])

  // --- glue the selection box to the live element rect ----------------------
  useEffect(() => {
    if (!enabled || !selected) {
      setSelRect(null)
      return
    }
    let raf = 0
    const tick = () => {
      setSelRect(rectOf(resolveSelector(selected)))
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [enabled, selected])

  // --- drag-move + resize ---------------------------------------------------
  const dragRef = useRef<{
    kind: 'move' | HandleDir
    startX: number
    startY: number
    baseW: number
    baseH: number
    baseTX: number
    baseTY: number
  } | null>(null)

  const beginDrag = useCallback((kind: 'move' | HandleDir, e: ReactPointerEvent) => {
    const sel = useDesignStore.getState().selected
    const el = resolveSelector(sel)
    if (!sel || !el) return
    e.preventDefault()
    e.stopPropagation()
    ;(e.target as HTMLElement).setPointerCapture(e.pointerId)

    const bp = useDesignStore.getState().activeBreakpoint
    const r = el.getBoundingClientRect()
    const tform = useDesignStore.getState().overrides[sel]?.[bp]?.['transform']
    const t = parseTranslate(tform)
    dragRef.current = {
      kind,
      startX: e.clientX,
      startY: e.clientY,
      baseW: r.width,
      baseH: r.height,
      baseTX: t.x,
      baseTY: t.y,
    }
    useDesignStore.getState().pushHistory()
    useDesignStore.setState({ dragging: true })

    const onPointerMove = (ev: PointerEvent) => {
      const d = dragRef.current
      if (!d) return
      const dx = ev.clientX - d.startX
      const dy = ev.clientY - d.startY
      const store = useDesignStore.getState()
      const selector = store.selected
      if (!selector) return
      const props: Record<string, string> = {}

      if (d.kind === 'move') {
        props['transform'] = formatTranslate(d.baseTX + dx, d.baseTY + dy)
      } else {
        let w = d.baseW
        let h = d.baseH
        let tx = d.baseTX
        let ty = d.baseTY
        const dir = d.kind
        if (dir.includes('e')) w = d.baseW + dx
        if (dir.includes('w')) {
          w = d.baseW - dx
          tx = d.baseTX + dx
        }
        if (dir.includes('s')) h = d.baseH + dy
        if (dir.includes('n')) {
          h = d.baseH - dy
          ty = d.baseTY + dy
        }
        props['width'] = `${Math.max(0, Math.round(w))}px`
        props['height'] = `${Math.max(0, Math.round(h))}px`
        if (dir.includes('w') || dir.includes('n')) {
          props['transform'] = formatTranslate(tx, ty)
        }
      }
      store.setPropsLive(selector, store.activeBreakpoint, props)
    }

    const onPointerUp = () => {
      dragRef.current = null
      useDesignStore.setState({ dragging: false })
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('pointerup', onPointerUp)
    }

    window.addEventListener('pointermove', onPointerMove)
    window.addEventListener('pointerup', onPointerUp)
  }, [])

  if (!enabled) return null

  return (
    <div data-mx-ui="true" className="pointer-events-none fixed inset-0 z-[2147483600]">
      {/* hover outline */}
      {hoverRect &&
      (!selRect || hoverRect.top !== selRect.top || hoverRect.left !== selRect.left) ? (
        <div
          className="absolute"
          style={{
            top: hoverRect.top,
            left: hoverRect.left,
            width: hoverRect.width,
            height: hoverRect.height,
            outline: `1px dashed ${ACCENT}`,
            outlineOffset: '-1px',
          }}
        >
          <span
            className="absolute -top-[18px] left-0 rounded-[3px] px-1.5 py-0.5 text-[10px] font-medium whitespace-nowrap text-white"
            style={{ backgroundColor: ACCENT }}
          >
            {hoverLabel}
          </span>
        </div>
      ) : null}

      {/* selection box */}
      {selRect ? (
        <div
          className="absolute"
          style={{
            top: selRect.top,
            left: selRect.left,
            width: selRect.width,
            height: selRect.height,
            outline: `1.5px solid ${ACCENT}`,
            outlineOffset: '-1px',
          }}
        >
          {/* move grip */}
          <div
            className="pointer-events-auto absolute -top-[20px] left-0 flex cursor-move items-center gap-1 rounded-[3px] px-1.5 py-0.5 text-[10px] font-semibold text-white"
            style={{ backgroundColor: ACCENT, touchAction: 'none' }}
            onPointerDown={(e) => beginDrag('move', e)}
          >
            {selected ? selectorLabel(selected) : ''}
          </div>

          {/* full-body drag surface in Move mode */}
          {mode === 'move' ? (
            <div
              className="pointer-events-auto absolute inset-0 cursor-move"
              style={{ touchAction: 'none' }}
              onPointerDown={(e) => beginDrag('move', e)}
            />
          ) : null}

          {/* size readout */}
          <span className="absolute right-0 -bottom-[18px] rounded-[3px] bg-[#1a1a1a] px-1.5 py-0.5 text-[10px] font-medium text-[#A1A1AA]">
            {Math.round(selRect.width)} × {Math.round(selRect.height)}
          </span>

          {/* resize handles */}
          {HANDLES.map((dir) => (
            <Handle key={dir} dir={dir} onPointerDown={(e) => beginDrag(dir, e)} />
          ))}
        </div>
      ) : null}
    </div>
  )
}

function Handle({
  dir,
  onPointerDown,
}: {
  dir: HandleDir
  onPointerDown: (e: ReactPointerEvent) => void
}) {
  const pos: CSSProperties = { position: 'absolute', touchAction: 'none' }
  const v = dir.includes('n') ? 'n' : dir.includes('s') ? 's' : 'c'
  const h = dir.includes('w') ? 'w' : dir.includes('e') ? 'e' : 'c'
  pos.top = v === 'n' ? -4 : v === 's' ? undefined : 'calc(50% - 4px)'
  pos.bottom = v === 's' ? -4 : undefined
  pos.left = h === 'w' ? -4 : h === 'e' ? undefined : 'calc(50% - 4px)'
  pos.right = h === 'e' ? -4 : undefined
  return (
    <div
      className="pointer-events-auto h-2 w-2 rounded-[2px] bg-white"
      style={{ ...pos, outline: `1px solid ${ACCENT}`, cursor: CURSOR[dir] }}
      onPointerDown={onPointerDown}
    />
  )
}
