/**
 * Paxeer embedded-wallet REST client.
 *
 * Talks to connect.paxportwallet.com using the Matrix client's existing
 * Supabase session (lib/auth/session.getAccessToken) as the Bearer token —
 * the same identity that authenticates the Matrix router. Covers two
 * surfaces:
 *
 *   - Primary wallet      /v1/wallet/*   (the user's standard embedded wallet)
 *   - Owner control plane /v1/agents/*   (the agent wallets the user owns:
 *                                          inventory, leash, fund/sweep)
 *
 * Every call returns server-shaped JSON; higher-level hooks (hooks/api/*)
 * adapt it for the UI. Signing/sending is policy-enforced server-side.
 */
import { getAccessToken } from '@/lib/auth/session'
import { walletApiBase } from '@/lib/paxeer/config'
import {
  PaxeerWalletError,
  type AgentActivityEvent,
  type AgentBudget,
  type AgentDetail,
  type AgentPolicy,
  type AgentPolicyPatch,
  type AgentRule,
  type AgentRuleEffect,
  type AgentRuleSubject,
  type AgentSummary,
  type FundAgentInput,
  type PrimaryWalletResponse,
  type SendTxResponse,
  type SignMessageResponse,
  type SweepAgentInput,
  type TransferResult,
  type TxRequest,
} from '@/lib/paxeer/types'

const DEFAULT_TIMEOUT_MS = 30_000

interface RequestOptions {
  /** Skip the Authorization header (public routes, e.g. /v1/funded/tiers). */
  auth?: boolean
  body?: unknown
  signal?: AbortSignal
  timeoutMs?: number
}

/** Core auth-bearing fetch. Throws `PaxeerWalletError` on any non-2xx. */
async function request<T>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE',
  path: string,
  opts: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }

  if (opts.auth !== false) {
    const token = await getAccessToken()
    if (!token) {
      throw new PaxeerWalletError('not authenticated', 'NO_SESSION', 401)
    }
    headers.Authorization = `Bearer ${token}`
  }
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }

  const ctrl = new AbortController()
  const onAbort = () => ctrl.abort(opts.signal?.reason)
  opts.signal?.addEventListener('abort', onAbort, { once: true })
  const timer = setTimeout(
    () => ctrl.abort(new DOMException('timeout', 'TimeoutError')),
    opts.timeoutMs ?? DEFAULT_TIMEOUT_MS,
  )

  try {
    const res = await fetch(`${walletApiBase()}${path}`, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      signal: ctrl.signal,
    })

    let payload: unknown = null
    try {
      payload = await res.json()
    } catch {
      payload = null
    }

    if (!res.ok) {
      throw toError(res.status, payload)
    }
    return payload as T
  } catch (err) {
    if (err instanceof PaxeerWalletError) throw err
    if (isAbortError(err)) {
      const timeout = (err as { name?: string }).name === 'TimeoutError'
      throw new PaxeerWalletError(
        timeout ? 'request timed out' : 'request aborted',
        timeout ? 'TIMEOUT' : 'ABORTED',
        timeout ? 504 : 499,
      )
    }
    const msg = err instanceof Error ? err.message : String(err)
    throw new PaxeerWalletError(`${method} ${path}: ${msg}`, 'NETWORK', 0)
  } finally {
    clearTimeout(timer)
    opts.signal?.removeEventListener('abort', onAbort)
  }
}

function toError(status: number, body: unknown): PaxeerWalletError {
  const code =
    body && typeof body === 'object' && 'error' in body
      ? String((body as { error: unknown }).error)
      : `HTTP_${status}`
  const message =
    body && typeof body === 'object' && 'message' in body
      ? String((body as { message: unknown }).message)
      : `request failed: ${status}`
  return new PaxeerWalletError(message, code, status, body)
}

function isAbortError(e: unknown): boolean {
  if (!e || typeof e !== 'object') return false
  const name = (e as { name?: string }).name
  return name === 'AbortError' || name === 'TimeoutError'
}

function isNotFound(e: unknown): boolean {
  return e instanceof PaxeerWalletError && e.status === 404
}

/* ── Primary wallet (/v1/wallet/*) ──────────────────────────────────────── */

/** GET /v1/wallet/me — the user's primary wallet, or null if not provisioned. */
export async function getPrimaryWallet(
  signal?: AbortSignal,
): Promise<PrimaryWalletResponse | null> {
  try {
    return await request<PrimaryWalletResponse>('GET', '/v1/wallet/me', { signal })
  } catch (err) {
    if (isNotFound(err)) return null
    throw err
  }
}

/** POST /v1/wallet/provision — idempotent; creates the primary wallet. */
export async function provisionPrimaryWallet(): Promise<PrimaryWalletResponse> {
  return request<PrimaryWalletResponse>('POST', '/v1/wallet/provision', { body: {} })
}

/** POST /v1/wallet/send — sign + broadcast from the primary wallet. */
export async function sendFromPrimary(tx: TxRequest): Promise<SendTxResponse> {
  return request<SendTxResponse>('POST', '/v1/wallet/send', { body: { tx } })
}

/** POST /v1/wallet/sign-message — EIP-191 personal_sign with the primary wallet. */
export async function signMessageFromPrimary(message: string): Promise<SignMessageResponse> {
  return request<SignMessageResponse>('POST', '/v1/wallet/sign-message', { body: { message } })
}

/* ── Owner control plane (/v1/agents/*) ─────────────────────────────────── */

/** GET /v1/agents — every agent the signed-in user owns. */
export async function listAgents(signal?: AbortSignal): Promise<AgentSummary[]> {
  const r = await request<{ agents: AgentSummary[] }>('GET', '/v1/agents', { signal })
  return r.agents ?? []
}

/** GET /v1/agents/:did — full agent record + leash. */
export async function getAgent(did: string, signal?: AbortSignal): Promise<AgentDetail> {
  return request<AgentDetail>('GET', `/v1/agents/${encodeURIComponent(did)}`, { signal })
}

/** PUT /v1/agents/:did/policy — adjust the leash. Returns the effective policy. */
export async function updateAgentPolicy(
  did: string,
  patch: AgentPolicyPatch,
): Promise<AgentPolicy> {
  const r = await request<{ did: string; policy: AgentPolicy }>(
    'PUT',
    `/v1/agents/${encodeURIComponent(did)}/policy`,
    { body: patch },
  )
  return r.policy
}

/** POST /v1/agents/:did/freeze — instant kill switch. */
export async function freezeAgent(did: string): Promise<boolean> {
  const r = await request<{ did: string; is_frozen: boolean }>(
    'POST',
    `/v1/agents/${encodeURIComponent(did)}/freeze`,
    { body: {} },
  )
  return r.is_frozen
}

/** POST /v1/agents/:did/unfreeze — lift the freeze. */
export async function unfreezeAgent(did: string): Promise<boolean> {
  const r = await request<{ did: string; is_frozen: boolean }>(
    'POST',
    `/v1/agents/${encodeURIComponent(did)}/unfreeze`,
    { body: {} },
  )
  return r.is_frozen
}

/** POST /v1/agents/:did/claim — claim an unowned agent principal for the
 *  signed-in user. 409 when another user already owns it. */
export async function claimAgent(did: string): Promise<void> {
  await request<{ did: string; owner_user_id: string | null }>(
    'POST',
    `/v1/agents/${encodeURIComponent(did)}/claim`,
    { body: {} },
  )
}

export interface AddAgentRuleInput {
  effect: AgentRuleEffect
  subject: AgentRuleSubject
  value: string
  max_value_wei?: string | null
  note?: string | null
}

/** POST /v1/agents/:did/rules — add an allow/deny rule. */
export async function addAgentRule(did: string, input: AddAgentRuleInput): Promise<AgentRule> {
  const r = await request<{ rule: AgentRule }>(
    'POST',
    `/v1/agents/${encodeURIComponent(did)}/rules`,
    { body: input },
  )
  return r.rule
}

/** DELETE /v1/agents/:did/rules/:ruleId. */
export async function deleteAgentRule(did: string, ruleId: string): Promise<boolean> {
  const r = await request<{ deleted: boolean }>(
    'DELETE',
    `/v1/agents/${encodeURIComponent(did)}/rules/${encodeURIComponent(ruleId)}`,
  )
  return r.deleted
}

export interface AddAgentBudgetInput {
  cap_wei: string
  expires_in_seconds: number
  target_contract?: string | null
  token?: string | null
}

/** POST /v1/agents/:did/budgets — grant a time-boxed spend allowance. */
export async function addAgentBudget(
  did: string,
  input: AddAgentBudgetInput,
): Promise<AgentBudget> {
  const r = await request<{ budget: AgentBudget }>(
    'POST',
    `/v1/agents/${encodeURIComponent(did)}/budgets`,
    { body: input },
  )
  return r.budget
}

/** DELETE /v1/agents/:did/budgets/:budgetId. */
export async function deactivateAgentBudget(did: string, budgetId: string): Promise<boolean> {
  const r = await request<{ deactivated: boolean }>(
    'DELETE',
    `/v1/agents/${encodeURIComponent(did)}/budgets/${encodeURIComponent(budgetId)}`,
  )
  return r.deactivated
}

/** POST /v1/agents/:did/fund — move funds from the owner's primary wallet. */
export async function fundAgent(did: string, input: FundAgentInput): Promise<TransferResult> {
  return request<TransferResult>('POST', `/v1/agents/${encodeURIComponent(did)}/fund`, {
    body: input,
  })
}

/** POST /v1/agents/:did/sweep — pull funds back to the owner. */
export async function sweepAgent(did: string, input: SweepAgentInput): Promise<TransferResult> {
  return request<TransferResult>('POST', `/v1/agents/${encodeURIComponent(did)}/sweep`, {
    body: input,
  })
}

/** GET /v1/agents/:did/activity — recent on-chain signatures for this agent. */
export async function getAgentActivity(
  did: string,
  signal?: AbortSignal,
): Promise<AgentActivityEvent[]> {
  const r = await request<{ did: string; events: AgentActivityEvent[] }>(
    'GET',
    `/v1/agents/${encodeURIComponent(did)}/activity`,
    { signal },
  )
  return r.events ?? []
}
