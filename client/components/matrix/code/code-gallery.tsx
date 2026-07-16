'use client'

/**
 * /code — the preset template gallery. Users browse, preview, and pick a
 * design spec that gets injected into the Cody agent workspace.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import Image from 'next/image'
import { useRouter } from '@/i18n/navigation'

import {
  loadPresets,
  previewUrl,
  PRESET_CATEGORIES,
  type Preset,
  type PresetCategory,
} from '@/lib/data/presets'
import { useIsMobile } from '@/components/ui/use-mobile'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Drawer,
  DrawerClose,
  DrawerContent,
  DrawerDescription,
  DrawerFooter,
  DrawerHeader,
  DrawerTitle,
} from '@/components/ui/drawer'

/* ------------------------------------------------------------------ */
/*  Category filter bar                                                */
/* ------------------------------------------------------------------ */

function CategoryBar({
  active,
  onChange,
  counts,
}: {
  active: PresetCategory
  onChange: (c: PresetCategory) => void
  counts: Record<string, number>
}) {
  return (
    <div className="scrollbar-none flex gap-1.5 overflow-x-auto pb-1">
      {PRESET_CATEGORIES.map((cat) => {
        const n = cat === 'All' ? counts['All'] : (counts[cat] ?? 0)
        if (n === 0 && cat !== 'All') return null
        return (
          <button
            key={cat}
            onClick={() => onChange(cat)}
            className={
              active === cat
                ? 'bg-foreground text-background shrink-0 rounded-full px-3 py-1 text-xs font-medium'
                : 'bg-surface-secondary hover:bg-surface-hover text-muted-foreground shrink-0 rounded-full px-3 py-1 text-xs'
            }
          >
            {cat}
            <span className="ml-1 opacity-60">{n}</span>
          </button>
        )
      })}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/*  Preset card                                                        */
/* ------------------------------------------------------------------ */

function PresetCard({ preset, onSelect }: { preset: Preset; onSelect: (p: Preset) => void }) {
  const [imgError, setImgError] = useState(false)
  const src = previewUrl(preset.slug)

  return (
    <button
      onClick={() => onSelect(preset)}
      className="bg-surface-secondary hover:bg-surface-hover group flex flex-col overflow-hidden rounded-xl text-left transition-all active:scale-[0.98]"
    >
      <div className="relative aspect-[16/10] w-full overflow-hidden bg-black/5">
        {!imgError ? (
          <Image
            src={src}
            alt={preset.title}
            fill
            sizes="(max-width: 640px) 100vw, (max-width: 1024px) 50vw, 33vw"
            className="object-cover transition-transform duration-300 group-hover:scale-[1.03]"
            onError={() => setImgError(true)}
            unoptimized
          />
        ) : (
          <div className="text-muted-foreground flex h-full items-center justify-center text-xs">
            No preview
          </div>
        )}
      </div>
      <div className="flex flex-col gap-1 p-3">
        <span className="text-foreground line-clamp-1 text-sm font-medium">{preset.title}</span>
        <div className="flex items-center gap-1.5">
          <Badge variant="secondary" className="text-[10px]">
            {preset.category}
          </Badge>
          {preset.deslop_score >= 7 ? (
            <span className="text-[10px] text-amber-500" title="Design quality score">
              {'★'.repeat(Math.round(preset.deslop_score / 2))}
            </span>
          ) : null}
        </div>
        <p className="text-muted-foreground line-clamp-2 text-xs">{preset.description}</p>
      </div>
    </button>
  )
}

/* ------------------------------------------------------------------ */
/*  Confirm detail — shared inner content for Dialog (desktop) and     */
/*  Drawer (mobile).                                                   */
/* ------------------------------------------------------------------ */

function PresetDetail({
  preset,
  onConfirm,
  onCancel,
  isMobile,
}: {
  preset: Preset
  onConfirm: () => void
  onCancel: () => void
  isMobile: boolean
}) {
  const [imgError, setImgError] = useState(false)
  const src = previewUrl(preset.slug)

  const tags = (
    <div className="flex flex-wrap gap-1.5">
      <Badge variant="secondary">{preset.category}</Badge>
      <Badge variant="outline">{preset.industry}</Badge>
      {preset.tags.slice(0, 6).map((t) => (
        <Badge key={t} variant="outline" className="text-[10px]">
          {t}
        </Badge>
      ))}
    </div>
  )

  const preview = (
    <div className="relative aspect-[16/10] w-full shrink-0 overflow-hidden rounded-lg bg-black/5">
      {!imgError ? (
        <Image
          src={src}
          alt={preset.title}
          fill
          className="object-cover"
          onError={() => setImgError(true)}
          unoptimized
        />
      ) : null}
    </div>
  )

  if (isMobile) {
    return (
      <DrawerContent className="max-h-[90vh]">
        <DrawerHeader className="text-left">
          <DrawerTitle>{preset.title}</DrawerTitle>
          <DrawerDescription>{preset.description}</DrawerDescription>
        </DrawerHeader>
        <div className="flex flex-col gap-4 overflow-y-auto px-4 pb-4">
          {preview}
          {tags}
          <p className="text-muted-foreground text-xs">
            This will open the Neo agent workspace and start building this design automatically.
          </p>
        </div>
        <DrawerFooter className="pt-2">
          <Button onClick={onConfirm}>Use this template</Button>
          <DrawerClose asChild>
            <Button variant="ghost" onClick={onCancel}>
              Cancel
            </Button>
          </DrawerClose>
        </DrawerFooter>
      </DrawerContent>
    )
  }

  return (
    <DialogContent className="flex max-h-[85vh] flex-col sm:max-w-lg">
      <DialogHeader>
        <DialogTitle>{preset.title}</DialogTitle>
        <DialogDescription>{preset.description}</DialogDescription>
      </DialogHeader>
      <div className="flex flex-1 flex-col gap-4 overflow-y-auto">
        {preview}
        {tags}
        <p className="text-muted-foreground text-xs">
          This will open the Neo agent workspace and start building this design automatically.
        </p>
      </div>
      <DialogFooter className="shrink-0 pt-2">
        <Button variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button onClick={onConfirm}>Use this template</Button>
      </DialogFooter>
    </DialogContent>
  )
}

/* ------------------------------------------------------------------ */
/*  Confirm wrapper — picks Dialog or Drawer based on viewport         */
/* ------------------------------------------------------------------ */

function ConfirmDialog({
  preset,
  onConfirm,
  onCancel,
}: {
  preset: Preset
  onConfirm: () => void
  onCancel: () => void
}) {
  const isMobile = useIsMobile()

  if (isMobile) {
    return (
      <Drawer open onOpenChange={(open) => !open && onCancel()}>
        <PresetDetail preset={preset} onConfirm={onConfirm} onCancel={onCancel} isMobile />
      </Drawer>
    )
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <PresetDetail preset={preset} onConfirm={onConfirm} onCancel={onCancel} isMobile={false} />
    </Dialog>
  )
}

/* ------------------------------------------------------------------ */
/*  Main gallery                                                       */
/* ------------------------------------------------------------------ */

export function CodeGallery() {
  const router = useRouter()
  const [presets, setPresets] = useState<Preset[]>([])
  const [loaded, setLoaded] = useState(false)
  const [search, setSearch] = useState('')
  const [category, setCategory] = useState<PresetCategory>('All')
  const [selected, setSelected] = useState<Preset | null>(null)

  useEffect(() => {
    loadPresets().then((data) => {
      data.sort((a, b) => b.tryCount - a.tryCount)
      setPresets(data)
      setLoaded(true)
    })
  }, [])

  const counts = useMemo(() => {
    const map: Record<string, number> = { All: presets.length }
    for (const p of presets) {
      map[p.category] = (map[p.category] ?? 0) + 1
    }
    return map
  }, [presets])

  const filtered = useMemo(() => {
    let list = presets
    if (category !== 'All') list = list.filter((p) => p.category === category)
    if (search.trim()) {
      const q = search.toLowerCase()
      list = list.filter(
        (p) =>
          p.title.toLowerCase().includes(q) ||
          p.description.toLowerCase().includes(q) ||
          p.tags.some((t) => t.toLowerCase().includes(q)),
      )
    }
    return list
  }, [presets, category, search])

  const handleConfirm = useCallback(() => {
    if (!selected) return
    router.push({ pathname: '/cody', query: { preset: selected.slug } })
  }, [selected, router])

  return (
    <div className="flex min-h-svh flex-col">
      {/* Sticky header + filters */}
      <header className="bg-background sticky top-0 z-10 border-b px-4 py-3 sm:px-6 sm:py-4">
        <div className="mx-auto max-w-6xl">
          <h1 className="text-xl font-semibold tracking-tight sm:text-2xl">Templates</h1>
          <p className="text-muted-foreground mt-0.5 text-xs sm:mt-1 sm:text-sm">
            Pick a design preset and Neo will build it for you — real specs, not generic AI slop.
          </p>
          <div className="mt-3 flex flex-col gap-2 sm:mt-3 sm:gap-3">
            <Input
              placeholder="Search templates…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="max-w-sm"
            />
            <CategoryBar active={category} onChange={setCategory} counts={counts} />
          </div>
        </div>
      </header>

      {/* Grid — scrolls naturally under the sticky header */}
      <div className="flex-1 px-4 py-4 sm:px-6 sm:py-6">
        <div className="mx-auto max-w-6xl">
          {!loaded ? (
            <div className="text-muted-foreground py-24 text-center text-sm">
              Loading templates…
            </div>
          ) : filtered.length === 0 ? (
            <div className="text-muted-foreground py-24 text-center text-sm">
              No templates match your search.
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 sm:gap-4 lg:grid-cols-3">
              {filtered.map((preset) => (
                <PresetCard key={preset.slug} preset={preset} onSelect={setSelected} />
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Confirm dialog / drawer */}
      {selected ? (
        <ConfirmDialog
          preset={selected}
          onConfirm={handleConfirm}
          onCancel={() => setSelected(null)}
        />
      ) : null}
    </div>
  )
}
