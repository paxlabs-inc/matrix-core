'use client'

/**
 * NeoFiles — the Workspace / Files page.
 *
 * Neo has a whole machine per user; this page is the window into its REAL
 * file space: the persisted workspace volume (/data/workspace) that Neo's fs
 * and exec tools mutate. It browses the live directory tree, downloads any
 * file's exact bytes, and uploads files into the directory being viewed —
 * every row is a real dirent reported by the daemon, nothing is fabricated.
 *
 * Backend: GET /workspace/tree (listing), GET /workspace/raw (byte-exact
 * authed download / image thumbnails), POST /workspace/upload (multipart
 * into the workspace).
 *
 * Design system: full-page overlay separated by background TONE only
 * (bg-background / bg-card), single Matrix Sage accent on interactive chrome,
 * no border strokes for depth, no emojis / glow.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { toast } from 'sonner'
import { Dialog } from '@astryxdesign/core/Dialog'
import { Button } from '@astryxdesign/core/Button'
import { TextInput } from '@astryxdesign/core/TextInput'
import { Heading, Text } from '@astryxdesign/core/Text'
import {
  ArrowLeft,
  ChevronRight,
  Code,
  Download,
  FileText,
  FolderIcon,
  ImageIcon,
  Loader2,
  Music2Icon,
  Paperclip,
  PlayIcon,
  RotateCcw,
  Search,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import {
  downloadWorkspaceFile,
  getFile,
  getTree,
  loadWorkspaceFileURL,
  uploadWorkspaceFiles,
  type TreeEntry,
  type WorkspaceFile,
} from '@/lib/api/workspace'
import { NeoIllustration } from '@/components/matrix/neo/neo-illustration'

type FileKind = 'image' | 'video' | 'audio' | 'file'
export type WorkspaceViewerKind = 'text' | 'image' | 'video' | 'audio' | 'pdf' | 'binary'

const KIND_EXTS: Record<Exclude<FileKind, 'file'>, string[]> = {
  image: ['png', 'jpg', 'jpeg', 'webp', 'gif', 'svg', 'ico', 'bmp', 'avif'],
  video: ['mp4', 'webm', 'mov', 'mkv', 'avi'],
  audio: ['mp3', 'wav', 'm4a', 'flac', 'ogg', 'opus', 'aac'],
}

const TEXT_EXTS = new Set([
  'txt',
  'md',
  'mdx',
  'csv',
  'tsv',
  'json',
  'jsonl',
  'yaml',
  'yml',
  'toml',
  'xml',
  'html',
  'htm',
  'css',
  'scss',
  'less',
  'js',
  'jsx',
  'mjs',
  'cjs',
  'ts',
  'tsx',
  'py',
  'go',
  'rs',
  'java',
  'kt',
  'swift',
  'c',
  'cc',
  'cpp',
  'h',
  'hpp',
  'sh',
  'bash',
  'zsh',
  'fish',
  'sql',
  'graphql',
  'gql',
  'ini',
  'conf',
  'config',
  'log',
  'env',
])

export function workspaceViewerKind(name: string): WorkspaceViewerKind {
  const mediaKind = kindForName(name)
  if (mediaKind !== 'file') return mediaKind
  const dot = name.lastIndexOf('.')
  const extension = dot >= 0 ? name.slice(dot + 1).toLowerCase() : ''
  if (extension === 'pdf') return 'pdf'
  if (TEXT_EXTS.has(extension)) return 'text'
  const normalized = name.toLowerCase()
  if (
    dot === -1 ||
    normalized === 'dockerfile' ||
    normalized === 'makefile' ||
    normalized.startsWith('readme') ||
    normalized.startsWith('license')
  ) {
    return 'text'
  }
  return 'binary'
}

function kindForName(name: string): FileKind {
  const dot = name.lastIndexOf('.')
  if (dot <= 0) return 'file'
  const ext = name.slice(dot + 1).toLowerCase()
  for (const kind of Object.keys(KIND_EXTS) as (keyof typeof KIND_EXTS)[]) {
    if (KIND_EXTS[kind].includes(ext)) return kind
  }
  return 'file'
}

function baseName(path: string): string {
  return path.split('/').pop() || path
}

function parentOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i === -1 ? '' : path.slice(0, i)
}

export function NeoFiles({ open, onClose }: { open: boolean; onClose: () => void }) {
  const reduce = useReducedMotion()
  const [entries, setEntries] = useState<TreeEntry[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(false)
  const [loadError, setLoadError] = useState(false)
  const [nonce, setNonce] = useState(0)
  const [cwd, setCwd] = useState('')
  const [query, setQuery] = useState('')
  const [uploading, setUploading] = useState(false)
  const [dragging, setDragging] = useState(false)
  const [selectedFile, setSelectedFile] = useState<TreeEntry | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  // Load the real workspace tree on open (and on refresh / after uploads).
  useEffect(() => {
    if (!open) return
    const ctrl = new AbortController()
    let alive = true
    setLoading(true)
    setLoadError(false)
    getTree(undefined, ctrl.signal)
      .then((tree) => {
        if (!alive) return
        setEntries(tree.entries)
        setTruncated(tree.truncated)
      })
      .catch(() => {
        if (!alive || ctrl.signal.aborted) return
        setLoadError(true)
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
      ctrl.abort()
    }
  }, [open, nonce])

  // A reopened page starts at the workspace root with a clean search.
  useEffect(() => {
    if (!open) return
    setCwd('')
    setQuery('')
    setSelectedFile(null)
  }, [open])

  useEffect(() => {
    if (!selectedFile) return
    if (!entries.some((entry) => !entry.dir && entry.path === selectedFile.path)) {
      setSelectedFile(null)
    }
  }, [entries, selectedFile])

  // Escape closes.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  // Shallow item counts per directory (what "12 items" on a folder row means).
  const childCounts = useMemo(() => {
    const counts = new Map<string, number>()
    for (const e of entries) {
      const parent = parentOf(e.path)
      counts.set(parent, (counts.get(parent) ?? 0) + 1)
    }
    return counts
  }, [entries])

  // The rows on screen: the cwd's children, or every matching file when
  // searching (search spans the WHOLE tree, so results show their path).
  const searching = query.trim().length > 0
  const shown = useMemo(() => {
    if (searching) {
      const q = query.trim().toLowerCase()
      return entries
        .filter((e) => !e.dir && e.path.toLowerCase().includes(q))
        .sort((a, b) => a.path.localeCompare(b.path))
    }
    return entries
      .filter((e) => parentOf(e.path) === cwd)
      .sort((a, b) => {
        if (a.dir !== b.dir) return a.dir ? -1 : 1
        return baseName(a.path).localeCompare(baseName(b.path), undefined, {
          sensitivity: 'base',
        })
      })
  }, [entries, cwd, query, searching])

  const crumbs = useMemo(() => (cwd ? cwd.split('/') : []), [cwd])

  const refresh = useCallback(() => setNonce((n) => n + 1), [])

  const onPickFiles = useCallback(
    async (fileList: FileList | null) => {
      if (!fileList || fileList.length === 0) return
      const files = Array.from(fileList)
      setUploading(true)
      try {
        const up = await uploadWorkspaceFiles(undefined, cwd, files)
        toast.success(up.length === 1 ? `Uploaded ${up[0].name}` : `Uploaded ${up.length} files`)
        refresh()
      } catch {
        toast.error(
          files.length === 1 ? `Couldn't upload ${files[0].name}` : "Couldn't upload the files",
        )
      } finally {
        setUploading(false)
      }
    },
    [cwd, refresh],
  )

  return (
    <Dialog
      isOpen={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      variant="fullscreen"
      purpose="info"
      padding={0}
      aria-label="Workspace files"
    >
      <motion.div
        initial={reduce ? false : { opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.2 }}
        className="bg-background flex h-dvh flex-col"
        onDragOver={(e) => {
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={(e) => {
          if (e.currentTarget === e.target) setDragging(false)
        }}
        onDrop={(e) => {
          e.preventDefault()
          setDragging(false)
          void onPickFiles(e.dataTransfer.files)
        }}
      >
        {/* header */}
        <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
          <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
            <FolderIcon className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <Heading level={1}>Workspace</Heading>
            <Text type="supporting" color="secondary" display="block">
              Browse and preview the files on Neo&apos;s machine
            </Text>
          </div>
          <Button
            label="Upload"
            variant="primary"
            icon={<Paperclip className="size-4" />}
            isLoading={uploading}
            isDisabled={uploading}
            onClick={() => fileInput.current?.click()}
          />
          <Button
            label="Close workspace"
            variant="ghost"
            size="sm"
            icon={<X className="size-5" />}
            isIconOnly
            onClick={onClose}
          />
          <input
            ref={fileInput}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => {
              void onPickFiles(e.target.files)
              e.target.value = ''
            }}
          />
        </div>

        {/* search + location */}
        <div className="mx-auto flex w-full max-w-6xl shrink-0 flex-col gap-3 px-4 pb-3 sm:px-6">
          <div className="bg-card rounded-xl">
            <TextInput
              label="Search all files"
              isLabelHidden
              value={query}
              onChange={setQuery}
              placeholder="Search all files…"
              startIcon={<Search className="size-4" />}
              hasClear
              width="100%"
            />
          </div>
          {!searching ? (
            <div className="flex min-w-0 items-center gap-1">
              <nav
                aria-label="Current directory"
                className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto text-sm"
              >
                <button
                  type="button"
                  onClick={() => {
                    setCwd('')
                    setSelectedFile(null)
                  }}
                  className={cn(
                    'shrink-0 rounded-md px-1.5 py-0.5 font-medium transition-colors',
                    cwd === '' ? 'text-foreground' : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  Workspace
                </button>
                {crumbs.map((part, i) => {
                  const target = crumbs.slice(0, i + 1).join('/')
                  const last = i === crumbs.length - 1
                  return (
                    <span key={target} className="flex shrink-0 items-center gap-1">
                      <ChevronRight className="text-muted-foreground/60 size-3.5" />
                      <button
                        type="button"
                        onClick={() => {
                          setCwd(target)
                          setSelectedFile(null)
                        }}
                        className={cn(
                          'rounded-md px-1.5 py-0.5 font-medium transition-colors',
                          last ? 'text-foreground' : 'text-muted-foreground hover:text-foreground',
                        )}
                      >
                        {part}
                      </button>
                    </span>
                  )
                })}
              </nav>
              <button
                type="button"
                onClick={refresh}
                aria-label="Refresh files"
                className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-8 shrink-0 place-items-center rounded-full transition"
              >
                <RotateCcw className={cn('size-4', loading && 'animate-spin')} />
              </button>
            </div>
          ) : null}
        </div>

        {/* Master/detail workspace: the file list remains navigable while the
              authenticated viewer owns the detail pane. */}
        <div className="mx-auto flex min-h-0 w-full max-w-6xl flex-1 gap-4 px-4 pb-8 sm:px-6">
          <section
            aria-label="Workspace file list"
            className={cn(
              'min-h-0 w-full overflow-y-auto lg:w-[23rem] lg:shrink-0',
              selectedFile && 'max-lg:hidden',
            )}
          >
            {loading && entries.length === 0 ? (
              <FilesSkeleton />
            ) : loadError && entries.length === 0 ? (
              <div className="flex flex-col items-center gap-4 py-16 text-center">
                <NeoIllustration art="workspace" width={180} />
                <div className="flex flex-col gap-1">
                  <p className="text-foreground text-base font-bold">
                    Couldn&apos;t reach the workspace
                  </p>
                  <p className="text-muted-foreground mx-auto max-w-sm text-sm">
                    Neo&apos;s machine didn&apos;t answer. It may be waking up — try again in a
                    moment.
                  </p>
                </div>
                <button
                  type="button"
                  onClick={refresh}
                  className="text-primary hover:bg-primary/10 rounded-full px-3.5 py-2 text-sm font-medium transition-colors"
                >
                  Retry
                </button>
              </div>
            ) : shown.length === 0 ? (
              <div className="flex flex-col items-center gap-4 py-16 text-center">
                <NeoIllustration art={searching || cwd ? 'files' : 'workspace'} width={180} />
                <div className="flex flex-col gap-1">
                  <p className="text-foreground text-base font-bold">
                    {searching ? 'No matching files' : 'Nothing here yet'}
                  </p>
                  <p className="text-muted-foreground mx-auto max-w-sm text-sm">
                    {searching
                      ? 'Try a different search.'
                      : 'Files Neo works on show up here, and anything you upload lands on its machine for Neo to use.'}
                  </p>
                </div>
                {!searching ? (
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
              <ul className="flex flex-col gap-1">
                {shown.map((entry) =>
                  entry.dir ? (
                    <DirRow
                      key={entry.path}
                      entry={entry}
                      count={childCounts.get(entry.path) ?? 0}
                      onOpen={() => {
                        setQuery('')
                        setCwd(entry.path)
                        setSelectedFile(null)
                      }}
                    />
                  ) : (
                    <FileRow
                      key={entry.path}
                      entry={entry}
                      showPath={searching}
                      selected={selectedFile?.path === entry.path}
                      onOpen={() => setSelectedFile(entry)}
                    />
                  ),
                )}
              </ul>
            )}
            {truncated ? (
              <p className="text-muted-foreground/70 px-2 pt-3 text-xs">
                Large workspace — not every file is listed.
              </p>
            ) : null}
          </section>

          <section
            aria-label="File preview"
            className={cn(
              'bg-card min-h-0 min-w-0 flex-1 overflow-hidden rounded-2xl',
              !selectedFile && 'max-lg:hidden',
            )}
          >
            {selectedFile ? (
              <WorkspaceFileViewer
                key={selectedFile.path}
                entry={selectedFile}
                onBack={() => setSelectedFile(null)}
              />
            ) : (
              <div className="grid h-full min-h-80 place-items-center px-6 text-center">
                <div>
                  <FileText className="text-muted-foreground/50 mx-auto size-8" />
                  <p className="text-foreground mt-3 text-sm font-medium">
                    Select a file to preview
                  </p>
                  <p className="text-muted-foreground mt-1 text-xs">
                    Text, code, images, media, and PDFs open here.
                  </p>
                </div>
              </div>
            )}
          </section>
        </div>

        {/* drop hint */}
        {dragging ? (
          <div className="bg-background/80 pointer-events-none absolute inset-0 z-10 grid place-items-center backdrop-blur-sm">
            <div className="bg-card flex items-center gap-3 rounded-2xl px-6 py-4">
              <Paperclip className="text-primary size-5" />
              <p className="text-foreground text-sm font-semibold">
                Drop to upload {cwd ? `into ${baseName(cwd)}` : 'to the workspace'}
              </p>
            </div>
          </div>
        ) : null}
      </motion.div>
    </Dialog>
  )
}

function DirRow({ entry, count, onOpen }: { entry: TreeEntry; count: number; onOpen: () => void }) {
  return (
    <li>
      <button
        type="button"
        onClick={onOpen}
        className="bg-card hover:bg-muted/60 flex w-full items-center gap-3 rounded-xl px-3 py-2.5 text-left transition-colors"
      >
        <span className="text-muted-foreground grid size-9 shrink-0 place-items-center rounded-lg">
          <FolderIcon className="size-5" />
        </span>
        <span className="text-foreground min-w-0 flex-1 truncate text-sm font-medium">
          {baseName(entry.path)}
        </span>
        <span className="text-muted-foreground/70 shrink-0 text-xs">
          {count === 1 ? '1 item' : `${count} items`}
        </span>
        <ChevronRight className="text-muted-foreground/60 size-4 shrink-0" />
      </button>
    </li>
  )
}

export function FileRow({
  entry,
  showPath,
  selected,
  onOpen,
}: {
  entry: TreeEntry
  showPath: boolean
  selected: boolean
  onOpen: () => void
}) {
  const name = baseName(entry.path)
  const kind = kindForName(name)
  const [downloading, setDownloading] = useState(false)

  const onDownload = useCallback(async () => {
    setDownloading(true)
    const ok = await downloadWorkspaceFile(undefined, entry.path, name)
    setDownloading(false)
    if (!ok) toast.error(`Couldn't download ${name}`)
  }, [entry.path, name])

  return (
    <li
      className={cn(
        'group/file flex items-center rounded-xl transition-colors',
        selected ? 'bg-muted' : 'bg-card hover:bg-muted/60',
      )}
    >
      <button
        type="button"
        onClick={onOpen}
        aria-label={`Open ${name}`}
        aria-pressed={selected}
        className="flex min-w-0 flex-1 items-center gap-3 px-3 py-2.5 text-left"
      >
        <FileThumb path={entry.path} name={name} kind={kind} />
        <span className="min-w-0 flex-1">
          <span className="text-foreground block truncate text-sm font-medium" title={entry.path}>
            {name}
          </span>
          {showPath && entry.path.includes('/') ? (
            <span className="text-muted-foreground/70 block truncate text-xs">
              {parentOf(entry.path)}
            </span>
          ) : null}
        </span>
        {entry.size ? (
          <span className="text-muted-foreground/70 shrink-0 text-xs">
            {formatBytes(entry.size)}
          </span>
        ) : null}
      </button>
      <button
        type="button"
        onClick={() => void onDownload()}
        aria-label={`Download ${name}`}
        title="Download"
        className="text-muted-foreground hover:bg-background/60 hover:text-foreground mr-1 grid size-8 shrink-0 place-items-center rounded-full transition"
      >
        {downloading ? (
          <Loader2 className="size-4 animate-spin" />
        ) : (
          <Download className="size-4" />
        )}
      </button>
    </li>
  )
}

export function WorkspaceFileViewer({ entry, onBack }: { entry: TreeEntry; onBack: () => void }) {
  const name = baseName(entry.path)
  const kind = workspaceViewerKind(name)
  const [textFile, setTextFile] = useState<WorkspaceFile | null>(null)
  const [objectURL, setObjectURL] = useState<string | null>(null)
  const [loading, setLoading] = useState(kind !== 'binary')
  const [failed, setFailed] = useState(false)
  const [downloading, setDownloading] = useState(false)

  useEffect(() => {
    const controller = new AbortController()
    let alive = true
    let madeURL: string | null = null
    setTextFile(null)
    setObjectURL(null)
    setFailed(false)

    if (kind === 'binary') {
      setLoading(false)
      return () => controller.abort()
    }

    setLoading(true)
    const read =
      kind === 'text'
        ? getFile(undefined, entry.path, controller.signal).then((file) => {
            if (alive) setTextFile(file)
          })
        : loadWorkspaceFileURL(undefined, entry.path).then((url) => {
            if (!alive) {
              if (url) URL.revokeObjectURL(url)
              return
            }
            if (!url) throw new Error('preview unavailable')
            madeURL = url
            setObjectURL(url)
          })

    void read
      .catch(() => {
        if (alive && !controller.signal.aborted) setFailed(true)
      })
      .finally(() => {
        if (alive) setLoading(false)
      })

    return () => {
      alive = false
      controller.abort()
      if (madeURL) URL.revokeObjectURL(madeURL)
    }
  }, [entry.path, kind])

  const download = useCallback(async () => {
    setDownloading(true)
    const ok = await downloadWorkspaceFile(undefined, entry.path, name)
    setDownloading(false)
    if (!ok) toast.error(`Couldn't download ${name}`)
  }, [entry.path, name])

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 items-center gap-2 px-3 py-3 sm:px-4">
        <button
          type="button"
          onClick={onBack}
          aria-label="Back to files"
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full lg:hidden"
        >
          <ArrowLeft className="size-4" />
        </button>
        <div className="min-w-0 flex-1">
          <p className="text-foreground truncate text-sm font-semibold" title={entry.path}>
            {name}
          </p>
          <p className="text-muted-foreground truncate text-[0.68rem]">{entry.path}</p>
        </div>
        {entry.size ? (
          <span className="text-muted-foreground hidden shrink-0 text-xs sm:inline">
            {formatBytes(entry.size)}
          </span>
        ) : null}
        <button
          type="button"
          onClick={() => void download()}
          aria-label={`Download ${name}`}
          className="text-muted-foreground hover:bg-muted hover:text-foreground flex min-h-9 shrink-0 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium"
        >
          {downloading ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Download className="size-4" />
          )}
          <span className="max-sm:hidden">Download</span>
        </button>
      </header>

      <div className="bg-background/50 min-h-0 flex-1 overflow-auto">
        {loading ? (
          <div className="grid h-full min-h-72 place-items-center">
            <div className="text-muted-foreground flex items-center gap-2 text-sm">
              <Loader2 className="size-4 animate-spin" />
              Loading preview…
            </div>
          </div>
        ) : failed ? (
          <ViewerFallback
            icon={FileText}
            title="Preview unavailable"
            body="Workspace could not read this file right now. Download remains available."
          />
        ) : kind === 'text' && textFile ? (
          <div className="min-h-full">
            {textFile.truncated ? (
              <p className="bg-muted text-muted-foreground px-4 py-2 text-xs">
                This file is large. The authenticated preview shows the bounded readable portion;
                download for the complete file.
              </p>
            ) : null}
            <pre className="text-foreground min-w-full p-4 font-mono text-xs leading-5 [overflow-wrap:anywhere] whitespace-pre-wrap">
              {textFile.content}
            </pre>
          </div>
        ) : kind === 'image' && objectURL ? (
          <WorkspaceImagePreview src={objectURL} name={name} />
        ) : kind === 'video' && objectURL ? (
          <div className="grid min-h-full place-items-center p-4">
            <video src={objectURL} controls className="max-h-full max-w-full rounded-xl">
              Your browser cannot play this video.
            </video>
          </div>
        ) : kind === 'audio' && objectURL ? (
          <div className="grid min-h-full place-items-center p-6">
            <div className="bg-card w-full max-w-md rounded-2xl p-5">
              <Music2Icon className="text-primary mb-4 size-7" />
              <p className="text-foreground mb-3 truncate text-sm font-medium">{name}</p>
              <audio src={objectURL} controls className="w-full">
                Your browser cannot play this audio.
              </audio>
            </div>
          </div>
        ) : kind === 'pdf' && objectURL ? (
          <iframe src={objectURL} title={name} className="h-full min-h-[34rem] w-full" />
        ) : (
          <ViewerFallback
            icon={kind === 'text' ? Code : FileText}
            title="No inline preview for this file"
            body="This file type is kept byte-exact. Use Download to open it with an app on your device."
          />
        )}
      </div>
    </div>
  )
}

export function WorkspaceImagePreview({ src, name }: { src: string; name: string }) {
  const [failed, setFailed] = useState(false)

  if (failed) {
    return (
      <ViewerFallback
        icon={ImageIcon}
        title="Image preview unavailable"
        body="The authenticated file loaded, but the browser could not decode this image format. Download remains available."
      />
    )
  }

  return (
    <div className="grid min-h-full place-items-center p-4">
      {/* eslint-disable-next-line @next/next/no-img-element -- authenticated blob URL */}
      <img
        src={src}
        alt={name}
        onError={() => setFailed(true)}
        className="max-h-[calc(100svh-14rem)] max-w-full rounded-xl object-contain"
      />
    </div>
  )
}

function ViewerFallback({
  icon: Icon,
  title,
  body,
}: {
  icon: typeof FileText
  title: string
  body: string
}) {
  return (
    <div className="grid h-full min-h-72 place-items-center p-6 text-center">
      <div className="max-w-sm">
        <span className="bg-card text-muted-foreground mx-auto grid size-12 place-items-center rounded-2xl">
          <Icon className="size-5" />
        </span>
        <p className="text-foreground mt-3 text-sm font-semibold">{title}</p>
        <p className="text-muted-foreground mt-1 text-xs leading-relaxed">{body}</p>
      </div>
    </div>
  )
}

/** Thumbnail: authed image preview for images, a kind glyph otherwise. */
function FileThumb({ path, name, kind }: { path: string; name: string; kind: FileKind }) {
  const [src, setSrc] = useState<string | null>(null)
  useEffect(() => {
    if (kind !== 'image') return
    let alive = true
    let made: string | null = null
    loadWorkspaceFileURL(undefined, path).then((url) => {
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
  }, [path, kind])

  if (kind === 'image' && src) {
    return (
      <span className="bg-muted/50 size-9 shrink-0 overflow-hidden rounded-lg">
        {/* eslint-disable-next-line @next/next/no-img-element -- authed blob object URL, not an optimizable remote asset */}
        <img src={src} alt={name} className="size-full object-cover" />
      </span>
    )
  }
  const Icon = kindIcon(kind)
  return (
    <span className="text-muted-foreground grid size-9 shrink-0 place-items-center rounded-lg">
      <Icon className="size-5" />
    </span>
  )
}

function kindIcon(kind: FileKind) {
  if (kind === 'image') return ImageIcon
  if (kind === 'video') return PlayIcon
  if (kind === 'audio') return Music2Icon
  return FileText
}

function FilesSkeleton() {
  return (
    <ul className="flex flex-col gap-1">
      {Array.from({ length: 8 }).map((_, i) => (
        <li key={i} className="bg-card flex items-center gap-3 rounded-xl px-3 py-2.5">
          <div className="bg-muted size-9 shrink-0 rounded-lg" />
          <div className="flex min-w-0 flex-1 flex-col gap-1.5">
            <div className="bg-muted h-3 w-1/2 rounded" />
          </div>
          <div className="bg-muted h-3 w-10 rounded" />
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
