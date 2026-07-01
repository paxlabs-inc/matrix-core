'use client'

/**
 * NeoWorkspace — the live, animated "window into Neo working".
 *
 * This is the show-the-work differentiator rendered as MOTION, not labels.
 * Each step in the run is painted as the surface of the action itself:
 *
 *   - terminal : a terminal window with the real command + streamed output
 *   - browser  : a browser chrome (address bar + viewport / screenshot)
 *   - editor   : a code-editor window with the file + a content preview
 *   - search   : a compact running chip (rich source cards render separately)
 *   - media    : a compact running chip (the media grid renders separately)
 *   - action   : a minimal animated chip for everything else
 *   - narration: Neo thinking out loud between actions
 *
 * Fused to the client design system: separation by background TONE only (no
 * border strokes for depth, per the surface rule), single accent (#004CED via
 * `--primary`), rounded brand font for chrome, mono for code/terminal.
 */
import { useState } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import {
  ArrowLeft,
  ArrowRight,
  BrainIcon,
  Check,
  ChevronDown,
  CircleX,
  Code,
  ExternalLink,
  FileText,
  Globe,
  ImageIcon,
  Loader2,
  Lock,
  Play,
  RotateCcw,
  Search,
  Wrench,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { NeoStep } from '@/hooks/api/useChat'
import type { BundledLanguage } from 'shiki'
import {
  Terminal,
  TerminalActions,
  TerminalContent,
  TerminalCopyButton,
  TerminalHeader,
  TerminalStatus,
  TerminalTitle,
} from '@/components/ai-elements/terminal'
import { CodeBlock, CodeBlockCopyButton } from '@/components/ai-elements/code-block'
import { WebPreview, WebPreviewBody } from '@/components/ai-elements/web-preview'

const EASE = [0.32, 0.72, 0, 1] as const

/**
 * NeoThinkingStrip — a subtle, collapsible glimpse of Neo's chain-of-thought.
 * Secondary by design: a single quiet line the user can expand to read the
 * latest reasoning. Never the answer; disappears with the task.
 */
export function NeoThinkingStrip({ text }: { text?: string }) {
  const [open, setOpen] = useState(false)
  if (!text) return null
  return (
    <div className="bg-foreground/[0.03] overflow-hidden rounded-xl">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="flex w-full items-center gap-2.5 px-3 py-2.5 text-left"
      >
        <BrainIcon className="text-muted-foreground/80 size-4 shrink-0" />
        <span className="text-muted-foreground min-w-0 flex-1 truncate text-xs font-medium">
          {open ? 'Thinking' : text}
        </span>
        <ChevronDown
          className={cn(
            'text-muted-foreground/60 size-4 shrink-0 transition-transform',
            open && 'rotate-180',
          )}
        />
      </button>
      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.22, ease: EASE }}
          >
            <p className="text-muted-foreground/80 px-3 pb-3 text-xs leading-relaxed whitespace-pre-wrap">
              {text}
            </p>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}

export function NeoWorkspace({ steps }: { steps: NeoStep[] }) {
  if (steps.length === 0) return null
  return (
    <div className="flex flex-col gap-2.5">
      <AnimatePresence initial={false}>
        {steps.map((step) => (
          <motion.div
            key={step.id}
            layout
            initial={{ opacity: 0, y: 6 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.26, ease: EASE }}
          >
            <StepView step={step} />
          </motion.div>
        ))}
      </AnimatePresence>
    </div>
  )
}

function StepView({ step }: { step: NeoStep }) {
  switch (step.kind) {
    case 'terminal':
      return <TerminalView step={step} />
    case 'browser':
      return <BrowserView step={step} />
    case 'editor':
      return <EditorView step={step} />
    case 'narration':
      return <NarrationView step={step} />
    default:
      return <ChipView step={step} />
  }
}

/* ------------------------------------------------------------------ */
/* Status glyph                                                       */
/* ------------------------------------------------------------------ */

function StatusGlyph({ step, className }: { step: NeoStep; className?: string }) {
  if (step.running) {
    return <Loader2 className={cn('text-primary size-3.5 animate-spin', className)} />
  }
  if (!step.ok) {
    return <CircleX className={cn('text-destructive size-3.5', className)} />
  }
  return <Check className={cn('text-primary size-3.5', className)} />
}

/* ------------------------------------------------------------------ */
/* Window chrome — three traffic dots, tone-only (no borders)         */
/* ------------------------------------------------------------------ */

function TrafficDots() {
  return (
    <span aria-hidden className="flex shrink-0 items-center gap-1.5">
      <span className="bg-foreground/15 size-2.5 rounded-full" />
      <span className="bg-foreground/15 size-2.5 rounded-full" />
      <span className="bg-foreground/15 size-2.5 rounded-full" />
    </span>
  )
}

/* ------------------------------------------------------------------ */
/* Terminal                                                           */
/* ------------------------------------------------------------------ */

function TerminalView({ step }: { step: NeoStep }) {
  // Real CLI: a true ANSI terminal (ansi-to-react) fed the agent's actual
  // command + captured stdout/stderr. The prompt line uses a real ANSI escape
  // so color survives copy. Auto-scrolls to the tail and shows a live cursor.
  const promptLine = step.command ? `\u001b[1;32m$\u001b[0m ${step.command}` : ''
  const body = step.output ?? ''
  const composed = [promptLine, body].filter(Boolean).join('\n')
  return (
    <Terminal output={composed} isStreaming={step.running} autoScroll className="rounded-xl">
      <TerminalHeader className="border-zinc-800/70">
        <TerminalTitle>
          <span className="max-w-[15rem] truncate font-mono text-[0.72rem]">
            {step.cwd || 'bash'}
          </span>
        </TerminalTitle>
        <div className="flex items-center gap-1">
          <TerminalStatus>
            <Loader2 className="size-3 animate-spin" />
            running
          </TerminalStatus>
          {!step.running && !step.ok ? (
            <span className="text-destructive mr-1 text-[0.7rem]">exited with error</span>
          ) : null}
          <TerminalActions>
            <TerminalCopyButton />
          </TerminalActions>
        </div>
      </TerminalHeader>
      <TerminalContent className="max-h-80" />
    </Terminal>
  )
}

/* ------------------------------------------------------------------ */
/* Browser                                                            */
/* ------------------------------------------------------------------ */

function hostOf(url?: string): string {
  if (!url) return 'about:blank'
  try {
    return new URL(url).host || url
  } catch {
    return url.replace(/^https?:\/\//, '').split('/')[0] || url
  }
}

function browserStatusLine(step: NeoStep): string {
  const verb = step.running ? 'ing' : 'ed'
  switch (step.action) {
    case 'navigate':
      return step.running ? `Opening ${hostOf(step.url)}` : `Opened ${hostOf(step.url)}`
    case 'read':
      return step.running ? `Reading ${hostOf(step.url)}` : `Read ${hostOf(step.url)}`
    case 'click':
      return `Click${verb}${step.target ? ` ${step.target}` : ''}`
    case 'type':
    case 'fill_form':
      return step.typed ? `Typed “${step.typed}”` : `Typ${verb} text`
    case 'navigate_back':
      return 'Went back'
    case 'take_screenshot':
    case 'snapshot':
      return step.running ? 'Looking at the page' : 'Captured the page'
    default:
      return step.title
  }
}

function BrowserView({ step }: { step: NeoStep }) {
  // Real browser: Chrome-style chrome (nav + secure address bar + live favicon)
  // over EITHER the agent's real captured screenshot (the page Neo actually
  // saw) or a live sandboxed iframe of the real URL (toggle). Reader view is
  // the fallback when a page yielded only extracted text.
  const [mode, setMode] = useState<'shot' | 'live'>('shot')
  const host = hostOf(step.url)
  const isHttp = !!step.url && /^https?:\/\//i.test(step.url)
  const favicon = isHttp ? `https://www.google.com/s2/favicons?domain=${host}&sz=64` : null
  return (
    <div className="overflow-hidden rounded-xl border border-zinc-800/70 bg-zinc-950">
      <div className="relative flex items-center gap-1.5 border-b border-zinc-800/70 bg-zinc-900/60 px-2.5 py-2">
        <TrafficDots />
        <div className="ml-1 flex items-center gap-0.5">
          <span className="grid size-6 place-items-center rounded text-zinc-600">
            <ArrowLeft className="size-3.5" />
          </span>
          <span className="grid size-6 place-items-center rounded text-zinc-600">
            <ArrowRight className="size-3.5" />
          </span>
          <span className="grid size-6 place-items-center rounded text-zinc-400">
            <RotateCcw className={cn('size-3.5', step.running && 'animate-spin')} />
          </span>
        </div>
        <div className="flex min-w-0 flex-1 items-center gap-1.5 rounded-md bg-zinc-800/70 px-2 py-1">
          {favicon ? (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              src={favicon}
              alt=""
              className="size-3.5 shrink-0 rounded-sm"
              onError={(e) => {
                e.currentTarget.style.display = 'none'
              }}
            />
          ) : (
            <Lock className="size-3 shrink-0 text-zinc-500" />
          )}
          <span className="truncate font-mono text-[0.68rem] text-zinc-300">
            {step.url || 'about:blank'}
          </span>
        </div>
        {isHttp ? (
          <div className="flex shrink-0 items-center rounded-md bg-zinc-800/70 p-0.5 text-[0.62rem] font-medium">
            <button
              type="button"
              onClick={() => setMode('shot')}
              className={cn(
                'rounded px-1.5 py-0.5 transition-colors',
                mode === 'shot' ? 'bg-zinc-700 text-zinc-100' : 'text-zinc-400',
              )}
            >
              Shot
            </button>
            <button
              type="button"
              onClick={() => setMode('live')}
              className={cn(
                'rounded px-1.5 py-0.5 transition-colors',
                mode === 'live' ? 'bg-zinc-700 text-zinc-100' : 'text-zinc-400',
              )}
            >
              Live
            </button>
          </div>
        ) : null}
        {isHttp ? (
          <a
            href={step.url}
            target="_blank"
            rel="noreferrer noopener"
            title="Open in a new tab"
            className="grid size-6 shrink-0 place-items-center rounded text-zinc-400 transition-colors hover:bg-zinc-800 hover:text-zinc-100"
          >
            <ExternalLink className="size-3.5" />
          </a>
        ) : null}
        {step.running ? (
          <motion.div
            className="bg-primary absolute bottom-0 left-0 h-0.5"
            initial={{ width: '0%' }}
            animate={{ width: ['0%', '75%', '92%'] }}
            transition={{ duration: 2.4, ease: 'easeOut' }}
          />
        ) : null}
      </div>
      {mode === 'live' && isHttp ? (
        <div className="h-[60vh] max-h-[32rem] w-full bg-white">
          <WebPreview defaultUrl={step.url} className="size-full rounded-none border-0">
            <WebPreviewBody src={step.url} className="bg-white" />
          </WebPreview>
        </div>
      ) : step.screenshotUrl ? (
        <div className="max-h-[32rem] overflow-auto bg-white">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img
            src={step.screenshotUrl}
            alt={step.pageTitle || host}
            className="w-full object-top"
          />
        </div>
      ) : step.excerpt ? (
        <div className="max-h-80 overflow-auto px-4 py-3.5">
          <p className="mb-2 flex items-center gap-1.5 text-[0.68rem] text-zinc-500">
            <Globe className="size-3" />
            {browserStatusLine(step)}
          </p>
          {step.pageTitle ? (
            <h4 className="mb-1.5 text-sm font-semibold text-zinc-100">{step.pageTitle}</h4>
          ) : null}
          <p className="text-xs leading-relaxed whitespace-pre-line text-zinc-400">
            {step.excerpt}
          </p>
        </div>
      ) : (
        <div className="flex min-h-[8rem] flex-col items-center justify-center gap-2 px-4 py-8 text-center">
          <Globe className={cn('size-6 text-zinc-600', step.running && 'animate-pulse')} />
          <p className="text-sm font-medium text-zinc-300">{browserStatusLine(step)}</p>
          {step.pageTitle ? (
            <p className="max-w-md text-xs leading-relaxed text-zinc-500">{step.pageTitle}</p>
          ) : null}
        </div>
      )}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Editor                                                             */
/* ------------------------------------------------------------------ */

function baseName(p?: string): string {
  if (!p) return 'untitled'
  const parts = p.split('/').filter(Boolean)
  return parts[parts.length - 1] || p
}

const EDITOR_LANG_MAP: Record<string, BundledLanguage> = {
  ts: 'typescript',
  tsx: 'tsx',
  js: 'javascript',
  jsx: 'jsx',
  mjs: 'javascript',
  cjs: 'javascript',
  py: 'python',
  rb: 'ruby',
  go: 'go',
  rs: 'rust',
  java: 'java',
  c: 'c',
  h: 'c',
  cc: 'cpp',
  cpp: 'cpp',
  hpp: 'cpp',
  cs: 'csharp',
  php: 'php',
  sh: 'bash',
  bash: 'bash',
  zsh: 'bash',
  yml: 'yaml',
  yaml: 'yaml',
  json: 'json',
  jsonc: 'jsonc',
  md: 'markdown',
  markdown: 'markdown',
  html: 'html',
  css: 'css',
  scss: 'scss',
  sql: 'sql',
  toml: 'toml',
  dockerfile: 'docker',
  proto: 'proto',
  swift: 'swift',
  kt: 'kotlin',
  sol: 'solidity',
}

/** Resolve a free-form language hint or file extension to a shiki bundled
 *  language. Unknown values pass through (highlightCode falls back to plain
 *  text safely), so this never throws on an unexpected hint. */
function coerceLang(language?: string, path?: string): BundledLanguage {
  const raw = (language || '').toLowerCase().trim()
  if (raw && EDITOR_LANG_MAP[raw]) return EDITOR_LANG_MAP[raw]
  const ext = path ? (path.split('.').pop() || '').toLowerCase() : ''
  if (ext && EDITOR_LANG_MAP[ext]) return EDITOR_LANG_MAP[ext]
  return (raw || 'text') as BundledLanguage
}

/** Heuristic: does this preview look like a unified diff / patch? */
function looksLikeDiff(s: string): boolean {
  return (
    /^@@ .* @@/m.test(s) || /^diff --git /m.test(s) || (/^\+\+\+ /m.test(s) && /^--- /m.test(s))
  )
}

/** Render a unified diff with per-line add / remove / hunk coloring. */
function DiffView({ text }: { text: string }) {
  // Stable, content-derived keys (occurrence-counted) — no array-index keys.
  const seen = new Map<string, number>()
  const keyed = text.split('\n').map((ln) => {
    const n = (seen.get(ln) ?? 0) + 1
    seen.set(ln, n)
    return { ln, key: `${n}:${ln}` }
  })
  return (
    <pre className="max-h-80 overflow-auto bg-zinc-950 px-2 py-3 font-mono text-[0.74rem] leading-relaxed">
      {keyed.map(({ ln, key }) => {
        const add = ln.startsWith('+') && !ln.startsWith('+++')
        const del = ln.startsWith('-') && !ln.startsWith('---')
        const hunk = ln.startsWith('@@')
        const meta =
          ln.startsWith('diff ') ||
          ln.startsWith('index ') ||
          ln.startsWith('+++') ||
          ln.startsWith('---')
        return (
          <div
            key={key}
            className={cn(
              'px-1.5 break-all whitespace-pre-wrap',
              add && 'bg-green-500/10 text-green-400',
              del && 'bg-red-500/10 text-red-400',
              hunk && 'text-primary',
              meta && 'text-zinc-500',
              !add && !del && !hunk && !meta && 'text-zinc-400',
            )}
          >
            {ln || ' '}
          </div>
        )
      })}
    </pre>
  )
}

function EditorView({ step }: { step: NeoStep }) {
  // Real editor: shiki-highlighted source (CodeBlock, line numbers + copy) or a
  // colored unified-diff view when the agent's preview is a patch.
  const lang = coerceLang(step.language, step.path)
  const diff = step.preview ? looksLikeDiff(step.preview) : false
  return (
    <div className="overflow-hidden rounded-xl border border-zinc-800/70 bg-zinc-950">
      <div className="flex items-center gap-2 border-b border-zinc-800/70 px-3 py-2">
        <FileText className="size-3.5 shrink-0 text-zinc-400" />
        <span className="truncate font-mono text-[0.72rem] text-zinc-300">
          {step.path || baseName(step.path)}
        </span>
        {step.language ? (
          <span className="ml-1 shrink-0 rounded bg-zinc-800 px-1.5 py-0.5 text-[0.6rem] tracking-wide text-zinc-400 uppercase">
            {step.language}
          </span>
        ) : null}
        <span className="ml-auto shrink-0 text-[0.68rem] text-zinc-500">{step.title}</span>
        <StatusGlyph step={step} className="shrink-0" />
      </div>
      {step.preview ? (
        diff ? (
          <DiffView text={step.preview} />
        ) : (
          <CodeBlock
            code={step.preview}
            language={lang}
            showLineNumbers
            className="relative max-h-80 overflow-auto text-[0.78rem]"
          >
            <CodeBlockCopyButton className="absolute top-2 right-2 z-10" />
          </CodeBlock>
        )
      ) : step.running ? (
        <div className="px-3.5 py-4 font-mono text-[0.72rem] text-zinc-500">
          opening {baseName(step.path)}…
        </div>
      ) : null}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Narration + compact chips                                          */
/* ------------------------------------------------------------------ */

function NarrationView({ step }: { step: NeoStep }) {
  if (!step.text) return null
  return (
    <div className="flex items-start gap-3 py-0.5">
      <span className="mt-[0.45rem] flex shrink-0">
        <span
          className={cn(
            'size-2 rounded-full',
            step.running ? 'bg-primary animate-pulse' : 'bg-primary/70',
          )}
        />
      </span>
      <p
        className={cn(
          'text-sm leading-relaxed',
          step.running ? 'text-muted-foreground' : 'text-foreground',
        )}
      >
        <span className={step.running ? 'shimmer' : undefined}>{step.text}</span>
      </p>
    </div>
  )
}

function ChipIcon({ step }: { step: NeoStep }) {
  const cls = 'size-4 text-muted-foreground'
  switch (step.kind) {
    case 'search':
      return <Search className={cls} />
    case 'media':
      return step.title.toLowerCase().includes('video') ? (
        <Play className={cls} />
      ) : (
        <ImageIcon className={cls} />
      )
    case 'editor':
      return <Code className={cls} />
    default:
      return <Wrench className={cls} />
  }
}

function ChipView({ step }: { step: NeoStep }) {
  const detail =
    step.kind === 'search' && step.query
      ? `“${step.query}”`
      : step.kind === 'action'
        ? undefined
        : undefined
  return (
    <div className="bg-foreground/[0.03] flex items-center gap-2.5 rounded-xl px-3 py-2.5">
      <span className="bg-foreground/[0.05] grid size-7 shrink-0 place-items-center rounded-lg">
        <ChipIcon step={step} />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground/90 truncate text-sm font-medium">
          <span className={step.running ? 'shimmer' : undefined}>{step.title}</span>
        </p>
        {detail && <p className="text-muted-foreground/70 truncate text-xs">{detail}</p>}
      </div>
      <StatusGlyph step={step} className="shrink-0" />
    </div>
  )
}
