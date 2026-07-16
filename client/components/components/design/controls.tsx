'use client'

/**
 * Design Mode — inspector control primitives.
 *
 * Token-styled, dark, single accent (#004CED). Surfaces separate by background
 * tone only (no border strokes for depth). Focus rings are interaction
 * affordances, not separators, so they are allowed.
 */
import { type ReactNode, useId } from 'react'
import { cn } from '@/lib/utils'

export const ACCENT = '#004CED'

export function Row({
  label,
  children,
  hint,
}: {
  label: string
  children: ReactNode
  hint?: string
}) {
  return (
    <label className="flex items-center justify-between gap-3 py-1.5">
      <span className="min-w-0 shrink-0 text-[11px] font-medium text-[#A1A1AA]" title={hint}>
        {label}
      </span>
      <div className="flex min-w-0 flex-1 items-center justify-end gap-1.5">{children}</div>
    </label>
  )
}

export function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-md bg-[#0f0f10] p-2.5">
      <div className="mb-1 px-0.5 text-[10px] font-semibold tracking-wider text-[#71717A] uppercase">
        {title}
      </div>
      {children}
    </div>
  )
}

const inputBase =
  'h-7 w-full rounded bg-[#1c1c1f] px-2 text-[12px] text-white outline-none transition-colors ' +
  'placeholder:text-[#52525B] focus:bg-[#222226] focus:ring-1 focus:ring-[#004CED]'

export function LenInput({
  value,
  placeholder,
  onChange,
  className,
}: {
  value: string | undefined
  placeholder?: string
  onChange: (v: string | null) => void
  className?: string
}) {
  return (
    <input
      type="text"
      spellCheck={false}
      value={value ?? ''}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value === '' ? null : e.target.value)}
      className={cn(inputBase, className)}
    />
  )
}

export interface SegOption {
  value: string
  label?: string
  icon?: ReactNode
  title?: string
}

export function Seg({
  value,
  options,
  onChange,
  full,
}: {
  value: string | undefined
  options: SegOption[]
  onChange: (v: string) => void
  full?: boolean
}) {
  return (
    <div className={cn('flex items-center gap-0.5 rounded bg-[#161618] p-0.5', full && 'w-full')}>
      {options.map((opt) => {
        const active = value === opt.value
        return (
          <button
            key={opt.value}
            type="button"
            title={opt.title ?? opt.label ?? opt.value}
            onClick={() => onChange(opt.value)}
            className={cn(
              'flex h-6 flex-1 items-center justify-center gap-1 rounded-[3px] px-1.5 text-[11px] font-medium transition-colors',
              active ? 'text-white' : 'text-[#A1A1AA] hover:text-white',
            )}
            style={active ? { backgroundColor: ACCENT } : undefined}
          >
            {opt.icon}
            {opt.label ? <span>{opt.label}</span> : null}
          </button>
        )
      })}
    </div>
  )
}

const SWATCHES = [
  '#000000',
  '#111111',
  '#1a1a1a',
  '#27272A',
  '#52525B',
  '#A1A1AA',
  '#FFFFFF',
  '#004CED',
]

export function ColorField({
  value,
  placeholder,
  onChange,
}: {
  value: string | undefined
  placeholder?: string
  onChange: (v: string | null) => void
}) {
  const id = useId()
  const colorValue = value && /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.test(value) ? value : '#000000'
  return (
    <div className="flex w-full flex-col gap-1.5">
      <div className="flex items-center gap-1.5">
        <input
          id={id}
          type="color"
          value={colorValue}
          onChange={(e) => onChange(e.target.value)}
          className="h-7 w-8 shrink-0 cursor-pointer rounded bg-[#1c1c1f] p-0.5"
        />
        <LenInput value={value} placeholder={placeholder ?? '#000000'} onChange={onChange} />
      </div>
      <div className="flex flex-wrap gap-1">
        {SWATCHES.map((c) => (
          <button
            key={c}
            type="button"
            title={c}
            onClick={() => onChange(c)}
            className="h-4 w-4 rounded-[3px]"
            style={{
              backgroundColor: c,
              outline: c === '#000000' ? '1px solid #27272A' : undefined,
            }}
          />
        ))}
      </div>
    </div>
  )
}

export function SidesField({
  values,
  placeholders,
  onChange,
}: {
  values: { top?: string; right?: string; bottom?: string; left?: string }
  placeholders: { top?: string; right?: string; bottom?: string; left?: string }
  onChange: (side: 'top' | 'right' | 'bottom' | 'left', v: string | null) => void
}) {
  const sides: Array<'top' | 'right' | 'bottom' | 'left'> = ['top', 'right', 'bottom', 'left']
  const labels: Record<string, string> = { top: 'T', right: 'R', bottom: 'B', left: 'L' }
  return (
    <div className="grid grid-cols-4 gap-1">
      {sides.map((s) => (
        <div key={s} className="flex flex-col items-center gap-0.5">
          <input
            type="text"
            spellCheck={false}
            value={values[s] ?? ''}
            placeholder={placeholders[s]}
            onChange={(e) => onChange(s, e.target.value === '' ? null : e.target.value)}
            className="h-7 w-full rounded bg-[#1c1c1f] px-1 text-center text-[11px] text-white outline-none placeholder:text-[#52525B] focus:bg-[#222226] focus:ring-1 focus:ring-[#004CED]"
          />
          <span className="text-[9px] text-[#52525B]">{labels[s]}</span>
        </div>
      ))}
    </div>
  )
}

export function IconButton({
  children,
  onClick,
  active,
  title,
  disabled,
}: {
  children: ReactNode
  onClick?: () => void
  active?: boolean
  title?: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      title={title}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        'flex h-7 w-7 items-center justify-center rounded transition-colors',
        disabled
          ? 'cursor-not-allowed text-[#3f3f46]'
          : active
            ? 'text-white'
            : 'text-[#A1A1AA] hover:bg-[#1c1c1f] hover:text-white',
      )}
      style={active ? { backgroundColor: ACCENT } : undefined}
    >
      {children}
    </button>
  )
}
