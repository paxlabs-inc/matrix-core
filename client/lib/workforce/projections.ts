import type { WorkforceResourceItem } from '@/lib/api/workforce'

export interface WorkforceMailThread {
  id: string
  messages: WorkforceResourceItem[]
}

export function groupWorkforceMail(items: WorkforceResourceItem[]): WorkforceMailThread[] {
  const threads = new Map<string, WorkforceResourceItem[]>()
  for (const item of items) {
    const threadId = stringField(item, 'thread_id') || item.id
    const messages = threads.get(threadId) ?? []
    messages.push(item)
    threads.set(threadId, messages)
  }
  return [...threads.entries()]
    .map(([id, messages]) => ({
      id,
      messages: messages.sort((left, right) => left.updated_at.localeCompare(right.updated_at)),
    }))
    .sort((left, right) =>
      (right.messages.at(-1)?.updated_at ?? '').localeCompare(
        left.messages.at(-1)?.updated_at ?? '',
      ),
    )
}

export function approvalState(item: WorkforceResourceItem, now: Date): string {
  if (item.fields.revoked_at) return 'revoked'
  const expiry = new Date(stringField(item, 'expires_at'))
  if (!Number.isNaN(expiry.getTime()) && expiry.getTime() <= now.getTime()) return 'expired'
  const ceiling = numberField(item, 'aggregate_ceiling_microunits')
  const consumed = numberField(item, 'consumed_microunits')
  if (ceiling > 0 && consumed >= ceiling) return 'consumed'
  return 'available'
}

export function isVerifiedCompletionReceipt(item: WorkforceResourceItem): boolean {
  return stringField(item, 'disposition') === 'goal_completed'
}

export function stringField(item: WorkforceResourceItem, field: string): string {
  const value = item.fields[field]
  return typeof value === 'string' ? value : ''
}

export function stringListField(item: WorkforceResourceItem, field: string): string[] {
  const value = item.fields[field]
  return Array.isArray(value)
    ? value.filter((entry): entry is string => typeof entry === 'string')
    : []
}

export function objectListField(
  item: WorkforceResourceItem,
  field: string,
): Record<string, unknown>[] {
  const value = item.fields[field]
  return Array.isArray(value)
    ? value.filter(
        (entry): entry is Record<string, unknown> =>
          typeof entry === 'object' && entry !== null && !Array.isArray(entry),
      )
    : []
}

export function numberField(item: WorkforceResourceItem, field: string): number {
  const value = Number(item.fields[field] ?? 0)
  return Number.isFinite(value) ? value : 0
}
