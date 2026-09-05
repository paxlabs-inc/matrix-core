'use client'

import { useEffect, useMemo, useState } from 'react'

export type TeachingAction =
  | { action: 'start_recording'; title: string }
  | { action: 'pause_recording' | 'resume_recording' | 'complete_recording'; demonstration_id: string }
  | { action: 'add_narration'; demonstration_id: string; narration: string }
  | { action: 'compile_recipe'; demonstration_id: string }
  | { action: 'label_parameter'; recipe_id: string; revision: number; input_name: string; label: string }
  | { action: 'remove_step'; recipe_id: string; revision: number; step_id: string }
  | { action: 'correct_target'; recipe_id: string; revision: number; step_id: string; role: string; accessible_name: string }
  | { action: 'add_confirmation'; recipe_id: string; revision: number; step_id: string; reason: string }
  | { action: 'replay_shadow'; recipe_id: string; revision: number }
  | { action: 'replay_checkpoint'; recipe_id: string; revision: number; checkpoint: string }
  | { action: 'accept_recipe'; recipe_id: string; revision: number }
  | { action: 'publish_recipe'; recipe_id: string; revision: number; skill_id: string }
  | { action: 'rollback_recipe'; recipe_id: string; revision: number; target_revision: number }
  | { action: 'delete_demonstration'; demonstration_id: string }

export interface TeachingActionResult {
  ok: boolean
  safe_error?: string | null
}

export interface TeachingTimelineEvent {
  sequence: number
  elapsed_ms: number
  kind: string
  summary: string
  control_owner: 'keith_control' | 'user_control' | 'paused'
  redacted: boolean
}

export interface TeachingRecordingProjection {
  id: string
  title: string
  state: 'recording' | 'paused' | 'completed'
  elapsed_ms: number
  event_count: number
  control_owner: 'keith_control' | 'user_control' | 'paused'
  timeline: TeachingTimelineEvent[]
}

export interface TeachingParameterProjection {
  name: string
  label: string
  kind: 'text' | 'url' | 'file' | 'credential' | 'choice'
  required: boolean
}

export interface TeachingStepProjection {
  id: string
  title: string
  target_role?: string | null
  target_name?: string | null
  checkpoint?: string | null
  approval_reason?: string | null
  expected: string[]
}

export interface TeachingVersionProjection {
  revision: number
  parent_revision?: number | null
  rollback_of?: number | null
  created_at: number
  active: boolean
}

export interface TeachingComparisonProjection {
  state: 'ready' | 'running' | 'passed' | 'failed' | 'cancelled'
  mode?: 'shadow' | 'explicit_test' | null
  checks: Array<{ description: string; passed: boolean }>
  suggested_targets: Array<{ role: string; accessible_name: string }>
}

export interface TeachingRecipeProjection {
  id: string
  title: string
  description: string
  revision: number
  inputs: TeachingParameterProjection[]
  steps: TeachingStepProjection[]
  completion: string[]
  checks_passed: number
  checks_total: number
  accepted: boolean
  published_skill_id?: string | null
  versions: TeachingVersionProjection[]
  replay: TeachingComparisonProjection
}

export interface TeachingProjection {
  recording: TeachingRecordingProjection | null
  recipe: TeachingRecipeProjection | null
  safe_error?: string | null
}

export interface TeachTaskPanelProps {
  teaching: TeachingProjection | null
  onAction: (action: TeachingAction) => Promise<TeachingActionResult>
}

export function TeachTaskPanel({ teaching, onAction }: TeachTaskPanelProps) {
  const [busy, setBusy] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [recordingTitle, setRecordingTitle] = useState('')
  const [narration, setNarration] = useState('')
  const [parameterLabels, setParameterLabels] = useState<Record<string, string>>({})
  const [targetStep, setTargetStep] = useState<string | null>(null)
  const [targetRole, setTargetRole] = useState('')
  const [targetName, setTargetName] = useState('')
  const [checkpoint, setCheckpoint] = useState('')
  const [publishAccepted, setPublishAccepted] = useState(false)
  const [skillId, setSkillId] = useState('')
  const [confirmDelete, setConfirmDelete] = useState(false)
  const recording = teaching?.recording
  const recipe = teaching?.recipe

  useEffect(() => {
    setParameterLabels(Object.fromEntries(recipe?.inputs.map((input) => [input.name, input.label]) || []))
    setSkillId(recipe?.published_skill_id || recipe?.title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 64) || '')
    setPublishAccepted(false)
    setConfirmDelete(false)
  }, [recipe?.id, recipe?.revision])

  const checkpoints = useMemo(() => recipe?.steps.flatMap((step) => step.checkpoint ? [step.checkpoint] : []) || [], [recipe])

  const mutate = async (key: string, action: TeachingAction) => {
    if (busy) return false
    setBusy(key)
    setNotice(null)
    try {
      const result = await onAction(action)
      if (!result.ok) {
        setNotice(optionalText(result.safe_error) || 'Keith safely rejected that teaching action.')
        return false
      }
      return true
    } catch {
      setNotice('Keith could not reach the teaching service. Nothing was changed.')
      return false
    } finally {
      setBusy(null)
    }
  }

  const start = async () => {
    const title = recordingTitle.trim()
    if (!title) return setNotice('Give this task a short name before recording.')
    if (await mutate('start', { action: 'start_recording', title })) setRecordingTitle('')
  }

  const saveNarration = async () => {
    if (!recording || !narration.trim()) return
    if (await mutate('narration', { action: 'add_narration', demonstration_id: recording.id, narration: narration.trim() })) setNarration('')
  }

  const saveTarget = async (step: TeachingStepProjection) => {
    if (!recipe || !targetRole.trim() || !targetName.trim()) return setNotice('A corrected target needs both a role and a visible name.')
    if (await mutate(`target-${step.id}`, {
      action: 'correct_target', recipe_id: recipe.id, revision: recipe.revision, step_id: step.id,
      role: targetRole.trim(), accessible_name: targetName.trim(),
    })) {
      setTargetStep(null)
      setTargetRole('')
      setTargetName('')
    }
  }

  return (
    <section className="teach-task" aria-label="Teach Keith a task">
      <header className="teach-task-header">
        <div><span className="teach-task-eyebrow">Keith Computer</span><h2>Teach a task</h2><p>Demonstrate it once, review the readable procedure, then test it before publishing.</p></div>
        {recording ? <span className={`teach-recording-state ${recording.state}`}><i aria-hidden="true" />{friendly(recording.state)}</span> : null}
      </header>

      {teaching?.safe_error || notice ? <div className="teach-safe-error" role="alert"><span>{notice || teaching?.safe_error}</span>{notice ? <button type="button" onClick={() => setNotice(null)} aria-label="Dismiss teaching error">×</button> : null}</div> : null}

      {!recording ? (
        <div className="teach-start-card">
          <div><strong>Record a new demonstration</strong><p>Screen evidence and input timing are synchronized. Sensitive fields become named parameters before they are saved.</p></div>
          <div className="teach-inline-form"><label>Task name<input value={recordingTitle} maxLength={128} placeholder="Submit the weekly report" onChange={(event) => setRecordingTitle(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') void start() }} /></label><button type="button" className="primary-button" disabled={Boolean(busy)} onClick={() => void start()}>Start recording</button></div>
        </div>
      ) : (
        <div className="teach-recording-card">
          <div className="teach-recording-summary"><div><strong>{recording.title}</strong><span>{formatElapsed(recording.elapsed_ms)} · {recording.event_count} captured events · {ownerLabel(recording.control_owner)}</span></div><div className="teach-row-actions">
            {recording.state === 'recording' ? <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => void mutate('pause', { action: 'pause_recording', demonstration_id: recording.id })}>Pause</button> : null}
            {recording.state === 'paused' ? <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => void mutate('resume', { action: 'resume_recording', demonstration_id: recording.id })}>Resume</button> : null}
            {recording.state !== 'completed' ? <button type="button" className="primary-button" disabled={Boolean(busy)} onClick={() => void mutate('complete', { action: 'complete_recording', demonstration_id: recording.id })}>Finish recording</button> : recipe ? <span className="teach-current">Procedure created</span> : <button type="button" className="primary-button" disabled={Boolean(busy)} onClick={() => void mutate('compile', { action: 'compile_recipe', demonstration_id: recording.id })}>Create procedure</button>}
          </div></div>
          {recording.state !== 'completed' ? <div className="teach-narration"><label>Optional narration<textarea value={narration} maxLength={2048} placeholder="Explain what matters about this step…" onChange={(event) => setNarration(event.target.value)} /></label><button type="button" className="secondary-button" disabled={Boolean(busy) || !narration.trim()} onClick={() => void saveNarration()}>Add note</button></div> : null}
          <ol className="teach-timeline" aria-label="Recorded timeline">{recording.timeline.length ? recording.timeline.map((event) => <li key={event.sequence}><span className="teach-timeline-time">{formatElapsed(event.elapsed_ms)}</span><span className="teach-timeline-dot" aria-hidden="true" /><div><strong>{friendly(event.kind)}</strong><p>{event.summary}</p><small>{ownerLabel(event.control_owner)}{event.redacted ? ' · sensitive value replaced' : ''}</small></div></li>) : <li className="teach-empty">Actions will appear here as you demonstrate them.</li>}</ol>
        </div>
      )}

      {recipe ? <div className="teach-recipe">
        <div className="teach-section-heading"><div><span className="teach-task-eyebrow">Editable procedure · revision {recipe.revision}</span><h3>{recipe.title}</h3><p>{recipe.description}</p></div>{recipe.published_skill_id ? <span className="teach-published">Published as {recipe.published_skill_id}</span> : null}</div>

        <section className="teach-parameters" aria-labelledby="teach-parameters-title"><div className="teach-subheading"><h4 id="teach-parameters-title">Inputs</h4><span>{recipe.inputs.length} parameter{recipe.inputs.length === 1 ? '' : 's'}</span></div>{recipe.inputs.length ? <div className="teach-parameter-grid">{recipe.inputs.map((input) => <div key={input.name}><span>{friendly(input.kind)}{input.required ? ' · required' : ''}</span><input aria-label={`Label for ${input.name}`} maxLength={128} value={parameterLabels[input.name] ?? input.label} onChange={(event) => setParameterLabels((labels) => ({ ...labels, [input.name]: event.target.value }))} /><button type="button" className="text-button" disabled={Boolean(busy) || (parameterLabels[input.name] ?? input.label).trim() === input.label} onClick={() => void mutate(`label-${input.name}`, { action: 'label_parameter', recipe_id: recipe.id, revision: recipe.revision, input_name: input.name, label: (parameterLabels[input.name] ?? input.label).trim() })}>Save label</button></div>)}</div> : <p className="teach-empty">This procedure has no runtime inputs.</p>}</section>

        <section aria-labelledby="teach-procedure-title"><div className="teach-subheading"><h4 id="teach-procedure-title">Procedure</h4><span>{recipe.steps.length} step{recipe.steps.length === 1 ? '' : 's'}</span></div><ol className="teach-steps">{recipe.steps.map((step, index) => <li key={step.id}><span className="teach-step-number">{index + 1}</span><div className="teach-step-body"><strong>{step.title}</strong>{step.target_name ? <p>Target: {step.target_role} “{step.target_name}”</p> : null}<ul>{step.expected.map((expected) => <li key={expected}>Check: {expected}</li>)}</ul>{step.checkpoint ? <small>Checkpoint: {step.checkpoint}</small> : null}{step.approval_reason ? <div className="teach-confirmation">Confirmation required: {step.approval_reason}</div> : null}
              {targetStep === step.id ? <div className="teach-target-editor"><label>Role<input value={targetRole} maxLength={128} onChange={(event) => setTargetRole(event.target.value)} /></label><label>Visible name<input value={targetName} maxLength={1024} onChange={(event) => setTargetName(event.target.value)} /></label><button type="button" className="primary-button" disabled={Boolean(busy)} onClick={() => void saveTarget(step)}>Save correction</button><button type="button" className="text-button" onClick={() => setTargetStep(null)}>Cancel</button></div> : null}
              <div className="teach-row-actions"><button type="button" className="text-button" onClick={() => { setTargetStep(step.id); setTargetRole(step.target_role || ''); setTargetName(step.target_name || '') }}>Correct target</button>{!step.approval_reason ? <button type="button" className="text-button" disabled={Boolean(busy)} onClick={() => void mutate(`confirm-${step.id}`, { action: 'add_confirmation', recipe_id: recipe.id, revision: recipe.revision, step_id: step.id, reason: `Confirm before: ${step.title}` })}>Add confirmation</button> : null}<button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void mutate(`remove-${step.id}`, { action: 'remove_step', recipe_id: recipe.id, revision: recipe.revision, step_id: step.id })}>Remove step</button></div>
            </div></li>)}</ol></section>

        <section className="teach-replay" aria-labelledby="teach-replay-title"><div className="teach-subheading"><div><h4 id="teach-replay-title">Test replay</h4><p>Replays stay in shadow or an explicit test context until every declared check passes.</p></div><span className={`teach-replay-state ${recipe.replay.state}`}>{friendly(recipe.replay.state)}</span></div><div className="teach-replay-controls"><button type="button" className="primary-button" disabled={Boolean(busy) || recipe.replay.state === 'running'} onClick={() => void mutate('shadow-replay', { action: 'replay_shadow', recipe_id: recipe.id, revision: recipe.revision })}>Run shadow replay</button><select aria-label="Replay checkpoint" value={checkpoint} onChange={(event) => setCheckpoint(event.target.value)}><option value="">Choose checkpoint</option>{checkpoints.map((value) => <option key={value} value={value}>{friendly(value)}</option>)}</select><button type="button" className="secondary-button" disabled={Boolean(busy) || !checkpoint} onClick={() => void mutate('checkpoint-replay', { action: 'replay_checkpoint', recipe_id: recipe.id, revision: recipe.revision, checkpoint })}>Replay from checkpoint</button></div>
          {recipe.replay.checks.length ? <ul className="teach-checks">{recipe.replay.checks.map((check) => <li key={check.description} data-passed={check.passed}><span aria-hidden="true">{check.passed ? '✓' : '!'}</span>{check.description}</li>)}</ul> : <p className="teach-empty">No replay result yet.</p>}
          {recipe.replay.suggested_targets.length ? <div className="teach-suggestions"><strong>The layout changed. Suggested targets:</strong>{recipe.replay.suggested_targets.map((target) => <button type="button" key={`${target.role}-${target.accessible_name}`} onClick={() => { setTargetStep(recipe.steps.find((step) => step.target_role === target.role)?.id || recipe.steps[0]?.id || null); setTargetRole(target.role); setTargetName(target.accessible_name) }}>{target.role}: {target.accessible_name}</button>)}</div> : null}
        </section>

        <section className="teach-publish" aria-labelledby="teach-publish-title"><div className="teach-subheading"><div><h4 id="teach-publish-title">Review and publish</h4><p>{recipe.checks_passed} of {recipe.checks_total} declared checks passed.</p></div></div><div className="teach-readable"><strong>{recipe.title}</strong><p>{recipe.description}</p><ol>{recipe.steps.map((step) => <li key={step.id}>{step.title}{step.approval_reason ? <small> — asks for confirmation</small> : null}</li>)}</ol><p><strong>Done when:</strong> {recipe.completion.join('; ')}</p></div><label className="teach-accept"><input type="checkbox" checked={publishAccepted} onChange={(event) => setPublishAccepted(event.target.checked)} />I reviewed this procedure and accept what Keith will do.</label><div className="teach-publish-actions"><label>Skill name<input value={skillId} pattern="[a-zA-Z0-9_-]+" maxLength={96} onChange={(event) => setSkillId(event.target.value)} /></label>{!recipe.accepted ? <button type="button" className="secondary-button" disabled={Boolean(busy) || recipe.checks_passed !== recipe.checks_total || !publishAccepted} onClick={() => void mutate('accept', { action: 'accept_recipe', recipe_id: recipe.id, revision: recipe.revision })}>Accept procedure</button> : null}<button type="button" className="primary-button" disabled={Boolean(busy) || !recipe.accepted || !publishAccepted || !safeId(skillId)} onClick={() => void mutate('publish', { action: 'publish_recipe', recipe_id: recipe.id, revision: recipe.revision, skill_id: skillId })}>Publish as skill</button></div></section>

        <section className="teach-history" aria-labelledby="teach-history-title"><div className="teach-subheading"><h4 id="teach-history-title">Version history</h4><span>{recipe.versions.length} revisions</span></div><ul>{recipe.versions.map((version) => <li key={version.revision}><span><strong>Revision {version.revision}</strong><small>{version.rollback_of ? `Rollback of revision ${version.rollback_of}` : version.parent_revision ? `From revision ${version.parent_revision}` : 'Original compilation'}</small></span>{version.active ? <span className="teach-current">Current</span> : <button type="button" className="text-button" disabled={Boolean(busy)} onClick={() => void mutate(`rollback-${version.revision}`, { action: 'rollback_recipe', recipe_id: recipe.id, revision: recipe.revision, target_revision: version.revision })}>Restore</button>}</li>)}</ul></section>
      </div> : null}

      {recording ? <footer className="teach-delete"><div><strong>Delete recording and derived procedures</strong><p>This also removes recording media that is not shared by another recording.</p></div>{confirmDelete ? <div className="teach-row-actions"><button type="button" className="danger-button" disabled={Boolean(busy)} onClick={() => void mutate('delete', { action: 'delete_demonstration', demonstration_id: recording.id })}>Delete permanently</button><button type="button" className="text-button" onClick={() => setConfirmDelete(false)}>Cancel</button></div> : <button type="button" className="danger-text" onClick={() => setConfirmDelete(true)}>Delete recording…</button>}</footer> : null}
    </section>
  )
}

export function teachingProjection(value: unknown): TeachingProjection | null {
  if (!plainObject(value) || hasForbiddenKeys(value) || !validOptionalText(value.safe_error)) return null
  const recording = value.recording === null ? null : parseRecording(value.recording)
  const recipe = value.recipe === null ? null : parseRecipe(value.recipe)
  if ((value.recording !== null && !recording) || (value.recipe !== null && !recipe)) return null
  return { recording, recipe, safe_error: optionalText(value.safe_error) }
}

function parseRecording(value: unknown): TeachingRecordingProjection | null {
  if (!plainObject(value) || !safeId(value.id) || !safeText(value.title) || !['recording', 'paused', 'completed'].includes(String(value.state)) || !safeNumber(value.elapsed_ms) || !safeNumber(value.event_count) || !owner(value.control_owner) || !Array.isArray(value.timeline) || value.timeline.length > 10000) return null
  const timeline = value.timeline.map(parseTimeline)
  if (timeline.some((event) => !event)) return null
  return { id: value.id, title: value.title, state: value.state as TeachingRecordingProjection['state'], elapsed_ms: value.elapsed_ms, event_count: value.event_count, control_owner: value.control_owner, timeline: timeline as TeachingTimelineEvent[] }
}

function parseTimeline(value: unknown): TeachingTimelineEvent | null {
  if (!plainObject(value) || !safeNumber(value.sequence) || !safeNumber(value.elapsed_ms) || !safeText(value.kind) || !safeText(value.summary) || !owner(value.control_owner) || typeof value.redacted !== 'boolean') return null
  return { sequence: value.sequence, elapsed_ms: value.elapsed_ms, kind: value.kind, summary: value.summary, control_owner: value.control_owner, redacted: value.redacted }
}

function parseRecipe(value: unknown): TeachingRecipeProjection | null {
  if (!plainObject(value) || !safeId(value.id) || !safeText(value.title) || !safeText(value.description) || !positiveRevision(value.revision) || !Array.isArray(value.inputs) || !Array.isArray(value.steps) || !Array.isArray(value.completion) || !Array.isArray(value.versions) || !safeNumber(value.checks_passed) || !safeNumber(value.checks_total) || value.checks_passed > value.checks_total || typeof value.accepted !== 'boolean' || !validOptionalId(value.published_skill_id)) return null
  const inputs = value.inputs.map(parseInput); const steps = value.steps.map(parseStep); const versions = value.versions.map(parseVersion); const replay = parseReplay(value.replay)
  if (inputs.some((item) => !item) || steps.some((item) => !item) || versions.some((item) => !item) || !replay || inputs.length > 256 || steps.length > 2048 || versions.length > 128 || value.completion.length > 128 || !value.completion.every(safeText)) return null
  return { id: value.id, title: value.title, description: value.description, revision: value.revision, inputs: inputs as TeachingParameterProjection[], steps: steps as TeachingStepProjection[], completion: value.completion as string[], checks_passed: value.checks_passed, checks_total: value.checks_total, accepted: value.accepted, published_skill_id: optionalId(value.published_skill_id), versions: versions as TeachingVersionProjection[], replay }
}

function parseInput(value: unknown): TeachingParameterProjection | null { if (!plainObject(value) || !safeId(value.name) || !safeText(value.label) || !['text', 'url', 'file', 'credential', 'choice'].includes(String(value.kind)) || typeof value.required !== 'boolean') return null; return value as unknown as TeachingParameterProjection }
function parseStep(value: unknown): TeachingStepProjection | null { if (!plainObject(value) || !safeId(value.id) || !safeText(value.title) || !Array.isArray(value.expected) || value.expected.length > 32 || !value.expected.every(safeText) || !validOptionalText(value.target_role) || !validOptionalText(value.target_name) || !validOptionalId(value.checkpoint) || !validOptionalText(value.approval_reason)) return null; return { id: value.id, title: value.title, target_role: optionalText(value.target_role), target_name: optionalText(value.target_name), checkpoint: optionalId(value.checkpoint), approval_reason: optionalText(value.approval_reason), expected: value.expected as string[] } }
function parseVersion(value: unknown): TeachingVersionProjection | null { if (!plainObject(value) || !positiveRevision(value.revision) || !safeNumber(value.created_at) || typeof value.active !== 'boolean') return null; const parent = optionalRevision(value.parent_revision); const rollback = optionalRevision(value.rollback_of); if (parent === undefined || rollback === undefined) return null; return { revision: value.revision, parent_revision: parent, rollback_of: rollback, created_at: value.created_at, active: value.active } }
function parseReplay(value: unknown): TeachingComparisonProjection | null { if (!plainObject(value) || !['ready', 'running', 'passed', 'failed', 'cancelled'].includes(String(value.state)) || !Array.isArray(value.checks) || !Array.isArray(value.suggested_targets) || value.checks.length > 128 || value.suggested_targets.length > 8) return null; const checks = value.checks.filter((check) => plainObject(check) && safeText(check.description) && typeof check.passed === 'boolean') as Array<{ description: string; passed: boolean }>; const targets = value.suggested_targets.filter((target) => plainObject(target) && safeText(target.role) && safeText(target.accessible_name)) as Array<{ role: string; accessible_name: string }>; if (checks.length !== value.checks.length || targets.length !== value.suggested_targets.length) return null; const mode = value.mode === null || value.mode === undefined ? null : ['shadow', 'explicit_test'].includes(String(value.mode)) ? value.mode as 'shadow' | 'explicit_test' : undefined; if (mode === undefined) return null; return { state: value.state as TeachingComparisonProjection['state'], mode, checks, suggested_targets: targets } }

function plainObject(value: unknown): value is Record<string, unknown> { return Boolean(value) && typeof value === 'object' && !Array.isArray(value) }
function safeId(value: unknown): value is string { return typeof value === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(value) }
function safeText(value: unknown): value is string { if (typeof value !== 'string' || !value.trim() || value.length > 4096 || /[\u0000-\u0008\u000b\u000c\u000e-\u001f]/.test(value)) return false; const normalized = value.toLowerCase(); return !['authorization: bearer', 'access_token', 'refresh_token', 'api_key', 'password=', 'secret=', 'sk-'].some((marker) => normalized.includes(marker)) }
function optionalText(value: unknown): string | null { return value === null || value === undefined ? null : safeText(value) ? value : null }
function optionalId(value: unknown): string | null { return value === null || value === undefined ? null : safeId(value) ? value : null }
function validOptionalText(value: unknown): boolean { return value === null || value === undefined || safeText(value) }
function validOptionalId(value: unknown): boolean { return value === null || value === undefined || safeId(value) }
function safeNumber(value: unknown): value is number { return Number.isSafeInteger(value) && Number(value) >= 0 }
function positiveRevision(value: unknown): value is number { return safeNumber(value) && value > 0 }
function optionalRevision(value: unknown): number | null | undefined { return value === null || value === undefined ? null : positiveRevision(value) ? value : undefined }
function owner(value: unknown): value is TeachingTimelineEvent['control_owner'] { return ['keith_control', 'user_control', 'paused'].includes(String(value)) }
function ownerLabel(value: TeachingTimelineEvent['control_owner']): string { return value === 'user_control' ? 'You had control' : value === 'keith_control' ? 'Keith had control' : 'Input was paused' }
function friendly(value: string): string { return value.replaceAll('_', ' ').replace(/\b\w/g, (letter) => letter.toUpperCase()) }
function formatElapsed(milliseconds: number): string { const seconds = Math.floor(milliseconds / 1000); return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}` }
function hasForbiddenKeys(value: unknown): boolean { if (Array.isArray(value)) return value.some(hasForbiddenKeys); if (!plainObject(value)) return false; return Object.entries(value).some(([key, child]) => /(^|_)(api_?key|access_?token|refresh_?token|secret|password_value|credential_value|opaque_handle|stream_url|mcp_endpoint)$/i.test(key) || hasForbiddenKeys(child)) }
