'use client'

import { Component, type ErrorInfo, type ReactNode } from 'react'
import { AlertTriangle } from '@/lib/matrix-icons'
import { Button } from '@/components/ui/button'
import { captureException } from '@/lib/observability/sentry'

interface Props {
  children: ReactNode
  /** Panel label for the fallback heading. */
  label: string
  onReset?: () => void
}

interface State {
  error: Error | null
}

/** Isolates panel failures so one widget cannot blank the dashboard. */
export class PanelErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    void captureException(error, { componentStack: info.componentStack, panel: this.props.label })
  }

  private reset = () => {
    this.setState({ error: null })
    this.props.onReset?.()
  }

  render() {
    if (this.state.error) {
      return (
        <div
          role="alert"
          className="border-destructive/30 bg-destructive/5 flex flex-col items-center gap-3 rounded-lg border p-8 text-center"
        >
          <AlertTriangle className="text-destructive size-8" aria-hidden />
          <div>
            <p className="text-foreground text-sm font-medium">{this.props.label} failed to load</p>
            <p className="text-muted-foreground mt-1 max-w-sm text-xs text-pretty">
              {this.state.error.message || 'An unexpected error occurred.'}
            </p>
          </div>
          <Button type="button" variant="outline" size="sm" onClick={this.reset}>
            Try again
          </Button>
        </div>
      )
    }
    return this.props.children
  }
}
