/**
 * Mode-tiered disclosure (spec req 4, task 6.6).
 *
 * One engine truth, three disclosure tiers. Every mode gets the same run data;
 * tiers differ ONLY in what the surface reveals and the register of its copy —
 * never in the underlying run model or the constitution. This table is the
 * single source both the shell and the panes read so "Prototype hides the
 * terminal/tree/diff, Architect shows the spec viewer + terminal" is asserted
 * in one place (and by task 7.4).
 */
import type { CodyMode } from '@/lib/api/cody'

export type Register = 'outcome' | 'technical'

export interface Tier {
  /** Copy register: outcome language (Prototype) vs technical (Engineer/Architect). */
  register: Register
  /** Preview is the hero viewport (Prototype) vs a pane among others. */
  previewHero: boolean
  /** The waved task board as centerpiece. */
  board: boolean
  /** Per-task verification evidence detail in turn-in cards. */
  turninEvidence: boolean
  /** File tree pane. */
  fileTree: boolean
  /** Read-only code viewer as a first-class pane (Prototype: only via the escape hatch). */
  viewer: boolean
  /** A single show-me-the-code escape hatch (Prototype only). */
  codeEscapeHatch: boolean
  /** Hunk-level diff review pane. */
  diff: boolean
  /** Terminal pane (Architect only). */
  terminal: boolean
  /** Live .cody/spec viewer (Architect only). */
  specViewer: boolean
  /** Property-test results + full turn-in detail (Architect). */
  fullTurninDetail: boolean
  /** Git surface: staged diff + proposed commit, user drives the commit (Architect). */
  gitSurface: boolean
  /** SDR/DLR decision cards block the run (Engineer/Architect) vs informational (Prototype). */
  decisionBlocking: boolean
  /** Named checkpoint/restore (Engineer/Architect) vs one-button undo (Prototype). */
  namedCheckpoints: boolean
}

const PROTOTYPE: Tier = {
  register: 'outcome',
  previewHero: true,
  board: false,
  turninEvidence: false,
  fileTree: false,
  viewer: false,
  codeEscapeHatch: true,
  diff: false,
  terminal: false,
  specViewer: false,
  fullTurninDetail: false,
  gitSurface: false,
  decisionBlocking: false,
  namedCheckpoints: false,
}

const ENGINEER: Tier = {
  register: 'technical',
  previewHero: false,
  board: true,
  turninEvidence: true,
  fileTree: true,
  viewer: true,
  codeEscapeHatch: false,
  diff: true,
  terminal: false,
  specViewer: false,
  fullTurninDetail: false,
  gitSurface: false,
  decisionBlocking: true,
  namedCheckpoints: true,
}

const ARCHITECT: Tier = {
  ...ENGINEER,
  terminal: true,
  specViewer: true,
  fullTurninDetail: true,
  gitSurface: true,
}

const TIERS: Record<CodyMode, Tier> = {
  prototype: PROTOTYPE,
  engineer: ENGINEER,
  architect: ARCHITECT,
}

export function tierFor(mode: CodyMode | undefined): Tier {
  return TIERS[mode ?? 'engineer'] ?? ENGINEER
}

export const MODE_LABELS: Record<CodyMode, string> = {
  prototype: 'Prototype',
  engineer: 'Engineer',
  architect: 'Architect',
}

export const MODE_ORDER: CodyMode[] = ['prototype', 'engineer', 'architect']
