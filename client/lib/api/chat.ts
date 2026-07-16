/**
 * Chat resource — the Liaison front door (POST /chat).
 *
 * Every user message hits the Liaison first. It either answers directly
 * (kind: "reply" — greetings, status, capability questions) or dispatches
 * the agent pipeline (kind: "dispatch") and narrates the run as
 * chat.assistant SSE events on /events?intent_id=<id>.
 *
 * Mirrors executor/cmd/mcl-execute/daemon_chat.go.
 */
import { apiSend } from '@/lib/api/client'

export interface ChatInput {
  message: string
  /** Threads turns together; omit on the first turn — the daemon mints
   *  one and echoes it back to reuse. */
  conversationId?: string
  /** Clarify re-entry: the intent that asked for input. */
  intentId?: string
  /** Clarify answers keyed by slot name. */
  slotValues?: Record<string, string>
  /** Override the daemon default skill for the dispatched run. */
  skill?: string
  /** Pin the dispatched run to a cortex Goal for cost roll-ups. */
  goalId?: string
  /** Workbench project tag: a coding-surface chat names its project so
   *  History/Workspace scope per project (Neo daemon persists the tag). */
  project?: string
}

/** Liaison answered directly — no pipeline run. */
export interface ChatReply {
  conversation_id: string
  kind: 'reply'
  text: string
}

/** Liaison dispatched a pipeline run — narration streams over SSE. */
export interface ChatDispatch {
  conversation_id: string
  kind: 'dispatch'
  intent_id: string
  events_url: string
  poll_url: string
}

export type ChatResponse = ChatReply | ChatDispatch

/** POST /chat — send one user turn through the Liaison. */
export async function sendChat(input: ChatInput): Promise<ChatResponse> {
  return apiSend<ChatResponse>('/chat', {
    message: input.message,
    conversation_id: input.conversationId,
    intent_id: input.intentId,
    slot_values: input.slotValues,
    skill: input.skill,
    goal_id: input.goalId,
    project: input.project,
  })
}
