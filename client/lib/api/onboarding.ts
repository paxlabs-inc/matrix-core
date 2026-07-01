/**
 * Onboarding + beta-launch API — mirrors the JWT-authed router endpoints
 * (POST /invite/redeem, GET/PUT /consent, POST /disclosure/ack,
 * POST /reports, GET /provision/status) and the daemon profile endpoint
 * (GET/PUT /profile). All calls go through the router origin via apiFetch,
 * which attaches the Supabase Bearer token automatically.
 */
import { apiFetch } from '@/lib/api/client'

// --- Invite ---------------------------------------------------------------

export interface RedeemResponse {
  status: string
}

export async function redeemInvite(code: string, signal?: AbortSignal): Promise<RedeemResponse> {
  return apiFetch<RedeemResponse>('/invite/redeem', {
    method: 'POST',
    body: JSON.stringify({ code }),
    signal,
    retries: 0,
  })
}

// --- Consent ---------------------------------------------------------------

export interface ConsentResponse {
  user_id: string
  training_opt_in: boolean
  policy_version: string
  decided_at: string | null
  has_consented: boolean
}

export async function getConsent(signal?: AbortSignal): Promise<ConsentResponse> {
  return apiFetch<ConsentResponse>('/consent', { signal })
}

export async function putConsent(
  trainingOptIn: boolean,
  policyVersion: string,
  signal?: AbortSignal,
): Promise<ConsentResponse> {
  return apiFetch<ConsentResponse>('/consent', {
    method: 'PUT',
    body: JSON.stringify({ training_opt_in: trainingOptIn, policy_version: policyVersion }),
    signal,
    retries: 0,
  })
}

// --- Disclosure ack --------------------------------------------------------

export async function ackDisclosure(
  disclosureVersion: string,
  signal?: AbortSignal,
): Promise<{ status: string }> {
  return apiFetch<{ status: string }>('/disclosure/ack', {
    method: 'POST',
    body: JSON.stringify({ disclosure_version: disclosureVersion }),
    signal,
    retries: 0,
  })
}

// --- Bug reports -----------------------------------------------------------

export interface ReportRequest {
  message: string
  context?: Record<string, unknown>
  attachment_ref?: string
}

export interface ReportResponse {
  id: number
  status: string
  message: string
}

export async function submitReport(
  req: ReportRequest,
  signal?: AbortSignal,
): Promise<ReportResponse> {
  return apiFetch<ReportResponse>('/reports', {
    method: 'POST',
    body: JSON.stringify(req),
    signal,
    retries: 0,
  })
}

// --- Provisioning status ---------------------------------------------------

export interface ProvisionStatusResponse {
  state: 'none' | 'provisioning' | 'active' | 'failed' | string
  user_id: string
}

export async function getProvisionStatus(signal?: AbortSignal): Promise<ProvisionStatusResponse> {
  return apiFetch<ProvisionStatusResponse>('/provision/status', { signal })
}

// --- Profile (daemon, proxied through router) ------------------------------

export interface ProfileResponse {
  preferred_name: string
  agent_name: string
  expertise_domains: string[]
  uri?: string
}

export interface ProfileRequest {
  preferred_name: string
  agent_name: string
  expertise_domains: string[]
}

export async function getProfile(signal?: AbortSignal): Promise<ProfileResponse> {
  return apiFetch<ProfileResponse>('/profile', { signal })
}

export async function putProfile(
  req: ProfileRequest,
  signal?: AbortSignal,
): Promise<ProfileResponse> {
  return apiFetch<ProfileResponse>('/profile', {
    method: 'PUT',
    body: JSON.stringify(req),
    signal,
    retries: 0,
  })
}
