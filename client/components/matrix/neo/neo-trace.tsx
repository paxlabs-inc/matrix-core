'use client'

/**
 * NeoTrace — the visible "thought process" for the active surface task.
 *
 * Renders the agent's narration + tool steps as a vertical trace: a filled
 * node per step, joined by a stem. The in-flight step shimmers; completed
 * steps read in full-contrast foreground. This is the show-the-work
 * differentiator — surfacing the INTENTION (per the transparency rule), never
 * the mechanism.
 */
// Deprecated: the flat text trace was replaced by the animated NeoWorkspace
// viewport (terminal / browser / editor surfaces). This re-export keeps any
// stale import working; the prop shape (`steps`) is unchanged.
export { NeoWorkspace as NeoTrace } from '@/components/centra/agents/neo/neo-workspace'
