'use client'

/**
 * CodeEditor — the CodeMirror 6 binding of the workbench editor pane
 * (NEO-WORKBENCH req 5.1, 6). All buffer/conflict/save STATE lives in the
 * pure editor-buffer controller; this component only binds text ↔ CodeMirror.
 *
 * CodeMirror is loaded dynamically (client-only, one chunk) so the rest of
 * the workbench never pays for it — and, until the @codemirror/* packages are
 * installed, the pane degrades to an honest read-only fallback instead of
 * breaking the build.
 */
import { useEffect, useRef, useState } from 'react'

type CM = {
  EditorView: typeof import('@codemirror/view').EditorView
  EditorState: typeof import('@codemirror/state').EditorState
  Compartment: typeof import('@codemirror/state').Compartment
  basicExtensions: (view: unknown) => unknown[]
  languageFor: (path: string) => Promise<unknown | null>
}

let cmPromise: Promise<CM | null> | null = null

/** Load CodeMirror once, per session. Returns null when unavailable. */
function loadCM(): Promise<CM | null> {
  cmPromise ??= (async () => {
    try {
      const [view, state, commands, language, search, autocomplete, theme] = await Promise.all([
        import('@codemirror/view'),
        import('@codemirror/state'),
        import('@codemirror/commands'),
        import('@codemirror/language'),
        import('@codemirror/search'),
        import('@codemirror/autocomplete'),
        import('@/lib/cody/cm-theme'),
      ])
      const languageFor = async (path: string): Promise<unknown | null> => {
        const ext = path.slice(path.lastIndexOf('.') + 1).toLowerCase()
        try {
          switch (ext) {
            case 'js':
            case 'jsx':
            case 'ts':
            case 'tsx':
            case 'mjs':
            case 'cjs': {
              const m = await import('@codemirror/lang-javascript')
              return m.javascript({ jsx: true, typescript: ext.startsWith('t') })
            }
            case 'css':
            case 'scss':
              return (await import('@codemirror/lang-css')).css()
            case 'html':
            case 'htm':
              return (await import('@codemirror/lang-html')).html()
            case 'json':
              return (await import('@codemirror/lang-json')).json()
            case 'py':
              return (await import('@codemirror/lang-python')).python()
            case 'md':
            case 'markdown':
              return (await import('@codemirror/lang-markdown')).markdown()
            default:
              return null
          }
        } catch {
          return null
        }
      }
      return {
        EditorView: view.EditorView,
        EditorState: state.EditorState,
        Compartment: state.Compartment,
        basicExtensions: () => [
          view.lineNumbers(),
          view.highlightActiveLine(),
          view.keymap.of([
            ...commands.defaultKeymap,
            ...commands.historyKeymap,
            ...search.searchKeymap,
            ...autocomplete.completionKeymap,
          ]),
          commands.history(),
          language.bracketMatching(),
          language.indentOnInput(),
          theme.matrixEditorTheme,
        ],
        languageFor,
      }
    } catch {
      // Packages not installed / chunk failed: honest fallback, no crash.
      return null
    }
  })()
  return cmPromise
}

export function CodeEditor({
  path,
  content,
  readOnly,
  onChange,
  onSave,
}: {
  path: string
  /** The authoritative text to display. While the user types, the parent
   *  receives onChange and feeds the SAME text back (controlled-ish); a
   *  DIFFERENT incoming text (Neo's live typing, a reopened file) replaces
   *  the document. */
  content: string
  readOnly?: boolean
  onChange?: (content: string) => void
  onSave?: () => void
}) {
  const hostRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<import('@codemirror/view').EditorView | null>(null)
  const cmRef = useRef<CM | null>(null)
  const readOnlyComp = useRef<import('@codemirror/state').Compartment | null>(null)
  const langComp = useRef<import('@codemirror/state').Compartment | null>(null)
  const [engine, setEngine] = useState<'loading' | 'ready' | 'unavailable'>('loading')
  const onChangeRef = useRef(onChange)
  onChangeRef.current = onChange
  const onSaveRef = useRef(onSave)
  onSaveRef.current = onSave

  // Mount the editor once the engine loads (or fall back).
  useEffect(() => {
    let disposed = false
    loadCM().then((cm) => {
      if (disposed) return
      if (!cm || !hostRef.current) {
        setEngine('unavailable')
        return
      }
      cmRef.current = cm
      readOnlyComp.current = new cm.Compartment()
      langComp.current = new cm.Compartment()
      const view = new cm.EditorView({
        parent: hostRef.current,
        state: cm.EditorState.create({
          doc: content,
          extensions: [
            ...(cm.basicExtensions(null) as never[]),
            readOnlyComp.current.of(cm.EditorState.readOnly.of(!!readOnly)),
            langComp.current.of([]),
            cm.EditorView.updateListener.of((u) => {
              if (u.docChanged) onChangeRef.current?.(u.state.doc.toString())
            }),
            cm.EditorView.domEventHandlers({
              keydown: (e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === 's') {
                  e.preventDefault()
                  onSaveRef.current?.()
                  return true
                }
                return false
              },
            }),
            cm.EditorView.theme({
              '&': { height: '100%', fontSize: '13px' },
              '.cm-scroller': { fontFamily: 'var(--font-mono, monospace)' },
            }),
          ],
        }),
      })
      viewRef.current = view
      setEngine('ready')
    })
    return () => {
      disposed = true
      viewRef.current?.destroy()
      viewRef.current = null
    }
    // Mount once; content/readOnly sync below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Language by extension.
  useEffect(() => {
    const view = viewRef.current
    const cm = cmRef.current
    if (!view || !cm || !langComp.current) return
    let live = true
    cm.languageFor(path).then((lang) => {
      if (!live || !viewRef.current) return
      viewRef.current.dispatch({
        effects: langComp.current!.reconfigure((lang as never) ?? []),
      })
    })
    return () => {
      live = false
    }
  }, [path, engine])

  // Incoming content: replace the document only when it truly differs
  // (external change — Neo typing, reopen); user keystrokes round-trip as
  // the same string and dispatch nothing.
  useEffect(() => {
    const view = viewRef.current
    if (!view) return
    const cur = view.state.doc.toString()
    if (cur === content) return
    view.dispatch({
      changes: { from: 0, to: cur.length, insert: content },
      // Follow the tail while Neo types (bolt's live-typing feel).
      selection: { anchor: content.length },
      scrollIntoView: true,
    })
  }, [content, engine])

  // Read-only follows the in-flight-write / truncated flags.
  useEffect(() => {
    const view = viewRef.current
    const cm = cmRef.current
    if (!view || !cm || !readOnlyComp.current) return
    view.dispatch({
      effects: readOnlyComp.current.reconfigure(cm.EditorState.readOnly.of(!!readOnly)),
    })
  }, [readOnly, engine])

  if (engine === 'unavailable') {
    return (
      <div className="flex h-full min-h-0 flex-col">
        <div className="text-muted-foreground px-3 py-2 text-xs">
          The code editor engine is not installed in this build — showing the file read-only.
        </div>
        <pre className="min-h-0 flex-1 overflow-auto px-3 pb-3 font-mono text-[13px] leading-relaxed whitespace-pre">
          {content}
        </pre>
      </div>
    )
  }
  return <div ref={hostRef} data-testid="code-editor" className="h-full min-h-0 overflow-hidden" />
}
