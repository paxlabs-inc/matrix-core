/* Centra AI Icon Set — 24×24 line icons, 1.75px stroke, round caps/joins.
   Each icon `b` (body) is inner SVG markup; the host <svg> sets the stroke. */
window.MATRIX_ICON_CATEGORIES = [
  'Navigation',
  'Actions',
  'Status',
  'Content & Files',
  'Communication',
  'Account & Security',
  'Media',
  'Centra AI Core',
  'Paxeer Chain',
]

window.MATRIX_ICONS = [
  /* ——— Navigation ——————————————————————————————— */
  {
    n: 'home',
    c: 'Navigation',
    k: 'house start index dashboard',
    b: `<path d="M4 10.5 12 4l8 6.5"/><path d="M6 9.5V19a1 1 0 0 0 1 1h3v-4.5a2 2 0 0 1 4 0V20h3a1 1 0 0 0 1-1V9.5"/>`,
  },
  {
    n: 'dashboard',
    c: 'Navigation',
    k: 'grid apps panels overview tiles',
    b: `<rect x="4" y="4" width="7" height="7" rx="1.4"/><rect x="13" y="4" width="7" height="7" rx="1.4"/><rect x="4" y="13" width="7" height="7" rx="1.4"/><rect x="13" y="13" width="7" height="7" rx="1.4"/>`,
  },
  {
    n: 'arrow-left',
    c: 'Navigation',
    k: 'back previous return',
    b: `<path d="M19 12H5"/><path d="M11 6l-6 6 6 6"/>`,
  },
  {
    n: 'arrow-right',
    c: 'Navigation',
    k: 'forward next continue',
    b: `<path d="M5 12h14"/><path d="M13 6l6 6-6 6"/>`,
  },
  { n: 'chevron-up', c: 'Navigation', k: 'collapse expand caret', b: `<path d="M6 15l6-6 6 6"/>` },
  {
    n: 'chevron-down',
    c: 'Navigation',
    k: 'expand dropdown caret more',
    b: `<path d="M6 9l6 6 6-6"/>`,
  },
  { n: 'chevron-left', c: 'Navigation', k: 'previous back caret', b: `<path d="M15 6l-6 6 6 6"/>` },
  {
    n: 'chevron-right',
    c: 'Navigation',
    k: 'next forward caret disclosure',
    b: `<path d="M9 6l6 6-6 6"/>`,
  },
  {
    n: 'menu',
    c: 'Navigation',
    k: 'hamburger nav lines bars',
    b: `<path d="M4 7h16"/><path d="M4 12h16"/><path d="M4 17h16"/>`,
  },
  {
    n: 'close',
    c: 'Navigation',
    k: 'x dismiss cancel remove',
    b: `<path d="M6 6l12 12"/><path d="M18 6 6 18"/>`,
  },
  {
    n: 'search',
    c: 'Navigation',
    k: 'find query magnify look',
    b: `<circle cx="11" cy="11" r="6.5"/><path d="M16 16l4.5 4.5"/>`,
  },
  {
    n: 'more',
    c: 'Navigation',
    k: 'ellipsis kebab options overflow',
    b: `<circle cx="5" cy="12" r="1.1"/><circle cx="12" cy="12" r="1.1"/><circle cx="19" cy="12" r="1.1"/>`,
  },
  {
    n: 'filter',
    c: 'Navigation',
    k: 'sort funnel refine narrow',
    b: `<path d="M4 6h16"/><path d="M7 12h10"/><path d="M10 18h4"/>`,
  },
  {
    n: 'external-link',
    c: 'Navigation',
    k: 'open new tab outbound launch',
    b: `<path d="M14 5h5v5"/><path d="M19 5l-8 8"/><path d="M17 13.5V18a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 5 18V9a1.5 1.5 0 0 1 1.5-1.5H11"/>`,
  },

  /* ——— Actions ——————————————————————————————————— */
  {
    n: 'plus',
    c: 'Actions',
    k: 'add create new increment',
    b: `<path d="M12 5v14"/><path d="M5 12h14"/>`,
  },
  { n: 'minus', c: 'Actions', k: 'remove subtract decrement collapse', b: `<path d="M5 12h14"/>` },
  {
    n: 'check',
    c: 'Actions',
    k: 'done confirm accept tick',
    b: `<path d="M5 12.5l4.5 4.5L19 7"/>`,
  },
  {
    n: 'edit',
    c: 'Actions',
    k: 'pencil write modify compose',
    b: `<path d="M14.5 5.5l4 4"/><path d="M4 20l1-4.2L15.5 5.3a1.5 1.5 0 0 1 2.1 0l1.1 1.1a1.5 1.5 0 0 1 0 2.1L8.2 19 4 20z"/>`,
  },
  {
    n: 'trash',
    c: 'Actions',
    k: 'delete bin remove discard',
    b: `<path d="M5 7h14"/><path d="M9.5 7V5.5a1.2 1.2 0 0 1 1.2-1.2h2.6a1.2 1.2 0 0 1 1.2 1.2V7"/><path d="M7 7l.9 12a1.5 1.5 0 0 0 1.5 1.4h5.2a1.5 1.5 0 0 0 1.5-1.4L17 7"/><path d="M10.5 11v5"/><path d="M13.5 11v5"/>`,
  },
  {
    n: 'copy',
    c: 'Actions',
    k: 'duplicate clone clipboard',
    b: `<rect x="8.5" y="8.5" width="11" height="11" rx="2"/><path d="M15.5 8.5V6.5a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/>`,
  },
  {
    n: 'share',
    c: 'Actions',
    k: 'send distribute nodes export',
    b: `<circle cx="6" cy="12" r="2.4"/><circle cx="18" cy="6" r="2.4"/><circle cx="18" cy="18" r="2.4"/><path d="M8.1 10.9 15.9 7.1"/><path d="M8.1 13.1 15.9 16.9"/>`,
  },
  {
    n: 'download',
    c: 'Actions',
    k: 'save import pull receive',
    b: `<path d="M12 4v12"/><path d="M7.5 11.5 12 16l4.5-4.5"/><path d="M5 19h14"/>`,
  },
  {
    n: 'upload',
    c: 'Actions',
    k: 'export push send submit',
    b: `<path d="M12 20V8"/><path d="M7.5 12.5 12 8l4.5 4.5"/><path d="M5 5h14"/>`,
  },
  {
    n: 'refresh',
    c: 'Actions',
    k: 'reload sync retry cycle update',
    b: `<path d="M4.5 12a7.5 7.5 0 0 1 12.9-5.2"/><path d="M18 3v4h-4"/><path d="M19.5 12a7.5 7.5 0 0 1-12.9 5.2"/><path d="M6 21v-4h4"/>`,
  },
  {
    n: 'settings',
    c: 'Actions',
    k: 'gear cog preferences config options',
    b: `<circle cx="12" cy="12" r="3"/><path d="M19.4 13a7.6 7.6 0 0 0 0-2l2-1.5-2-3.5-2.4 1a7.6 7.6 0 0 0-1.7-1L14.9 2.5h-3.8L10.7 5a7.6 7.6 0 0 0-1.7 1L6.6 5l-2 3.5L6.6 10a7.6 7.6 0 0 0 0 2l-2 1.5 2 3.5 2.4-1a7.6 7.6 0 0 0 1.7 1l.4 2.5h3.8l.4-2.5a7.6 7.6 0 0 0 1.7-1l2.4 1 2-3.5z"/>`,
  },
  {
    n: 'link',
    c: 'Actions',
    k: 'chain url connect anchor attach',
    b: `<path d="M9.5 14.5 14.5 9.5"/><path d="M8.5 11.5 6.5 13.5a3 3 0 0 0 4.2 4.2l2-2"/><path d="M15.5 12.5l2-2a3 3 0 0 0-4.2-4.2l-2 2"/>`,
  },
  {
    n: 'bookmark',
    c: 'Actions',
    k: 'save favorite flag pin keep',
    b: `<path d="M7 4.5h10a1 1 0 0 1 1 1V20l-6-4-6 4V5.5a1 1 0 0 1 1-1z"/>`,
  },

  /* ——— Status ——————————————————————————————————— */
  {
    n: 'success',
    c: 'Status',
    k: 'check ok done valid complete pass',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M8.5 12.2l2.4 2.4 4.6-4.8"/>`,
  },
  {
    n: 'error',
    c: 'Status',
    k: 'fail invalid reject cross stop',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M9 9l6 6"/><path d="M15 9l-6 6"/>`,
  },
  {
    n: 'warning',
    c: 'Status',
    k: 'alert caution attention risk',
    b: `<path d="M10.3 4.8 2.6 18a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 4.8a2 2 0 0 0-3.4 0z"/><path d="M12 10v4"/><path d="M12 17.5h.01"/>`,
  },
  {
    n: 'info',
    c: 'Status',
    k: 'information about detail note',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><path d="M12 8h.01"/>`,
  },
  {
    n: 'loading',
    c: 'Status',
    k: 'spinner busy progress wait pending',
    b: `<path d="M12 3.5a8.5 8.5 0 1 0 8.5 8.5"/>`,
  },
  {
    n: 'help',
    c: 'Status',
    k: 'question support faq unknown',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M9.6 9.6a2.5 2.5 0 0 1 4.7 1.1c0 1.7-2.3 2-2.3 3.5"/><path d="M12 17h.01"/>`,
  },

  /* ——— Content & Files ——————————————————————————— */
  {
    n: 'file',
    c: 'Content & Files',
    k: 'document page blank empty',
    b: `<path d="M7 3.5h6l5 5V19a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 19V5a1.5 1.5 0 0 1 1.5-1.5z"/><path d="M13 3.5V9h5"/>`,
  },
  {
    n: 'document',
    c: 'Content & Files',
    k: 'file text lines page content',
    b: `<path d="M7 3.5h6l5 5V19a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 19V5a1.5 1.5 0 0 1 1.5-1.5z"/><path d="M13 3.5V9h5"/><path d="M9 13.5h6"/><path d="M9 16.5h4"/>`,
  },
  {
    n: 'folder',
    c: 'Content & Files',
    k: 'directory files group archive',
    b: `<path d="M4 8a2 2 0 0 1 2-2h3.2l1.6 1.8H18a2 2 0 0 1 2 2V17a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 17V8z"/>`,
  },
  {
    n: 'attachment',
    c: 'Content & Files',
    k: 'paperclip attach clip file',
    b: `<path d="M18.5 11.5 11 19a4 4 0 0 1-5.7-5.7l8-8a2.7 2.7 0 0 1 3.8 3.8l-7.7 7.7a1.3 1.3 0 0 1-1.9-1.9l6.9-6.9"/>`,
  },
  {
    n: 'tag',
    c: 'Content & Files',
    k: 'label price category metadata',
    b: `<path d="M3.5 12.5V6a2 2 0 0 1 2-2h6.5L20 12.5 12.5 20 3.5 12.5z"/><circle cx="8" cy="8" r="1.3"/>`,
  },
  {
    n: 'image',
    c: 'Content & Files',
    k: 'photo picture media gallery',
    b: `<rect x="4" y="5" width="16" height="14" rx="2"/><circle cx="9" cy="10" r="1.6"/><path d="M5 17l4.5-4 3 2.5L16 11l3 3.5"/>`,
  },
  {
    n: 'code',
    c: 'Content & Files',
    k: 'script source brackets develop syntax',
    b: `<path d="M9 8l-4 4 4 4"/><path d="M15 8l4 4-4 4"/><path d="M13.5 6l-3 12"/>`,
  },
  {
    n: 'terminal',
    c: 'Content & Files',
    k: 'console command shell cli prompt',
    b: `<rect x="3.5" y="5" width="17" height="14" rx="2"/><path d="M8 10l2.5 2.5L8 15"/><path d="M13 15h4"/>`,
  },
  {
    n: 'database',
    c: 'Content & Files',
    k: 'storage data store records pebble',
    b: `<ellipse cx="12" cy="6" rx="7" ry="3"/><path d="M5 6v6c0 1.7 3.1 3 7 3s7-1.3 7-3V6"/><path d="M5 12v6c0 1.7 3.1 3 7 3s7-1.3 7-3v-6"/>`,
  },
  {
    n: 'globe',
    c: 'Content & Files',
    k: 'world network decentralized global web',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17"/><path d="M12 3.5c2.2 2.3 3.4 5.3 3.4 8.5s-1.2 6.2-3.4 8.5c-2.2-2.3-3.4-5.3-3.4-8.5S9.8 5.8 12 3.5z"/>`,
  },
  {
    n: 'clock',
    c: 'Content & Files',
    k: 'time history recent schedule pending',
    b: `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>`,
  },

  /* ——— Communication ————————————————————————————— */
  {
    n: 'chat',
    c: 'Communication',
    k: 'bubble talk conversation comment',
    b: `<path d="M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H9l-5 4V6z"/>`,
  },
  {
    n: 'message',
    c: 'Communication',
    k: 'bubble dots conversation reply chat',
    b: `<path d="M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H9l-5 4V6z"/><path d="M8.5 9.5h.01"/><path d="M12 9.5h.01"/><path d="M15.5 9.5h.01"/>`,
  },
  {
    n: 'bell',
    c: 'Communication',
    k: 'notification alert ring alarm',
    b: `<path d="M6 9.5a6 6 0 0 1 12 0c0 4 1.2 5.3 2 6.3H4c.8-1 2-2.3 2-6.3z"/><path d="M9.5 18.5a2.5 2.5 0 0 0 5 0"/>`,
  },
  {
    n: 'mail',
    c: 'Communication',
    k: 'email envelope inbox letter',
    b: `<rect x="3" y="5.5" width="18" height="13" rx="2"/><path d="M3.5 7.5l8.5 6 8.5-6"/>`,
  },
  {
    n: 'send',
    c: 'Communication',
    k: 'paper plane submit dispatch deliver',
    b: `<path d="M21 3.5 2.5 11.2l7.4 2.4 2.4 7.4L21 3.5z"/><path d="M9.9 13.6 14 9.5"/>`,
  },
  {
    n: 'mention',
    c: 'Communication',
    k: 'at reference tag user handle',
    b: `<circle cx="12" cy="12" r="4"/><path d="M16 8v5a2.5 2.5 0 0 0 4.4 1.6A8.5 8.5 0 1 0 16.5 19"/>`,
  },
  {
    n: 'phone',
    c: 'Communication',
    k: 'call dial telephone contact',
    b: `<path d="M5 4.5h3l1.5 4-2 1.5a11 11 0 0 0 5 5l1.5-2 4 1.5v3a1.5 1.5 0 0 1-1.6 1.5A15.5 15.5 0 0 1 3.5 6.1 1.5 1.5 0 0 1 5 4.5z"/>`,
  },

  /* ——— Account & Security ————————————————————————— */
  {
    n: 'user',
    c: 'Account & Security',
    k: 'person profile account avatar',
    b: `<circle cx="12" cy="8.5" r="3.5"/><path d="M5.5 20a6.5 6.5 0 0 1 13 0"/>`,
  },
  {
    n: 'users',
    c: 'Account & Security',
    k: 'people team group members',
    b: `<circle cx="9" cy="8.5" r="3.2"/><path d="M3.5 19.5a5.5 5.5 0 0 1 11 0"/><path d="M16 5.6a3.2 3.2 0 0 1 0 6"/><path d="M17 14.4a5.5 5.5 0 0 1 3.5 5.1"/>`,
  },
  {
    n: 'lock',
    c: 'Account & Security',
    k: 'secure private locked encrypt closed',
    b: `<rect x="5" y="10.5" width="14" height="10" rx="2"/><path d="M8 10.5V8a4 4 0 0 1 8 0v2.5"/><path d="M12 14.5v2.5"/>`,
  },
  {
    n: 'unlock',
    c: 'Account & Security',
    k: 'open access unsecure decrypt',
    b: `<rect x="5" y="10.5" width="14" height="10" rx="2"/><path d="M8 10.5V8a4 4 0 0 1 7.7-1.5"/><path d="M12 14.5v2.5"/>`,
  },
  {
    n: 'key',
    c: 'Account & Security',
    k: 'access credential secret unlock auth',
    b: `<circle cx="8.5" cy="8.5" r="4"/><path d="M11.3 11.3 19 19"/><path d="M16.5 18.5l2-2"/><path d="M14.5 16.5l2-2"/>`,
  },
  {
    n: 'shield',
    c: 'Account & Security',
    k: 'protect security defend safe guard',
    b: `<path d="M12 3.5 19 6.2V11c0 4.5-3 8-7 9.5C8 19 5 15.5 5 11V6.2L12 3.5z"/>`,
  },
  {
    n: 'shield-check',
    c: 'Account & Security',
    k: 'verified secure trusted protected valid',
    b: `<path d="M12 3.5 19 6.2V11c0 4.5-3 8-7 9.5C8 19 5 15.5 5 11V6.2L12 3.5z"/><path d="M9 11.5l2 2 4-4"/>`,
  },
  {
    n: 'logout',
    c: 'Account & Security',
    k: 'exit sign out leave quit',
    b: `<path d="M9 4.5H6.5A1.5 1.5 0 0 0 5 6v12a1.5 1.5 0 0 0 1.5 1.5H9"/><path d="M16 16l4-4-4-4"/><path d="M20 12H10"/>`,
  },
  {
    n: 'fingerprint',
    c: 'Account & Security',
    k: 'biometric identity did auth unique',
    b: `<path d="M5.5 11a6.5 6.5 0 0 1 12.7-2"/><path d="M8 12a4 4 0 0 1 8 0v1.5"/><path d="M12 12v3a4 4 0 0 0 1.5 3.2"/><path d="M16 14.5a8 8 0 0 1-1 4.5"/><path d="M8.8 19a8 8 0 0 1-1.3-5"/>`,
  },

  /* ——— Media ——————————————————————————————————— */
  {
    n: 'play',
    c: 'Media',
    k: 'start run execute resume go',
    b: `<path d="M8 5.5v13l11-6.5-11-6.5z"/>`,
  },
  {
    n: 'pause',
    c: 'Media',
    k: 'stop hold suspend wait',
    b: `<path d="M9 5v14"/><path d="M15 5v14"/>`,
  },
  {
    n: 'stop',
    c: 'Media',
    k: 'halt end terminate cancel',
    b: `<rect x="6.5" y="6.5" width="11" height="11" rx="2.5"/>`,
  },
  {
    n: 'mic',
    c: 'Media',
    k: 'microphone voice record speak audio',
    b: `<rect x="9" y="3.5" width="6" height="11" rx="3"/><path d="M6 11a6 6 0 0 0 12 0"/><path d="M12 17v3.5"/><path d="M8.5 20.5h7"/>`,
  },
  {
    n: 'volume',
    c: 'Media',
    k: 'sound speaker audio output',
    b: `<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15.5 9.5a4 4 0 0 1 0 5"/><path d="M18 7.5a7 7 0 0 1 0 9"/>`,
  },
  {
    n: 'camera',
    c: 'Media',
    k: 'photo capture lens picture',
    b: `<path d="M4 8.5a1.5 1.5 0 0 1 1.5-1.5H8l1.5-2h5L16 7h2.5A1.5 1.5 0 0 1 20 8.5V18a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 18V8.5z"/><circle cx="12" cy="12.5" r="3.2"/>`,
  },
  {
    n: 'eye',
    c: 'Media',
    k: 'view visible preview watch show inspect',
    b: `<path d="M2.5 12S6 6 12 6s9.5 6 9.5 6-3.5 6-9.5 6-9.5-6-9.5-6z"/><circle cx="12" cy="12" r="2.8"/>`,
  },

  /* ——— Centra AI Core ——————————————————————————————— */
  {
    n: 'mcl',
    c: 'Centra AI Core',
    k: 'centrascript compiler runtime natural language intent transform',
    b: `<path d="M4 8h5"/><path d="M4 12h4"/><path d="M4 16h5"/><path d="M11.5 12h6"/><path d="M15 9.5 17.5 12l-2.5 2.5"/><path d="M20.5 8v8"/>`,
  },
  {
    n: 'intent',
    c: 'Centra AI Core',
    k: 'aim goal target objective ask resolve',
    b: `<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3.2"/><path d="M12 1.5V5"/><path d="M12 19v3.5"/><path d="M1.5 12H5"/><path d="M19 12h3.5"/>`,
  },
  {
    n: 'intent-ir',
    c: 'Centra AI Core',
    k: 'typed intermediate representation structured inspectable braces',
    b: `<path d="M9 5c-1.7 0-2.4 1-2.4 2.6v1.5c0 1.3-.7 2.1-2 2.4 1.3.3 2 1.1 2 2.4v1.5C6.6 19 7.3 20 9 20"/><path d="M15 5c1.7 0 2.4 1 2.4 2.6v1.5c0 1.3.7 2.1 2 2.4-1.3.3-2 1.1-2 2.4v1.5C17.4 19 16.7 20 15 20"/><path d="M12 12h.01"/>`,
  },
  {
    n: 'cortex',
    c: 'Centra AI Core',
    k: 'memory graph typed knowledge nodes facts beliefs context',
    b: `<circle cx="12" cy="12" r="2.3"/><circle cx="5" cy="6.5" r="1.8"/><circle cx="19" cy="6.5" r="1.8"/><circle cx="5.5" cy="18" r="1.8"/><circle cx="18.5" cy="18" r="1.8"/><path d="M10.2 10.7 6.4 7.6"/><path d="M13.8 10.7 17.6 7.6"/><path d="M10.4 13.3 7 16.5"/><path d="M13.6 13.3 17 16.5"/>`,
  },
  {
    n: 'agent',
    c: 'Centra AI Core',
    k: 'did bound assistant bot dispatch reliable autonomous',
    b: `<path d="M12 3.2l7.4 4.2v8.4L12 20l-7.4-4.2V7.4z"/><circle cx="12" cy="10.5" r="2.2"/><path d="M8.4 16.4a4 4 0 0 1 7.2 0"/>`,
  },
  {
    n: 'tools',
    c: 'Centra AI Core',
    k: 'wrench primitives chain wrap utility configure',
    b: `<path d="M15.5 4.5a4.5 4.5 0 0 0-5.8 5.6L4 15.8a1.6 1.6 0 0 0 2.3 2.3l5.7-5.7a4.5 4.5 0 0 0 5.6-5.8l-2.7 2.7-2.5-.7-.7-2.5 2.5-2.6z"/>`,
  },
  {
    n: 'merkle',
    c: 'Centra AI Core',
    k: 'tree hash proof anchor root leaves verify',
    b: `<circle cx="12" cy="4.5" r="1.7"/><circle cx="7" cy="12" r="1.7"/><circle cx="17" cy="12" r="1.7"/><circle cx="4.5" cy="19.5" r="1.5"/><circle cx="9.5" cy="19.5" r="1.5"/><circle cx="14.5" cy="19.5" r="1.5"/><circle cx="19.5" cy="19.5" r="1.5"/><path d="M10.6 5.6 8.4 10.9"/><path d="M13.4 5.6 15.6 10.9"/><path d="M6.2 13.6 5.3 18"/><path d="M7.8 13.6 8.7 18"/><path d="M16.2 13.6 15.3 18"/><path d="M17.8 13.6 18.7 18"/>`,
  },
  {
    n: 'sign',
    c: 'Centra AI Core',
    k: 'signature approve authorize endorse commit consent',
    b: `<path d="M4 16.5c2.4 0 3-7 5.4-7 1.8 0 1.1 4 2.9 4 1.3 0 1.7-2 3.4-2"/><path d="M4 20.5h16"/><path d="M17.6 8.7l1.2-1.2a1.2 1.2 0 0 0-1.7-1.7L15.9 6"/>`,
  },
  {
    n: 'correction',
    c: 'Centra AI Core',
    k: 'drift fix revise undo amend repair adjust',
    b: `<path d="M5 10h7.5a4.5 4.5 0 0 1 0 9H9"/><path d="M5 10l3.2-3.2"/><path d="M5 10l3.2 3.2"/><path d="M17.5 4l.9 2 2 .9-2 .9-.9 2-.9-2-2-.9 2-.9.9-2z"/>`,
  },
  {
    n: 'versioned-uri',
    c: 'Centra AI Core',
    k: 'pinned reference commit version snapshot resolve uri',
    b: `<circle cx="12" cy="12" r="3.2"/><path d="M12 3.5v5.3"/><path d="M12 15.2v5.3"/>`,
  },
  {
    n: 'did',
    c: 'Centra AI Core',
    k: 'decentralized identifier identity card credential profile',
    b: `<rect x="2.5" y="6" width="19" height="12" rx="2"/><circle cx="8" cy="11.5" r="2.4"/><path d="M5 16a3.5 3.5 0 0 1 6 0"/><path d="M14 10.5h4.5"/><path d="M14 14h3"/>`,
  },
  {
    n: 'orchestration',
    c: 'Centra AI Core',
    k: 'dispatch sub coordinate workflow pipeline route fan',
    b: `<circle cx="5" cy="5.5" r="1.9"/><circle cx="5" cy="12" r="1.9"/><circle cx="5" cy="18.5" r="1.9"/><circle cx="18.5" cy="12" r="2.1"/><path d="M6.9 6.2 16.6 11.1"/><path d="M7 12h9.4"/><path d="M6.9 17.8 16.6 12.9"/>`,
  },
  {
    n: 'attestation',
    c: 'Centra AI Core',
    k: 'seal verify award certify proof stamp attest',
    b: `<circle cx="12" cy="9.5" r="5.5"/><path d="M9.5 9.5l1.8 1.8 3.2-3.4"/><path d="M9 14l-1.5 6.5 4.5-2.4 4.5 2.4L15 14"/>`,
  },

  /* ——— Paxeer Chain ——————————————————————————————— */
  {
    n: 'block',
    c: 'Paxeer Chain',
    k: 'cube ledger blockchain anchor unit data',
    b: `<path d="M12 3.5 20 7.8v8.4L12 20.5 4 16.2V7.8z"/><path d="M4 7.8 12 12l8-4.2"/><path d="M12 12v8.5"/>`,
  },
  {
    n: 'network',
    c: 'Paxeer Chain',
    k: 'decentralized nodes peers distributed mesh paxeer',
    b: `<circle cx="12" cy="5" r="2"/><circle cx="5.5" cy="18" r="2"/><circle cx="18.5" cy="18" r="2"/><path d="M11 6.8 6.5 16.2"/><path d="M13 6.8 17.5 16.2"/><path d="M7.5 18h9"/>`,
  },
  {
    n: 'payments',
    c: 'Paxeer Chain',
    k: 'card pay transfer money transaction credit',
    b: `<rect x="2.5" y="6" width="19" height="12" rx="2"/><path d="M2.5 10h19"/><path d="M6 14.5h4"/>`,
  },
  {
    n: 'liquidity',
    c: 'Paxeer Chain',
    k: 'pool drop water depth provide stake flow',
    b: `<path d="M12 3.5C8.5 8 6.5 11 6.5 14a5.5 5.5 0 0 0 11 0c0-3-2-6-5.5-10.5z"/><path d="M9.5 14.5a2.5 2.5 0 0 0 2 2.3"/>`,
  },
  {
    n: 'reputation',
    c: 'Paxeer Chain',
    k: 'star rating trust score quality rank',
    b: `<path d="M12 4l2.4 4.9 5.4.8-3.9 3.8.9 5.4L12 16.4 7.2 18.9l.9-5.4L4.2 9.7l5.4-.8L12 4z"/>`,
  },
  {
    n: 'orderbook',
    c: 'Paxeer Chain',
    k: 'trade depth bids asks market exchange ledger',
    b: `<path d="M11.5 4v16"/><path d="M11.5 7H6.5"/><path d="M11.5 10H4.5"/><path d="M11.5 13H7"/><path d="M13.5 9h5"/><path d="M13.5 12h6.5"/><path d="M13.5 15h4"/>`,
  },
  {
    n: 'swap',
    c: 'Paxeer Chain',
    k: 'exchange convert trade rotate switch',
    b: `<path d="M5 8.5h12"/><path d="M14 5.5l3 3-3 3"/><path d="M19 15.5H7"/><path d="M10 12.5l-3 3 3 3"/>`,
  },
  {
    n: 'rpc',
    c: 'Paxeer Chain',
    k: 'request response call generic procedure endpoint',
    b: `<circle cx="5" cy="12" r="2"/><circle cx="19" cy="12" r="2"/><path d="M7.5 9.8h7"/><path d="M12.5 7.8l2.5 2-2.5 2"/><path d="M16.5 14.2h-7"/><path d="M11.5 12.2l-2.5 2 2.5 2"/>`,
  },
  {
    n: 'wallet',
    c: 'Paxeer Chain',
    k: 'account funds balance pay easy paxeer',
    b: `<path d="M4 8a2 2 0 0 1 2-2h11a1 1 0 0 1 1 1v1"/><rect x="4" y="8" width="16" height="11" rx="2"/><path d="M20 12.5h-3.3a2 2 0 0 0 0 4H20"/>`,
  },
]
