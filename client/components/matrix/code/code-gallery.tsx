'use client'

/**
 * /code — the preset template gallery. Users browse, preview, and pick a
 * design spec that gets injected into the Cody agent workspace.
 */
import { useCallback, useMemo, useState } from 'react'
import Image from 'next/image'
import {
  Layout,
  LayoutContent,
  LayoutFooter,
  LayoutHeader,
  VStack,
} from '@astryxdesign/core/Layout'
import { Grid } from '@astryxdesign/core/Grid'
import { Heading, Text } from '@astryxdesign/core/Text'
import { TextInput } from '@astryxdesign/core/TextInput'
import { ToggleButton, ToggleButtonGroup } from '@astryxdesign/core/ToggleButton'
import { ClickableCard } from '@astryxdesign/core/ClickableCard'
import { Dialog as AstryxDialog } from '@astryxdesign/core/Dialog'
import { Button } from '@astryxdesign/core/Button'
import { useRouter } from '@/i18n/navigation'
import { ArrowLeft } from '@/lib/matrix-icons'

import {
  previewUrl,
  PRESET_CATEGORIES,
  type PresetCategory,
  type PresetSummary,
} from '@/lib/data/presets'
import { Badge } from '@/components/ui/badge'

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
      <ToggleButtonGroup
        label="Template categories"
        value={active}
        onChange={(value) => value && onChange(value as PresetCategory)}
        size="sm"
      >
        {PRESET_CATEGORIES.map((cat) => {
          const n = cat === 'All' ? counts['All'] : (counts[cat] ?? 0)
          if (n === 0 && cat !== 'All') return null
          return <ToggleButton key={cat} value={cat} label={`${cat} ${n}`} />
        })}
      </ToggleButtonGroup>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/*  Preset card                                                        */
/* ------------------------------------------------------------------ */

function PresetCard({
  preset,
  onSelect,
}: {
  preset: PresetSummary
  onSelect: (p: PresetSummary) => void
}) {
  const [imgError, setImgError] = useState(false)
  const src = previewUrl(preset.slug)

  return (
    <ClickableCard
      label={preset.title}
      onClick={() => onSelect(preset)}
      variant="muted"
      padding={0}
      elevation="none"
      className="group flex h-full flex-col overflow-hidden text-left"
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
        <Text type="body" weight="medium" maxLines={1}>
          {preset.title}
        </Text>
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
        <Text type="supporting" color="secondary" maxLines={2}>
          {preset.description}
        </Text>
      </div>
    </ClickableCard>
  )
}

function PresetDetail({
  preset,
  onConfirm,
  onCancel,
}: {
  preset: PresetSummary
  onConfirm: () => void
  onCancel: () => void
}) {
  const [imgError, setImgError] = useState(false)
  const src = previewUrl(preset.slug)

  return (
    <Layout
      height="auto"
      header={
        <LayoutHeader>
          <VStack gap={1}>
            <Heading level={2} type="display-3">
              {preset.title}
            </Heading>
            <Text type="supporting" color="secondary">
              {preset.description}
            </Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent>
          <VStack gap={4}>
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
            <div className="flex flex-wrap gap-1.5">
              <Badge variant="secondary">{preset.category}</Badge>
              <Badge variant="outline">{preset.industry}</Badge>
              {preset.tags.slice(0, 6).map((tag) => (
                <Badge key={tag} variant="outline">
                  {tag}
                </Badge>
              ))}
            </div>
            <Text type="supporting" color="secondary">
              This will open the Neo agent workspace and start building this design automatically.
            </Text>
          </VStack>
        </LayoutContent>
      }
      footer={
        <LayoutFooter className="flex justify-end gap-2">
          <Button label="Cancel" variant="ghost" onClick={onCancel} />
          <Button label="Use this template" onClick={onConfirm} />
        </LayoutFooter>
      }
    />
  )
}

function ConfirmDialog({
  preset,
  onConfirm,
  onCancel,
}: {
  preset: PresetSummary
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <AstryxDialog
      isOpen
      onOpenChange={(open) => !open && onCancel()}
      width="min(560px, calc(100vw - 24px))"
      maxHeight="90vh"
      purpose="info"
      padding={0}
    >
      <PresetDetail preset={preset} onConfirm={onConfirm} onCancel={onCancel} />
    </AstryxDialog>
  )
}

/* ------------------------------------------------------------------ */
/*  Main gallery                                                       */
/* ------------------------------------------------------------------ */

export function CodeGallery({ initialPresets }: { initialPresets: PresetSummary[] }) {
  const router = useRouter()
  const [search, setSearch] = useState('')
  const [category, setCategory] = useState<PresetCategory>('All')
  const [selected, setSelected] = useState<PresetSummary | null>(null)
  const presets = useMemo(
    () => initialPresets.toSorted((a, b) => b.tryCount - a.tryCount),
    [initialPresets],
  )

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
    <>
      <Layout
        height="auto"
        contentWidth={1152}
        padding={4}
        className="min-h-svh"
        header={
          <LayoutHeader className="bg-background sticky top-0 z-10">
            <VStack gap={3} width="100%">
              <div className="flex items-start gap-3">
                <Button
                  label="Back home"
                  href="/"
                  icon={<ArrowLeft className="size-4" />}
                  isIconOnly
                  variant="ghost"
                />
                <VStack gap={1} className="min-w-0 flex-1">
                  <Heading level={1} type="display-3">
                    Templates
                  </Heading>
                  <Text type="supporting" color="secondary">
                    Pick a design preset and Neo will build it for you — real specs, not generic AI
                    slop.
                  </Text>
                </VStack>
              </div>

              <TextInput
                label="Search templates"
                isLabelHidden
                placeholder="Search templates…"
                value={search}
                onChange={setSearch}
                hasClear
                width={384}
              />
              <CategoryBar active={category} onChange={setCategory} counts={counts} />
            </VStack>
          </LayoutHeader>
        }
        content={
          <LayoutContent>
            {filtered.length === 0 ? (
              <Text type="body" color="secondary" display="block" justify="center">
                No templates match your search.
              </Text>
            ) : (
              <Grid columns={{ minWidth: 280, max: 3, repeat: 'fit' }} gap={4} width="100%">
                {filtered.map((preset) => (
                  <PresetCard key={preset.slug} preset={preset} onSelect={setSelected} />
                ))}
              </Grid>
            )}
          </LayoutContent>
        }
      />
      {selected && (
        <ConfirmDialog
          preset={selected}
          onConfirm={handleConfirm}
          onCancel={() => setSelected(null)}
        />
      )}
    </>
  )
}
