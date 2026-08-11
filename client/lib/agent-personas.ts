/**
 * Agent persona catalog — curated character profiles for sub-agents.
 *
 * Each persona carries a role title, a one-line backstory/motto, and a
 * warm-toned accent color that fits the Centra AI design system. The swarm
 * assigner picks one deterministically per agent index so the same agent
 * gets the same persona across re-renders.
 */

export interface AgentPersona {
  role: string
  backstory: string
  /** Accent color for the card strip + avatar pattern. Warm-toned, not cold. */
  color: string
}

export const AGENT_PERSONAS: readonly AgentPersona[] = [
  {
    role: 'Architect',
    backstory:
      'Sees systems as living structures. Finds the load-bearing wall before moving anything.',
    color: '#99bd9c',
  },
  {
    role: 'Engineer',
    backstory: "Precision is care. Every edge case is someone's real experience.",
    color: '#c4a882',
  },
  {
    role: 'Researcher',
    backstory: 'Follows threads others miss. The answer is usually in the footnote.',
    color: '#8ba4b8',
  },
  {
    role: 'Designer',
    backstory: "The best interface is the one you don't notice. Clarity over cleverness.",
    color: '#b89ec4',
  },
  {
    role: 'Reviewer',
    backstory: 'Trust but verify. Every assumption deserves a second look.',
    color: '#c49e9e',
  },
  {
    role: 'Strategist',
    backstory: 'Plays the board, not the piece. Three moves ahead, one step at a time.',
    color: '#a8b4c4',
  },
  {
    role: 'Operator',
    backstory: 'Gets things across the line. Shipping is a craft, not a checkbox.',
    color: '#c4b89e',
  },
  {
    role: 'Analyst',
    backstory: 'Numbers tell stories if you ask the right question. Asks the right question.',
    color: '#9ec4b4',
  },
  {
    role: 'Debugger',
    backstory: 'Every bug has a history. Reads the history, finds the root cause.',
    color: '#c4a49e',
  },
  {
    role: 'Integrator',
    backstory: 'The seams between systems are where the real work happens. Holds them together.',
    color: '#a4c49e',
  },
] as const

/**
 * Deterministic persona assignment: given an agent index and name, pick a
 * stable persona from the catalog. The same (index, name) always yields the
 * same persona so avatars and roles never flicker across re-renders.
 */
export function pickPersona(index: number, name: string): AgentPersona {
  let hash = index * 31
  for (let i = 0; i < name.length; i++) {
    hash = (hash * 31 + name.charCodeAt(i)) | 0
  }
  return AGENT_PERSONAS[Math.abs(hash) % AGENT_PERSONAS.length]
}
