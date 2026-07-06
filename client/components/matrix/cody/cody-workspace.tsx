'use client'

/**
 * Cody Workspace — the tier dispatcher (design.body.md, per-tier reference
 * target, locked 2026-07-05). One engine truth, three information
 * architectures:
 *
 *   Prototype -> the Emergent-style hero + chat|preview split (layout-prototype)
 *   Engineer  -> the Leap-style rail + tabbed workspace + console (layout-engineer)
 *   Architect -> the Replit-style rail | tabbed center | Library (layout-architect)
 *
 * Every layout composes the same shared run parts (cody-run-parts) over the
 * same CodyRun model — tiers differ ONLY in disclosure and register, never in
 * the run model or the constitution. Adopt each reference's IA and interaction
 * patterns only, never its chrome: separation by background tone, no border
 * strokes, no gradients, no glow, no emojis.
 */
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { CodyNewRun } from '@/components/matrix/cody/cody-new-run'
import { PrototypeHero, PrototypeLayout } from '@/components/matrix/cody/layout-prototype'
import { EngineerLayout } from '@/components/matrix/cody/layout-engineer'
import { ArchitectLayout } from '@/components/matrix/cody/layout-architect'
import type { Tier } from '@/components/matrix/cody/tiering'
import type { CodyMode } from '@/lib/api/cody'
import type { CodyRun } from '@/hooks/api/useCody'
import type { WorkspaceActions } from '@/components/matrix/cody/cody-run-parts'

export { composerIntent, type ComposerIntent } from '@/components/matrix/cody/cody-run-parts'
export type { WorkspaceActions } from '@/components/matrix/cody/cody-run-parts'

export function CodyWorkspace({
  run,
  tier,
  connecting,
  projectID,
  mode,
  actions,
}: {
  run: CodyRun | null
  tier: Tier
  connecting: boolean
  projectID?: string
  mode: CodyMode
  actions: WorkspaceActions
}) {
  if (!run) {
    if (tier.previewHero) {
      return <PrototypeHero projectID={projectID} onStart={actions.onStartRun} />
    }
    return (
      <CodyNewRun
        projectID={projectID}
        outcome={tier.register === 'outcome'}
        onStart={actions.onStartRun}
      />
    )
  }

  if (!run.goal && connecting) {
    return (
      <div className="flex h-full items-center justify-center">
        <CodyLoader variant="ring" label="Starting…" />
      </div>
    )
  }

  if (mode === 'prototype') {
    return <PrototypeLayout run={run} tier={tier} projectID={projectID} actions={actions} />
  }
  if (mode === 'architect') {
    return <ArchitectLayout run={run} tier={tier} projectID={projectID} actions={actions} />
  }
  return <EngineerLayout run={run} tier={tier} projectID={projectID} actions={actions} />
}
