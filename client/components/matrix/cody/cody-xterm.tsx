'use client'

/**
 * CodyShell — the live terminal pane at the Claude Code bar (req 12.3).
 *
 * Renders through @xterm/xterm (the VS Code terminal engine) with the fit,
 * webgl (canvas/DOM fallback), search, and web-links addons, so output carries
 * real ANSI color, wrapping, and scrollback. Transport stays the bounded
 * POST /workspace/exec runner: a local line editor (prompt, backspace, ↑/↓
 * history, Ctrl+C/Ctrl+L) collects one command, runs it to completion, and the
 * scrollback records output, exit code, and wall-clock duration. The Terminal
 * instance is created once per mount and re-fit on container resize.
 * Architect gating is the caller's job.
 */
import { useEffect, useRef } from 'react'

import '@xterm/xterm/css/xterm.css'

import { execCommand } from '@/lib/api/cody'

const PROMPT = '\x1b[38;5;33m$\x1b[0m '

const THEME = {
  background: '#00000000',
  foreground: '#d4d4d8',
  cursor: '#d4d4d8',
  selectionBackground: '#264f78',
  blue: '#3b82f6',
  brightBlue: '#60a5fa',
}

function fmtDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)}s`
}

export function CodyShell({ projectID }: { projectID?: string }) {
  const hostRef = useRef<HTMLDivElement>(null)
  const projectRef = useRef(projectID)
  projectRef.current = projectID

  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    let disposed = false
    let cleanup: (() => void) | null = null

    void (async () => {
      const [{ Terminal }, { FitAddon }, { WebLinksAddon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-fit'),
        import('@xterm/addon-web-links'),
      ])
      if (disposed) return

      const term = new Terminal({
        cursorBlink: true,
        fontSize: 12,
        fontFamily: 'var(--font-mono, ui-monospace), monospace',
        lineHeight: 1.35,
        scrollback: 10_000,
        allowTransparency: true,
        theme: THEME,
        convertEol: true,
      })
      const fit = new FitAddon()
      term.loadAddon(fit)
      term.loadAddon(new WebLinksAddon())
      term.open(host)

      try {
        const { WebglAddon } = await import('@xterm/addon-webgl')
        if (!disposed) {
          const webgl = new WebglAddon()
          webgl.onContextLoss(() => webgl.dispose())
          term.loadAddon(webgl)
        }
      } catch {
        /* WebGL unavailable — the DOM renderer is fine for one pane */
      }
      if (disposed) {
        term.dispose()
        return
      }

      fit.fit()
      const observer = new ResizeObserver(() => {
        try {
          fit.fit()
        } catch {
          /* mid-teardown resize */
        }
      })
      observer.observe(host)

      term.writeln('\x1b[2mBounded shell — each command runs to completion in the workspace.\x1b[0m')
      term.write(PROMPT)

      let buffer = ''
      let cursor = 0
      let running = false
      const history: string[] = []
      let histIdx = -1

      const redraw = () => {
        term.write('\x1b[2K\r' + PROMPT + buffer)
        const back = buffer.length - cursor
        if (back > 0) term.write(`\x1b[${back}D`)
      }

      const run = async (cmd: string) => {
        running = true
        const started = performance.now()
        try {
          const result = await execCommand(cmd, { projectID: projectRef.current })
          if (disposed) return
          if (result.output) term.write(result.output.replace(/\r?\n/g, '\r\n'))
          if (result.output && !result.output.endsWith('\n')) term.write('\r\n')
          const ok = result.exit === 0
          const color = ok ? '\x1b[38;5;33m' : '\x1b[31m'
          const timedOut = result.timed_out ? ' \x1b[31m(timed out)\x1b[0m' : ''
          term.writeln(
            `${color}exit ${result.exit}\x1b[0m \x1b[2m${fmtDuration(performance.now() - started)}\x1b[0m${timedOut}`,
          )
        } catch (err) {
          if (disposed) return
          const msg = err instanceof Error ? err.message : 'Command failed'
          term.writeln(`\x1b[31m${msg}\x1b[0m`)
        } finally {
          if (!disposed) {
            running = false
            term.write(PROMPT)
          }
        }
      }

      const onData = term.onData((data) => {
        if (running) return
        for (const ch of data) {
          const code = ch.charCodeAt(0)
          if (ch === '\r') {
            term.write('\r\n')
            const cmd = buffer.trim()
            buffer = ''
            cursor = 0
            histIdx = -1
            if (cmd) {
              history.push(cmd)
              void run(cmd)
            } else {
              term.write(PROMPT)
            }
          } else if (code === 127 || ch === '\b') {
            if (cursor > 0) {
              buffer = buffer.slice(0, cursor - 1) + buffer.slice(cursor)
              cursor--
              redraw()
            }
          } else if (ch === '\x03') {
            term.write('^C\r\n' + PROMPT)
            buffer = ''
            cursor = 0
            histIdx = -1
          } else if (ch === '\x0c') {
            term.clear()
            redraw()
          } else if (data.startsWith('\x1b[')) {
            const seq = data.slice(2)
            if (seq === 'A') {
              if (history.length > 0) {
                histIdx = histIdx === -1 ? history.length - 1 : Math.max(0, histIdx - 1)
                buffer = history[histIdx]
                cursor = buffer.length
                redraw()
              }
            } else if (seq === 'B') {
              if (histIdx !== -1) {
                histIdx = histIdx + 1
                if (histIdx >= history.length) {
                  histIdx = -1
                  buffer = ''
                } else {
                  buffer = history[histIdx]
                }
                cursor = buffer.length
                redraw()
              }
            } else if (seq === 'C') {
              if (cursor < buffer.length) {
                cursor++
                term.write('\x1b[C')
              }
            } else if (seq === 'D') {
              if (cursor > 0) {
                cursor--
                term.write('\x1b[D')
              }
            }
            return
          } else if (code >= 32) {
            buffer = buffer.slice(0, cursor) + ch + buffer.slice(cursor)
            cursor++
            redraw()
          }
        }
      })

      cleanup = () => {
        observer.disconnect()
        onData.dispose()
        term.dispose()
      }
    })()

    return () => {
      disposed = true
      cleanup?.()
    }
  }, [])

  return (
    <section className="bg-surface-tertiary flex h-full min-h-0 flex-col overflow-hidden rounded-lg">
      <div className="flex shrink-0 items-center gap-2 px-3 py-2">
        <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
          Shell
        </span>
        <span className="text-muted-foreground ml-auto font-mono text-[10px]">
          bounded run · workspace root
        </span>
      </div>
      <div ref={hostRef} className="min-h-0 flex-1 px-2 pb-2" />
    </section>
  )
}
