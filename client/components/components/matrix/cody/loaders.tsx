/**
 * Cody loaders — a curated set of the custom spinners, adapted to the client.
 *
 * Ported from the design drop (temp/components RoundSpinners / LinesDotsSpinners
 * / TextSpinners). Changes from source:
 *   - Keyframes the source assumed in global CSS are shipped here in a single
 *     deduped <style> (React precedence hoisting) so app/globals.css is never
 *     edited.
 *   - Multi-hue gradients dropped for solid accent (no-gradient house rule);
 *     glow/off-palette variants (NeonGlow, GlitchText) and the 3D CanvasCloud
 *     (three.js/gsap) are intentionally not ported.
 *   - The source's `font-space` maps to the client's `font-mono`.
 *   - Fixed demo heights removed; callers size the container.
 *
 * Palette matches the theme (foreground #e3d9d4, accent sage #99bd9c == the
 * `pax` token); marks use those values directly so they read identically to
 * the rest of the surface.
 */
'use client'

import { cn } from '@/lib/utils'

const FG = '#e3d9d4'
const ACCENT = '#99bd9c'

/**
 * Keyframes the ported loaders rely on. Rendered with a stable `href` +
 * `precedence` so React hoists and de-duplicates it — mounting many loaders
 * yields one <style>, and app/globals.css stays untouched.
 */
function LoaderKeyframes() {
  return (
    <style href="cody-loader-keyframes" precedence="default">{`
@keyframes cody-spin { to { transform: rotate(360deg); } }
@keyframes cody-counter-spin { to { transform: rotate(-360deg); } }
@keyframes cody-pulse-ring { 0% { transform: scale(0.5); opacity: 0.8; } 100% { transform: scale(1.4); opacity: 0; } }
@keyframes cody-dot-pulse { 0%, 100% { opacity: 0.2; } 50% { opacity: 1; } }
@keyframes cody-wave-bar { 0%, 100% { transform: scaleY(0.4); } 50% { transform: scaleY(1); } }
@keyframes cody-stagger-dot { 0%, 100% { transform: translateY(0); opacity: 0.5; } 50% { transform: translateY(-7px); opacity: 1; } }
@keyframes cody-terminal-blink { 0%, 100% { opacity: 1; } 50% { opacity: 0; } }
@keyframes cody-progress-bar { 0% { left: -40%; width: 40%; } 50% { width: 60%; } 100% { left: 100%; width: 40%; } }
`}</style>
  )
}

function Center({ className, children }: { className?: string; children: React.ReactNode }) {
  return <div className={cn('flex items-center justify-center', className)}>{children}</div>
}

/** Stroke Dash Ring — the primary run/loading spinner. */
export function StrokeDashRing({ className, size = 44 }: { className?: string; size?: number }) {
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <svg width={size} height={size} viewBox="0 0 60 60" role="status" aria-label="Loading">
        <circle cx="30" cy="30" r="24" fill="none" stroke="#2a2a27" strokeWidth="3" />
        <circle
          cx="30"
          cy="30"
          r="24"
          fill="none"
          stroke={ACCENT}
          strokeWidth="3"
          strokeLinecap="square"
          strokeDasharray="80 70"
          style={{ animation: 'cody-spin 1.2s linear infinite', transformOrigin: 'center' }}
        />
      </svg>
    </Center>
  )
}

/** Dual Ring Orbit — two counter-rotating arcs. */
export function DualRingOrbit({ className, size = 44 }: { className?: string; size?: number }) {
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <div className="relative" style={{ width: size, height: size }}>
        <div
          className="absolute inset-0 border-2 border-transparent"
          style={{
            borderTopColor: FG,
            borderRightColor: `${FG}4d`,
            borderRadius: '50%',
            animation: 'cody-spin 1.5s linear infinite',
          }}
        />
        <div
          className="absolute inset-2 border-2 border-transparent"
          style={{
            borderBottomColor: ACCENT,
            borderLeftColor: `${ACCENT}4d`,
            borderRadius: '50%',
            animation: 'cody-counter-spin 1s linear infinite',
          }}
        />
      </div>
    </Center>
  )
}

/** Pulse Rings — concentric expanding rings around an accent core. */
export function PulseRings({ className, size = 44 }: { className?: string; size?: number }) {
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <div
        className="relative flex items-center justify-center"
        style={{ width: size, height: size }}
      >
        {[0, 1, 2].map((i) => (
          <div
            key={i}
            className="absolute h-full w-full border"
            style={{
              borderColor: ACCENT,
              borderRadius: '50%',
              animation: `cody-pulse-ring 2s ease-out ${i * 0.6}s infinite`,
            }}
          />
        ))}
        <div style={{ width: 8, height: 8, background: ACCENT, borderRadius: '50%' }} />
      </div>
    </Center>
  )
}

/** Segment Spinner — a ring of fading ticks. */
export function SegmentSpinner({ className, size = 44 }: { className?: string; size?: number }) {
  const segments = 12
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <div className="relative" style={{ width: size, height: size }}>
        {Array.from({ length: segments }).map((_, i) => (
          <div
            key={i}
            className="absolute"
            style={{
              width: 3,
              height: size * 0.22,
              background: FG,
              top: 0,
              left: '50%',
              transformOrigin: `50% ${size / 2}px`,
              transform: `rotate(${(360 / segments) * i}deg)`,
              opacity: 0.1 + (i / segments) * 0.9,
              animation: `cody-dot-pulse 1.2s linear ${(i / segments) * 1.2}s infinite`,
            }}
          />
        ))}
      </div>
    </Center>
  )
}

/** Wave Bars — equalizer bars (solid accent; no gradient). */
export function WaveBars({
  className,
  size = 28,
  bars = 7,
}: {
  className?: string
  size?: number
  bars?: number
}) {
  const barWidth = Math.max(2, size * 0.18)
  const gap = Math.max(1, size * 0.14)
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <div className="flex items-end" style={{ gap }}>
        {Array.from({ length: bars }).map((_, i) => (
          <div
            key={i}
            style={{
              width: barWidth,
              height: size,
              background: ACCENT,
              transformOrigin: 'bottom',
              animation: `cody-wave-bar 1s ease-in-out ${i * 0.1}s infinite`,
            }}
          />
        ))}
      </div>
    </Center>
  )
}

/** Pixel Grid — a 5x5 field of dots pulsing outward from the center. */
export function PixelGrid({ className, size = 32 }: { className?: string; size?: number }) {
  const grid = 5
  return (
    <Center className={className}>
      <LoaderKeyframes />
      <div
        style={{
          width: size,
          height: size,
          display: 'grid',
          gridTemplateColumns: `repeat(${grid}, 1fr)`,
          gridTemplateRows: `repeat(${grid}, 1fr)`,
          gap: size * 0.06,
        }}
      >
        {Array.from({ length: grid * grid }).map((_, i) => {
          const row = Math.floor(i / grid)
          const col = i % grid
          const centerDist = Math.abs(2 - row) + Math.abs(2 - col)
          return (
            <div
              key={i}
              style={{
                background: ACCENT,
                animation: `cody-dot-pulse 1.5s ease-in-out ${centerDist * 0.1}s infinite`,
              }}
            />
          )
        })}
      </div>
    </Center>
  )
}

/** Stagger Dots — a row of bouncing dots. */
export function StaggerDots({ className }: { className?: string }) {
  return (
    <Center className={cn('gap-2', className)}>
      <LoaderKeyframes />
      {[0, 1, 2, 3, 4].map((i) => (
        <div
          key={i}
          style={{
            width: 10,
            height: 10,
            background: FG,
            borderRadius: '50%',
            animation: `cody-stagger-dot 1s ease-in-out ${i * 0.15}s infinite`,
          }}
        />
      ))}
    </Center>
  )
}

/** Typing Dots — inline "working" indicator with three pulsing dots. */
export function TypingDots({ className, label }: { className?: string; label?: string }) {
  return (
    <div className={cn('flex items-center gap-2', className)}>
      <LoaderKeyframes />
      {label ? <span className="text-muted-foreground font-mono text-xs">{label}</span> : null}
      {[0, 1, 2].map((i) => (
        <div
          key={i}
          style={{
            width: 7,
            height: 7,
            background: ACCENT,
            borderRadius: '50%',
            animation: `cody-dot-pulse 1.4s ease-in-out ${i * 0.2}s infinite`,
          }}
        />
      ))}
    </div>
  )
}

/** Infinite Progress Bar — indeterminate sweep for pane-level loading. */
export function InfiniteProgressBar({ className }: { className?: string }) {
  return (
    <div
      className={cn('relative h-[2px] w-full overflow-hidden', className)}
      style={{ background: '#2a2a27' }}
    >
      <LoaderKeyframes />
      <div
        className="absolute h-full"
        style={{ background: ACCENT, animation: 'cody-progress-bar 1.8s ease-in-out infinite' }}
      />
    </div>
  )
}

/** Terminal Typing — a monospace line that types itself, for run activity. */
export function TerminalTyping({
  text = 'Working…',
  className,
}: {
  text?: string
  className?: string
}) {
  return (
    <div className={cn('font-mono text-sm', className)} style={{ color: ACCENT }}>
      <LoaderKeyframes />
      <span>{text}</span>
      <span
        className="ml-1 inline-block align-middle"
        style={{
          width: 8,
          height: 15,
          background: ACCENT,
          animation: 'cody-terminal-blink 1s step-end infinite',
        }}
      />
    </div>
  )
}

export type CodyLoaderVariant =
  | 'ring'
  | 'dual-ring'
  | 'pulse'
  | 'segment'
  | 'wave'
  | 'pixel-grid'
  | 'stagger'
  | 'dots'
  | 'bar'

/**
 * CodyLoader — the one entry point the surface uses. Defaults to the stroke
 * dash ring; pass a `variant` for a different mark and an optional `label`.
 */
export function CodyLoader({
  variant = 'ring',
  label,
  className,
  size,
}: {
  variant?: CodyLoaderVariant
  label?: string
  className?: string
  size?: number
}) {
  const mark = (() => {
    switch (variant) {
      case 'dual-ring':
        return <DualRingOrbit size={size} />
      case 'pulse':
        return <PulseRings size={size} />
      case 'segment':
        return <SegmentSpinner size={size} />
      case 'wave':
        return <WaveBars size={size} />
      case 'pixel-grid':
        return <PixelGrid size={size} />
      case 'stagger':
        return <StaggerDots />
      case 'dots':
        return <TypingDots />
      case 'bar':
        return <InfiniteProgressBar />
      case 'ring':
      default:
        return <StrokeDashRing size={size} />
    }
  })()
  if (!label) return <div className={className}>{mark}</div>
  return (
    <div className={cn('flex flex-col items-center gap-3', className)}>
      {mark}
      <span className="text-muted-foreground font-mono text-xs tracking-wide">{label}</span>
    </div>
  )
}
