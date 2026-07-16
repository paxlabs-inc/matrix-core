/**
 * The workbench editor theme — the CREAO warm-charcoal system applied to
 * CodeMirror. One low-saturation palette derived from the app tokens (cream
 * text, sage accent, warm stone neutrals) so code reads calm on the dark
 * surface instead of the library's light-theme defaults. Chrome (gutters,
 * active line, selection, search panel) separates by background tone only.
 */
import { EditorView } from '@codemirror/view'
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language'
import { tags as t } from '@lezer/highlight'
import type { Extension } from '@codemirror/state'

// The warm ladder (globals.css) + a few derived code tints of the same
// temperature: sage for structure, sand for literals, clay for values.
const cream = '#e3d9d4'
const creamDim = '#cbbfb8'
const stone = '#96918e'
const stoneDim = '#6e6a62'
const sage = '#99bd9c'
const sagePale = '#b8c6b9'
const sand = '#c9b590'
const clay = '#d0a97e'
const parchment = '#e0cfa8'
const steel = '#9fb3bd'
const rose = '#d99a8e'

const chrome = EditorView.theme(
  {
    '&': { color: cream, backgroundColor: 'transparent' },
    '.cm-content': { caretColor: cream },
    '.cm-cursor, .cm-dropCursor': { borderLeftColor: cream },
    '&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, ::selection':
      { backgroundColor: '#4a463f66' },
    '.cm-activeLine': { backgroundColor: '#22222066' },
    '.cm-gutters': {
      backgroundColor: 'transparent',
      color: stoneDim,
      border: 'none',
    },
    '.cm-activeLineGutter': { backgroundColor: 'transparent', color: stone },
    '.cm-matchingBracket, &.cm-focused .cm-matchingBracket': {
      backgroundColor: '#35353099',
      outline: 'none',
    },
    '.cm-nonmatchingBracket, &.cm-focused .cm-nonmatchingBracket': {
      backgroundColor: '#4a2b2699',
    },
    '.cm-searchMatch': { backgroundColor: '#99bd9c2e', outline: 'none' },
    '.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: '#99bd9c52' },
    '.cm-selectionMatch': { backgroundColor: '#35353080' },
    '.cm-panels': { backgroundColor: '#222220', color: cream },
    '.cm-panels input, .cm-panels button': {
      backgroundColor: '#2a2a27',
      color: cream,
      border: 'none',
      borderRadius: '4px',
      outline: 'none',
    },
    '.cm-panels button:hover': { backgroundColor: '#353530' },
    '.cm-tooltip': {
      backgroundColor: '#222220',
      color: cream,
      border: 'none',
      borderRadius: '6px',
    },
    '.cm-tooltip-autocomplete ul li[aria-selected]': {
      backgroundColor: '#353530',
      color: cream,
    },
    '.cm-placeholder': { color: stoneDim },
  },
  { dark: true },
)

const highlight = HighlightStyle.define([
  { tag: [t.comment, t.blockComment, t.lineComment], color: stoneDim, fontStyle: 'italic' },
  { tag: [t.docComment, t.docString], color: stoneDim },
  {
    tag: [t.keyword, t.operatorKeyword, t.modifier, t.controlKeyword, t.moduleKeyword, t.self],
    color: sage,
  },
  { tag: [t.string, t.special(t.string), t.character, t.attributeValue], color: sand },
  { tag: [t.regexp, t.escape], color: clay },
  {
    tag: [t.number, t.integer, t.float, t.bool, t.atom, t.null, t.unit, t.color],
    color: clay,
  },
  { tag: [t.constant(t.name), t.standard(t.name), t.special(t.variableName)], color: clay },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.macroName], color: parchment },
  { tag: [t.definition(t.variableName), t.definition(t.propertyName)], color: creamDim },
  { tag: [t.typeName, t.className, t.namespace], color: sagePale },
  { tag: [t.propertyName, t.attributeName, t.labelName], color: creamDim },
  { tag: t.tagName, color: sage },
  { tag: [t.angleBracket, t.bracket, t.punctuation, t.separator], color: stone },
  { tag: [t.operator, t.definitionOperator, t.compareOperator, t.logicOperator], color: stone },
  { tag: [t.meta, t.processingInstruction, t.documentMeta, t.annotation], color: stoneDim },
  { tag: t.url, color: steel },
  { tag: t.link, color: steel, textDecoration: 'underline' },
  { tag: t.heading, color: cream, fontWeight: '600' },
  { tag: t.strong, fontWeight: '600' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.strikethrough, textDecoration: 'line-through' },
  { tag: t.quote, color: creamDim, fontStyle: 'italic' },
  { tag: t.monospace, color: sand },
  { tag: [t.inserted], color: sage },
  { tag: [t.deleted, t.invalid], color: rose },
  { tag: t.changed, color: clay },
])

/** The full editor look: chrome + syntax colors, one extension. */
export const matrixEditorTheme: Extension = [chrome, syntaxHighlighting(highlight)]
