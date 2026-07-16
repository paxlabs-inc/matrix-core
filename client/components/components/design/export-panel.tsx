'use client'

/**
 * Design Mode — export panel.
 *
 * Renders the current override map as portable CSS, Tailwind classes, or JSON,
 * with copy + download. This is how visual edits flow back into source.
 */
import { useMemo, useState } from 'react'
import { Check, Copy, Download, X } from 'lucide-react'
import { exportCSS, exportJSON, exportTailwindText } from '@/lib/design/export'
import { useDesignStore } from '@/lib/design/store'

type Format = 'css' | 'tailwind' | 'json'

export function ExportPanel({ onClose }: { onClose: () => void }) {
  const overrides = useDesignStore((s) => s.overrides)
  const [format, setFormat] = useState<Format>('css')
  const [copied, setCopied] = useState(false)

  const content = useMemo(() => {
    if (format === 'css') return exportCSS(overrides)
    if (format === 'tailwind') return exportTailwindText(overrides)
    return exportJSON(overrides)
  }, [format, overrides])

  const count = Object.keys(overrides).length

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(content)
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    } catch {
      /* clipboard blocked — user can select manually */
    }
  }

  const download = () => {
    const ext = format === 'tailwind' ? 'txt' : format
    const blob = new Blob([content], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `matrix-design.${ext}`
    a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <div
      data-mx-ui="true"
      className="fixed inset-0 z-[2147483650] flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="flex h-[70vh] w-[640px] max-w-full flex-col overflow-hidden rounded-lg bg-[#0a0a0b] text-white shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between bg-[#111113] px-4 py-2.5">
          <div className="text-[13px] font-semibold">
            Export · {count} element{count === 1 ? '' : 's'} styled
          </div>
          <button type="button" onClick={onClose} className="text-[#A1A1AA] hover:text-white">
            <X size={16} />
          </button>
        </div>

        <div className="flex items-center gap-1 px-4 pt-3">
          {(['css', 'tailwind', 'json'] as Format[]).map((f) => (
            <button
              key={f}
              type="button"
              onClick={() => setFormat(f)}
              className="rounded px-2.5 py-1 text-[11px] font-medium capitalize transition-colors"
              style={
                format === f ? { backgroundColor: '#004CED', color: '#fff' } : { color: '#A1A1AA' }
              }
            >
              {f}
            </button>
          ))}
          <div className="ml-auto flex items-center gap-1.5">
            <button
              type="button"
              onClick={copy}
              className="flex items-center gap-1.5 rounded bg-[#1c1c1f] px-2.5 py-1 text-[11px] font-medium text-[#E4E4E7] hover:bg-[#26262a]"
            >
              {copied ? <Check size={13} /> : <Copy size={13} />}
              {copied ? 'Copied' : 'Copy'}
            </button>
            <button
              type="button"
              onClick={download}
              className="flex items-center gap-1.5 rounded bg-[#1c1c1f] px-2.5 py-1 text-[11px] font-medium text-[#E4E4E7] hover:bg-[#26262a]"
            >
              <Download size={13} />
              Download
            </button>
          </div>
        </div>

        <div className="m-4 flex-1 overflow-hidden rounded-md bg-[#101012]">
          <textarea
            readOnly
            value={content || '/* No edits yet. Select a component and start styling. */'}
            spellCheck={false}
            className="h-full w-full resize-none bg-transparent p-3 font-mono text-[12px] leading-relaxed text-[#E4E4E7] outline-none"
          />
        </div>
      </div>
    </div>
  )
}
