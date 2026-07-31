import { apiFetch } from '@/lib/api/client'
import { workforceEndpoint } from '@/lib/env'

export const WORKFORCE_SCHEMA_VERSION = 'workforce.control.v1'
const WORKFORCE_CREDENTIALS: RequestCredentials = 'same-origin'
const DASHBOARD_READ_TIMEOUT_MS = 5_000

export type WorkforceResource =
  | 'organization'
  | 'departments'
  | 'seats'
  | 'work-orders'
  | 'graph'
  | 'graph-edges'
  | 'mail'
  | 'approvals'
  | 'receipts'
  | 'policies'
  | 'schedules'
  | 'incidents'
  | 'project-brain'
  | 'corrections'
  | 'audit-disagreements'
  | 'replay-lineage'
  | 'effect-status'
  | 'control-versions'

export interface WorkforceResourceItem {
  id: string
  version: number
  updated_at: string
  fields: Record<string, unknown>
}

export interface WorkforceResourcePage {
  schema_version: string
  resource: WorkforceResource
  items: WorkforceResourceItem[]
  next_cursor?: string
}

export interface WorkforceSession {
  schema_version: string
  organization_id: string
  owner_id: string
  model_provider: string
  model_id: string
}

export interface WorkforceLifecycleEvent {
  schema_version: string
  cursor: number
  event_id: string
  organization_id: string
  event_type: string
  resource_kind: string
  resource_id: string
  resource_version: number
  verified_completion: boolean
  receipt_id?: string
  fields: Record<string, unknown>
  created_at: string
}

export interface WorkforceEventPage {
  schema_version: string
  events: WorkforceLifecycleEvent[]
  next_cursor: number
}

export interface WorkforceSignature {
  algorithm: 'ed25519'
  key_id: string
  value: string
}

export interface WorkforceSeat {
  schema_version: string
  seat_id: string
  version: number
  seat_did: string
  organization_id: string
  department_id: string
  role: 'lead' | 'executor' | 'auditor'
  mandate_id: string
  mandate_version: number
  binding_id: string
  binding_version: number
  effective_at: string
  signature: WorkforceSignature
}

export interface WorkforceDepartment {
  schema_version: string
  department_id: string
  organization_id: string
  kind: WorkforceDepartmentKind
  seats: WorkforceSeat[]
  enabled: boolean
}

export type WorkforceDepartmentKind =
  | 'developer'
  | 'executive'
  | 'research_and_development'
  | 'marketing_and_social'
  | 'legal'
  | 'accounting'
  | 'back_office'

export interface WorkforceOrganization {
  schema_version: string
  organization_id: string
  owner_id: string
  version: number
  name: string
  departments: WorkforceDepartment[]
  effective_at: string
  signature: WorkforceSignature
}

export interface WorkforceMandate {
  schema_version: string
  mandate_id: string
  version: number
  organization_id: string
  department_kind: WorkforceDepartmentKind
  seat_role: 'lead' | 'executor' | 'auditor'
  allowed_skills: string[]
  data_scopes: Array<{ name: string; classification: string; purpose: string }>
  escalation_rules: Array<{ condition: string; action: string }>
  prohibitions: Array<{ clause_id: string; description: string }>
  effective_at: string
  expires_at: string | null
  signature: WorkforceSignature
}

export interface WorkforceActivationSeed {
  organization: WorkforceOrganization
  mandates: WorkforceMandate[]
  runtime_authority: WorkforceRuntimeAuthority
  policies: WorkforcePolicy[]
}

export interface WorkforceRuntimeAuthority {
  schema_version: string
  runtime_authority_id: string
  version: number
  organization_id: string
  key_id: string
  public_key: string
  purposes: ['wake_lease_signing']
  effective_at: string
  expires_at: string | null
  signature: WorkforceSignature
}

export interface WorkforcePolicy {
  schema_version: string
  policy_id: string
  version: number
  organization_id: string
  kind: string
  effective_at: string
  expires_at: string | null
  rules: Array<{
    clause_id: string
    outcome: 'deny' | 'allow' | 'require_review' | 'escalate'
    scope: string
  }>
  signature: WorkforceSignature
}

export interface WorkforceActivationPreview {
  schema_version: string
  seed: WorkforceActivationSeed
  skill_contracts: WorkforceSignedSkillContract[]
}

export interface WorkforceSignedSkillContract {
  schema_version: string
  organization_id: string
  contract: Record<string, unknown>
  effective_at: string
  signature: WorkforceSignature
}

export interface WorkforceActivationBundle {
  seed: WorkforceActivationSeed
  skill_contracts: WorkforceSignedSkillContract[]
}

export interface WorkforceActivationResult {
  schema_version: string
  organization_id: string
  departments: number
  seats: number
  deduplicated: boolean
  event_cursor?: number
}

export interface WorkforceWorkOrder {
  schema_version: 'workforce.work-order.v1'
  work_order_id: string
  organization_id: string
  owner_id: string
  version: 1
  objective: string
  scope: string
  project_id: string
  workspace_id: string
  scope_files: string[]
  scope_symbols: string[]
  departments: WorkforceDepartmentKind[]
  priority: number
  budget: { max_tasks: number; max_spend_microunits: number }
  deadline: string
  autonomy: 'supervised' | 'review_required' | 'bounded_auto'
  acceptance_criteria: string[]
  model_provider: string
  model_id: string
  mgs_reference: string
  mgs_digest: string
  created_at: string
  idempotency_key: string
  signature: WorkforceSignature
}

export interface WorkforceWorkOrderResult {
  schema_version: string
  work_order_id: string
  goal_id: string
  intent_ids: string[]
  wake_id: string
  deduplicated: boolean
  event_cursor: number
}

export interface WorkforceSignedCommand {
  schema_version: string
  command_id: string
  organization_id: string
  owner_id: string
  action:
    | 'set_policy'
    | 'set_mandate'
    | 'set_autonomy'
    | 'set_schedule'
    | 'cancel_work'
    | 'force_wake'
    | 'approve_batch'
  resource_kind: string
  resource_id: string
  expected_version: number
  change: Record<string, unknown>
  effective_at: string
  signature: WorkforceSignature
}

export interface WorkforceCommandResult {
  schema_version: string
  command_id: string
  version: number
  event_cursor: number
}

export async function getWorkforceSession(signal?: AbortSignal): Promise<WorkforceSession> {
  return apiFetch<WorkforceSession>(workforceEndpoint('/v1/workforce/session'), {
    signal,
    credentials: WORKFORCE_CREDENTIALS,
    timeoutMs: DASHBOARD_READ_TIMEOUT_MS,
    retries: 0,
  })
}

export async function getWorkforceResource(
  resource: WorkforceResource,
  options: { cursor?: string; limit?: number; signal?: AbortSignal } = {},
): Promise<WorkforceResourcePage> {
  const query = new URLSearchParams()
  query.set('limit', String(options.limit ?? 200))
  if (options.cursor) query.set('cursor', options.cursor)
  return apiFetch<WorkforceResourcePage>(
    workforceEndpoint(`/v1/workforce/${encodeURIComponent(resource)}?${query}`),
    {
      signal: options.signal,
      credentials: WORKFORCE_CREDENTIALS,
      timeoutMs: DASHBOARD_READ_TIMEOUT_MS,
      retries: 0,
    },
  )
}

export async function getWorkforceEvents(
  after = 0,
  signal?: AbortSignal,
): Promise<WorkforceEventPage> {
  return apiFetch<WorkforceEventPage>(
    workforceEndpoint(`/v1/workforce/events?after=${after}&limit=500`),
    {
      signal,
      credentials: WORKFORCE_CREDENTIALS,
      timeoutMs: DASHBOARD_READ_TIMEOUT_MS,
      retries: 0,
    },
  )
}

export async function registerWorkforceControlKey(keyId: string, publicKey: string): Promise<void> {
  await apiFetch(workforceEndpoint('/v1/workforce/control-keys'), {
    method: 'POST',
    credentials: WORKFORCE_CREDENTIALS,
    body: JSON.stringify({ key_id: keyId, public_key: publicKey }),
  })
}

export async function previewWorkforceActivation(
  name: string,
  keyId: string,
  effectiveAt: string,
): Promise<WorkforceActivationPreview> {
  return apiFetch<WorkforceActivationPreview>(
    workforceEndpoint('/v1/workforce/activation/preview'),
    {
      method: 'POST',
      credentials: WORKFORCE_CREDENTIALS,
      body: JSON.stringify({ name, key_id: keyId, effective_at: effectiveAt }),
    },
  )
}

export async function activateWorkforce(
  bundle: WorkforceActivationBundle,
): Promise<WorkforceActivationResult> {
  return apiFetch<WorkforceActivationResult>(workforceEndpoint('/v1/workforce/activation'), {
    method: 'POST',
    credentials: WORKFORCE_CREDENTIALS,
    body: JSON.stringify(bundle),
  })
}

export async function createWorkforceWorkOrder(
  order: WorkforceWorkOrder,
): Promise<WorkforceWorkOrderResult> {
  return apiFetch<WorkforceWorkOrderResult>(workforceEndpoint('/v1/workforce/work-orders'), {
    method: 'POST',
    credentials: WORKFORCE_CREDENTIALS,
    body: JSON.stringify(order),
  })
}

export async function submitWorkforceCommand(
  command: WorkforceSignedCommand,
): Promise<WorkforceCommandResult> {
  return apiFetch<WorkforceCommandResult>(workforceEndpoint('/v1/workforce/commands'), {
    method: 'POST',
    credentials: WORKFORCE_CREDENTIALS,
    body: JSON.stringify(command),
  })
}
