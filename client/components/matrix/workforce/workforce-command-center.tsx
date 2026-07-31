'use client'

import Link from 'next/link'
import { lazy, Suspense, useState } from 'react'
import { useTranslations } from 'next-intl'
import {
  Activity,
  ArrowLeft,
  ArrowRight,
  BadgeCheck,
  BriefcaseBusiness,
  Building2,
  CalendarClock,
  CircleAlert,
  CircleStop,
  Gauge,
  RefreshCw,
  ShieldCheck,
  Users,
  Workflow,
  X,
  Zap,
} from 'lucide-react'
import { useWorkforceCommandCenter } from '@/hooks/api/useWorkforce'
import type {
  WorkforceResourceItem,
  WorkforceSession,
  WorkforceWorkOrder,
} from '@/lib/api/workforce'
import type { WorkforceDepartmentKind } from '@/lib/api/workforce'
import type {
  WorkforceCommandDraft,
  WorkforceWorkOrderDraft,
} from '@/lib/workforce/control-signing'
import { prepareWorkforceWorkOrder } from '@/lib/workforce/control-signing'

const WorkforceRecordSurfaces = lazy(async () => {
  const surfaces = await import('./workforce-record-surfaces')
  return { default: surfaces.WorkforceRecordSurfaces }
})

const departmentDefinitions = [
  { key: 'developer', aliases: ['developer'] },
  { key: 'executive', aliases: ['executive'] },
  { key: 'research', aliases: ['research', 'development'] },
  { key: 'marketing', aliases: ['marketing', 'social'] },
  { key: 'legal', aliases: ['legal'] },
  { key: 'accounting', aliases: ['accounting'] },
  { key: 'backOffice', aliases: ['back-office', 'back_office', 'backoffice'] },
] as const

const seatRoles = ['lead', 'executor', 'auditor'] as const

export function WorkforceCommandCenter() {
  const t = useTranslations('workforce')
  const workforce = useWorkforceCommandCenter()
  const [preview, setPreview] = useState<WorkforceCommandDraft | null>(null)
  const departments = workforce.items('departments')
  const seats = workforce.items('seats')
  const workOrders = workforce.items('work-orders')
  const schedules = workforce.items('schedules')
  const incidents = workforce.items('incidents')
  const receipts = workforce.items('receipts')

  const activeWork = workOrders.filter((item) => !isTerminal(String(item.fields.state ?? '')))
  const verifiedCompletions = receipts.filter(
    (item) => String(item.fields.disposition ?? '') === 'goal_completed',
  )
  const nextWakes = [...schedules]
    .filter((item) => String(item.fields.state ?? '') === 'queued')
    .sort((left, right) =>
      String(left.fields.scheduled_at ?? '').localeCompare(String(right.fields.scheduled_at ?? '')),
    )
    .slice(0, 5)
  const spendCeiling = schedules.reduce(
    (total, item) => total + numberField(item, 'budget_spend_microunits'),
    0,
  )
  const autonomyVersion = workforce.controlVersion('policy', 'policy:global-autonomy')
  const firstSeat = seats[0]
  const cancellable = activeWork[0]

  const metrics = [
    {
      label: t('departments'),
      value: `${departments.length}/7`,
      detail: departments.length === 7 ? t('reported') : t('partialState'),
      icon: Workflow,
      progress: Math.min(100, (departments.length / 7) * 100),
    },
    {
      label: t('seats'),
      value: `${seats.length}/21`,
      detail: seats.length === 21 ? t('reported') : t('partialState'),
      icon: Users,
      progress: Math.min(100, (seats.length / 21) * 100),
    },
    {
      label: t('activeWork'),
      value: String(activeWork.length),
      detail: t('fromBackend'),
      icon: Activity,
      progress: activeWork.length > 0 ? 100 : 0,
    },
    {
      label: t('verifiedCompletion'),
      value: String(verifiedCompletions.length),
      detail: t('receiptBacked'),
      icon: BadgeCheck,
      progress: verifiedCompletions.length > 0 ? 100 : 0,
    },
  ]

  const applyPreview = async () => {
    if (!preview) return
    try {
      await workforce.command.mutateAsync(preview)
      setPreview(null)
    } catch {
      // The mutation exposes its typed error in the review panel.
    }
  }

  return (
    <main id="main-content" className="bg-muted/30 text-foreground min-h-dvh">
      <header className="bg-background/95 text-foreground sticky top-0 z-20 backdrop-blur-xl">
        <div className="mx-auto flex max-w-[1480px] items-center justify-between gap-4 px-4 py-3 sm:px-6 lg:px-8">
          <div className="flex min-w-0 items-center gap-3">
            <Link
              href="/"
              prefetch={false}
              aria-label={t('backHome')}
              className="bg-muted text-foreground hover:bg-accent grid size-9 shrink-0 place-items-center rounded-lg transition-colors"
            >
              <ArrowLeft className="size-4" />
            </Link>
            <span className="bg-foreground text-background grid size-9 shrink-0 place-items-center rounded-lg">
              <Workflow className="size-4" />
            </span>
            <div className="min-w-0">
              <p className="text-muted-foreground font-mono text-[10px] font-medium tracking-[0.14em] uppercase">
                Matrix · {t('ownerControls')}
              </p>
              <h1 className="truncate text-base font-semibold tracking-tight sm:text-lg">
                {t('title')}
              </h1>
            </div>
          </div>
          <LiveStatus state={workforce.streamState} />
        </div>
      </header>

      <div className="mx-auto max-w-[1480px] space-y-4 px-4 py-4 sm:px-6 sm:py-6 lg:px-8">
        {departments.length > 0 && workforce.error ? (
          <section
            role="status"
            className="bg-danger-100 text-danger-900 rounded-lg px-4 py-3 text-sm"
          >
            <p className="font-medium">{t('backendUnavailable')}</p>
            <p className="mt-1 opacity-80">{workforce.error}</p>
          </section>
        ) : departments.length > 0 && workforce.partial ? (
          <section role="status" className="bg-warning-100 text-warning-950 rounded-lg px-4 py-3">
            {t('partialNotice')}
          </section>
        ) : null}

        {departments.length === 0 ? (
          <WorkforceSetup
            disabled={workforce.loading || workforce.session === null}
            pending={workforce.activation.isPending}
            connectionState={
              workforce.loading || workforce.session === null
                ? 'loading'
                : workforce.error
                  ? 'unavailable'
                  : workforce.partial
                    ? 'partial'
                    : 'ready'
            }
            error={
              workforce.activation.error instanceof Error
                ? workforce.activation.error.message
                : null
            }
            onActivate={(name) => workforce.activation.mutateAsync(name)}
          />
        ) : (
          <>
            <section
              aria-labelledby="summary-heading"
              className="bg-card overflow-hidden rounded-xl p-5 sm:p-6"
            >
              <div className="flex flex-col justify-between gap-5 lg:flex-row lg:items-end">
                <div className="max-w-3xl">
                  <p className="text-muted-foreground flex items-center gap-2 font-mono text-[10px] font-medium tracking-[0.13em] uppercase">
                    <span className="bg-success-700 size-1.5 rounded-sm" />
                    {t('fromBackend')}
                  </p>
                  <h2
                    id="summary-heading"
                    className="mt-2 text-2xl font-semibold tracking-[-0.025em] text-balance sm:text-[2rem]"
                  >
                    {t('operatingPicture')}
                  </h2>
                  <p className="text-muted-foreground mt-2 max-w-2xl text-sm leading-6">
                    {t('operatingPictureDetail')}
                  </p>
                </div>
                <div className="bg-background flex min-w-0 items-center gap-3 rounded-lg px-4 py-3">
                  <span className="bg-muted text-foreground grid size-8 shrink-0 place-items-center rounded-md">
                    {workforce.loading ? (
                      <RefreshCw aria-label={t('loading')} className="size-4 animate-spin" />
                    ) : (
                      <ShieldCheck className="size-4" />
                    )}
                  </span>
                  <div className="min-w-0">
                    <p className="text-muted-foreground text-[11px] font-medium tracking-wide uppercase">
                      {t('organization')}
                    </p>
                    <p className="mt-0.5 truncate font-mono text-xs">
                      {workforce.session?.organization_id ?? t('notReported')}
                    </p>
                  </div>
                </div>
              </div>
              <div className="mt-7 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                {metrics.map((metric) => (
                  <article key={metric.label} className="bg-background rounded-lg p-4">
                    <div className="flex items-start justify-between gap-4">
                      <div className="min-w-0">
                        <p className="text-muted-foreground truncate text-xs font-medium">
                          {metric.label}
                        </p>
                        <p className="mt-1.5 text-2xl font-semibold tracking-[-0.03em] tabular-nums">
                          {metric.value}
                        </p>
                      </div>
                      <span className="bg-muted text-muted-foreground grid size-8 shrink-0 place-items-center rounded-md">
                        <metric.icon className="size-4" />
                      </span>
                    </div>
                    <div className="bg-muted mt-4 h-0.5 overflow-hidden rounded-sm">
                      <div
                        className="bg-foreground h-full rounded-sm transition-[width]"
                        style={{ width: `${metric.progress}%` }}
                      />
                    </div>
                    <p className="text-muted-foreground mt-2 text-[11px]">{metric.detail}</p>
                  </article>
                ))}
              </div>
            </section>

            <div className="grid items-start gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
              <section
                aria-labelledby="departments-heading"
                className="bg-card rounded-xl p-4 sm:p-5"
              >
                <div className="mb-5 flex flex-col justify-between gap-3 sm:flex-row sm:items-end">
                  <div>
                    <p className="text-muted-foreground text-xs font-semibold tracking-[0.15em] uppercase">
                      {t('organization')}
                    </p>
                    <h2
                      id="departments-heading"
                      className="mt-2 text-xl font-semibold tracking-tight"
                    >
                      {t('organization')}
                    </h2>
                    <p className="text-muted-foreground mt-1 text-sm">{t('organizationDetail')}</p>
                  </div>
                  <span className="bg-success-100 text-success-900 w-fit rounded-md px-2.5 py-1 text-xs font-medium tabular-nums">
                    {departments.length}/7
                  </span>
                </div>
                <div className="grid gap-3 md:grid-cols-2">
                  {departmentDefinitions.map((definition) => {
                    const department = findDepartment(departments, definition.aliases)
                    return (
                      <article key={definition.key} className="bg-muted/70 rounded-lg p-4">
                        <div className="flex items-start justify-between gap-3">
                          <div>
                            <h3 className="font-medium">{t(`department.${definition.key}`)}</h3>
                            <p className="text-muted-foreground mt-1 font-mono text-xs">
                              {department?.id ?? t('notReported')}
                            </p>
                          </div>
                          <div className="flex items-center gap-2">
                            <span className="text-muted-foreground text-[10px] font-medium uppercase">
                              {department ? t('reported') : t('notReported')}
                            </span>
                            <StateMark active={Boolean(department)} />
                          </div>
                        </div>
                        <div className="mt-4 grid grid-cols-3 gap-2">
                          {seatRoles.map((role) => {
                            const seat = findSeat(seats, department?.id, definition.aliases, role)
                            return (
                              <div
                                key={role}
                                className="bg-background min-w-0 rounded-md px-2.5 py-2.5"
                                title={seat?.id ?? t('notReported')}
                              >
                                <p className="truncate text-xs font-medium">{t(`role.${role}`)}</p>
                                <p className="text-muted-foreground mt-1 truncate text-[11px]">
                                  {seat
                                    ? seatState(seat, t('active'), t('inactive'))
                                    : t('notReported')}
                                </p>
                              </div>
                            )
                          })}
                        </div>
                      </article>
                    )
                  })}
                </div>
              </section>

              <aside
                className="bg-foreground text-background space-y-2 rounded-xl p-3 sm:p-4 xl:sticky xl:top-20"
                aria-labelledby="controls-heading"
              >
                <div className="px-1 pb-2">
                  <p className="text-background/55 font-mono text-[10px] font-medium tracking-[0.13em] uppercase">
                    {t('signedChange')}
                  </p>
                  <h2 id="controls-heading" className="mt-2 text-xl font-semibold tracking-tight">
                    {t('ownerControls')}
                  </h2>
                  <p className="text-background/60 mt-1 text-sm leading-5">
                    {t('ownerControlsDetail')}
                  </p>
                </div>
                <section className="bg-background/10 rounded-lg p-4">
                  <div className="flex items-center gap-3">
                    <span className="bg-background/10 grid size-9 place-items-center rounded-md">
                      <ShieldCheck className="size-4" />
                    </span>
                    <div>
                      <h3 className="font-medium">{t('autonomy')}</h3>
                      <p className="text-background/60 text-xs">
                        {autonomyVersion > 0
                          ? t('configuredVersion', { version: autonomyVersion })
                          : t('notConfigured')}
                      </p>
                    </div>
                  </div>
                  <button
                    type="button"
                    disabled={workforce.loading || workforce.partial}
                    className="bg-background text-foreground hover:bg-background/90 mt-4 w-full rounded-md px-3 py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45"
                    onClick={() =>
                      setPreview({
                        action: 'set_autonomy',
                        resourceKind: 'policy',
                        resourceId: 'policy:global-autonomy',
                        expectedVersion: autonomyVersion,
                        change: { autonomy: 'bounded', effect_authority: 'approval_required' },
                      })
                    }
                  >
                    {t('reviewAutonomy')}
                  </button>
                </section>

                <section className="bg-background/10 rounded-lg p-4">
                  <div className="flex items-center gap-3">
                    <span className="bg-background/10 grid size-9 place-items-center rounded-md">
                      <Zap className="size-4" />
                    </span>
                    <div className="min-w-0">
                      <h3 className="font-medium">{t('forceWake')}</h3>
                      <p className="text-background/60 truncate text-xs">
                        {firstSeat?.id ?? t('noSeatAvailable')}
                      </p>
                    </div>
                  </div>
                  <button
                    type="button"
                    disabled={!firstSeat}
                    className="bg-background/10 text-background hover:bg-background/15 mt-4 w-full rounded-md px-3 py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45"
                    onClick={() =>
                      firstSeat &&
                      setPreview({
                        action: 'force_wake',
                        resourceKind: 'seat',
                        resourceId: firstSeat.id,
                        expectedVersion: workforce.controlVersion('seat', firstSeat.id),
                        change: { reason: 'owner_requested', priority: 'normal' },
                      })
                    }
                  >
                    {t('reviewForceWake')}
                  </button>
                </section>

                <section className="bg-background/10 rounded-lg p-4">
                  <div className="flex items-center gap-3">
                    <span className="bg-danger-100 text-danger-800 grid size-9 place-items-center rounded-md">
                      <CircleStop className="size-4" />
                    </span>
                    <div className="min-w-0">
                      <h3 className="font-medium">{t('cancelWork')}</h3>
                      <p className="text-background/60 truncate text-xs">
                        {cancellable?.id ?? t('noCancellableWork')}
                      </p>
                    </div>
                  </div>
                  <button
                    type="button"
                    disabled={!cancellable}
                    className="bg-danger-100 text-danger-900 hover:bg-danger-200 mt-4 w-full rounded-md px-3 py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45"
                    onClick={() =>
                      cancellable &&
                      setPreview({
                        action: 'cancel_work',
                        resourceKind: 'work_order',
                        resourceId: cancellable.id,
                        expectedVersion: workforce.controlVersion('work_order', cancellable.id),
                        change: { reason: 'owner_cancelled', terminal: false },
                      })
                    }
                  >
                    {t('reviewCancellation')}
                  </button>
                </section>
              </aside>
            </div>

            <WorkOrderComposer
              pending={workforce.workOrder.isPending}
              session={workforce.session}
              modelProvider={workforce.session?.model_provider ?? ''}
              modelId={workforce.session?.model_id ?? ''}
              error={
                workforce.workOrder.error instanceof Error
                  ? workforce.workOrder.error.message
                  : null
              }
              onCreate={(draft) => workforce.workOrder.mutateAsync(draft)}
            />

            <div className="grid items-start gap-4 lg:grid-cols-2">
              <WorkOrders items={workOrders} />
              <Operations nextWakes={nextWakes} incidents={incidents} spendCeiling={spendCeiling} />
            </div>

            <Suspense fallback={null}>
              <WorkforceRecordSurfaces
                items={workforce.items}
                controlVersion={workforce.controlVersion}
                onPreview={setPreview}
              />
            </Suspense>
          </>
        )}
      </div>

      {preview ? (
        <CommandReview
          draft={preview}
          pending={workforce.command.isPending}
          error={workforce.command.error instanceof Error ? workforce.command.error.message : null}
          onClose={() => setPreview(null)}
          onApply={applyPreview}
        />
      ) : null}
    </main>
  )
}

function WorkforceSetup({
  disabled,
  pending,
  connectionState,
  error,
  onActivate,
}: {
  disabled: boolean
  pending: boolean
  connectionState: 'loading' | 'unavailable' | 'partial' | 'ready'
  error: string | null
  onActivate: (name: string) => Promise<unknown>
}) {
  const t = useTranslations('workforce')
  const [name, setName] = useState('')

  return (
    <section className="bg-card overflow-hidden rounded-xl">
      <div className="grid lg:grid-cols-[minmax(0,1.2fr)_minmax(340px,0.8fr)]">
        <div className="p-6 sm:p-10 lg:p-12">
          <span className="bg-foreground text-background grid size-10 place-items-center rounded-lg">
            <Building2 className="size-5" />
          </span>
          <p className="text-muted-foreground mt-8 text-xs font-semibold tracking-[0.16em] uppercase">
            {t('setup.eyebrow')}
          </p>
          <h2 className="mt-3 max-w-xl text-3xl font-semibold tracking-tight text-balance sm:text-4xl">
            {t('setup.title')}
          </h2>
          <p className="text-muted-foreground mt-4 max-w-2xl text-sm leading-6 sm:text-base">
            {t('setup.detail')}
          </p>
          <div className="mt-8 grid gap-3 sm:grid-cols-3">
            {(['organization', 'authority', 'firstWork'] as const).map((step, index) => (
              <div key={step} className="bg-muted rounded-lg p-4">
                <span className="text-muted-foreground text-xs font-semibold tabular-nums">
                  {String(index + 1).padStart(2, '0')}
                </span>
                <p className="mt-3 text-sm font-medium">{t(`setup.step.${step}`)}</p>
              </div>
            ))}
          </div>
        </div>
        <form
          className="bg-muted m-3 flex flex-col justify-between rounded-lg p-5 sm:m-4 sm:p-6"
          onSubmit={(event) => {
            event.preventDefault()
            void onActivate(name)
          }}
        >
          <div>
            <p className="text-xs font-semibold tracking-[0.15em] uppercase">
              {t('setup.activate')}
            </p>
            <div
              role="status"
              className="bg-background text-muted-foreground mt-4 flex min-h-16 items-center rounded-lg px-4 py-3 text-sm"
            >
              {connectionState === 'loading'
                ? t('loading')
                : connectionState === 'unavailable'
                  ? t('backendUnavailable')
                  : connectionState === 'partial'
                    ? t('partialNotice')
                    : t('reported')}
            </div>
            <label className="mt-6 block text-sm font-medium" htmlFor="workforce-name">
              {t('setup.name')}
            </label>
            <input
              id="workforce-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder={t('setup.namePlaceholder')}
              maxLength={160}
              required
              className="bg-background focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 text-sm outline-none focus-visible:ring-2"
            />
            <div className="bg-background mt-4 rounded-lg p-4">
              <p className="text-sm font-medium">{t('setup.localSigning')}</p>
              <p className="text-muted-foreground mt-1 text-xs leading-5">
                {t('setup.localSigningDetail')}
              </p>
            </div>
            {error ? (
              <p
                role="alert"
                className="bg-danger-100 text-danger-900 mt-4 rounded-xl px-3 py-2 text-sm"
              >
                {error}
              </p>
            ) : null}
          </div>
          <button
            type="submit"
            disabled={disabled || pending || !name.trim()}
            className="bg-foreground text-background hover:bg-foreground/90 mt-8 flex w-full items-center justify-center gap-2 rounded-lg px-4 py-3 text-sm font-semibold transition-colors disabled:opacity-50"
          >
            {pending ? t('setup.activating') : t('setup.activateButton')}
            {!pending ? <ArrowRight className="size-4" /> : null}
          </button>
        </form>
      </div>
    </section>
  )
}

const workOrderDepartments: Array<{ value: WorkforceDepartmentKind; label: string }> = [
  { value: 'developer', label: 'developer' },
  { value: 'executive', label: 'executive' },
  { value: 'research_and_development', label: 'research' },
  { value: 'marketing_and_social', label: 'marketing' },
  { value: 'legal', label: 'legal' },
  { value: 'accounting', label: 'accounting' },
  { value: 'back_office', label: 'backOffice' },
]

function WorkOrderComposer({
  pending,
  session,
  modelProvider,
  modelId,
  error,
  onCreate,
}: {
  pending: boolean
  session: WorkforceSession | null
  modelProvider: string
  modelId: string
  error: string | null
  onCreate: (order: WorkforceWorkOrder) => Promise<unknown>
}) {
  const t = useTranslations('workforce')
  const [open, setOpen] = useState(false)
  const [objective, setObjective] = useState('')
  const [scope, setScope] = useState('/root/matrix')
  const [projectId, setProjectId] = useState('')
  const [workspaceId, setWorkspaceId] = useState('')
  const [scopeFiles, setScopeFiles] = useState('')
  const [scopeSymbols, setScopeSymbols] = useState('')
  const [criteria, setCriteria] = useState('')
  const [departments, setDepartments] = useState<WorkforceDepartmentKind[]>(['developer'])
  const [mgsReference, setMgsReference] = useState('')
  const [mgsDigest, setMgsDigest] = useState('')
  const [prepared, setPrepared] = useState<WorkforceWorkOrder | null>(null)
  const [prepareError, setPrepareError] = useState<string | null>(null)

  if (!open) {
    return (
      <section className="bg-card flex flex-col justify-between gap-5 rounded-xl p-5 sm:flex-row sm:items-center sm:p-6">
        <div>
          <p className="text-muted-foreground text-xs font-semibold tracking-[0.15em] uppercase">
            {t('createWorkOrder.eyebrow')}
          </p>
          <h2 className="mt-2 text-xl font-semibold tracking-tight">
            {t('createWorkOrder.title')}
          </h2>
          <p className="text-muted-foreground mt-1 text-sm">{t('createWorkOrder.detail')}</p>
        </div>
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="bg-foreground text-background hover:bg-foreground/90 flex shrink-0 items-center justify-center gap-2 rounded-lg px-5 py-3 text-sm font-semibold"
        >
          <BriefcaseBusiness className="size-4" />
          {t('createWorkOrder.open')}
        </button>
      </section>
    )
  }

  return (
    <form
      className="bg-card rounded-xl p-5 sm:p-6"
      onSubmit={async (event) => {
        event.preventDefault()
        if (!session) return
        const includesDeveloper = departments.includes('developer')
        const draft: WorkforceWorkOrderDraft = {
          objective,
          scope,
          projectId: includesDeveloper ? projectId : '',
          workspaceId: includesDeveloper ? workspaceId : '',
          scopeFiles: includesDeveloper
            ? scopeFiles
                .split('\n')
                .map((value) => value.trim())
                .filter(Boolean)
            : [],
          scopeSymbols: includesDeveloper
            ? scopeSymbols
                .split('\n')
                .map((value) => value.trim())
                .filter(Boolean)
            : [],
          departments,
          priority: 10,
          maxTasks: 10,
          maxSpendMicrounits: 0,
          deadline: new Date(Date.now() + 7 * 24 * 60 * 60 * 1000),
          autonomy: 'supervised',
          acceptanceCriteria: criteria
            .split('\n')
            .map((value) => value.trim())
            .filter(Boolean),
          modelProvider,
          modelId,
          mgsReference,
          mgsDigest,
        }
        try {
          setPrepareError(null)
          setPrepared(await prepareWorkforceWorkOrder(session, draft))
        } catch (cause) {
          setPrepareError(cause instanceof Error ? cause.message : String(cause))
        }
      }}
    >
      <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
        <div>
          <p className="text-muted-foreground text-xs font-semibold tracking-[0.15em] uppercase">
            {t('createWorkOrder.eyebrow')}
          </p>
          <h2 className="mt-2 text-2xl font-semibold tracking-tight">
            {t('createWorkOrder.formTitle')}
          </h2>
        </div>
        <button
          type="button"
          onClick={() => setOpen(false)}
          className="bg-muted hover:bg-accent rounded-xl px-4 py-2 text-sm font-medium"
        >
          {t('close')}
        </button>
      </div>
      <div className="mt-7 grid gap-5 lg:grid-cols-2">
        <label className="block text-sm font-medium">
          {t('createWorkOrder.objective')}
          <textarea
            value={objective}
            onChange={(event) => setObjective(event.target.value)}
            required
            maxLength={512}
            rows={4}
            className="bg-muted focus-visible:ring-accent-700 mt-2 w-full resize-none rounded-lg px-4 py-3 outline-none focus-visible:ring-2"
          />
        </label>
        <label className="block text-sm font-medium">
          {t('createWorkOrder.acceptance')}
          <textarea
            value={criteria}
            onChange={(event) => setCriteria(event.target.value)}
            required
            rows={4}
            placeholder={t('createWorkOrder.acceptancePlaceholder')}
            className="bg-muted focus-visible:ring-accent-700 mt-2 w-full resize-none rounded-lg px-4 py-3 outline-none focus-visible:ring-2"
          />
          <span className="text-muted-foreground mt-2 block text-xs">
            {t('createWorkOrder.acceptanceDetail')}
          </span>
        </label>
        <label className="block text-sm font-medium">
          {t('createWorkOrder.scope')}
          <input
            value={scope}
            onChange={(event) => setScope(event.target.value)}
            required
            maxLength={1024}
            className="bg-muted focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
          />
          <span className="text-muted-foreground mt-2 block text-xs">
            {t('createWorkOrder.scopeDetail')}
          </span>
        </label>
        {departments.includes('developer') ? (
          <>
            <div className="grid grid-cols-2 gap-3">
              <label className="block text-sm font-medium">
                {t('createWorkOrder.projectId')}
                <input
                  value={projectId}
                  onChange={(event) => setProjectId(event.target.value)}
                  required
                  maxLength={128}
                  className="bg-muted focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
                />
              </label>
              <label className="block text-sm font-medium">
                {t('createWorkOrder.workspaceId')}
                <input
                  value={workspaceId}
                  onChange={(event) => setWorkspaceId(event.target.value)}
                  required
                  maxLength={128}
                  className="bg-muted focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
                />
              </label>
            </div>
            <label className="block text-sm font-medium">
              {t('createWorkOrder.scopeFiles')}
              <textarea
                value={scopeFiles}
                onChange={(event) => setScopeFiles(event.target.value)}
                required
                rows={4}
                placeholder={t('createWorkOrder.scopeFilesPlaceholder')}
                className="bg-muted focus-visible:ring-accent-700 mt-2 w-full resize-none rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
              />
            </label>
            <label className="block text-sm font-medium">
              {t('createWorkOrder.scopeSymbols')}
              <textarea
                value={scopeSymbols}
                onChange={(event) => setScopeSymbols(event.target.value)}
                required
                rows={4}
                placeholder={t('createWorkOrder.scopeSymbolsPlaceholder')}
                className="bg-muted focus-visible:ring-accent-700 mt-2 w-full resize-none rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
              />
            </label>
          </>
        ) : null}
        <div className="grid grid-cols-2 gap-3">
          <label className="block text-sm font-medium">
            {t('createWorkOrder.provider')}
            <input
              value={modelProvider}
              readOnly
              required
              className="bg-muted text-muted-foreground mt-2 w-full rounded-lg px-4 py-3 text-sm"
            />
          </label>
          <label className="block text-sm font-medium">
            {t('createWorkOrder.model')}
            <input
              value={modelId}
              readOnly
              required
              placeholder={t('createWorkOrder.modelPlaceholder')}
              className="bg-muted text-muted-foreground mt-2 w-full rounded-lg px-4 py-3 text-sm"
            />
          </label>
        </div>
        <label className="block text-sm font-medium">
          {t('createWorkOrder.mgsReference')}
          <input
            value={mgsReference}
            onChange={(event) => setMgsReference(event.target.value)}
            required
            placeholder={t('createWorkOrder.mgsReferencePlaceholder')}
            className="bg-muted focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
          />
        </label>
        <label className="block text-sm font-medium">
          {t('createWorkOrder.mgsDigest')}
          <input
            value={mgsDigest}
            onChange={(event) => setMgsDigest(event.target.value.toLowerCase())}
            required
            minLength={64}
            maxLength={64}
            pattern="[0-9a-f]{64}"
            placeholder={t('createWorkOrder.mgsDigestPlaceholder')}
            className="bg-muted focus-visible:ring-accent-700 mt-2 w-full rounded-lg px-4 py-3 font-mono text-sm outline-none focus-visible:ring-2"
          />
        </label>
      </div>
      {prepared ? (
        <section className="bg-muted mt-6 rounded-lg p-4 sm:p-5" aria-live="polite">
          <p className="text-xs font-semibold tracking-[0.15em] uppercase">{t('reviewChange')}</p>
          <p className="text-muted-foreground mt-2 text-sm">{t('signingNotice')}</p>
          <pre className="bg-background mt-4 max-h-72 overflow-auto rounded-lg p-4 text-xs whitespace-pre-wrap">
            {JSON.stringify(prepared, null, 2)}
          </pre>
          <div className="mt-4 flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
            <button
              type="button"
              onClick={() => setPrepared(null)}
              className="bg-background hover:bg-card rounded-xl px-4 py-2 text-sm font-medium"
            >
              {t('keepCurrent')}
            </button>
            <button
              type="button"
              disabled={pending}
              onClick={async () => {
                try {
                  await onCreate(prepared)
                  setObjective('')
                  setCriteria('')
                  setPrepared(null)
                  setOpen(false)
                } catch {
                  // The mutation retains the owner-visible error in this form.
                }
              }}
              className="bg-accent-700 text-on-accent-700 hover:bg-accent-800 rounded-xl px-4 py-2 text-sm font-semibold disabled:opacity-50"
            >
              {pending ? t('signing') : t('approveAndSign')}
            </button>
          </div>
        </section>
      ) : null}
      <fieldset className="mt-6">
        <legend className="text-sm font-medium">{t('createWorkOrder.departments')}</legend>
        <div className="mt-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
          {workOrderDepartments.map((department) => {
            const selected = departments.includes(department.value)
            return (
              <label
                key={department.value}
                className={`cursor-pointer rounded-lg px-4 py-3 text-sm font-medium transition-colors ${
                  selected ? 'bg-accent-700 text-on-accent-700' : 'bg-muted hover:bg-accent'
                }`}
              >
                <input
                  type="checkbox"
                  className="sr-only"
                  checked={selected}
                  onChange={() =>
                    setDepartments((current) =>
                      selected
                        ? current.filter((value) => value !== department.value)
                        : [...current, department.value],
                    )
                  }
                />
                {t(`department.${department.label}`)}
              </label>
            )
          })}
        </div>
      </fieldset>
      {error || prepareError ? (
        <p role="alert" className="bg-danger-100 text-danger-900 mt-5 rounded-xl px-3 py-2 text-sm">
          {error ?? prepareError}
        </p>
      ) : null}
      <div className="mt-7 flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
        <button
          type="submit"
          disabled={
            pending ||
            prepared !== null ||
            session === null ||
            !objective.trim() ||
            !criteria.trim() ||
            !modelProvider.trim() ||
            !modelId.trim() ||
            !mgsReference.trim() ||
            !/^[0-9a-f]{64}$/.test(mgsDigest) ||
            (departments.includes('developer') &&
              (!projectId.trim() ||
                !workspaceId.trim() ||
                !scopeFiles.trim() ||
                !scopeSymbols.trim())) ||
            departments.length === 0
          }
          className="bg-foreground text-background hover:bg-foreground/90 rounded-lg px-5 py-3 text-sm font-semibold disabled:opacity-50"
        >
          {t('reviewChange')}
        </button>
      </div>
    </form>
  )
}

function LiveStatus({ state }: { state: 'connecting' | 'live' | 'reconnecting' | 'closed' }) {
  const t = useTranslations('workforce')
  const live = state === 'live'
  return (
    <div
      role="status"
      className={`flex min-w-28 items-center justify-center gap-2 rounded-md px-3 py-1.5 font-mono text-[11px] font-medium ${
        live ? 'bg-success-100 text-success-900' : 'bg-muted text-muted-foreground'
      }`}
    >
      <span className={`size-2 rounded-full ${live ? 'bg-success-700' : 'bg-muted-foreground'}`} />
      {t(`stream.${state}`)}
    </div>
  )
}

function StateMark({ active }: { active: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={`size-2.5 shrink-0 rounded-full ${
        active ? 'bg-success-700' : 'bg-muted-foreground'
      }`}
    />
  )
}

function WorkOrders({ items }: { items: WorkforceResourceItem[] }) {
  const t = useTranslations('workforce')
  return (
    <section aria-labelledby="work-orders-heading" className="bg-card rounded-xl p-4 sm:p-5">
      <div className="mb-5">
        <p className="text-muted-foreground text-xs font-semibold tracking-[0.15em] uppercase">
          {t('activeWork')}
        </p>
        <h2 id="work-orders-heading" className="mt-2 text-xl font-semibold tracking-tight">
          {t('workOrders')}
        </h2>
        <p className="text-muted-foreground mt-1 text-sm">{t('workOrdersDetail')}</p>
      </div>
      <div className="bg-muted/70 space-y-2 rounded-lg p-2">
        {items.length === 0 ? (
          <EmptyState label={t('noWorkOrders')} />
        ) : (
          items.slice(0, 8).map((item) => {
            const state = String(item.fields.state ?? 'unknown')
            return (
              <article
                key={item.id}
                className="bg-background flex items-center justify-between gap-3 rounded-md px-3 py-3"
              >
                <div className="min-w-0">
                  <h3 className="truncate text-sm font-medium">
                    {String(item.fields.title ?? item.id)}
                  </h3>
                  <p className="text-muted-foreground mt-1 truncate font-mono text-xs">{item.id}</p>
                </div>
                <span className={stateClass(state)}>{plainState(state)}</span>
              </article>
            )
          })
        )}
      </div>
    </section>
  )
}

function Operations({
  nextWakes,
  incidents,
  spendCeiling,
}: {
  nextWakes: WorkforceResourceItem[]
  incidents: WorkforceResourceItem[]
  spendCeiling: number
}) {
  const t = useTranslations('workforce')
  return (
    <section aria-labelledby="operations-heading" className="bg-card rounded-xl p-4 sm:p-5">
      <div className="mb-5">
        <p className="text-muted-foreground text-xs font-semibold tracking-[0.15em] uppercase">
          {t('fromBackend')}
        </p>
        <h2 id="operations-heading" className="mt-2 text-xl font-semibold tracking-tight">
          {t('operations')}
        </h2>
        <p className="text-muted-foreground mt-1 text-sm">{t('operationsDetail')}</p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <article className="bg-muted/70 rounded-lg p-4">
          <div className="flex items-center gap-2">
            <Gauge className="text-muted-foreground size-4" />
            <h3 className="text-sm font-medium">{t('budgetCeiling')}</h3>
          </div>
          <p className="mt-3 text-2xl font-semibold tabular-nums">
            {formatMicrounits(spendCeiling)}
          </p>
          <p className="text-muted-foreground mt-1 text-xs">{t('scheduledCeilings')}</p>
        </article>
        <article className="bg-muted/70 rounded-lg p-4">
          <div className="flex items-center gap-2">
            <CircleAlert className="text-muted-foreground size-4" />
            <h3 className="text-sm font-medium">{t('incidents')}</h3>
          </div>
          <p className="mt-3 text-2xl font-semibold tabular-nums">{incidents.length}</p>
          <p className="text-muted-foreground mt-1 text-xs">{t('unresolvedAndHistorical')}</p>
        </article>
      </div>
      <div className="bg-muted/70 mt-3 rounded-lg p-4">
        <div className="flex items-center gap-2">
          <CalendarClock className="text-muted-foreground size-4" />
          <h3 className="text-sm font-medium">{t('nextWakes')}</h3>
        </div>
        <div className="mt-3 space-y-2">
          {nextWakes.length === 0 ? (
            <EmptyState label={t('noScheduledWakes')} />
          ) : (
            nextWakes.map((item) => (
              <div key={item.id} className="bg-background rounded-md px-3 py-2.5">
                <p className="truncate text-sm font-medium">
                  {String(item.fields.reason ?? item.id)}
                </p>
                <p className="text-muted-foreground mt-1 text-xs">
                  {formatDate(item.fields.scheduled_at)}
                </p>
              </div>
            ))
          )}
        </div>
      </div>
    </section>
  )
}

function CommandReview({
  draft,
  pending,
  error,
  onClose,
  onApply,
}: {
  draft: WorkforceCommandDraft
  pending: boolean
  error: string | null
  onClose: () => void
  onApply: () => void
}) {
  const t = useTranslations('workforce')
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="command-review-title"
      className="fixed inset-0 z-50 grid place-items-end bg-black/55 p-3 backdrop-blur-sm sm:place-items-center"
    >
      <section className="bg-card max-h-[88dvh] w-full max-w-xl overflow-y-auto rounded-xl p-5 sm:p-6">
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="text-muted-foreground text-xs font-medium tracking-[0.16em] uppercase">
              {t('signedChange')}
            </p>
            <h2 id="command-review-title" className="mt-1 text-lg font-semibold">
              {t('reviewChange')}
            </h2>
          </div>
          <button
            type="button"
            aria-label={t('close')}
            className="bg-muted hover:bg-accent grid size-9 place-items-center rounded-xl"
            onClick={onClose}
          >
            <X className="size-4" />
          </button>
        </div>
        <dl className="mt-5 grid gap-3 text-sm sm:grid-cols-2">
          <ReviewField label={t('action')} value={draft.action} />
          <ReviewField label={t('expectedVersion')} value={String(draft.expectedVersion)} />
          <ReviewField
            label={t('resource')}
            value={`${draft.resourceKind} / ${draft.resourceId}`}
          />
        </dl>
        <div className="bg-muted mt-4 rounded-lg p-4">
          <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
            {t('exactChange')}
          </p>
          <pre className="mt-2 overflow-x-auto font-mono text-xs whitespace-pre-wrap">
            {JSON.stringify(draft.change, null, 2)}
          </pre>
        </div>
        <p className="text-muted-foreground mt-4 text-sm">{t('signingNotice')}</p>
        {error ? (
          <p
            role="alert"
            className="bg-danger-100 text-danger-900 mt-4 rounded-xl px-3 py-2 text-sm"
          >
            {error}
          </p>
        ) : null}
        <div className="mt-5 grid gap-2 sm:grid-cols-2">
          <button
            type="button"
            className="bg-muted hover:bg-accent rounded-xl px-4 py-2.5 text-sm font-medium"
            onClick={onClose}
            disabled={pending}
          >
            {t('keepCurrent')}
          </button>
          <button
            type="button"
            className="bg-accent-700 text-on-accent-700 hover:bg-accent-800 rounded-xl px-4 py-2.5 text-sm font-medium disabled:opacity-50"
            onClick={onApply}
            disabled={pending}
          >
            {pending ? t('signing') : t('approveAndSign')}
          </button>
        </div>
      </section>
    </div>
  )
}

function ReviewField({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-muted rounded-xl px-3 py-2">
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="mt-1 font-mono text-xs break-all">{value}</dd>
    </div>
  )
}

function EmptyState({ label }: { label: string }) {
  return <p className="text-muted-foreground px-3 py-6 text-center text-sm">{label}</p>
}

function findDepartment(
  departments: WorkforceResourceItem[],
  aliases: readonly string[],
): WorkforceResourceItem | undefined {
  return departments.find((item) => {
    const value = item.id.toLowerCase()
    return aliases.some((alias) => value.includes(alias))
  })
}

function findSeat(
  seats: WorkforceResourceItem[],
  departmentId: string | undefined,
  aliases: readonly string[],
  role: (typeof seatRoles)[number],
): WorkforceResourceItem | undefined {
  return seats.find((item) => {
    const seatDepartment = String(item.fields.department_id ?? '').toLowerCase()
    const identity = item.id.toLowerCase()
    const departmentMatches = departmentId
      ? seatDepartment === departmentId.toLowerCase()
      : aliases.some((alias) => seatDepartment.includes(alias) || identity.includes(alias))
    return departmentMatches && identity.includes(role)
  })
}

function seatState(
  item: WorkforceResourceItem,
  activeLabel: string,
  inactiveLabel: string,
): string {
  return item.fields.active === true ? activeLabel : inactiveLabel
}

function numberField(item: WorkforceResourceItem, field: string): number {
  const value = Number(item.fields[field] ?? 0)
  return Number.isFinite(value) ? value : 0
}

function isTerminal(state: string): boolean {
  return ['completed', 'cancelled', 'failed'].includes(state)
}

function stateClass(state: string): string {
  const base = 'shrink-0 rounded-full px-2.5 py-1 text-xs font-medium'
  if (state === 'completed') return `${base} bg-success-100 text-success-900`
  if (state === 'failed' || state === 'cancelled') return `${base} bg-danger-100 text-danger-900`
  if (state === 'waiting' || state === 'contested') return `${base} bg-warning-100 text-warning-950`
  return `${base} bg-muted text-foreground`
}

function plainState(value: string): string {
  return value.replaceAll('_', ' ')
}

function formatDate(value: unknown): string {
  if (typeof value !== 'string') return 'Not reported'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'Not reported' : date.toLocaleString()
}

function formatMicrounits(value: number): string {
  if (value === 0) return '0'
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value / 1_000_000)
}
