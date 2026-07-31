import {
  activateWorkforce,
  createWorkforceWorkOrder,
  previewWorkforceActivation,
  WORKFORCE_SCHEMA_VERSION,
  registerWorkforceControlKey,
  type WorkforceActivationResult,
  type WorkforceActivationSeed,
  type WorkforceDepartmentKind,
  type WorkforceSession,
  type WorkforceSignedCommand,
  type WorkforceWorkOrder,
  type WorkforceWorkOrderResult,
} from '@/lib/api/workforce'

const DB_NAME = 'matrix-workforce-control'
const STORE_NAME = 'owner-keys'
const RECORD_ID = 'active-owner-key'
const ZERO_SIGNATURE = base64url(new Uint8Array(64))

interface StoredControlKey {
  id: string
  keyId: string
  privateKey: CryptoKey
  publicKey: Uint8Array
}

export interface WorkforceCommandDraft {
  action: WorkforceSignedCommand['action']
  resourceKind: string
  resourceId: string
  expectedVersion: number
  change: Record<string, unknown>
}

export interface WorkforceWorkOrderDraft {
  objective: string
  scope: string
  projectId: string
  workspaceId: string
  scopeFiles: string[]
  scopeSymbols: string[]
  departments: WorkforceDepartmentKind[]
  priority: number
  maxTasks: number
  maxSpendMicrounits: number
  deadline: Date
  autonomy: WorkforceWorkOrder['autonomy']
  acceptanceCriteria: string[]
  modelProvider: string
  modelId: string
  mgsReference: string
  mgsDigest: string
}

export async function activateSignedWorkforce(
  session: WorkforceSession,
  name: string,
): Promise<WorkforceActivationResult> {
  const key = await getOrCreateKey()
  await registerWorkforceControlKey(key.keyId, base64url(key.publicKey))
  const effectiveAt = canonicalTimestamp(new Date())
  const preview = await previewWorkforceActivation(name, key.keyId, effectiveAt)
  const seed = structuredClone(preview.seed) as WorkforceActivationSeed
  const skillContracts = structuredClone(preview.skill_contracts)
  for (const department of seed.organization.departments) {
    for (const seat of department.seats) {
      seat.signature.value = await signCanonicalRecord(key.privateKey, seat)
    }
  }
  for (const mandate of seed.mandates) {
    mandate.signature.value = await signCanonicalRecord(key.privateKey, mandate)
  }
  seed.runtime_authority.signature.value = await signCanonicalRecord(
    key.privateKey,
    seed.runtime_authority,
  )
  for (const policy of seed.policies) {
    policy.signature.value = await signCanonicalRecord(key.privateKey, policy)
  }
  seed.organization.signature.value = await signCanonicalRecord(key.privateKey, seed.organization)
  for (const contract of skillContracts) {
    contract.signature.value = await signCanonicalSkillContract(key.privateKey, contract)
  }
  return activateWorkforce({ seed, skill_contracts: skillContracts })
}

export async function prepareWorkforceWorkOrder(
  session: WorkforceSession,
  draft: WorkforceWorkOrderDraft,
): Promise<WorkforceWorkOrder> {
  const key = await getOrCreateKey()
  const createdAt = canonicalTimestamp(new Date())
  return {
    schema_version: 'workforce.work-order.v1',
    work_order_id: `work-order:${crypto.randomUUID()}`,
    organization_id: session.organization_id,
    owner_id: session.owner_id,
    version: 1,
    objective: draft.objective.trim(),
    scope: draft.scope.trim(),
    project_id: draft.projectId.trim(),
    workspace_id: draft.workspaceId.trim(),
    scope_files: draft.scopeFiles.map((value) => value.trim()),
    scope_symbols: draft.scopeSymbols.map((value) => value.trim()),
    departments: draft.departments,
    priority: draft.priority,
    budget: {
      max_tasks: draft.maxTasks,
      max_spend_microunits: draft.maxSpendMicrounits,
    },
    deadline: canonicalTimestamp(draft.deadline),
    autonomy: draft.autonomy,
    acceptance_criteria: draft.acceptanceCriteria.map((criterion) => criterion.trim()),
    model_provider: draft.modelProvider.trim(),
    model_id: draft.modelId.trim(),
    mgs_reference: draft.mgsReference.trim(),
    mgs_digest: draft.mgsDigest.trim(),
    created_at: createdAt,
    idempotency_key: `work-order:web:${crypto.randomUUID()}`,
    signature: { algorithm: 'ed25519', key_id: key.keyId, value: ZERO_SIGNATURE },
  }
}

export async function submitPreparedWorkOrder(
  prepared: WorkforceWorkOrder,
): Promise<WorkforceWorkOrderResult> {
  const key = await getOrCreateKey()
  if (prepared.signature.key_id !== key.keyId || prepared.signature.value !== ZERO_SIGNATURE) {
    throw new Error('Prepared Work Order does not match the current owner key')
  }
  await registerWorkforceControlKey(key.keyId, base64url(key.publicKey))
  const order = structuredClone(prepared)
  order.signature.value = await signCanonicalRecord(key.privateKey, order)
  return createWorkforceWorkOrder(order)
}

export async function createSignedWorkOrder(
  session: WorkforceSession,
  draft: WorkforceWorkOrderDraft,
): Promise<WorkforceWorkOrderResult> {
  return submitPreparedWorkOrder(await prepareWorkforceWorkOrder(session, draft))
}

export async function signWorkforceCommand(
  session: WorkforceSession,
  draft: WorkforceCommandDraft,
): Promise<WorkforceSignedCommand> {
  const key = await getOrCreateKey()
  await registerWorkforceControlKey(key.keyId, base64url(key.publicKey))
  const command: WorkforceSignedCommand = {
    schema_version: WORKFORCE_SCHEMA_VERSION,
    command_id: `command:web:${crypto.randomUUID()}`,
    organization_id: session.organization_id,
    owner_id: session.owner_id,
    action: draft.action,
    resource_kind: draft.resourceKind,
    resource_id: draft.resourceId,
    expected_version: draft.expectedVersion,
    change: draft.change,
    effective_at: canonicalTimestamp(new Date()),
    signature: { algorithm: 'ed25519', key_id: key.keyId, value: ZERO_SIGNATURE },
  }
  const signingPayload = canonicalCommand(command)
  const signature = await crypto.subtle.sign(
    { name: 'Ed25519' },
    key.privateKey,
    new TextEncoder().encode(signingPayload),
  )
  command.signature.value = base64url(new Uint8Array(signature))
  return command
}

export function canonicalCommand(command: WorkforceSignedCommand): string {
  return JSON.stringify({
    schema_version: command.schema_version,
    command_id: command.command_id,
    organization_id: command.organization_id,
    owner_id: command.owner_id,
    action: command.action,
    resource_kind: command.resource_kind,
    resource_id: command.resource_id,
    expected_version: command.expected_version,
    change: canonicalValue(command.change),
    effective_at: canonicalTimestamp(command.effective_at),
    signature: {
      algorithm: 'ed25519',
      key_id: command.signature.key_id,
      value: ZERO_SIGNATURE,
    },
  })
}

async function getOrCreateKey(): Promise<StoredControlKey> {
  const db = await openKeyDatabase()
  const existing = await transaction<StoredControlKey | undefined>(db, 'readonly', (store) =>
    store.get(RECORD_ID),
  )
  if (existing?.privateKey && existing.publicKey && existing.keyId) return existing

  const generated = (await crypto.subtle.generateKey({ name: 'Ed25519' }, true, [
    'sign',
    'verify',
  ])) as CryptoKeyPair
  const publicKey = new Uint8Array(await crypto.subtle.exportKey('raw', generated.publicKey))
  const privateBytes = new Uint8Array(await crypto.subtle.exportKey('pkcs8', generated.privateKey))
  const privateKey = await crypto.subtle.importKey(
    'pkcs8',
    privateBytes,
    { name: 'Ed25519' },
    false,
    ['sign'],
  )
  privateBytes.fill(0)
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', publicKey))
  const record: StoredControlKey = {
    id: RECORD_ID,
    keyId: `key:web:${base64url(digest.slice(0, 12))}`,
    privateKey,
    publicKey,
  }
  await transaction(db, 'readwrite', (store) => store.put(record))
  return record
}

async function signCanonicalRecord(privateKey: CryptoKey, value: object): Promise<string> {
  const { signature: _signature, ...payload } = value as Record<string, unknown>
  const signature = await crypto.subtle.sign(
    { name: 'Ed25519' },
    privateKey,
    new TextEncoder().encode(JSON.stringify(payload)),
  )
  return base64url(new Uint8Array(signature))
}

async function signCanonicalSkillContract(privateKey: CryptoKey, value: object): Promise<string> {
  const payload = structuredClone(value) as Record<string, unknown>
  payload.signature = {
    algorithm: 'ed25519',
    key_id: (payload.signature as WorkforceSignedCommand['signature']).key_id,
    value: ZERO_SIGNATURE,
  }
  const signature = await crypto.subtle.sign(
    { name: 'Ed25519' },
    privateKey,
    new TextEncoder().encode(JSON.stringify(payload)),
  )
  return base64url(new Uint8Array(signature))
}

function openKeyDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1)
    request.onupgradeneeded = () => {
      if (!request.result.objectStoreNames.contains(STORE_NAME)) {
        request.result.createObjectStore(STORE_NAME, { keyPath: 'id' })
      }
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error ?? new Error('Unable to open owner key store'))
  })
}

function transaction<T>(
  database: IDBDatabase,
  mode: IDBTransactionMode,
  run: (store: IDBObjectStore) => IDBRequest,
): Promise<T> {
  return new Promise((resolve, reject) => {
    const tx = database.transaction(STORE_NAME, mode)
    const request = run(tx.objectStore(STORE_NAME))
    request.onsuccess = () => resolve(request.result as T)
    request.onerror = () => reject(request.error ?? new Error('Owner key operation failed'))
    tx.oncomplete = () => database.close()
  })
}

function canonicalValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalValue)
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, nested]) => [key, canonicalValue(nested)]),
    )
  }
  return value
}

function canonicalTimestamp(value: string | Date): string {
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) return String(value)
  const iso = date.toISOString()
  const separator = iso.indexOf('.')
  const fraction = iso.slice(separator + 1, -1).replace(/0+$/, '')
  return fraction ? `${iso.slice(0, separator)}.${fraction}Z` : `${iso.slice(0, separator)}Z`
}

function base64url(value: Uint8Array): string {
  let binary = ''
  for (const byte of value) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '')
}
