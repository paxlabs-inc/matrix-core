'use client'

import { Component, type ReactNode } from 'react'
import { captureException } from '@/lib/observability/sentry'

interface Props {
  children: ReactNode
  /** Rendered in place of the children when markdown rendering throws —
   *  e.g. the raw text in a <pre>. */
  fallback: ReactNode
  /** When this value changes the boundary clears its error and retries the
   *  children. Keying it on the streaming text lets a later well-formed
   *  token recover from a transiently malformed partial (unterminated
   *  ```mermaid / math / code block) without blanking the surface. */
  resetKey?: unknown
}

interface State {
  error: boolean
  prevResetKey: unknown
}

/**
 * MarkdownErrorBoundary — isolates streamdown rendering failures.
 *
 * Streaming markdown can momentarily produce a pathological partial token
 * (an unterminated mermaid/math/code fence) that throws inside the parser.
 * Without a boundary that throw would blank the whole surface. This catches
 * it, shows a raw-text fallback, and — because `resetKey` tracks the live
 * text — re-attempts rendering on the next token so the answer recovers and
 * completes cleanly. Incremental parsing stays owned by streamdown.
 */
export class MarkdownErrorBoundary extends Component<Props, State> {
  state: State = { error: false, prevResetKey: undefined }

  static getDerivedStateFromError(): Partial<State> {
    return { error: true }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey !== state.prevResetKey) {
      return { error: false, prevResetKey: props.resetKey }
    }
    return null
  }

  componentDidCatch(error: Error) {
    void captureException(error, { panel: 'markdown' })
  }

  render() {
    return this.state.error ? this.props.fallback : this.props.children
  }
}
