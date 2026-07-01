'use client'

/**
 * NeoFiles — the Workspace / Files page.
 *
 * Neo has a whole machine per user; this page is the window into its file
 * space. It shows the files Neo has made for you (generated media + deliverable
 * artifacts) and the files you've handed Neo, lets you download any of them, and
 * lets you upload new files and media for Neo to see or use in a task.
 *
 * Sources, merged + de-duped by reference:
 *   - The durable workspace listing (GET /files) when the daemon provides it.
 *     Until it does, that call degrades to empty and the page still works from:
 *   - The open conversation's generated media + deliverable artifacts (task).
 *   - Files you upload in this session (POST /upload → /media ref).
 * Nothing here is fabricated — every row is a real, fetchable reference.
 *
 * Design system: full-page overlay separated by background TONE only
 * (bg-background / bg-card), single Paxeer-blue accent on interactive chrome,
 * no border strokes for depth, no emojis / glow.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { toast } from 'sonner'
import {
  Download,
  FileIcon,
  FileText,
  ImageIcon,
  Loader2,
  Music2Icon,
  Paperclip,
  PlayIcon,
  Search,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import {
  downloadMediaRef,
  listWorkspaceFiles,
  loadMediaObjectURL,
  mediaKindForMime,
  uploadMedia,
  type MediaKind,
  type WorkspaceFile,
} from '@/lib/api/media'
import type { NeoTask } from '@/hooks/api/useChat'
import { NeoIllustration } from '@/components/matrix/neo/neo-illustration'

const EASE = [0.32, 0.72, 0, 1] as const

type KindFilter = 'all' | MediaKind

const KIND_FILTERS: { id: KindFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'image', label: 'Images' },
  { id: 'video', label: 'Video' },
  { id: 'audio', label: 'Audio' },
  { id: 'file', label: 'Files' },
]

/** Derive the display files from the open conversation's task. */
function filesFromTask(task: NeoTask | null): WorkspaceFile[] {
  if (!task) return []
  const out: WorkspaceFile[] = []
  for (const m of task.media) {
    out.push({
      url: m.url,
      name: m.prompt?.slice(0, 60) || m.url.split('/').pop() || 'media',
      kind: m.kind,
      mime: m.mime,
      source: 'generated',
    })
  }
  for (const a of task.artifacts) {
    if (a.kind === 'site') continue // deployed sites aren't downloadable files
    out.push({
      url: a.url,
      name: a.name || a.url.split('/').pop() || 'file',
      kind: 'file',
      mime: a.mime,
      bytes: a.size,
      source: 'artifact',
    })
  }
  return out
}

function dedupe(files: WorkspaceFile[]): WorkspaceFile[] {
  const seen = new Set<string>()
  const out: WorkspaceFile[] = []
  for (const f of files) {
    if (!f.url || seen.has(f.url)) continue
    seen.add(f.url)
    out.push(f)
  }
  return out
}

export function NeoFiles({
  open,
  onClose,
  task,
}: {
  open: boolean
  onClose: () => void
  /** The open conversation's task — its media/artifacts are shown as files. */
  task: NeoTask | null
}) {
  const reduce = useReducedMotion()
  const [query, setQuery] = useState('')
  const [kind, setKind] = useState<KindFilter>('all')
  const [listed, setListed] = useState<WorkspaceFile[]>([])
  const [uploads, setUploads] = useState<WorkspaceFile[]>([])
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  // Load the durable workspace listing on open (degrades to [] when absent).
  useEffect(() => {
    if (!open) return
    let alive = true
    setLoading(true)
    listWorkspaceFiles()
      .then((files) => {
        if (alive) setListed(files)
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [open])

  // Escape closes.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  const all = useMemo(
    () => dedupe([...uploads, ...listed, ...filesFromTask(task)]),
    [uploads, listed, task],
  )

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    return all.filter((f) => {
      if (kind !== 'all' && f.kind !== kind) return false
      if (q && !(f.name || '').toLowerCase().includes(q)) return false
      return true
    })
  }, [all, kind, query])

  const onPickFiles = useCallback(async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return
    setBusy(true)
    try {
      for (const file of Array.from(fileList)) {
        try {
          const up = await uploadMedia(file, file.name)
          setUploads((prev) => [
            {
              url: up.url,
              name: up.name || file.name,
              kind: up.kind || mediaKindForMime(file.type),
              mime: up.mime || file.type,
              bytes: up.bytes,
              source: 'upload',
            },
            ...prev,
          ])
        } catch {
          toast.error(`Couldn't upload ${file.name}`)
        }
      }
    } finally {
      setBusy(false)
    }
  }, [])

  return (
    <AnimatePresence>
      {open ? (
        <motion.div
          initial={reduce ? false : { opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.2 }}
          className="bg-background fixed inset-0 z-50 flex flex-col"
          role="dialog"
          aria-modal="true"
          aria-label="Workspace files"
        >
          {/* header */}
          <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
            <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
              <FolderGlyph />
            </span>
            <div className="min-w-0 flex-1">
              <h1 className="text-foreground text-lg font-bold tracking-tight">Workspace</h1>
              <p className="text-muted-foreground text-xs">
                Files Neo made for you, and files you share with Neo
              </p>
            </div>
            <button
              type="button"
              onClick={() => fileInput.current?.click()}
              disabled={busy}
              className="bg-primary text-primary-foreground hover:bg-primary/90 flex shrink-0 items-center gap-2 rounded-full px-3.5 py-2 text-sm font-semibold transition-colors disabled:opacity-60"
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : <Paperclip className="size-4" />}
              Upload
            </button>
            <button
              type="button"
              onClick={onClose}
              aria-label="Close workspace"
              className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition"
            >
              <X className="size-5" />
            </button>
            <input
              ref={fileInput}
              type="file"
              multiple
              accept="image/*,video/*,audio/*"
              className="hidden"
              onChange={(e) => {
                void onPickFiles(e.target.files)
                e.target.value = ''
              }}
            />
          </div>

          {/* search + kind filters */}
          <div className="mx-auto flex w-full max-w-4xl shrink-0 flex-col gap-3 px-4 pb-3 sm:px-6">
            <div className="bg-card flex items-center gap-2.5 rounded-xl px-3.5 py-2.5">
              <Search className="text-muted-foreground size-[1.05rem] shrink-0" />
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search files…"
                className="text-foreground placeholder:text-muted-foreground/70 min-w-0 flex-1 bg-transparent text-sm outline-none"
              />
              {query ? (
                <button
                  type="button"
                  onClick={() => setQuery('')}
                  aria-label="Clear search"
                  className="text-muted-foreground hover:text-foreground grid size-6 place-items-center rounded-full transition"
                >
                  <X className="size-4" />
                </button>
              ) : null}
            </div>
            <div className="flex flex-wrap items-center gap-1.5">
              {KIND_FILTERS.map((f) => (
                <button
                  key={f.id}
                  type="button"
                  onClick={() => setKind(f.id)}
                  className={cn(
                    'rounded-full px-3 py-1.5 text-xs font-medium transition-colors',
                    kind === f.id
                      ? 'bg-primary text-primary-foreground'
                      : 'bg-card text-muted-foreground hover:text-foreground',
                  )}
                >
                  {f.label}
                </button>
              ))}
            </div>
          </div>

          {/* body */}
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 sm:px-6">
            <div className="mx-auto w-full max-w-4xl">
              {loading && all.length === 0 ? (
                <FilesSkeleton />
              ) : shown.length === 0 ? (
                <div className="flex flex-col items-center gap-4 py-16 text-center">
                  <NeoIllustration art={all.length === 0 ? 'workspace' : 'files'} width={200} />
                  <div className="flex flex-col gap-1">
                    <p className="text-foreground text-base font-bold">
                      {all.length === 0 ? 'No files yet' : 'No matching files'}
                    </p>
                    <p className="text-muted-foreground mx-auto max-w-md text-sm">
                      {all.length === 0
                        ? 'Files Neo generates in a task show up here, and you can upload images, audio, or video for Neo to use.'
                        : 'Try a different search or filter.'}
                    </p>
                  </div>
                  {all.length === 0 ? (
                    <button
                      type="button"
                      onClick={() => fileInput.current?.click()}
                      className="text-primary hover:bg-primary/10 rounded-full px-3.5 py-2 text-sm font-medium transition-colors"
                    >
                      Upload a file
                    </button>
                  ) : null}
                </div>
              ) : (
                <ul className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
                  {shown.map((f) => (
                    <FileCard key={f.url} file={f} />
                  ))}
                </ul>
              )}
            </div>
          </div>
        </motion.div>
      ) : null}
    </AnimatePresence>
  )
}

function FileCard({ file }: { file: WorkspaceFile }) {
  const [downloading, setDownloading] = useState(false)
  const onDownload = useCallback(async () => {
    setDownloading(true)
    const ok = await downloadMediaRef(file.url, file.name)
    setDownloading(false)
    if (!ok) toast.error(`Couldn't download ${file.name}`)
  }, [file.url, file.name])

  return (
    <li className="bg-card group flex flex-col overflow-hidden rounded-xl">
      <div className="bg-muted/50 relative aspect-[4/3] w-full overflow-hidden">
        <FileThumb file={file} />
        <button
          type="button"
          onClick={onDownload}
          aria-label={`Download ${file.name}`}
          title="Download"
          className="bg-background/80 text-foreground hover:bg-background absolute top-2 right-2 grid size-8 place-items-center rounded-full opacity-0 backdrop-blur transition group-hover:opacity-100 focus-visible:opacity-100"
        >
          {downloading ? <Loader2 className="size-4 animate-spin" /> : <Download className="size-4" />}
        </button>
      </div>
      <div className="flex flex-col gap-0.5 p-2.5">
        <p className="text-foreground truncate text-[0.8rem] font-medium" title={file.name}>
          {file.name}
        </p>
        <p className="text-muted-foreground/70 text-[0.68rem]">
          {sourceLabel(file.source)}
          {file.bytes ? ` · ${formatBytes(file.bytes)}` : ''}
        </p>
      </div>
    </li>
  )
}

/** Thumbnail: authed image preview for images, a kind glyph otherwise. */
function FileThumb({ file }: { file: WorkspaceFile }) {
  const [src, setSrc] = useState<string | null>(null)
  useEffect(() => {
    if (file.kind !== 'image') return
    let alive = true
    let made: string | null = null
    loadMediaObjectURL(file.url).then((url) => {
      if (!alive) {
        if (url) URL.revokeObjectURL(url)
        return
      }
      made = url
      setSrc(url)
    })
    return () => {
      alive = false
      if (made) URL.revokeObjectURL(made)
    }
  }, [file.url, file.kind])

  if (file.kind === 'image' && src) {
    // eslint-disable-next-line @next/next/no-img-element -- authed blob object URL, not an optimizable remote asset
    return <img src={src} alt={file.name} className="size-full object-cover" />
  }
  const Icon = kindIcon(file.kind)
  return (
    <div className="text-muted-foreground/60 grid size-full place-items-center">
      <Icon className="size-8" />
    </div>
  )
}

function kindIcon(kind: MediaKind) {
  if (kind === 'image') return ImageIcon
  if (kind === 'video') return PlayIcon
  if (kind === 'audio') return Music2Icon
  return FileText
}

function sourceLabel(source?: WorkspaceFile['source']): string {
  if (source === 'upload') return 'You uploaded'
  if (source === 'generated') return 'Neo made'
  if (source === 'artifact') return 'Deliverable'
  return 'File'
}

function FolderGlyph() {
  return <FileIcon className="size-5" />
}

function FilesSkeleton() {
  return (
    <ul className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
      {Array.from({ length: 8 }).map((_, i) => (
        <li key={i} className="bg-card overflow-hidden rounded-xl">
          <div className="bg-muted aspect-[4/3] w-full" />
          <div className="flex flex-col gap-1.5 p-2.5">
            <div className="bg-muted h-3 w-3/4 rounded" />
            <div className="bg-muted h-2.5 w-1/2 rounded" />
          </div>
        </li>
      ))}
    </ul>
  )
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let v = n
  let u = 0
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024
    u++
  }
  return `${v < 10 && u > 0 ? v.toFixed(1) : Math.round(v)} ${units[u]}`
}
