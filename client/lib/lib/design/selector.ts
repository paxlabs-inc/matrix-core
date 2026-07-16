/**
 * Design Mode — stable selector engine.
 *
 * To edit the *real* rendered components without instrumenting every source
 * file, we derive a deterministic CSS selector path for any clicked element and
 * key all overrides by it. The same selector is used to (a) re-resolve the node
 * for the selection box, (b) inject the live override stylesheet, and (c) emit
 * portable CSS on export. The path is structural (tag + :nth-of-type), so it is
 * stable across React re-renders as long as the DOM shape is stable.
 */

const ID_RE = /^[A-Za-z][\w-]*$/

function esc(value: string): string {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(value)
  return value.replace(/([^\w-])/g, '\\$1')
}

/** True for editor chrome / nodes we must never treat as editable targets. */
export function isEditorNode(el: Element | null): boolean {
  if (!el) return true
  return !!el.closest('[data-mx-ui]')
}

/** The wrapper the app content is rendered inside; selectors anchor through it. */
export function canvasRoot(): HTMLElement | null {
  return document.querySelector('[data-mx-canvas]')
}

/**
 * Build a unique, structural selector for `el`, anchored at the canvas wrapper
 * (falling back to <body>). Prefers a clean id when one is present and unique.
 */
export function computeSelector(el: Element): string {
  if (el.id && ID_RE.test(el.id) && document.querySelectorAll(`#${esc(el.id)}`).length === 1) {
    return `#${esc(el.id)}`
  }

  const root = canvasRoot() ?? document.body
  const parts: string[] = []
  let node: Element | null = el

  while (node && node.nodeType === 1 && node !== root && node !== document.body) {
    let part = node.tagName.toLowerCase()
    const parent: Element | null = node.parentElement
    if (parent) {
      const tag = node.tagName
      const sameTag = Array.from(parent.children).filter((c) => c.tagName === tag)
      if (sameTag.length > 1) {
        part += `:nth-of-type(${sameTag.indexOf(node) + 1})`
      }
    }
    parts.unshift(part)
    node = node.parentElement
  }

  const anchor = root === document.body ? 'body' : '[data-mx-canvas]'
  return parts.length ? `${anchor} > ${parts.join(' > ')}` : anchor
}

/** Resolve a stored selector back to a live element (best-effort). */
export function resolveSelector(selector: string | null): HTMLElement | null {
  if (!selector) return null
  try {
    return document.querySelector<HTMLElement>(selector)
  } catch {
    return null
  }
}

/** A short, human-friendly label for a selector (last path segment). */
export function selectorLabel(selector: string): string {
  const last = selector.split('>').pop()?.trim() ?? selector
  return last.replace(/:nth-of-type\((\d+)\)/g, '[$1]')
}
