'use client'

import { useMemo, useState } from 'react'

export type HarnessMode = 'advisory' | 'shadow' | 'autonomous'
export type HarnessRepairPhase =
  | 'admitted'
  | 'awaiting_approval'
  | 'approved'
  | 'building'
  | 'build_failed'
  | 'built'
  | 'canary_running'
  | 'canary_rejected'
  | 'canary_passed'
  | 'promoting'
  | 'observing'
  | 'promoted'
  | 'reversal_required'
  | 'reversing'
  | 'reverted'
  | 'failed'

export interface HarnessModeAvailabilityProjection {
  advisory: boolean
  shadow: boolean
  autonomous: boolean
  shadow_unavailable_reason?: string | null
  autonomous_unavailable_reason?: string | null
}

export interface HarnessRepairMetricsProjection {
  cases: number
  task_success_basis_points: number
  truthful_completion_basis_points: number
  safety_basis_points: number
  correction_adherence_basis_points: number
  tokens: number
  external_cost_micros: number
  latency_ms: number
  retries: number
  cpu_ms: number
  peak_memory_bytes: number
  disk_bytes: number
}

export interface HarnessRepairItemProjection {
  id: string
  candidate_id: string
  mode: HarnessMode
  phase: HarnessRepairPhase
  headline: string
  summary: string
  metrics: HarnessRepairMetricsProjection
  needs_approval: boolean
  can_retry_current_task: boolean
  can_promote: boolean
  can_reverse: boolean
  created_at: number
  updated_at: number
}

export interface HarnessRepairsProjection {
  availability: HarnessModeAvailabilityProjection
  selected_mode: HarnessMode
  repairs: HarnessRepairItemProjection[]
  safe_error?: string | null
}

export type HarnessRepairAction =
  | { action: 'set_mode'; mode: HarnessMode }
  | { action: 'approve'; operation_id: string }
  | { action: 'promote'; operation_id: string }
  | { action: 'reverse'; operation_id: string }
  | { action: 'retry_current_task'; operation_id: string }
  | { action: 'refresh' }

export interface HarnessRepairActionResult {
  ok: boolean
  safe_error?: string | null
}

export function HarnessRepairsPanel({
  harness,
  onAction,
}: {
  harness: HarnessRepairsProjection | null
  onAction: (action: HarnessRepairAction) => Promise<HarnessRepairActionResult>
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [confirmReverse, setConfirmReverse] = useState<string | null>(null)
  const repairs = harness?.repairs ?? []
  const active = useMemo(
    () => repairs.filter((repair) => !['build_failed', 'canary_rejected', 'promoted', 'reverted', 'failed'].includes(repair.phase)),
    [repairs],
  )

  const run = async (key: string, action: HarnessRepairAction) => {
    if (busy) return
    setBusy(key)
    setNotice(null)
    try {
      const result = await onAction(action)
      if (!result.ok) setNotice(optionalText(result.safe_error) || 'Keith safely rejected that repair action.')
    } catch {
      setNotice('Keith could not reach the repair coordinator. Nothing was changed.')
    } finally {
      setBusy(null)
    }
  }

  return (
    <section className="harness-repairs" aria-label="Harness repairs">
      <header className="harness-header">
        <div>
          <span className="harness-eyebrow">Self-repair</span>
          <h2>Harness repairs</h2>
          <p>Keith can identify problems in the system around the model, test repairs in isolation, and restore the prior version if results regress.</p>
        </div>
        <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => void run('refresh', { action: 'refresh' })}>Refresh</button>
      </header>

      {harness?.safe_error || notice ? <div className="harness-alert" role="alert"><span>{notice || harness?.safe_error}</span>{notice ? <button type="button" aria-label="Dismiss repair error" onClick={() => setNotice(null)}>×</button> : null}</div> : null}

      <fieldset className="harness-modes" disabled={Boolean(busy)}>
        <legend>How Keith may handle repairs</legend>
        {(['advisory', 'shadow', 'autonomous'] as const).map((mode) => {
          const available = harness ? harness.availability[mode] : mode === 'advisory'
          const reason = mode === 'shadow' ? harness?.availability.shadow_unavailable_reason : mode === 'autonomous' ? harness?.availability.autonomous_unavailable_reason : null
          return <label key={mode} data-selected={harness?.selected_mode === mode} data-available={available}>
            <input type="radio" name="harness-mode" value={mode} checked={harness?.selected_mode === mode} disabled={!available || !harness} onChange={() => void run(`mode-${mode}`, { action: 'set_mode', mode })} />
            <span><strong>{modeTitle(mode)}</strong><small>{modeDescription(mode)}</small>{!available && reason ? <em>{reason}</em> : null}</span>
          </label>
        })}
      </fieldset>

      <div className="harness-section-heading">
        <div><h3>Repair activity</h3><p>{active.length ? `${active.length} repair${active.length === 1 ? '' : 's'} in progress` : 'No repair is currently changing the live system.'}</p></div>
        <span>{repairs.length} recorded</span>
      </div>

      {!harness ? <div className="harness-empty"><strong>Repair status is unavailable</strong><p>Keith has not returned an authoritative harness projection yet.</p></div> : null}
      {harness && !repairs.length ? <div className="harness-empty"><strong>No harness repairs yet</strong><p>When Keith finds a reproducible harness defect, its diagnosis and guarded repair will appear here.</p></div> : null}

      <div className="harness-repair-list" aria-live="polite">
        {repairs.map((repair) => <article className="harness-repair-card" key={repair.id}>
          <header>
            <div><span className={`harness-phase harness-phase-${phaseTone(repair.phase)}`}>{phaseLabel(repair.phase)}</span><h3>{repair.headline}</h3><p>{repair.summary}</p></div>
            <span className="harness-mode-chip">{modeTitle(repair.mode)}</span>
          </header>

          <RepairProgress phase={repair.phase} />
          <dl className="harness-metrics" aria-label={`${repair.headline} evaluation metrics`}>
            <Metric label="Task success" value={percent(repair.metrics.task_success_basis_points)} />
            <Metric label="Truthful results" value={percent(repair.metrics.truthful_completion_basis_points)} />
            <Metric label="Safety" value={percent(repair.metrics.safety_basis_points)} />
            <Metric label="Corrections followed" value={percent(repair.metrics.correction_adherence_basis_points)} />
            <Metric label="Evaluation cases" value={String(repair.metrics.cases)} />
            <Metric label="Tokens" value={repair.metrics.tokens.toLocaleString()} />
            <Metric label="External cost" value={`${repair.metrics.external_cost_micros.toLocaleString()} µ-units`} />
            <Metric label="Retries" value={String(repair.metrics.retries)} />
            <Metric label="Latency" value={duration(repair.metrics.latency_ms)} />
            <Metric label="CPU time" value={duration(repair.metrics.cpu_ms)} />
            <Metric label="Peak memory" value={bytes(repair.metrics.peak_memory_bytes)} />
            <Metric label="Peak disk" value={bytes(repair.metrics.disk_bytes)} />
          </dl>

          <div className="harness-boundary" data-authorized={repair.can_retry_current_task}>
            <span aria-hidden="true">{repair.can_retry_current_task ? '✓' : '—'}</span>
            <p><strong>{repair.can_retry_current_task ? 'Current-task retry is authorized' : 'Current-task retry remains blocked'}</strong><small>{repair.can_retry_current_task ? 'This exact repair was promoted or explicitly approved.' : 'Evaluation and shadow testing alone never rerun the user’s task.'}</small></p>
          </div>

          <footer>
            <time dateTime={new Date(repair.updated_at).toISOString()}>Updated {relativeTime(repair.updated_at)}</time>
            <div className="harness-actions">
              {repair.needs_approval ? <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => void run(`approve-${repair.id}`, { action: 'approve', operation_id: repair.id })}>Approve exact repair</button> : null}
              {repair.can_promote ? <button type="button" className="primary-button" disabled={Boolean(busy)} onClick={() => void run(`promote-${repair.id}`, { action: 'promote', operation_id: repair.id })}>Promote repair</button> : null}
              {repair.can_retry_current_task ? <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => void run(`retry-${repair.id}`, { action: 'retry_current_task', operation_id: repair.id })}>Retry current task</button> : null}
              {repair.can_reverse && confirmReverse !== repair.id ? <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => setConfirmReverse(repair.id)}>Restore prior version…</button> : null}
              {repair.can_reverse && confirmReverse === repair.id ? <><button type="button" className="danger-button" disabled={Boolean(busy)} onClick={() => { setConfirmReverse(null); void run(`reverse-${repair.id}`, { action: 'reverse', operation_id: repair.id }) }}>Confirm restore</button><button type="button" className="text-button" onClick={() => setConfirmReverse(null)}>Cancel</button></> : null}
            </div>
          </footer>
        </article>)}
      </div>
    </section>
  )
}

function RepairProgress({ phase }: { phase: HarnessRepairPhase }) {
  const steps = ['Evaluated', 'Built', 'Canary', 'Promoted', 'Observed']
  const progress = phaseProgress(phase)
  return <ol className="harness-progress" aria-label="Repair promotion progress">{steps.map((step, index) => <li key={step} data-state={index < progress ? 'complete' : index === progress ? 'current' : 'pending'}><span aria-hidden="true">{index < progress ? '✓' : index + 1}</span><small>{step}</small></li>)}</ol>
}

function Metric({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd>{value}</dd></div>
}

export function harnessRepairsProjection(value: unknown): HarnessRepairsProjection | null {
  if (!plainObject(value) || hasPrivateMaterial(value) || !plainObject(value.availability) || !mode(value.selected_mode) || !Array.isArray(value.repairs) || value.repairs.length > 2_048 || !validOptionalText(value.safe_error)) return null
  const availability = parseAvailability(value.availability)
  const repairs = value.repairs.map(parseRepair)
  if (!availability || repairs.some((repair) => !repair)) return null
  return { availability, selected_mode: value.selected_mode, repairs: repairs as HarnessRepairItemProjection[], safe_error: optionalText(value.safe_error) }
}

function parseAvailability(value: Record<string, unknown>): HarnessModeAvailabilityProjection | null {
  if (typeof value.advisory !== 'boolean' || typeof value.shadow !== 'boolean' || typeof value.autonomous !== 'boolean' || !validOptionalText(value.shadow_unavailable_reason) || !validOptionalText(value.autonomous_unavailable_reason) || !value.advisory || (value.autonomous && !value.shadow)) return null
  return { advisory: value.advisory, shadow: value.shadow, autonomous: value.autonomous, shadow_unavailable_reason: optionalText(value.shadow_unavailable_reason), autonomous_unavailable_reason: optionalText(value.autonomous_unavailable_reason) }
}

function parseRepair(value: unknown): HarnessRepairItemProjection | null {
  if (!plainObject(value) || !safeId(value.id) || !safeId(value.candidate_id) || !mode(value.mode) || !phase(value.phase) || !safeText(value.headline) || !safeText(value.summary) || !plainObject(value.metrics) || typeof value.needs_approval !== 'boolean' || typeof value.can_retry_current_task !== 'boolean' || typeof value.can_promote !== 'boolean' || typeof value.can_reverse !== 'boolean' || !safeInteger(value.created_at) || !safeInteger(value.updated_at) || value.updated_at < value.created_at) return null
  const metrics = parseMetrics(value.metrics)
  if (!metrics || (value.can_promote && value.phase !== 'canary_passed') || (value.can_reverse && !['promoted', 'reversal_required'].includes(value.phase)) || (value.can_retry_current_task && !['approved', 'building', 'built', 'canary_running', 'canary_passed', 'promoting', 'observing', 'promoted'].includes(value.phase))) return null
  return { id: value.id, candidate_id: value.candidate_id, mode: value.mode, phase: value.phase, headline: value.headline, summary: value.summary, metrics, needs_approval: value.needs_approval, can_retry_current_task: value.can_retry_current_task, can_promote: value.can_promote, can_reverse: value.can_reverse, created_at: value.created_at, updated_at: value.updated_at }
}

function parseMetrics(value: Record<string, unknown>): HarnessRepairMetricsProjection | null {
  const keys = ['cases', 'task_success_basis_points', 'truthful_completion_basis_points', 'safety_basis_points', 'correction_adherence_basis_points', 'tokens', 'external_cost_micros', 'latency_ms', 'retries', 'cpu_ms', 'peak_memory_bytes', 'disk_bytes'] as const
  if (keys.some((key) => !safeInteger(value[key])) || value.cases === 0 || keys.slice(1, 5).some((key) => Number(value[key]) > 10_000)) return null
  return Object.fromEntries(keys.map((key) => [key, value[key]])) as unknown as HarnessRepairMetricsProjection
}

function hasPrivateMaterial(value: unknown, depth = 0): boolean {
  if (depth > 12) return true
  if (typeof value === 'string') return containsSecretMarker(value)
  if (Array.isArray(value)) return value.some((item) => hasPrivateMaterial(item, depth + 1))
  if (!plainObject(value)) return false
  return Object.entries(value).some(([key, item]) => privateKey(key) || hasPrivateMaterial(item, depth + 1))
}

function privateKey(key: string): boolean {
  const normalized = key.toLowerCase().replaceAll('-', '_')
  return /(^|_)(password|secret|credential|authorization|access_token|refresh_token|api_key|expected_output|leakage_canary|held_out|evaluator|case_id)($|_)/.test(normalized)
}

function containsSecretMarker(value: string): boolean {
  return /(?:api[_-]?key|password|secret|access[_-]?token|refresh[_-]?token|authorization)\s*[:=]/i.test(value)
    || /\bbearer\s+[a-z0-9._~+\/-]{8,}/i.test(value)
    || /\b(?:sk|gh[opusr])-[a-z0-9_-]{16,}\b/i.test(value)
    || /\bAKIA[A-Z0-9]{16}\b/.test(value)
    || /\beyJ[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\b/.test(value)
}

function plainObject(value: unknown): value is Record<string, any> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) && Object.getPrototypeOf(value) === Object.prototype
}

function safeText(value: unknown): value is string { return typeof value === 'string' && value.trim().length > 0 && value.length <= 4_096 && !containsSecretMarker(value) }
function validOptionalText(value: unknown): boolean { return value === undefined || value === null || safeText(value) }
function optionalText(value: unknown): string | null { return safeText(value) ? value : null }
function safeId(value: unknown): value is string { return typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$/.test(value) }
function safeInteger(value: unknown): value is number { return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 }
function mode(value: unknown): value is HarnessMode { return ['advisory', 'shadow', 'autonomous'].includes(String(value)) }
function phase(value: unknown): value is HarnessRepairPhase { return ['admitted', 'awaiting_approval', 'approved', 'building', 'build_failed', 'built', 'canary_running', 'canary_rejected', 'canary_passed', 'promoting', 'observing', 'promoted', 'reversal_required', 'reversing', 'reverted', 'failed'].includes(String(value)) }
function modeTitle(value: HarnessMode): string { return value === 'advisory' ? 'Advise only' : value === 'shadow' ? 'Shadow test' : 'Autonomous repair' }
function modeDescription(value: HarnessMode): string { return value === 'advisory' ? 'Explain the repair and wait for approval before execution.' : value === 'shadow' ? 'Build and test safely, then wait before live promotion.' : 'Build, canary, promote, observe, and reverse within configured safety limits.' }
function phaseLabel(value: HarnessRepairPhase): string { return value.replaceAll('_', ' ') }
function phaseTone(value: HarnessRepairPhase): string { return ['promoted'].includes(value) ? 'success' : ['build_failed', 'canary_rejected', 'failed', 'reversal_required'].includes(value) ? 'danger' : ['reverted'].includes(value) ? 'neutral' : 'running' }
function percent(value: number): string { return `${(value / 100).toFixed(value % 100 ? 2 : 0)}%` }
function duration(value: number): string { return value < 1_000 ? `${value} ms` : `${(value / 1_000).toFixed(1)} s` }
function bytes(value: number): string { if (value < 1_024) return `${value} B`; if (value < 1_048_576) return `${(value / 1_024).toFixed(1)} KB`; return `${(value / 1_048_576).toFixed(1)} MB` }
function relativeTime(value: number): string { const delta = Math.max(0, Date.now() - value); if (delta < 60_000) return 'just now'; if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m ago`; if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h ago`; return `${Math.floor(delta / 86_400_000)}d ago` }
function phaseProgress(value: HarnessRepairPhase): number { if (['admitted', 'awaiting_approval', 'approved', 'building', 'build_failed'].includes(value)) return value === 'building' || value === 'build_failed' ? 1 : 0; if (value === 'built' || value === 'canary_running' || value === 'canary_rejected') return 2; if (value === 'canary_passed' || value === 'promoting') return 3; if (value === 'observing' || value === 'reversal_required' || value === 'reversing') return 4; return 5 }
