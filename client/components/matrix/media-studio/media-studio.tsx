'use client'

import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type ComponentType,
  type FormEvent,
} from 'react'
import { useTranslations } from 'next-intl'
import { Layout } from '@astryxdesign/core/Layout'
import { Button } from '@astryxdesign/core/Button'
import { TextArea } from '@astryxdesign/core/TextArea'
import { Selector } from '@astryxdesign/core/Selector'
import { Slider } from '@astryxdesign/core/Slider'
import { NumberInput } from '@astryxdesign/core/NumberInput'
import { FileInput } from '@astryxdesign/core/FileInput'
import { ClickableCard } from '@astryxdesign/core/ClickableCard'
import { Badge } from '@astryxdesign/core/Badge'
import { Card } from '@astryxdesign/core/Card'
import { Text } from '@astryxdesign/core/Text'
import { Link } from '@/i18n/navigation'
import {
  ArrowLeft,
  ArrowUpFromLine,
  Download,
  ImageIcon,
  Play,
  RotateCcw,
  Sparkles,
  Trash2Icon,
  VideoIcon,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import {
  createStudioJob,
  deleteStudioJob,
  getStudioStatus,
  listStudioJobs,
  type StudioAsset,
  type StudioJob,
  type StudioKind,
  type StudioProviderStatus,
  type StudioRequest,
} from '@/lib/api/media-studio'
import {
  downloadMediaRef,
  loadMediaObjectURL,
  uploadMedia,
  type UploadedMedia,
} from '@/lib/api/media'

interface Workflow {
  kind: StudioKind
  key: string
  icon: ComponentType<{ className?: string }>
  group: 'createImage' | 'createVideo' | 'editImage'
}

const WORKFLOWS: Workflow[] = [
  { kind: 'text-to-image', key: 'textToImage', icon: ImageIcon, group: 'createImage' },
  { kind: 'image-to-image', key: 'imageToImage', icon: RotateCcw, group: 'createImage' },
  { kind: 'inpainting', key: 'inpainting', icon: Sparkles, group: 'createImage' },
  { kind: 'text-to-video', key: 'textToVideo', icon: VideoIcon, group: 'createVideo' },
  { kind: 'image-to-video', key: 'imageToVideo', icon: Play, group: 'createVideo' },
  { kind: 'cleanup', key: 'cleanup', icon: Sparkles, group: 'editImage' },
  { kind: 'remove-background', key: 'removeBackground', icon: ImageIcon, group: 'editImage' },
  { kind: 'replace-background', key: 'replaceBackground', icon: ImageIcon, group: 'editImage' },
  { kind: 'remove-text', key: 'removeText', icon: RotateCcw, group: 'editImage' },
  { kind: 'merge-face', key: 'mergeFace', icon: ImageIcon, group: 'editImage' },
  { kind: 'upscale', key: 'upscale', icon: ArrowUpFromLine, group: 'editImage' },
]

const ACTIVE = new Set(['queued', 'running'])

export function MediaStudio() {
  const t = useTranslations('mediaStudio')
  const [status, setStatus] = useState<StudioProviderStatus>()
  const [jobs, setJobs] = useState<StudioJob[]>([])
  const [form, setForm] = useState<StudioRequest>(() => defaultRequest('text-to-image'))
  const [selectedID, setSelectedID] = useState<string>()
  const [loading, setLoading] = useState(true)
  const [submitting, setSubmitting] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [deleteArmed, setDeleteArmed] = useState(false)
  const [notice, setNotice] = useState('')
  const selected = useMemo(
    () => jobs.find((job) => job.id === selectedID) ?? jobs[0],
    [jobs, selectedID],
  )
  const workflow = WORKFLOWS.find((item) => item.kind === form.kind) ?? WORKFLOWS[0]

  const refreshJobs = useCallback(async (signal?: AbortSignal) => {
    const next = await listStudioJobs(signal)
    setJobs(next)
    setSelectedID((current) =>
      current !== undefined && next.some((job) => job.id === current) ? current : next[0]?.id,
    )
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    Promise.all([getStudioStatus(controller.signal), listStudioJobs(controller.signal)])
      .then(([nextStatus, nextJobs]) => {
        setStatus(nextStatus)
        setJobs(nextJobs)
        setSelectedID(nextJobs[0]?.id)
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setNotice(error instanceof Error ? error.message : t('errors.unavailable'))
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false)
      })
    return () => controller.abort()
  }, [t])

  const hasActiveJob = jobs.some((job) => ACTIVE.has(job.status))
  useEffect(() => {
    if (!hasActiveJob) return
    const timer = window.setInterval(() => {
      void refreshJobs().catch(() => undefined)
    }, 2_000)
    return () => window.clearInterval(timer)
  }, [hasActiveJob, refreshJobs])

  const chooseWorkflow = (kind: StudioKind) => {
    setForm(defaultRequest(kind))
    setNotice('')
    setDeleteArmed(false)
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setSubmitting(true)
    setNotice('')
    try {
      const job = await createStudioJob(form)
      setSelectedID(job.id)
      setJobs((current) => [job, ...current.filter((item) => item.id !== job.id)])
      setNotice(t('notices.started'))
    } catch (error) {
      setNotice(error instanceof Error ? error.message : t('errors.create'))
    } finally {
      setSubmitting(false)
    }
  }

  const removeSelected = async () => {
    if (selected === undefined) return
    if (!deleteArmed) {
      setDeleteArmed(true)
      setNotice(t('notices.confirmDelete'))
      return
    }
    setDeleting(true)
    try {
      await deleteStudioJob(selected.id)
      setJobs((current) => current.filter((job) => job.id !== selected.id))
      setSelectedID(undefined)
      setDeleteArmed(false)
      setNotice(t('notices.deleted'))
    } catch (error) {
      setNotice(error instanceof Error ? error.message : t('errors.delete'))
    } finally {
      setDeleting(false)
    }
  }

  const reuseSettings = () => {
    if (selected === undefined) return
    const next = defaultRequest(selected.kind)
    setForm({
      ...next,
      ...selected.request,
      image: undefined,
      mask: undefined,
      face: undefined,
    })
    setNotice(
      selected.request.image !== undefined ? t('notices.reupload') : t('notices.settingsRestored'),
    )
  }

  return (
    <Layout
      height="auto"
      padding={0}
      className="bg-background text-foreground min-h-dvh"
      content={
        <main className="min-h-dvh overflow-x-hidden">
          <header className="bg-card px-2 py-2 sm:px-6 sm:py-4 lg:px-8">
            <div className="mx-auto flex max-w-[1800px] flex-wrap items-center justify-between gap-2 sm:gap-4">
              <div className="flex min-w-0 items-center gap-2 sm:gap-4">
                <Button
                  href="/"
                  as={Link}
                  label={t('backToChat')}
                  icon={<ArrowLeft className="size-5" />}
                  variant="ghost"
                  size="md"
                  isIconOnly
                />
                <div className="min-w-0">
                  <Text
                    type="supporting"
                    color="accent"
                    weight="bold"
                    display="block"
                    className="hidden sm:block"
                  >
                    {t('eyebrow')}
                  </Text>
                  <Text type="display-3" as="h1" maxLines={1} className="text-lg sm:text-2xl">
                    {t('title')}
                  </Text>
                </div>
              </div>
              <Card variant="muted" padding={1} className="w-full sm:w-auto">
                <div className="flex items-center justify-between gap-3 sm:justify-start">
                  <Badge
                    variant={status?.configured ? 'success' : 'warning'}
                    label={status?.configured ? t('status.ready') : t('providerUnavailable')}
                  />
                  <div className="hidden sm:block">
                    <Text type="label" weight="bold" display="block">
                      {status?.provider ?? 'Matrix Media'}
                    </Text>
                    <Text type="supporting" color="secondary" display="block" maxLines={2}>
                      {loading
                        ? t('checkingProvider')
                        : (status?.message ?? t('providerUnavailable'))}
                    </Text>
                  </div>
                </div>
              </Card>
            </div>
          </header>

          <div className="mx-auto grid max-w-[1800px] grid-cols-[minmax(0,1fr)] gap-1.5 p-1.5 sm:gap-3 sm:p-4 xl:grid-cols-[15rem_minmax(0,1fr)_22rem]">
            <WorkflowRail active={form.kind} onChoose={chooseWorkflow} />

            <section className="bg-card order-3 min-w-0 rounded-lg p-2.5 sm:p-5 xl:order-2">
              <div className="mb-2 flex items-center justify-between gap-3 sm:mb-4 sm:gap-4">
                <div>
                  <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
                    {t('output')}
                  </p>
                  <h2 className="text-base font-semibold sm:text-lg">
                    {selected === undefined
                      ? t(`workflows.${workflow.key}.label`)
                      : t(
                          `workflows.${
                            WORKFLOWS.find((item) => item.kind === selected.kind)?.key ??
                            'textToImage'
                          }.label`,
                        )}
                  </h2>
                </div>
                {selected !== undefined ? <JobStatus job={selected} /> : null}
              </div>

              <OutputCanvas job={selected} />

              <section className="bg-muted/50 mt-2 rounded-lg p-2.5 sm:mt-4 sm:p-4">
                <div className="mb-2 flex items-end justify-between gap-3 sm:mb-3">
                  <div>
                    <h2 className="font-semibold">{t('library.title')}</h2>
                    <p className="text-muted-foreground text-xs">
                      {t('library.count', { count: jobs.length })}
                    </p>
                  </div>
                  <Button
                    label={t('library.refresh')}
                    variant="ghost"
                    size="md"
                    icon={<RotateCcw className="size-4" />}
                    isIconOnly
                    onClick={() => {
                      void refreshJobs().catch((error: unknown) =>
                        setNotice(error instanceof Error ? error.message : t('errors.unavailable')),
                      )
                    }}
                  />
                </div>
                {loading ? (
                  <p className="text-muted-foreground py-4 text-center text-sm sm:py-8">
                    {t('library.loading')}
                  </p>
                ) : jobs.length === 0 ? (
                  <p className="text-muted-foreground py-4 text-center text-sm sm:py-8">
                    {t('library.empty')}
                  </p>
                ) : (
                  <div className="flex max-w-full gap-2 overflow-x-auto pb-1">
                    {jobs.map((job) => (
                      <HistoryCard
                        job={job}
                        key={job.id}
                        selected={selected?.id === job.id}
                        onClick={() => {
                          setSelectedID(job.id)
                          setDeleteArmed(false)
                        }}
                      />
                    ))}
                  </div>
                )}
              </section>
            </section>

            <aside className="bg-card order-2 min-w-0 rounded-lg p-2.5 sm:p-5 xl:order-3">
              <form onSubmit={(event) => void submit(event)}>
                <div className="mb-3 flex items-start justify-between gap-3 sm:mb-5">
                  <div>
                    <p className="text-primary text-xs font-semibold tracking-wide uppercase">
                      {t('direction')}
                    </p>
                    <h2 className="text-base font-semibold sm:text-lg">
                      {t(`workflows.${workflow.key}.label`)}
                    </h2>
                  </div>
                  <span className="bg-muted text-muted-foreground hidden rounded-full px-3 py-1 text-xs sm:inline">
                    Matrix Media
                  </span>
                </div>

                <StudioControls form={form} setForm={setForm} />

                {notice !== '' ? (
                  <p className="bg-muted mt-3 rounded-lg px-3 py-2 text-sm sm:mt-4" role="status">
                    {notice}
                  </p>
                ) : null}

                <Button
                  label={submitting ? t('starting') : actionLabel(form.kind, t)}
                  type="submit"
                  isDisabled={submitting || status?.configured !== true}
                  isLoading={submitting}
                  icon={<Sparkles className="size-4" />}
                  width="100%"
                  size="md"
                  className="sticky bottom-2 z-10 mt-3 sm:mt-4"
                />
                {status?.configured === false ? (
                  <p className="text-muted-foreground mt-3 text-xs leading-relaxed">
                    {t('providerSetup')}
                  </p>
                ) : null}
              </form>

              {selected !== undefined ? (
                <section className="bg-muted/50 mt-3 rounded-lg p-2.5 sm:mt-5 sm:p-4">
                  <div className="mb-3">
                    <h2 className="font-semibold">{t('selectedJob')}</h2>
                    <span className="text-muted-foreground text-xs">
                      {formatDate(selected.created_at)}
                    </span>
                  </div>
                  <dl className="space-y-2 text-sm">
                    <Detail label={t('provider')} value="Matrix Media" />
                    <Detail label={t('seed')} value={String(selected.request.seed)} />
                    <Detail label={t('aspectRatio')} value={selected.request.aspect_ratio ?? '—'} />
                  </dl>
                  <div className="mt-4 grid grid-cols-2 gap-2">
                    <Button label={t('useSettings')} variant="secondary" onClick={reuseSettings} />
                    <Button
                      label={deleteArmed ? t('confirmDelete') : t('delete')}
                      variant="destructive"
                      isDisabled={ACTIVE.has(selected.status) || deleting}
                      isLoading={deleting}
                      onClick={() => void removeSelected()}
                    />
                  </div>
                </section>
              ) : null}
            </aside>
          </div>
        </main>
      }
    />
  )
}

function WorkflowRail({
  active,
  onChoose,
}: {
  active: StudioKind
  onChoose(kind: StudioKind): void
}) {
  const t = useTranslations('mediaStudio')
  const groups = ['createImage', 'createVideo', 'editImage'] as const
  return (
    <aside
      className="bg-card order-1 min-w-0 rounded-lg p-1.5 sm:p-3 xl:p-4"
      aria-label={t('workflowNavigation')}
    >
      <div className="flex max-w-full gap-1.5 overflow-x-auto xl:block xl:space-y-5">
        {groups.map((group) => (
          <section className="contents xl:block" key={group}>
            <h2 className="text-muted-foreground mb-2 hidden px-2 text-xs font-semibold tracking-wide uppercase xl:block">
              {t(`groups.${group}`)}
            </h2>
            <div className="contents xl:flex xl:flex-col xl:gap-1">
              {WORKFLOWS.filter((workflow) => workflow.group === group).map((workflow) => (
                <Button
                  key={workflow.kind}
                  label={t(`workflows.${workflow.key}.label`)}
                  variant={active === workflow.kind ? 'primary' : 'secondary'}
                  icon={<workflow.icon className="size-4 shrink-0" />}
                  size="sm"
                  onClick={() => onChoose(workflow.kind)}
                  className="min-w-[7.25rem] shrink-0 justify-start xl:w-full xl:min-w-0"
                  tooltip={t(`workflows.${workflow.key}.detail`)}
                />
              ))}
            </div>
          </section>
        ))}
      </div>
    </aside>
  )
}

function StudioControls({
  form,
  setForm,
}: {
  form: StudioRequest
  setForm: (value: StudioRequest | ((current: StudioRequest) => StudioRequest)) => void
}) {
  const t = useTranslations('mediaStudio')
  const needsImage = form.kind !== 'text-to-image' && form.kind !== 'text-to-video'
  const needsMask = form.kind === 'cleanup' || form.kind === 'inpainting'
  const needsFace = form.kind === 'merge-face'
  const showPrompt = [
    'text-to-image',
    'image-to-image',
    'text-to-video',
    'inpainting',
    'replace-background',
  ].includes(form.kind)
  const generated = ['text-to-image', 'image-to-image', 'text-to-video', 'inpainting'].includes(
    form.kind,
  )

  return (
    <div className="space-y-2.5 sm:space-y-4">
      {needsImage ? (
        <UploadField
          label={t('fields.sourceImage')}
          value={form.image}
          onUploaded={(media) => setForm((current) => ({ ...current, image: media.url }))}
        />
      ) : null}
      {needsMask ? (
        <UploadField
          label={t('fields.mask')}
          value={form.mask}
          onUploaded={(media) => setForm((current) => ({ ...current, mask: media.url }))}
        />
      ) : null}
      {needsFace ? (
        <UploadField
          label={t('fields.faceReference')}
          value={form.face}
          onUploaded={(media) => setForm((current) => ({ ...current, face: media.url }))}
        />
      ) : null}

      {showPrompt ? (
        <TextArea
          label={
            form.kind === 'replace-background' ? t('fields.newBackground') : t('fields.prompt')
          }
          rows={3}
          maxLength={4000}
          isRequired
          value={form.prompt ?? ''}
          placeholder={t(`placeholders.${placeholderKey(form.kind)}`)}
          onChange={(value) => setForm((current) => ({ ...current, prompt: value }))}
          width="100%"
        />
      ) : null}

      {generated ? (
        <TextArea
          label={t('fields.negativePrompt')}
          isOptional
          rows={2}
          maxLength={2000}
          value={form.negative_prompt ?? ''}
          placeholder={t('placeholders.negative')}
          onChange={(value) => setForm((current) => ({ ...current, negative_prompt: value }))}
          width="100%"
        />
      ) : null}

      {(form.kind.includes('image') || form.kind === 'inpainting') &&
      form.kind !== 'image-to-video' ? (
        <Selector
          label={t('fields.aspectRatio')}
          options={['1:1', '16:9', '9:16', '4:3', '3:2']}
          value={form.aspect_ratio}
          onChange={(value) => setForm((current) => ({ ...current, aspect_ratio: value }))}
          width="100%"
        />
      ) : null}

      {form.kind === 'text-to-video' ? (
        <Selector
          label={t('fields.aspectRatio')}
          options={['16:9', '9:16', '1:1']}
          value={form.aspect_ratio}
          onChange={(value) => setForm((current) => ({ ...current, aspect_ratio: value }))}
          width="100%"
        />
      ) : null}

      {form.kind === 'image-to-image' || form.kind === 'inpainting' ? (
        <RangeField
          label={t('fields.strength')}
          min={0.05}
          max={1}
          step={0.05}
          value={form.strength ?? 0.7}
          onChange={(value) => setForm((current) => ({ ...current, strength: value }))}
        />
      ) : null}

      {form.kind === 'text-to-video' || form.kind === 'image-to-video' ? (
        <div className="grid grid-cols-2 gap-2 sm:gap-3">
          <NumberField
            label={t('fields.frames')}
            min={14}
            max={form.kind === 'image-to-video' ? 50 : 128}
            value={form.frames ?? 25}
            onChange={(value) => setForm((current) => ({ ...current, frames: value }))}
          />
          <NumberField
            label={t('fields.fps')}
            min={1}
            max={30}
            value={form.fps ?? 6}
            onChange={(value) => setForm((current) => ({ ...current, fps: value }))}
          />
        </div>
      ) : null}

      {form.kind === 'upscale' ? (
        <RangeField
          label={t('fields.scale')}
          min={1.1}
          max={4}
          step={0.1}
          value={form.scale ?? 2}
          onChange={(value) => setForm((current) => ({ ...current, scale: value }))}
        />
      ) : null}

      {[
        'text-to-image',
        'image-to-image',
        'text-to-video',
        'image-to-video',
        'inpainting',
      ].includes(form.kind) ? (
        <NumberField
          label={t('fields.seed')}
          min={-1}
          max={2147483647}
          value={form.seed}
          onChange={(value) => setForm((current) => ({ ...current, seed: value }))}
        />
      ) : null}
    </div>
  )
}

function UploadField({
  label,
  value,
  onUploaded,
}: {
  label: string
  value?: string
  onUploaded(media: UploadedMedia): void
}) {
  const t = useTranslations('mediaStudio')
  const [file, setFile] = useState<File | null>(null)
  const [uploading, setUploading] = useState(false)
  const [error, setError] = useState('')
  const choose = async (files: File | File[] | null) => {
    const next = Array.isArray(files) ? files[0] : files
    setFile(next ?? null)
    if (next === null || next === undefined) return
    setUploading(true)
    setError('')
    try {
      const media = await uploadMedia(next, next.name)
      onUploaded(media)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t('errors.upload'))
    } finally {
      setUploading(false)
    }
  }
  return (
    <FileInput
      label={label}
      value={file}
      onChange={(files) => void choose(files)}
      accept="image/png,image/jpeg,image/webp"
      mode="dropzone"
      isLoading={uploading}
      placeholder={value?.split('/').pop() ?? t('upload.guidance')}
      description={value === undefined ? t('upload.guidance') : value.split('/').pop()}
      status={error === '' ? undefined : { type: 'error', message: error }}
      width="100%"
    />
  )
}

function OutputCanvas({ job }: { job?: StudioJob }) {
  const t = useTranslations('mediaStudio')
  if (job === undefined) {
    return (
      <div className="bg-muted/50 grid min-h-[13rem] place-items-center rounded-lg p-4 text-center sm:min-h-[26rem] sm:p-8 lg:min-h-[58vh]">
        <div className="max-w-sm">
          <span className="bg-background mx-auto mb-3 grid size-10 place-items-center rounded-lg sm:mb-4 sm:size-14 sm:rounded-2xl">
            <Sparkles className="text-primary size-5 sm:size-6" />
          </span>
          <strong className="text-base sm:text-lg">{t('canvas.emptyTitle')}</strong>
          <p className="text-muted-foreground mt-1.5 text-xs leading-5 sm:mt-2 sm:text-sm sm:leading-relaxed">
            {t('canvas.emptyBody')}
          </p>
        </div>
      </div>
    )
  }
  if (job.status === 'failed') {
    return (
      <div className="bg-destructive/8 grid min-h-[13rem] place-items-center rounded-lg p-4 text-center sm:min-h-[26rem] sm:p-8 lg:min-h-[58vh]">
        <div className="max-w-md">
          <strong className="text-lg">{t('canvas.failedTitle')}</strong>
          <p className="text-muted-foreground mt-2 text-sm">
            {job.error ?? t('canvas.failedBody')}
          </p>
        </div>
      </div>
    )
  }
  if (job.assets.length === 0) {
    return (
      <div className="bg-muted/50 grid min-h-[13rem] place-items-center rounded-lg p-4 text-center sm:min-h-[26rem] sm:p-8 lg:min-h-[58vh]">
        <div>
          <span className="bg-primary/15 mx-auto mb-3 grid size-10 animate-pulse place-items-center rounded-full sm:mb-4 sm:size-14">
            <Sparkles className="text-primary size-5 sm:size-6" />
          </span>
          <strong>{job.status === 'queued' ? t('status.queued') : t('status.generating')}</strong>
          <p className="text-muted-foreground mt-2 text-sm">{t('canvas.workingBody')}</p>
        </div>
      </div>
    )
  }
  return (
    <div
      className={cn(
        'grid min-h-[13rem] overflow-hidden rounded-lg bg-black/95 sm:min-h-[26rem] lg:min-h-[58vh]',
        job.assets.length > 1 && 'sm:grid-cols-2',
      )}
    >
      {job.assets.map((asset) => (
        <AssetView asset={asset} prompt={job.request.prompt} key={asset.id} />
      ))}
    </div>
  )
}

function AssetView({ asset, prompt }: { asset: StudioAsset; prompt?: string }) {
  const t = useTranslations('mediaStudio')
  const [url, setURL] = useState<string>()
  useEffect(() => {
    let live = true
    let objectURL: string | null = null
    void loadMediaObjectURL(asset.url).then((loaded) => {
      objectURL = loaded
      if (live && loaded !== null) setURL(loaded)
    })
    return () => {
      live = false
      if (objectURL !== null) URL.revokeObjectURL(objectURL)
    }
  }, [asset.url])
  return (
    <figure className="relative grid min-h-[13rem] place-items-center overflow-hidden sm:min-h-[26rem] lg:min-h-[58vh]">
      {url === undefined ? (
        <span className="text-sm text-white/60">{t('canvas.loadingAsset')}</span>
      ) : asset.media_type === 'video' ? (
        <video
          controls
          playsInline
          preload="metadata"
          src={url}
          className="max-h-[72vh] w-full object-contain"
        />
      ) : (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={url}
          alt={
            prompt === undefined
              ? t('canvas.generatedAlt')
              : t('canvas.generatedPromptAlt', { prompt })
          }
          className="max-h-[72vh] w-full object-contain"
        />
      )}
      <Button
        label={t('download')}
        variant="secondary"
        size="sm"
        icon={<Download className="size-4" />}
        onClick={() => void downloadMediaRef(asset.url, asset.name)}
        className="absolute right-2 bottom-2 sm:right-3 sm:bottom-3"
      />
    </figure>
  )
}

function HistoryCard({
  job,
  selected,
  onClick,
}: {
  job: StudioJob
  selected: boolean
  onClick(): void
}) {
  const t = useTranslations('mediaStudio')
  const workflow = WORKFLOWS.find((item) => item.kind === job.kind) ?? WORKFLOWS[0]
  return (
    <ClickableCard
      label={t(`workflows.${workflow.key}.label`)}
      aria-current={selected ? 'true' : undefined}
      onClick={onClick}
      variant={selected ? 'muted' : 'transparent'}
      padding={2}
      elevation={selected ? 'low' : 'none'}
      width={128}
      className="shrink-0"
    >
      <span className="bg-muted grid aspect-[4/3] place-items-center rounded-xl">
        {job.status === 'succeeded' ? (
          job.assets[0]?.media_type === 'video' ? (
            <Play className="size-5" />
          ) : (
            <ImageIcon className="size-5" />
          )
        ) : job.status === 'failed' ? (
          <Trash2Icon className="text-destructive size-5" />
        ) : (
          <span className="text-primary text-xs font-semibold">{job.progress}%</span>
        )}
      </span>
      <strong className="mt-2 block truncate text-xs">
        {t(`workflows.${workflow.key}.label`)}
      </strong>
      <small className="text-muted-foreground block text-[11px]">
        {relativeTime(job.updated_at)}
      </small>
    </ClickableCard>
  )
}

function JobStatus({ job }: { job: StudioJob }) {
  const t = useTranslations('mediaStudio')
  const label =
    job.status === 'queued'
      ? t('status.queued')
      : job.status === 'running'
        ? t('status.generating')
        : job.status === 'succeeded'
          ? t('status.ready')
          : t('status.failed')
  return (
    <Badge
      variant={job.status === 'succeeded' ? 'success' : job.status === 'failed' ? 'error' : 'info'}
      label={label}
    />
  )
}

function NumberField({
  label,
  value,
  min,
  max,
  onChange,
}: {
  label: string
  value: number
  min: number
  max: number
  onChange(value: number): void
}) {
  return (
    <NumberInput label={label} value={value} min={min} max={max} onChange={onChange} width="100%" />
  )
}

function RangeField({
  label,
  value,
  min,
  max,
  step,
  onChange,
}: {
  label: string
  value: number
  min: number
  max: number
  step: number
  onChange(value: number): void
}) {
  return (
    <Slider
      label={label}
      value={value}
      min={min}
      max={max}
      step={step}
      onChange={onChange}
      valueDisplay="text"
      formatValue={(next) => next.toFixed(1)}
      width="100%"
    />
  )
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="max-w-40 truncate font-mono text-xs">{value}</dd>
    </div>
  )
}

function defaultRequest(kind: StudioKind): StudioRequest {
  const video = kind === 'text-to-video' || kind === 'image-to-video'
  return {
    kind,
    prompt: '',
    negative_prompt: '',
    aspect_ratio: video ? '16:9' : '1:1',
    seed: -1,
    strength: 0.7,
    frames: kind === 'image-to-video' ? 25 : 64,
    fps: 6,
    scale: 2,
  }
}

function placeholderKey(kind: StudioKind): string {
  switch (kind) {
    case 'replace-background':
      return 'background'
    case 'inpainting':
      return 'inpainting'
    case 'image-to-image':
      return 'transform'
    case 'text-to-video':
      return 'video'
    default:
      return 'image'
  }
}

function actionLabel(
  kind: StudioKind,
  t: ReturnType<typeof useTranslations<'mediaStudio'>>,
): string {
  if (kind === 'text-to-video' || kind === 'image-to-video') return t('actions.generateVideo')
  if (kind === 'text-to-image' || kind === 'image-to-image') return t('actions.generateImage')
  return t('actions.applyEdit')
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )
}

function relativeTime(value: string): string {
  const seconds = Math.round((new Date(value).getTime() - Date.now()) / 1000)
  const absolute = Math.abs(seconds)
  const formatter = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
  if (absolute < 60) return formatter.format(seconds, 'second')
  if (absolute < 3600) return formatter.format(Math.round(seconds / 60), 'minute')
  if (absolute < 86400) return formatter.format(Math.round(seconds / 3600), 'hour')
  return formatter.format(Math.round(seconds / 86400), 'day')
}
