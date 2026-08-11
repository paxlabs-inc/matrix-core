/**
 * Centra AI icon set rendered as React components.
 *
 * The glyph geometry is ported from `public/matrix_icons/matrix-icons.js`
 * (24×24 line icons, 1.75px stroke, round caps/joins). This module exposes
 * those glyphs as Lucide-compatible React components so the rest of the app can
 * keep importing icons by name — only the import source changes from
 * `lucide-react` to `@/lib/matrix-icons`.
 *
 * A handful of glyphs in the "extensions" block are drawn in the same style to
 * cover UI primitives / concepts the base set doesn't include (radio dots, grip
 * handles, sparkles, cpu, etc.).
 */
import * as React from 'react'
import type { IconName } from '@/public/icons/name'
import { sanitizeSvgMarkup } from '@/lib/security/sanitize'

/** Compiled paxscan SVG sprite served from /public. */
export const SPRITE_URL = '/icons/sprite.svg'

/**
 * Maps a Centra AI icon name (the kebab key used across the app) to a paxscan
 * sprite symbol. When a name is present here, `createCentraIcon` renders the
 * paxscan glyph via <use> instead of the inline legacy path — so the ported
 * icon set visibly REPLACES the old line glyphs everywhere they're imported.
 * Names absent from this map keep their inline glyph as a fallback
 * (covers UI primitives the paxscan set doesn't include, e.g. chevrons).
 */
export const CENTRA_TO_SPRITE: Record<string, IconName> = {
  // Navigation / layout
  search: 'search',
  filter: 'filter',
  close: 'close',
  menu: 'burger',
  more: 'dots',
  'external-link': 'link_external',
  // Actions
  plus: 'plus',
  minus: 'minus',
  check: 'check',
  edit: 'edit',
  trash: 'delete',
  copy: 'copy',
  share: 'share',
  refresh: 'refresh',
  settings: 'gear',
  link: 'link',
  bookmark: 'star_outline',
  // Status
  success: 'status/success',
  error: 'status/error',
  warning: 'status/warning',
  help: 'info',
  // Content
  globe: 'globe',
  clock: 'clock',
  document: 'docs',
  // Security / account
  lock: 'lock',
  'shield-check': 'certified',
  logout: 'sign_out',
  user: 'profile',
  // Centra AI / Paxeer concepts
  attestation: 'verified',
  network: 'networks',
  block: 'block',
  payments: 'payment_link',
  wallet: 'wallet',
  send: 'rocket',
  zap: 'lightning',
}

/** name -> inner SVG markup (the host <svg> applies stroke/size). */
export const CENTRA_ICON_BODIES: Record<string, string> = {
  /* ——— Navigation ——— */
  home: `<path d="M4 10.5 12 4l8 6.5"/><path d="M6 9.5V19a1 1 0 0 0 1 1h3v-4.5a2 2 0 0 1 4 0V20h3a1 1 0 0 0 1-1V9.5"/>`,
  dashboard: `<rect x="4" y="4" width="7" height="7" rx="1.4"/><rect x="13" y="4" width="7" height="7" rx="1.4"/><rect x="4" y="13" width="7" height="7" rx="1.4"/><rect x="13" y="13" width="7" height="7" rx="1.4"/>`,
  'arrow-left': `<path d="M19 12H5"/><path d="M11 6l-6 6 6 6"/>`,
  'arrow-right': `<path d="M5 12h14"/><path d="M13 6l6 6-6 6"/>`,
  'arrow-up': `<path d="M12 19V5"/><path d="M6 11l6-6 6 6"/>`,
  'chevron-up': `<path d="M6 15l6-6 6 6"/>`,
  'chevron-down': `<path d="M6 9l6 6 6-6"/>`,
  'chevron-left': `<path d="M15 6l-6 6 6 6"/>`,
  'chevron-right': `<path d="M9 6l6 6-6 6"/>`,
  menu: `<path d="M4 7h16"/><path d="M4 12h16"/><path d="M4 17h16"/>`,
  close: `<path d="M6 6l12 12"/><path d="M18 6 6 18"/>`,
  search: `<circle cx="11" cy="11" r="6.5"/><path d="M16 16l4.5 4.5"/>`,
  more: `<circle cx="5" cy="12" r="1.1"/><circle cx="12" cy="12" r="1.1"/><circle cx="19" cy="12" r="1.1"/>`,
  filter: `<path d="M4 6h16"/><path d="M7 12h10"/><path d="M10 18h4"/>`,
  'external-link': `<path d="M14 5h5v5"/><path d="M19 5l-8 8"/><path d="M17 13.5V18a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 5 18V9a1.5 1.5 0 0 1 1.5-1.5H11"/>`,

  /* ——— Actions ——— */
  plus: `<path d="M12 5v14"/><path d="M5 12h14"/>`,
  minus: `<path d="M5 12h14"/>`,
  check: `<path d="M5 12.5l4.5 4.5L19 7"/>`,
  edit: `<path d="M14.5 5.5l4 4"/><path d="M4 20l1-4.2L15.5 5.3a1.5 1.5 0 0 1 2.1 0l1.1 1.1a1.5 1.5 0 0 1 0 2.1L8.2 19 4 20z"/>`,
  trash: `<path d="M5 7h14"/><path d="M9.5 7V5.5a1.2 1.2 0 0 1 1.2-1.2h2.6a1.2 1.2 0 0 1 1.2 1.2V7"/><path d="M7 7l.9 12a1.5 1.5 0 0 0 1.5 1.4h5.2a1.5 1.5 0 0 0 1.5-1.4L17 7"/><path d="M10.5 11v5"/><path d="M13.5 11v5"/>`,
  copy: `<rect x="8.5" y="8.5" width="11" height="11" rx="2"/><path d="M15.5 8.5V6.5a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/>`,
  share: `<circle cx="6" cy="12" r="2.4"/><circle cx="18" cy="6" r="2.4"/><circle cx="18" cy="18" r="2.4"/><path d="M8.1 10.9 15.9 7.1"/><path d="M8.1 13.1 15.9 16.9"/>`,
  download: `<path d="M12 4v12"/><path d="M7.5 11.5 12 16l4.5-4.5"/><path d="M5 19h14"/>`,
  upload: `<path d="M12 20V8"/><path d="M7.5 12.5 12 8l4.5 4.5"/><path d="M5 5h14"/>`,
  refresh: `<path d="M4.5 12a7.5 7.5 0 0 1 12.9-5.2"/><path d="M18 3v4h-4"/><path d="M19.5 12a7.5 7.5 0 0 1-12.9 5.2"/><path d="M6 21v-4h4"/>`,
  settings: `<circle cx="12" cy="12" r="3"/><path d="M19.4 13a7.6 7.6 0 0 0 0-2l2-1.5-2-3.5-2.4 1a7.6 7.6 0 0 0-1.7-1L14.9 2.5h-3.8L10.7 5a7.6 7.6 0 0 0-1.7 1L6.6 5l-2 3.5L6.6 10a7.6 7.6 0 0 0 0 2l-2 1.5 2 3.5 2.4-1a7.6 7.6 0 0 0 1.7 1l.4 2.5h3.8l.4-2.5a7.6 7.6 0 0 0 1.7-1l2.4 1 2-3.5z"/>`,
  link: `<path d="M9.5 14.5 14.5 9.5"/><path d="M8.5 11.5 6.5 13.5a3 3 0 0 0 4.2 4.2l2-2"/><path d="M15.5 12.5l2-2a3 3 0 0 0-4.2-4.2l-2 2"/>`,
  bookmark: `<path d="M7 4.5h10a1 1 0 0 1 1 1V20l-6-4-6 4V5.5a1 1 0 0 1 1-1z"/>`,

  /* ——— Status ——— */
  success: `<circle cx="12" cy="12" r="8.5"/><path d="M8.5 12.2l2.4 2.4 4.6-4.8"/>`,
  error: `<circle cx="12" cy="12" r="8.5"/><path d="M9 9l6 6"/><path d="M15 9l-6 6"/>`,
  warning: `<path d="M10.3 4.8 2.6 18a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 4.8a2 2 0 0 0-3.4 0z"/><path d="M12 10v4"/><path d="M12 17.5h.01"/>`,
  info: `<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><path d="M12 8h.01"/>`,
  loading: `<path d="M12 3.5a8.5 8.5 0 1 0 8.5 8.5"/>`,
  help: `<circle cx="12" cy="12" r="8.5"/><path d="M9.6 9.6a2.5 2.5 0 0 1 4.7 1.1c0 1.7-2.3 2-2.3 3.5"/><path d="M12 17h.01"/>`,

  /* ——— Content & Files ——— */
  file: `<path d="M7 3.5h6l5 5V19a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 19V5a1.5 1.5 0 0 1 1.5-1.5z"/><path d="M13 3.5V9h5"/>`,
  document: `<path d="M7 3.5h6l5 5V19a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 19V5a1.5 1.5 0 0 1 1.5-1.5z"/><path d="M13 3.5V9h5"/><path d="M9 13.5h6"/><path d="M9 16.5h4"/>`,
  folder: `<path d="M4 8a2 2 0 0 1 2-2h3.2l1.6 1.8H18a2 2 0 0 1 2 2V17a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 17V8z"/>`,
  attachment: `<path d="M18.5 11.5 11 19a4 4 0 0 1-5.7-5.7l8-8a2.7 2.7 0 0 1 3.8 3.8l-7.7 7.7a1.3 1.3 0 0 1-1.9-1.9l6.9-6.9"/>`,
  tag: `<path d="M3.5 12.5V6a2 2 0 0 1 2-2h6.5L20 12.5 12.5 20 3.5 12.5z"/><circle cx="8" cy="8" r="1.3"/>`,
  image: `<rect x="4" y="5" width="16" height="14" rx="2"/><circle cx="9" cy="10" r="1.6"/><path d="M5 17l4.5-4 3 2.5L16 11l3 3.5"/>`,
  code: `<path d="M9 8l-4 4 4 4"/><path d="M15 8l4 4-4 4"/><path d="M13.5 6l-3 12"/>`,
  terminal: `<rect x="3.5" y="5" width="17" height="14" rx="2"/><path d="M8 10l2.5 2.5L8 15"/><path d="M13 15h4"/>`,
  database: `<ellipse cx="12" cy="6" rx="7" ry="3"/><path d="M5 6v6c0 1.7 3.1 3 7 3s7-1.3 7-3V6"/><path d="M5 12v6c0 1.7 3.1 3 7 3s7-1.3 7-3v-6"/>`,
  globe: `<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17"/><path d="M12 3.5c2.2 2.3 3.4 5.3 3.4 8.5s-1.2 6.2-3.4 8.5c-2.2-2.3-3.4-5.3-3.4-8.5S9.8 5.8 12 3.5z"/>`,
  clock: `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>`,

  /* ——— Communication ——— */
  chat: `<path d="M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H9l-5 4V6z"/>`,
  message: `<path d="M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H9l-5 4V6z"/><path d="M8.5 9.5h.01"/><path d="M12 9.5h.01"/><path d="M15.5 9.5h.01"/>`,
  bell: `<path d="M6 9.5a6 6 0 0 1 12 0c0 4 1.2 5.3 2 6.3H4c.8-1 2-2.3 2-6.3z"/><path d="M9.5 18.5a2.5 2.5 0 0 0 5 0"/>`,
  mail: `<rect x="3" y="5.5" width="18" height="13" rx="2"/><path d="M3.5 7.5l8.5 6 8.5-6"/>`,
  send: `<path d="M21 3.5 2.5 11.2l7.4 2.4 2.4 7.4L21 3.5z"/><path d="M9.9 13.6 14 9.5"/>`,
  mention: `<circle cx="12" cy="12" r="4"/><path d="M16 8v5a2.5 2.5 0 0 0 4.4 1.6A8.5 8.5 0 1 0 16.5 19"/>`,
  phone: `<path d="M5 4.5h3l1.5 4-2 1.5a11 11 0 0 0 5 5l1.5-2 4 1.5v3a1.5 1.5 0 0 1-1.6 1.5A15.5 15.5 0 0 1 3.5 6.1 1.5 1.5 0 0 1 5 4.5z"/>`,

  /* ——— Account & Security ——— */
  user: `<circle cx="12" cy="8.5" r="3.5"/><path d="M5.5 20a6.5 6.5 0 0 1 13 0"/>`,
  users: `<circle cx="9" cy="8.5" r="3.2"/><path d="M3.5 19.5a5.5 5.5 0 0 1 11 0"/><path d="M16 5.6a3.2 3.2 0 0 1 0 6"/><path d="M17 14.4a5.5 5.5 0 0 1 3.5 5.1"/>`,
  lock: `<rect x="5" y="10.5" width="14" height="10" rx="2"/><path d="M8 10.5V8a4 4 0 0 1 8 0v2.5"/><path d="M12 14.5v2.5"/>`,
  unlock: `<rect x="5" y="10.5" width="14" height="10" rx="2"/><path d="M8 10.5V8a4 4 0 0 1 7.7-1.5"/><path d="M12 14.5v2.5"/>`,
  key: `<circle cx="8.5" cy="8.5" r="4"/><path d="M11.3 11.3 19 19"/><path d="M16.5 18.5l2-2"/><path d="M14.5 16.5l2-2"/>`,
  shield: `<path d="M12 3.5 19 6.2V11c0 4.5-3 8-7 9.5C8 19 5 15.5 5 11V6.2L12 3.5z"/>`,
  'shield-check': `<path d="M12 3.5 19 6.2V11c0 4.5-3 8-7 9.5C8 19 5 15.5 5 11V6.2L12 3.5z"/><path d="M9 11.5l2 2 4-4"/>`,
  logout: `<path d="M9 4.5H6.5A1.5 1.5 0 0 0 5 6v12a1.5 1.5 0 0 0 1.5 1.5H9"/><path d="M16 16l4-4-4-4"/><path d="M20 12H10"/>`,
  fingerprint: `<path d="M5.5 11a6.5 6.5 0 0 1 12.7-2"/><path d="M8 12a4 4 0 0 1 8 0v1.5"/><path d="M12 12v3a4 4 0 0 0 1.5 3.2"/><path d="M16 14.5a8 8 0 0 1-1 4.5"/><path d="M8.8 19a8 8 0 0 1-1.3-5"/>`,

  /* ——— Media ——— */
  play: `<path d="M8 5.5v13l11-6.5-11-6.5z"/>`,
  pause: `<path d="M9 5v14"/><path d="M15 5v14"/>`,
  stop: `<rect x="6.5" y="6.5" width="11" height="11" rx="2.5"/>`,
  mic: `<rect x="9" y="3.5" width="6" height="11" rx="3"/><path d="M6 11a6 6 0 0 0 12 0"/><path d="M12 17v3.5"/><path d="M8.5 20.5h7"/>`,
  volume: `<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15.5 9.5a4 4 0 0 1 0 5"/><path d="M18 7.5a7 7 0 0 1 0 9"/>`,
  camera: `<path d="M4 8.5a1.5 1.5 0 0 1 1.5-1.5H8l1.5-2h5L16 7h2.5A1.5 1.5 0 0 1 20 8.5V18a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 18V8.5z"/><circle cx="12" cy="12.5" r="3.2"/>`,
  eye: `<path d="M2.5 12S6 6 12 6s9.5 6 9.5 6-3.5 6-9.5 6-9.5-6-9.5-6z"/><circle cx="12" cy="12" r="2.8"/>`,

  /* ——— Centra AI Core ——— */
  mcl: `<path d="M4 8h5"/><path d="M4 12h4"/><path d="M4 16h5"/><path d="M11.5 12h6"/><path d="M15 9.5 17.5 12l-2.5 2.5"/><path d="M20.5 8v8"/>`,
  intent: `<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3.2"/><path d="M12 1.5V5"/><path d="M12 19v3.5"/><path d="M1.5 12H5"/><path d="M19 12h3.5"/>`,
  'intent-ir': `<path d="M9 5c-1.7 0-2.4 1-2.4 2.6v1.5c0 1.3-.7 2.1-2 2.4 1.3.3 2 1.1 2 2.4v1.5C6.6 19 7.3 20 9 20"/><path d="M15 5c1.7 0 2.4 1 2.4 2.6v1.5c0 1.3.7 2.1 2 2.4-1.3.3-2 1.1-2 2.4v1.5C17.4 19 16.7 20 15 20"/><path d="M12 12h.01"/>`,
  cortex: `<circle cx="12" cy="12" r="2.3"/><circle cx="5" cy="6.5" r="1.8"/><circle cx="19" cy="6.5" r="1.8"/><circle cx="5.5" cy="18" r="1.8"/><circle cx="18.5" cy="18" r="1.8"/><path d="M10.2 10.7 6.4 7.6"/><path d="M13.8 10.7 17.6 7.6"/><path d="M10.4 13.3 7 16.5"/><path d="M13.6 13.3 17 16.5"/>`,
  agent: `<path d="M12 3.2l7.4 4.2v8.4L12 20l-7.4-4.2V7.4z"/><circle cx="12" cy="10.5" r="2.2"/><path d="M8.4 16.4a4 4 0 0 1 7.2 0"/>`,
  tools: `<path d="M15.5 4.5a4.5 4.5 0 0 0-5.8 5.6L4 15.8a1.6 1.6 0 0 0 2.3 2.3l5.7-5.7a4.5 4.5 0 0 0 5.6-5.8l-2.7 2.7-2.5-.7-.7-2.5 2.5-2.6z"/>`,
  merkle: `<circle cx="12" cy="4.5" r="1.7"/><circle cx="7" cy="12" r="1.7"/><circle cx="17" cy="12" r="1.7"/><circle cx="4.5" cy="19.5" r="1.5"/><circle cx="9.5" cy="19.5" r="1.5"/><circle cx="14.5" cy="19.5" r="1.5"/><circle cx="19.5" cy="19.5" r="1.5"/><path d="M10.6 5.6 8.4 10.9"/><path d="M13.4 5.6 15.6 10.9"/><path d="M6.2 13.6 5.3 18"/><path d="M7.8 13.6 8.7 18"/><path d="M16.2 13.6 15.3 18"/><path d="M17.8 13.6 18.7 18"/>`,
  sign: `<path d="M4 16.5c2.4 0 3-7 5.4-7 1.8 0 1.1 4 2.9 4 1.3 0 1.7-2 3.4-2"/><path d="M4 20.5h16"/><path d="M17.6 8.7l1.2-1.2a1.2 1.2 0 0 0-1.7-1.7L15.9 6"/>`,
  correction: `<path d="M5 10h7.5a4.5 4.5 0 0 1 0 9H9"/><path d="M5 10l3.2-3.2"/><path d="M5 10l3.2 3.2"/><path d="M17.5 4l.9 2 2 .9-2 .9-.9 2-.9-2-2-.9 2-.9.9-2z"/>`,
  'versioned-uri': `<circle cx="12" cy="12" r="3.2"/><path d="M12 3.5v5.3"/><path d="M12 15.2v5.3"/>`,
  did: `<rect x="2.5" y="6" width="19" height="12" rx="2"/><circle cx="8" cy="11.5" r="2.4"/><path d="M5 16a3.5 3.5 0 0 1 6 0"/><path d="M14 10.5h4.5"/><path d="M14 14h3"/>`,
  orchestration: `<circle cx="5" cy="5.5" r="1.9"/><circle cx="5" cy="12" r="1.9"/><circle cx="5" cy="18.5" r="1.9"/><circle cx="18.5" cy="12" r="2.1"/><path d="M6.9 6.2 16.6 11.1"/><path d="M7 12h9.4"/><path d="M6.9 17.8 16.6 12.9"/>`,
  attestation: `<circle cx="12" cy="9.5" r="5.5"/><path d="M9.5 9.5l1.8 1.8 3.2-3.4"/><path d="M9 14l-1.5 6.5 4.5-2.4 4.5 2.4L15 14"/>`,

  /* ——— Paxeer Chain ——— */
  block: `<path d="M12 3.5 20 7.8v8.4L12 20.5 4 16.2V7.8z"/><path d="M4 7.8 12 12l8-4.2"/><path d="M12 12v8.5"/>`,
  network: `<circle cx="12" cy="5" r="2"/><circle cx="5.5" cy="18" r="2"/><circle cx="18.5" cy="18" r="2"/><path d="M11 6.8 6.5 16.2"/><path d="M13 6.8 17.5 16.2"/><path d="M7.5 18h9"/>`,
  payments: `<rect x="2.5" y="6" width="19" height="12" rx="2"/><path d="M2.5 10h19"/><path d="M6 14.5h4"/>`,
  liquidity: `<path d="M12 3.5C8.5 8 6.5 11 6.5 14a5.5 5.5 0 0 0 11 0c0-3-2-6-5.5-10.5z"/><path d="M9.5 14.5a2.5 2.5 0 0 0 2 2.3"/>`,
  reputation: `<path d="M12 4l2.4 4.9 5.4.8-3.9 3.8.9 5.4L12 16.4 7.2 18.9l.9-5.4L4.2 9.7l5.4-.8L12 4z"/>`,
  orderbook: `<path d="M11.5 4v16"/><path d="M11.5 7H6.5"/><path d="M11.5 10H4.5"/><path d="M11.5 13H7"/><path d="M13.5 9h5"/><path d="M13.5 12h6.5"/><path d="M13.5 15h4"/>`,
  swap: `<path d="M5 8.5h12"/><path d="M14 5.5l3 3-3 3"/><path d="M19 15.5H7"/><path d="M10 12.5l-3 3 3 3"/>`,
  rpc: `<circle cx="5" cy="12" r="2"/><circle cx="19" cy="12" r="2"/><path d="M7.5 9.8h7"/><path d="M12.5 7.8l2.5 2-2.5 2"/><path d="M16.5 14.2h-7"/><path d="M11.5 12.2l-2.5 2 2.5 2"/>`,
  wallet: `<path d="M4 8a2 2 0 0 1 2-2h11a1 1 0 0 1 1 1v1"/><rect x="4" y="8" width="16" height="11" rx="2"/><path d="M20 12.5h-3.3a2 2 0 0 0 0 4H20"/>`,

  /* ——— Extensions (same Centra AI style, for primitives/concepts not in the base set) ——— */
  circle: `<circle cx="12" cy="12" r="9"/>`,
  'circle-dot': `<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="2" fill="currentColor" stroke="none"/>`,
  activity: `<path d="M3 12h4l2.5-7 5 14 2.5-7H21"/>`,
  cpu: `<rect x="6" y="6" width="12" height="12" rx="2"/><rect x="9.5" y="9.5" width="5" height="5" rx="1"/><path d="M9 3v3"/><path d="M15 3v3"/><path d="M9 18v3"/><path d="M15 18v3"/><path d="M3 9h3"/><path d="M3 15h3"/><path d="M18 9h3"/><path d="M18 15h3"/>`,
  sparkles: `<path d="M12 4l1.6 4.4L18 10l-4.4 1.6L12 16l-1.6-4.4L6 10l4.4-1.6L12 4z"/><path d="M18.5 4.5l.7 1.9 1.9.7-1.9.7-.7 1.9-.7-1.9-1.9-.7 1.9-.7.7-1.9z"/>`,
  zap: `<path d="M13 3 5 13h6l-1 8 8-10h-6l1-8z"/>`,
  lightbulb: `<path d="M9 18h6"/><path d="M10 21h4"/><path d="M12 3a6 6 0 0 0-4 10.5c.8.8 1.3 1.6 1.5 2.5h5c.2-.9.7-1.7 1.5-2.5A6 6 0 0 0 12 3z"/>`,
  monitor: `<rect x="3" y="4.5" width="18" height="12" rx="2"/><path d="M9 20.5h6"/><path d="M12 16.5v4"/>`,
  wifi: `<path d="M5 11a10 10 0 0 1 14 0"/><path d="M8 14a6 6 0 0 1 8 0"/><path d="M11 17a2 2 0 0 1 2 0"/><circle cx="12" cy="19.5" r="0.6" fill="currentColor" stroke="none"/>`,
  'wifi-off': `<path d="M3 3 21 21"/><path d="M9 8.2A10 10 0 0 1 19 11"/><path d="M5 11a10 10 0 0 1 3-2.2"/><path d="M8.5 14.2A6 6 0 0 1 15 13"/><path d="M11 17a2 2 0 0 1 2 0"/><circle cx="12" cy="19.5" r="0.6" fill="currentColor" stroke="none"/>`,
  'grip-vertical': `<circle cx="9" cy="6" r="1.1" fill="currentColor" stroke="none"/><circle cx="9" cy="12" r="1.1" fill="currentColor" stroke="none"/><circle cx="9" cy="18" r="1.1" fill="currentColor" stroke="none"/><circle cx="15" cy="6" r="1.1" fill="currentColor" stroke="none"/><circle cx="15" cy="12" r="1.1" fill="currentColor" stroke="none"/><circle cx="15" cy="18" r="1.1" fill="currentColor" stroke="none"/>`,
  'panel-left': `<rect x="3.5" y="5" width="17" height="14" rx="2"/><path d="M9.5 5v14"/>`,
  'chevrons-up-down': `<path d="M8 9l4-4 4 4"/><path d="M8 15l4 4 4-4"/>`,
  'corner-down-left': `<path d="M9 10l-4 4 4 4"/><path d="M19 6v6a2 2 0 0 1-2 2H5"/>`,
  'git-commit': `<circle cx="12" cy="12" r="3.2"/><path d="M3 12h5.8"/><path d="M15.2 12H21"/>`,
}

/** All available Centra AI icon names. */
export const CENTRA_ICON_NAMES = Object.keys(CENTRA_ICON_BODIES)

/** Legacy exports retained while existing imports migrate to the Centra names. */
export const MATRIX_TO_SPRITE = CENTRA_TO_SPRITE
export const MATRIX_ICON_BODIES = CENTRA_ICON_BODIES
export const MATRIX_ICON_NAMES = CENTRA_ICON_NAMES

export interface CentraIconProps extends React.SVGProps<SVGSVGElement> {
  /** Width/height of the icon. Defaults to 24. */
  size?: string | number
  /** Keep the visual stroke width constant when scaling (Lucide compat). */
  absoluteStrokeWidth?: boolean
}

export type MatrixIconProps = CentraIconProps

/** Lucide-compatible type aliases so existing `LucideIcon`/`LucideProps` usages keep working. */
export type LucideProps = CentraIconProps
export type LucideIcon = React.ForwardRefExoticComponent<
  CentraIconProps & React.RefAttributes<SVGSVGElement>
>

const cache: Record<string, LucideIcon> = {}

/** Build (and memoize) a React component for a given Centra AI icon name. */
export function createCentraIcon(name: string): LucideIcon {
  if (cache[name]) return cache[name]

  const sprite = CENTRA_TO_SPRITE[name]
  const body = CENTRA_ICON_BODIES[name] ?? ''
  const Component = React.forwardRef<SVGSVGElement, CentraIconProps>(function CentraGlyph(
    {
      size = 30,
      strokeWidth = 2,
      absoluteStrokeWidth = false,
      color = 'currentColor',
      className,
      children: _children,
      ...props
    },
    ref,
  ) {
    // Ported paxscan glyph: reference the compiled sprite symbol. The host
    // <svg> carries no stroke/viewBox so monochrome symbols inherit
    // `currentColor` and polychrome symbols keep their own colours, each
    // scaled to the requested size by the symbol's intrinsic viewBox.
    if (sprite) {
      return (
        <svg
          ref={ref}
          xmlns="http://www.w3.org/2000/svg"
          width={size}
          height={size}
          fill="currentColor"
          color={color}
          className={className}
          aria-hidden="true"
          data-centra-icon={name}
          data-matrix-icon={name}
          {...props}
        >
          {/* Same-document fragment — see IconSpriteSheet in app layout. */}
          <use href={`#${sprite}`} fill="currentColor" />
        </svg>
      )
    }
    return (
      <svg
        ref={ref}
        xmlns="http://www.w3.org/2000/svg"
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        stroke={color}
        strokeWidth={absoluteStrokeWidth ? (Number(strokeWidth) * 24) / Number(size) : strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
        className={className}
        data-centra-icon={name}
        data-matrix-icon={name}
        {...props}
        dangerouslySetInnerHTML={{ __html: sanitizeSvgMarkup(body) }}
      />
    )
  })
  Component.displayName = name
  cache[name] = Component
  return Component
}

export const createMatrixIcon = createCentraIcon

export interface CentraIconByNameProps extends CentraIconProps {
  name: string
}

export type MatrixIconByNameProps = CentraIconByNameProps

/** Render any Centra AI icon by name: `<CentraIcon name="merkle" />`. */
export const CentraIcon = React.forwardRef<SVGSVGElement, CentraIconByNameProps>(
  function CentraIcon({ name, ...props }, ref) {
    const Icon = createCentraIcon(name)
    return <Icon ref={ref} {...props} />
  },
)

export const MatrixIcon = CentraIcon

/*
 * ───────────────────────────────────────────────────────────────────────────
 * Lucide-compatible named exports.
 *
 * Each former `lucide-react` import name is re-exported here, mapped to the
 * closest Centra AI glyph, so files only need to switch their import source.
 * ───────────────────────────────────────────────────────────────────────────
 */

// Navigation / arrows / chevrons
export const ArrowLeft = createCentraIcon('arrow-left')
export const ArrowLeftIcon = ArrowLeft
export const ArrowRight = createCentraIcon('arrow-right')
export const ArrowRightIcon = ArrowRight
export const ArrowUp = createCentraIcon('arrow-up')
export const ArrowUpIcon = ArrowUp
export const ChevronUp = createCentraIcon('chevron-up')
export const ChevronUpIcon = ChevronUp
export const ChevronDown = createCentraIcon('chevron-down')
export const ChevronDownIcon = ChevronDown
export const ArrowDownIcon = ChevronDown
export const ChevronLeft = createCentraIcon('chevron-left')
export const ChevronLeftIcon = ChevronLeft
export const ChevronRight = createCentraIcon('chevron-right')
export const ChevronRightIcon = ChevronRight
export const ChevronsUpDownIcon = createCentraIcon('chevrons-up-down')
export const CornerDownLeftIcon = createCentraIcon('corner-down-left')
export const Search = createCentraIcon('search')
export const SearchIcon = Search
export const MoreHorizontal = createCentraIcon('more')
export const MoreHorizontalIcon = MoreHorizontal
export const ExternalLink = createCentraIcon('external-link')
export const ExternalLinkIcon = ExternalLink
export const SlidersHorizontal = createCentraIcon('filter')
export const PanelLeftIcon = createCentraIcon('panel-left')
export const GripVerticalIcon = createCentraIcon('grip-vertical')

// Actions
export const Plus = createCentraIcon('plus')
export const PlusIcon = Plus
export const Minus = createCentraIcon('minus')
export const MinusIcon = Minus
export const Check = createCentraIcon('check')
export const CheckIcon = Check
export const CheckCheck = Check
export const X = createCentraIcon('close')
export const XIcon = X
export const Copy = createCentraIcon('copy')
export const CopyIcon = Copy
export const Trash2Icon = createCentraIcon('trash')
export const Download = createCentraIcon('download')
export const DownloadIcon = Download
export const ArrowDownToLine = Download
export const ArrowDownLeft = Download
export const ArrowUpFromLine = createCentraIcon('upload')
export const ArrowUpRight = ArrowUpFromLine
export const RotateCcw = createCentraIcon('refresh')
export const Repeat = RotateCcw
export const Settings = createCentraIcon('settings')
export const Link2 = createCentraIcon('link')
export const BookmarkIcon = createCentraIcon('bookmark')

// Status
export const CheckCircle = createCentraIcon('success')
export const CheckCircleIcon = CheckCircle
export const CheckCircle2 = CheckCircle
export const CheckCircle2Icon = CheckCircle
export const CircleCheck = CheckCircle
export const CircleX = createCentraIcon('error')
export const XCircleIcon = CircleX
export const AlertTriangle = createCentraIcon('warning')
export const AlertTriangleIcon = AlertTriangle
export const AlertCircle = AlertTriangle
export const CircleAlert = AlertTriangle
export const OctagonAlert = AlertTriangle
export const Loader2 = createCentraIcon('loading')
export const Loader2Icon = Loader2
export const LifeBuoy = createCentraIcon('help')

// Content & files
export const FileIcon = createCentraIcon('file')
export const FileText = createCentraIcon('document')
export const FileTextIcon = FileText
export const ReceiptText = FileText
export const BookIcon = FileText
export const FolderIcon = createCentraIcon('folder')
export const FolderOpenIcon = FolderIcon
export const Paperclip = createCentraIcon('attachment')
export const PaperclipIcon = Paperclip
export const ImageIcon = createCentraIcon('image')
export const Code = createCentraIcon('code')
export const Github = Code
export const TerminalIcon = createCentraIcon('terminal')
export const Database = createCentraIcon('database')
export const Globe = createCentraIcon('globe')
export const GlobeIcon = Globe
export const Clock = createCentraIcon('clock')
export const ClockIcon = Clock
export const Timer = Clock
export const CalendarClock = Clock

// Communication
export const MessageCircleIcon = createCentraIcon('chat')
export const MessageSquare = createCentraIcon('message')
export const Bell = createCentraIcon('bell')
export const Mail = createCentraIcon('mail')
export const Send = createCentraIcon('send')
export const Rocket = Send

// Account & security
export const Lock = createCentraIcon('lock')
export const ShieldCheck = createCentraIcon('shield-check')
export const ShieldAlert = ShieldCheck
export const LogOut = createCentraIcon('logout')
export const Bug = createCentraIcon('warning')
export const SkipForward = createCentraIcon('arrow-right')

// Media
export const Play = createCentraIcon('play')
export const PlayIcon = Play
export const VideoIcon = Play
export const Pause = createCentraIcon('pause')
export const PauseIcon = Pause
export const PauseCircle = Pause
export const SquareIcon = createCentraIcon('stop')
export const Mic = createCentraIcon('mic')
export const MicIcon = Mic
export const MicOffIcon = Mic
export const Music2Icon = createCentraIcon('volume')
export const Eye = createCentraIcon('eye')
export const EyeIcon = Eye
export const EyeOffIcon = Eye

// Centra AI core / Paxeer concepts
export const Bot = createCentraIcon('agent')
export const BotIcon = Bot
export const BrainIcon = createCentraIcon('cortex')
export const Wrench = createCentraIcon('tools')
export const WrenchIcon = Wrench
export const Puzzle = Wrench
export const Workflow = createCentraIcon('orchestration')
export const BadgeCheck = createCentraIcon('attestation')
export const Gauge = createCentraIcon('intent')
export const Radio = createCentraIcon('network')
export const Package = createCentraIcon('block')
export const PackageIcon = Package
export const Coins = createCentraIcon('payments')
export const Wallet = createCentraIcon('wallet')
export const LayoutDashboard = createCentraIcon('dashboard')
export const Cpu = createCentraIcon('cpu')
export const Activity = createCentraIcon('activity')

// Generic primitives / extras
export const CircleIcon = createCentraIcon('circle')
export const CircleSmallIcon = CircleIcon
export const DotIcon = CircleIcon
export const CircleDotIcon = createCentraIcon('circle-dot')
export const Sparkles = createCentraIcon('sparkles')
export const Zap = createCentraIcon('zap')
export const Lightbulb = createCentraIcon('lightbulb')
export const Monitor = createCentraIcon('monitor')
export const Wifi = createCentraIcon('wifi')
export const WifiOff = createCentraIcon('wifi-off')
export const GitCommitIcon = createCentraIcon('git-commit')
export const GitCommitHorizontal = GitCommitIcon

// Voice-persona icons have no dedicated equivalent — fall back to the user glyph.
export const MarsIcon = createCentraIcon('user')
export const VenusIcon = MarsIcon
export const NonBinaryIcon = MarsIcon
export const TransgenderIcon = MarsIcon
export const MarsStrokeIcon = MarsIcon
export const VenusAndMarsIcon = MarsIcon

/**
 * Registry used for dynamic, name-based lookups (e.g. ApprovalCard's `icon`
 * prop). Resolves both Centra AI kebab-case names and PascalCase aliases.
 */
export const icons: Record<string, LucideIcon> = new Proxy({} as Record<string, LucideIcon>, {
  get(_target, prop) {
    if (typeof prop !== 'string') return undefined
    const kebab = prop.includes('-')
      ? prop
      : prop.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase()
    if (CENTRA_ICON_BODIES[kebab]) return createCentraIcon(kebab)
    return undefined
  },
})
