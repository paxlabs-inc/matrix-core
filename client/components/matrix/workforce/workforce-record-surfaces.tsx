'use client'

import { useMemo, useState } from 'react'
import { useTranslations } from 'next-intl'
import {
  Archive,
  BadgeCheck,
  BrainCircuit,
  CircleAlert,
  FileClock,
  Mail,
  MessagesSquare,
  ShieldCheck,
} from 'lucide-react'
import type { WorkforceResource, WorkforceResourceItem } from '@/lib/api/workforce'
import {
  approvalState,
  groupWorkforceMail,
  isVerifiedCompletionReceipt,
  objectListField,
  stringField,
  stringListField,
} from '@/lib/workforce/projections'
import type { WorkforceCommandDraft } from '@/lib/workforce/control-signing'

type RecordsView = 'mail' | 'approvals' | 'receipts' | 'policies' | 'brain' | 'assurance'

const views: { id: RecordsView; icon: typeof Mail }[] = [
  { id: 'mail', icon: MessagesSquare },
  { id: 'approvals', icon: ShieldCheck },
  { id: 'receipts', icon: BadgeCheck },
  { id: 'policies', icon: FileClock },
  { id: 'brain', icon: BrainCircuit },
  { id: 'assurance', icon: CircleAlert },
]

export function WorkforceRecordSurfaces({
  items,
  controlVersion,
  onPreview,
}: {
  items: (resource: WorkforceResource) => WorkforceResourceItem[]
  controlVersion: (kind: string, id: string) => number
  onPreview: (draft: WorkforceCommandDraft) => void
}) {
  const t = useTranslations('workforce.records')
  const [view, setView] = useState<RecordsView>('mail')
  const content = {
    mail: <MailView items={items('mail')} />,
    approvals: (
      <ApprovalView
        items={items('approvals')}
        controlVersion={controlVersion}
        onPreview={onPreview}
      />
    ),
    receipts: <ReceiptView items={items('receipts')} />,
    policies: <PolicyView items={items('policies')} />,
    brain: <BrainView items={items('project-brain')} />,
    assurance: (
      <AssuranceView
        corrections={items('corrections')}
        audits={items('audit-disagreements')}
        replay={items('replay-lineage')}
        effects={items('effect-status')}
      />
    ),
  } satisfies Record<RecordsView, React.ReactNode>

  return (
    <section aria-labelledby="records-heading" className="bg-card rounded-xl p-4 sm:p-5">
      <div>
        <h2 id="records-heading" className="text-xl font-semibold tracking-tight">
          {t('title')}
        </h2>
        <p className="text-muted-foreground mt-1 max-w-3xl text-sm leading-6">{t('detail')}</p>
      </div>
      <div
        role="tablist"
        aria-label={t('title')}
        className="bg-muted/70 mt-5 flex max-w-full gap-1 overflow-x-auto rounded-lg p-1"
      >
        {views.map(({ id, icon: Icon }) => (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={view === id}
            aria-controls={`workforce-panel-${id}`}
            className={`flex shrink-0 items-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition-colors ${
              view === id
                ? 'bg-foreground text-background'
                : 'text-muted-foreground hover:bg-accent'
            }`}
            onClick={() => setView(id)}
          >
            <Icon className="size-4" />
            {t(`tab.${id}`)}
          </button>
        ))}
      </div>
      <div
        id={`workforce-panel-${view}`}
        role="tabpanel"
        className="bg-muted/70 mt-3 rounded-lg p-3 sm:p-4"
      >
        {content[view]}
      </div>
    </section>
  )
}

function MailView({ items }: { items: WorkforceResourceItem[] }) {
  const t = useTranslations('workforce.records')
  const threads = useMemo(() => groupWorkforceMail(items), [items])
  if (threads.length === 0) return <Empty label={t('empty.mail')} />
  return (
    <div className="space-y-3">
      {threads.map((thread) => (
        <article key={thread.id} className="bg-background rounded-lg p-3">
          <p className="text-muted-foreground font-mono text-xs">{thread.id}</p>
          <div className="mt-3 space-y-2">
            {thread.messages.map((message) => {
              const recipients = objectListField(message, 'recipients')
              return (
                <div key={message.id} className="bg-muted/70 rounded-md px-3 py-2.5">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <p className="text-sm font-medium">
                      {plain(stringField(message, 'kind') || t('unknown'))}
                    </p>
                    <State value={stringField(message, 'binding_state')} />
                  </div>
                  <p className="text-muted-foreground mt-1 text-xs">
                    {stringField(message, 'sender_seat_id')} · {formatDate(message.updated_at)}
                  </p>
                  <p className="text-muted-foreground mt-2 text-xs">
                    {recipients.length
                      ? recipients
                          .map(
                            (recipient) =>
                              `${String(recipient.seat_id ?? t('unknown'))}: ${plain(
                                String(recipient.state ?? t('unknown')),
                              )}`,
                          )
                          .join(' · ')
                      : t('recipientsUnavailable')}
                  </p>
                  <p className="text-muted-foreground mt-2 text-xs">{t('sealedContent')}</p>
                </div>
              )
            })}
          </div>
        </article>
      ))}
    </div>
  )
}

function ApprovalView({
  items,
  controlVersion,
  onPreview,
}: {
  items: WorkforceResourceItem[]
  controlVersion: (kind: string, id: string) => number
  onPreview: (draft: WorkforceCommandDraft) => void
}) {
  const t = useTranslations('workforce.records')
  if (items.length === 0) return <Empty label={t('empty.approvals')} />
  const now = new Date()
  return (
    <div className="grid gap-3 lg:grid-cols-2">
      {items.map((item) => {
        const intentIds = stringListField(item, 'intent_ids')
        const state = approvalState(item, now)
        return (
          <article key={item.id} className="bg-background rounded-lg p-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <p className="truncate font-mono text-xs">{item.id}</p>
                <p className="text-muted-foreground mt-1 text-xs">
                  {t('intentCount', { count: intentIds.length })}
                </p>
              </div>
              <State value={state} />
            </div>
            <div className="bg-muted/70 mt-3 rounded-md p-3">
              <p className="text-muted-foreground text-xs">{t('exactIntentSet')}</p>
              <p className="mt-1 font-mono text-xs break-all">
                {intentIds.length ? intentIds.join(', ') : t('notReported')}
              </p>
              <p className="text-muted-foreground mt-2 font-mono text-xs break-all">
                {stringField(item, 'intent_set_hash') || t('notReported')}
              </p>
            </div>
            <button
              type="button"
              disabled={state !== 'available' || intentIds.length === 0}
              className="bg-foreground text-background hover:bg-foreground/90 mt-3 w-full rounded-md px-3 py-2 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-45"
              onClick={() =>
                onPreview({
                  action: 'approve_batch',
                  resourceKind: 'approval',
                  resourceId: item.id,
                  expectedVersion: controlVersion('approval', item.id),
                  change: {
                    batch_id: item.id,
                    intent_ids: intentIds,
                    intent_set_hash: stringField(item, 'intent_set_hash'),
                    aggregate_ceiling_microunits: item.fields.aggregate_ceiling_microunits ?? 0,
                    expires_at: stringField(item, 'expires_at'),
                  },
                })
              }
            >
              {t('reviewExactBatch')}
            </button>
          </article>
        )
      })}
    </div>
  )
}

function ReceiptView({ items }: { items: WorkforceResourceItem[] }) {
  const t = useTranslations('workforce.records')
  if (items.length === 0) return <Empty label={t('empty.receipts')} />
  return (
    <div className="grid gap-3 lg:grid-cols-2">
      {items.map((item) => {
        const verified = isVerifiedCompletionReceipt(item)
        return (
          <article key={item.id} className="bg-background rounded-lg p-3">
            <div className="flex items-start justify-between gap-3">
              <p className="font-mono text-xs break-all">{item.id}</p>
              <span
                className={`shrink-0 rounded-md px-2.5 py-1 text-xs font-medium ${
                  verified ? 'bg-success-100 text-success-900' : 'bg-muted text-foreground'
                }`}
              >
                {verified ? t('verifiedCompletion') : plain(stringField(item, 'disposition'))}
              </span>
            </div>
            <RecordLine label={t('wake')} value={stringField(item, 'wake_id')} />
            <RecordLine label={t('intent')} value={stringField(item, 'intent_id')} />
            <RecordLine label={t('canonicalHash')} value={stringField(item, 'content_hash')} />
          </article>
        )
      })}
    </div>
  )
}

function PolicyView({ items }: { items: WorkforceResourceItem[] }) {
  const t = useTranslations('workforce.records')
  if (items.length === 0) return <Empty label={t('empty.policies')} />
  return (
    <div className="space-y-2">
      {items.map((item) => (
        <article
          key={item.id}
          className="bg-background grid gap-2 rounded-lg p-3 sm:grid-cols-[1fr_auto]"
        >
          <div className="min-w-0">
            <p className="truncate text-sm font-medium">
              {plain(stringField(item, 'authority_kind'))} · {stringField(item, 'authority_id')}
            </p>
            <p className="text-muted-foreground mt-1 text-xs">
              {t('version', { version: item.version })} · {formatDate(item.fields.effective_at)}
            </p>
            <p className="text-muted-foreground mt-2 font-mono text-xs break-all">
              {stringField(item, 'canonical_hash')}
            </p>
          </div>
          <State value={item.fields.material_change === true ? 'material change' : 'recorded'} />
        </article>
      ))}
    </div>
  )
}

function BrainView({ items }: { items: WorkforceResourceItem[] }) {
  const t = useTranslations('workforce.records')
  if (items.length === 0) return <Empty label={t('empty.brain')} />
  return (
    <div className="grid gap-3 lg:grid-cols-2">
      {items.map((item) => (
        <article key={item.id} className="bg-background rounded-lg p-3">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <p className="truncate text-sm font-medium">
                {stringField(item, 'project_id')} / {stringField(item, 'workspace_id')}
              </p>
              <p className="text-muted-foreground mt-1 text-xs">
                {plain(stringField(item, 'kind'))}
              </p>
            </div>
            <State value={item.fields.fresh === true ? 'current' : 'stale'} />
          </div>
          <RecordLine label={t('sourceRoot')} value={stringField(item, 'source_root')} />
          <RecordLine
            label={t('graphGeneration')}
            value={String(item.fields.graph_generation ?? t('notReported'))}
          />
          <RecordLine label={t('evidenceHash')} value={stringField(item, 'canonical_hash')} />
          {stringField(item, 'supersedes') || stringField(item, 'corrects') ? (
            <RecordLine
              label={t('lineage')}
              value={
                stringField(item, 'corrects') || stringField(item, 'supersedes') || t('notReported')
              }
            />
          ) : null}
        </article>
      ))}
    </div>
  )
}

function AssuranceView({
  corrections,
  audits,
  replay,
  effects,
}: {
  corrections: WorkforceResourceItem[]
  audits: WorkforceResourceItem[]
  replay: WorkforceResourceItem[]
  effects: WorkforceResourceItem[]
}) {
  const t = useTranslations('workforce.records')
  const ambiguous = effects.filter((item) => stringField(item, 'state') === 'externally_ambiguous')
  return (
    <div className="grid gap-3 xl:grid-cols-2">
      <RecordGroup title={t('corrections')} empty={t('empty.corrections')} items={corrections}>
        {(item) => (
          <>
            <RecordHeading item={item} state={stringField(item, 'status')} />
            <p className="text-muted-foreground mt-2 text-xs">
              {t('affectedCount', { count: objectListField(item, 'affected').length })}
            </p>
            {objectListField(item, 'affected').map((target) => (
              <p key={String(target.record_id)} className="mt-1 font-mono text-xs break-all">
                {String(target.record_id)} · {plain(String(target.state ?? t('unknown')))}
                {target.paused === true ? ` · ${t('paused')}` : ''}
              </p>
            ))}
          </>
        )}
      </RecordGroup>
      <RecordGroup title={t('auditChecks')} empty={t('empty.audits')} items={audits}>
        {(item) => (
          <>
            <RecordHeading
              item={item}
              state={item.fields.disagreement === true ? 'disagreement' : 'agreed'}
            />
            <p className="text-muted-foreground mt-2 text-xs">
              {plain(stringField(item, 'original_outcome'))} →{' '}
              {plain(stringField(item, 'reaudit_outcome'))}
            </p>
          </>
        )}
      </RecordGroup>
      <RecordGroup title={t('replayEvidence')} empty={t('empty.replay')} items={replay}>
        {(item) => (
          <>
            <RecordHeading
              item={item}
              state={item.fields.replay_retained === true ? 'retained' : 'not retained'}
            />
            <RecordLine label={t('wake')} value={stringField(item, 'wake_id')} />
            <RecordLine label={t('requestHash')} value={stringField(item, 'request_hash')} />
            <RecordLine label={t('responseHash')} value={stringField(item, 'response_hash')} />
          </>
        )}
      </RecordGroup>
      <RecordGroup title={t('unresolvedOutcomes')} empty={t('empty.ambiguity')} items={ambiguous}>
        {(item) => (
          <>
            <RecordHeading item={item} state="needs reconciliation" />
            <p className="text-muted-foreground mt-2 text-xs">
              {plain(stringField(item, 'operation'))} ·{' '}
              {stringField(item, 'safe_error_code') || t('notReported')}
            </p>
          </>
        )}
      </RecordGroup>
    </div>
  )
}

function RecordGroup({
  title,
  empty,
  items,
  children,
}: {
  title: string
  empty: string
  items: WorkforceResourceItem[]
  children: (item: WorkforceResourceItem) => React.ReactNode
}) {
  return (
    <section className="bg-background rounded-lg p-3">
      <h3 className="text-sm font-medium">{title}</h3>
      <div className="mt-3 space-y-2">
        {items.length ? (
          items.map((item) => (
            <article key={item.id} className="bg-muted/70 rounded-md p-3">
              {children(item)}
            </article>
          ))
        ) : (
          <Empty label={empty} />
        )}
      </div>
    </section>
  )
}

function RecordHeading({ item, state }: { item: WorkforceResourceItem; state: string }) {
  return (
    <div className="flex items-start justify-between gap-3">
      <p className="font-mono text-xs break-all">{item.id}</p>
      <State value={state} />
    </div>
  )
}

function RecordLine({ label, value }: { label: string; value: string }) {
  const t = useTranslations('workforce.records')
  return (
    <dl className="mt-2 grid gap-1 text-xs sm:grid-cols-[120px_1fr]">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-mono break-all">{value || t('notReported')}</dd>
    </dl>
  )
}

function State({ value }: { value: string }) {
  const normalized = value || 'unknown'
  const caution = [
    'expired',
    'revoked',
    'disagreement',
    'stale',
    'needs reconciliation',
    'material change',
  ].includes(normalized)
  return (
    <span
      className={`shrink-0 rounded-md px-2.5 py-1 text-xs font-medium ${
        caution ? 'bg-warning-100 text-warning-950' : 'bg-accent text-foreground'
      }`}
    >
      {plain(normalized)}
    </span>
  )
}

function Empty({ label }: { label: string }) {
  return (
    <div className="text-muted-foreground flex items-center justify-center gap-2 px-3 py-6 text-sm">
      <Archive className="size-4" />
      <p>{label}</p>
    </div>
  )
}

function plain(value: string): string {
  return value.replaceAll('_', ' ')
}

function formatDate(value: unknown): string {
  if (typeof value !== 'string') return ''
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString()
}
